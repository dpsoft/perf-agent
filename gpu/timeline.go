package gpu

import (
	"cmp"
	"maps"
	"slices"
	"sort"
	"sync"
)

var _ EventSink = (*Timeline)(nil)

// JoinKind reports how an ExecutionView's Launch was attached, if at all.
type JoinKind string

const (
	// JoinExact means the execution's Correlation matched a live launch in
	// the cache directly. This is vendor-provided truth, not a guess.
	JoinExact JoinKind = "exact"
	// JoinHeuristic means no exact correlation match was found and the
	// launch was instead inferred from queue, kernel name and timing. It is
	// always paired with Heuristic=true so it can never be mistaken for
	// JoinExact by a consumer that only checks the Launch pointer.
	JoinHeuristic JoinKind = "heuristic"
)

// ExecutionView is one kernel execution joined (or not) back to the launch
// that produced it, plus any PC samples attributed to it.
type ExecutionView struct {
	Launch    *GPUKernelLaunch `json:"launch,omitempty"`
	Exec      GPUKernelExec    `json:"exec"`
	PCSamples []GPUPCSample    `json:"pc_samples,omitempty"`
	Join      JoinKind         `json:"join,omitempty"`
	Heuristic bool             `json:"heuristic"`

	// Ambiguous means the heuristic LAUNCH join picked one of several
	// candidate launches. It says nothing about PC samples, and nothing about
	// PC samples ever sets it - see PCAttrib for why the two are kept apart
	// and what would break if they were not.
	Ambiguous bool `json:"ambiguous,omitempty"`

	// PCAttrib is gpu_pc_attrib: how the PC samples above reached this
	// execution. Empty exactly when PCSamples is empty, since there is then
	// nothing to describe; one of PCAttribs() otherwise.
	PCAttrib PCAttrib `json:"pc_attrib,omitempty"`

	// Serialized is the gpu_serialized disclosure: whether this execution's
	// measured duration was perturbed by the profiler serializing kernels
	// around it. Set on EVERY execution, in every tier, by Snapshot — never
	// left to a caller and never omitted.
	//
	// It is NOT `omitempty`, and the JSON tag says so on purpose. An absent
	// field would read as "not perturbed" to a consumer that does not know to
	// check for its absence, which is the same failure gpu_join's
	// unconditional label exists to prevent. The type's zero value is
	// "unknown" for the same reason (see SerializationState).
	Serialized SerializationState `json:"serialized"`
}

// TimelineDropStats counts what Timeline's own bounded storage evicted.
// LaunchCacheStats already covers launch attribution loss; this covers the
// exec/event/module ring buffers and the pending-PC-sample map layered on
// top of it. Modeled after SinkStats: nothing bounded is allowed to evict
// silently.
type TimelineDropStats struct {
	EvictedExecutions     uint64 `json:"evicted_executions,omitempty"`
	EvictedEvents         uint64 `json:"evicted_events,omitempty"`
	EvictedModules        uint64 `json:"evicted_modules,omitempty"`
	EvictedPendingSamples uint64 `json:"evicted_pending_samples,omitempty"`

	// EvictedPendingModuleSamples is EvictedPendingSamples' counterpart for
	// the correlation-less (continuous-mode) pending store — see
	// Timeline.pendingModule. Kept deliberately SEPARATE rather than summed
	// into EvictedPendingSamples: the two stores are keyed differently, are
	// bounded by different things, and fail for different reasons, so one
	// counter for both would make the two diagnoses indistinguishable at
	// exactly the moment they matter. A non-zero EvictedPendingSamples says
	// executions are not arriving for the correlations their samples carry;
	// a non-zero EvictedPendingModuleSamples says the module index is too
	// small for the workload's distinct device functions, its horizon is
	// shorter than the gap between a sample and its execution, or one
	// device function's samples outran MaxPendingSamplesPerCorrelation
	// because its execution never came. All three of the latter are Tier B
	// eviction storms and none of them is a Tier A problem.
	//
	// Zero on a healthy run of either tier.
	EvictedPendingModuleSamples uint64 `json:"evicted_pending_module_samples,omitempty"`

	// EvictedSamplingWindows counts PC-sampling bursts dropped from the
	// bounded serialization-disclosure store (see windowStore). Eviction there
	// only ever moves an execution's answer from "false" towards "unknown" —
	// never the other way — so a non-zero value here costs certainty, not
	// correctness. Zero on any run shorter than the store's bound.
	EvictedSamplingWindows uint64 `json:"evicted_sampling_windows,omitempty"`
}

// Snapshot is a point-in-time, fully-joined view of everything Timeline
// currently holds.
//
// Events' Device/Queue/Attributes fields are cloned once, at EmitEvent
// ingest time (see cloneTimelineEvent), so they never alias a backend's own
// buffers. They are not cloned again per Snapshot call: like
// LaunchCache.Get's return value, the events here should be treated as
// read-only by callers, since repeated Snapshot calls return values backed
// by the same ring storage.
type Snapshot struct {
	Executions  []ExecutionView    `json:"executions"`
	Events      []GPUTimelineEvent `json:"events,omitempty"`
	Modules     []GPUModule        `json:"modules,omitempty"`
	JoinStats   JoinStats          `json:"join_stats,omitempty"`
	LaunchCache LaunchCacheStats   `json:"launch_cache,omitempty"`
	Dropped     TimelineDropStats  `json:"dropped,omitempty"`

	// SinkStats is the ingestion-side loss record - review Important 2. It
	// is the zero value unless populated by the caller (see
	// CountingSink.SnapshotWith): Timeline itself never sees a
	// CountingSink, only whatever EventSink wraps it, so it cannot fill
	// this in on its own. Without it, admission-side losses (a full token
	// bucket, a rejected clock domain) were unreachable from a serialized
	// Snapshot even though they directly affect what the profile can show.
	SinkStats SinkStats `json:"sink_stats,omitempty"`

	// AttributedPCSamples counts PC samples that were attached to an
	// execution in THIS snapshot (i.e. included in some ExecutionView's
	// PCSamples above) - review Important 2's PC-sample reconciliation: the
	// exec path had no accounting at all for "attributed" vs "still
	// pending" before this.
	AttributedPCSamples uint64 `json:"attributed_pc_samples,omitempty"`

	// PendingSamples and PendingCorrelations are gauges (not deltas) of
	// what remains in Timeline's pending store immediately after this
	// snapshot: PendingCorrelations counts distinct not-yet-matched
	// correlations, PendingSamples counts the individual GPUPCSample
	// entries they hold in total. Together with AttributedPCSamples and
	// Dropped.EvictedPendingSamples, these let a caller reconcile every PC
	// sample ingested into exactly one of attributed / still pending /
	// evicted.
	PendingSamples      int `json:"pending_samples,omitempty"`
	PendingCorrelations int `json:"pending_correlations,omitempty"`

	// PendingModuleSamples and PendingModuleGroups are the same pair of
	// gauges for the correlation-less pending store (Timeline.pendingModule):
	// PendingModuleGroups counts distinct {backend, pid, cubin CRC, function
	// index} groups held, PendingModuleSamples the individual GPUPCSample
	// entries across them. Together with Dropped.EvictedPendingModuleSamples
	// they close the reconciliation identity for continuous-mode samples the
	// same way the correlation-keyed trio does for correlation-bearing ones:
	// every PC sample the sink accepted is attributed, still pending in one
	// of the two stores, or evicted from one of them.
	//
	// A group that this Snapshot's join could not place is left here rather
	// than attached to a plausible neighbour, so a persistently high
	// PendingModuleSamples alongside a low PCJoin.AttributedKernel means the
	// join is not finding executions for these kernels - see PCJoin for which
	// of the four reasons it was.
	PendingModuleSamples int `json:"pending_module_samples,omitempty"`
	PendingModuleGroups  int `json:"pending_module_groups,omitempty"`

	// PCJoin accounts for the module-keyed PC join: how each pending group
	// fared, how the attributed samples split between the two indexes, and how
	// many executions ended up carrying an inferred gpu_pc_attrib. See
	// PCJoinStats.
	PCJoin PCJoinStats `json:"pc_join,omitempty"`
	// ---- The serialization disclosure (Tier A).

	// SamplingWindowsReceived is the CUMULATIVE number of PC-sampling burst
	// records accepted into the disclosure store, and SamplingWindowsHeld /
	// SamplingWindowsOpen are gauges of what it holds right now. A burst
	// reaches the wire twice — open at its start, closed at its end — so on a
	// clean Tier A run Received is about twice the number of bursts and Held
	// is the number of distinct bursts.
	//
	// SamplingWindowsOpen is the one to read: non-zero means a burst's end is
	// unknown, which is what a hard exit mid-burst looks like, and every
	// execution from that burst's start onward reads "unknown".
	//
	// Zero everywhere in Tier B and with sampling off, where nothing is ever
	// serialized and no window is ever emitted.
	SamplingWindowsReceived uint64 `json:"sampling_windows_received,omitempty"`
	SamplingWindowsHeld     int    `json:"sampling_windows_held,omitempty"`
	SamplingWindowsOpen     int    `json:"sampling_windows_open,omitempty"`

	// The three gpu_serialized outcomes for the executions in THIS snapshot.
	//
	// THEY SUM TO len(Executions), EXACTLY. That identity is the same
	// discipline gpu_join's three outcomes carry, and it is what makes the
	// disclosure auditable rather than decorative: an execution that fell
	// through every branch would show up as a shortfall in the sum instead of
	// silently reading as one of the three. gpu/conformance_test.go asserts
	// it, including on an empty snapshot.
	//
	// ExecutionsSerializationUnknown is the one that matters. It must never
	// be reported as ExecutionsNotSerialized: "not perturbed" when the truth
	// is "cannot tell" is precisely what spec §4 forbids.
	ExecutionsSerialized           uint64 `json:"executions_serialized,omitempty"`
	ExecutionsNotSerialized        uint64 `json:"executions_not_serialized,omitempty"`
	ExecutionsSerializationUnknown uint64 `json:"executions_serialization_unknown,omitempty"`
}

// TimelineConfig configures Timeline's storage bounds.
type TimelineConfig struct {
	// LaunchCache bounds the launch cache (see LaunchCacheConfig). Its
	// normalized Capacity also bounds Timeline's exec/module ring buffers and
	// the pending-PC-sample map's cardinality: there is no separate capacity
	// field for them, and their volume tracks launch volume closely enough to
	// share one dial. EventCapacity is the one exception - see its own doc
	// comment.
	LaunchCache LaunchCacheConfig

	// EventCapacity bounds the GPUTimelineEvent ring independently of
	// LaunchCache.Capacity. Spec §7/§12 describes GPUTimelineEvent as raw
	// syscall/ioctl traffic, plausibly 10-100x launch volume - sizing it off
	// the launch-cache capacity (as every other store here does) would be
	// wrong in either direction: too small for event volume, or forced
	// artificially large just to fit events, wasting memory on the launch
	// cache and exec/module rings. Zero means "use LaunchCache's normalized
	// capacity", the historical default, so existing callers don't change
	// behavior.
	EventCapacity int

	// LaunchEventJoinWindowNs bounds how far back (in nanoseconds, in the CPU
	// monotonic domain) the heuristic join in Snapshot may look for a
	// candidate launch preceding an execution's StartNs. Without a bound, a
	// long-running kernel name shared across a workload's whole lifetime
	// makes every earlier launch on that (queue, kernel name) group a
	// "qualifying" candidate forever, which both risks attaching an
	// execution to a launch from an unrelated iteration and makes Ambiguous
	// permanently true (every repeated kernel accumulates candidates without
	// bound), carrying no information. Spec §10 calls the heuristic
	// "bounded-time"; this is that bound. Zero means unbounded - preserved
	// as the default so existing callers and tests that never set it keep
	// today's behavior; a caller that wants the bound (any real backend
	// using the heuristic path) sets this explicitly.
	LaunchEventJoinWindowNs uint64

	// MaxPendingSamplesPerCorrelation caps how many PC samples one
	// not-yet-matched correlation may accumulate in Timeline.pending before
	// further samples for it are dropped and counted in
	// Dropped.EvictedPendingSamples, instead of appended without bound.
	// evictPendingLocked's cardinality eviction bounds the number of
	// *distinct* orphaned correlations; without this, a single correlation
	// below that bound (e.g. one that never gets an exec, or an exec that
	// keeps producing samples under a stale ID) can still accumulate
	// unbounded memory on its own. Zero means
	// defaultMaxPendingSamplesPerCorrelation.
	MaxPendingSamplesPerCorrelation int

	// PendingSampleHorizonNs bounds how far behind the newest observed
	// PC-sample timestamp a pending correlation's most recent sample may
	// fall before that correlation is evicted as an orphan, in addition to
	// the cardinality bound (pendingCap). Mirrors LaunchCacheConfig.HorizonNs.
	// Zero disables horizon-based pending eviction, leaving only the
	// cardinality bound. It applies to BOTH pending stores: the
	// correlation-keyed one and the module-keyed one (Timeline.pendingModule)
	// share this horizon, because they hold the same kind of thing (a PC
	// sample waiting for its execution) for the same reason and over the same
	// producer drain interval. What they do not share is the counter the
	// eviction lands on.
	PendingSampleHorizonNs uint64

	// MaxPendingModuleGroups bounds how many distinct {backend, pid, cubin
	// CRC, function index} groups the correlation-less pending store may hold
	// before the oldest are evicted into
	// Dropped.EvictedPendingModuleSamples. It is deliberately NOT
	// LaunchCache.Capacity, which every other store here reuses: that
	// capacity tracks launch volume, and launch volume is a function of run
	// length, whereas this store's cardinality is a function of the profiled
	// *binary* — one group per device function per process, tens to low
	// hundreds for a real workload. Sizing it off launch volume would reserve
	// a 65,536-entry map for a workload that will only ever fill fifty of it.
	//
	// Zero means defaultMaxPendingModuleGroups.
	MaxPendingModuleGroups int

	// Modules is the module store the Snapshot join resolves a pending
	// group's (cubin CRC, functionIndex) through to a device function name.
	// It is injected rather than owned because it is filled by the cubin
	// transport, not by Timeline, and because the projection resolves source
	// lines against the same store: one store, two readers, no second copy of
	// the bytes.
	//
	// Nil is a supported configuration and is what every backend that does
	// not do PC sampling passes. With no store, no correlation-less group can
	// be named, so none is joined - every one of them is counted in
	// PCJoinStats.GroupsUnresolvedName and left pending. That is deliberately
	// the same accounting as "the cubin never arrived", because for the
	// profile it is the same fact: there is nothing to resolve the group
	// against. It is NOT silently skipped, which would make a missing store
	// look identical to a healthy run with no PC samples.
	Modules *ModuleStore
	// SerializedSampling says that KERNEL_SERIALIZED PC sampling (Tier A) was
	// SELECTED for this run. It is the agent's own configuration, not
	// something inferred from the wire, and that is the point: "Tier A was
	// asked for and no window arrived" and "Tier A was never asked for" are
	// different facts with different answers, and only the agent knows which
	// one holds.
	//
	// FALSE (the default) means every execution is gpu_serialized="false",
	// unconditionally and correctly — with sampling off or in continuous
	// collection nothing is ever serialized, so there is nothing to be unsure
	// about.
	//
	// TRUE routes every execution through the window store, where the answer
	// is "true", "false" or "unknown" depending on the evidence. Task 11 owns
	// the setting that flips this; nothing here selects a tier.
	SerializedSampling bool

	// MaxSamplingWindowsPerPID and MaxSamplingWindowPIDs bound the disclosure
	// store. Zero means defaultMaxSamplingWindowsPerPID /
	// defaultMaxSamplingWindowPIDs.
	MaxSamplingWindowsPerPID int
	MaxSamplingWindowPIDs    int
}

// Timeline is the indexed join point: it ingests launches, executions, PC
// samples, modules and lifecycle events, and produces a Snapshot that joins
// executions back to the launch that produced them. Correlation lookups go
// through LaunchCache (O(1)/O(capacity)) rather than the earlier prototype's
// linear scan of every launch ever seen.
type Timeline struct {
	mu sync.Mutex

	cache *LaunchCache

	// launchesSinceSnapshot counts EmitLaunch calls since the last Snapshot,
	// reset to 0 by Snapshot. It used to be a lifetime cumulative counter
	// (launchCount), which review Important 1 flagged as meaningless once
	// every other JoinStats field is per-snapshot: after the second
	// Snapshot, "unmatched = lifetime launches - this snapshot's matches"
	// mixes two different time scales and is not a loss figure.
	launchesSinceSnapshot uint64

	// joinWindowNs is TimelineConfig.LaunchEventJoinWindowNs, copied out at
	// construction time. Zero means unbounded (see that field's doc
	// comment).
	joinWindowNs uint64

	execs *ring[GPUKernelExec]

	// pending holds PC samples keyed by correlation ID - which carries the
	// producing process, so two processes reusing the same vendor value never
	// share a bucket (see CorrelationID) - until an exec with a matching
	// Correlation is joined at Snapshot time. A correlation's
	// samples are deleted from pending the moment they are attached to an
	// execution's view - they are consumed, not cached indefinitely. Since
	// review Critical 3 made Snapshot drain execs the same way (an exec is
	// reported in exactly one Snapshot call, never re-reported in a later
	// one), an exec's attach is a one-time event by construction: whatever
	// samples had accumulated in pending for its Correlation by that moment
	// are handed to it, once, and there is no "later Snapshot call for the
	// same still-live execution" for more to arrive into. A PC sample that
	// arrives under that same correlation ID afterward - the ID having been
	// consumed already - starts a fresh, ordinary pending entry (a new
	// generation; see the seq discussion below), exactly like any other
	// correlation whose exec hasn't arrived yet, not a continuation of the
	// one already delivered. Samples that are never claimed (exec never
	// arrives, or ages out of the ring first) are orphans; pendingCap/
	// pendingOrder/pendingHead bound how many distinct correlations' worth
	// of orphans can accumulate, evicting the oldest and counting it in
	// Dropped.EvictedPendingSamples.
	//
	// Both pending's entries and pendingOrder's positions carry a sequence
	// number, the same way LaunchCache pairs cacheEntry.seq with orderedFIFO
	// (see LaunchCache.isLiveLocked). This exists to solve the identical
	// hazard: a correlation ID consumed by Snapshot leaves its old
	// pendingOrder position behind with no map entry; if that same ID is
	// then reused by a later EmitPCSample, presence-only eviction cannot
	// tell that stale position apart from the ID's new, live generation, and
	// would delete the freshly re-inserted entry instead of the position it
	// actually corresponds to. A new sequence is assigned only on the
	// absent-to-present transition (a brand new correlation, or a reused one
	// after consumption/eviction), not on every append to an already-live
	// entry - unlike LaunchCache.Put, which always replaces the whole value,
	// pending accumulates samples across many EmitPCSample calls for the
	// same still-live generation, so an in-place append must not look like a
	// new generation. This divergence in bump cadence (vs. LaunchCache's
	// every-call bump) is deliberate; see EmitPCSample.
	//
	// pending itself stays a plain, owner-held map (not hidden inside a
	// generic type) because existing tests reach into it directly
	// (len(tl.pending), tl.pending[id], tl.pending[id].samples); only the
	// order/sequence/compaction bookkeeping is shared, via orderedFIFO, with
	// LaunchCache.
	pending      map[CorrelationID]pendingSamples
	pendingOrder *orderedFIFO[CorrelationID]
	pendingCap   int

	// pendingSampleCap bounds how many samples a single pending correlation's
	// entry may accumulate - review Critical 5: pendingCap above bounds the
	// number of distinct orphaned correlations, not the samples within any
	// one of them, so a single correlation below that bound could still grow
	// without bound. See TimelineConfig.MaxPendingSamplesPerCorrelation.
	pendingSampleCap int

	// pendingHorizonNs/pendingNewestNs age pending entries by time, the same
	// way LaunchCache's HorizonNs/newestNs age launches: pendingNewestNs
	// tracks the largest PC-sample TimeNs observed (clamped against a single
	// anomalous jump the same way LaunchCache.observeTimestampLocked is, via
	// the shared defaultMaxAdvanceNs bound); evictPendingLocked drops
	// pending entries whose most recent sample has fallen more than
	// pendingHorizonNs behind that anchor. Zero pendingHorizonNs disables
	// this pass, leaving only the cardinality-based eviction below it.
	pendingHorizonNs uint64
	pendingNewestNs  uint64

	// pendingSampleTotal is the running count of individual GPUPCSample
	// entries currently stored across every pending correlation - review
	// Important 2's PendingSamples gauge. Maintained incrementally (+1 on
	// every successful append, -len(samples) at every deletion site, in
	// EmitPCSample/evictPendingLocked/Snapshot) rather than recomputed by
	// scanning t.pending on every Snapshot call, which would add an
	// O(pendingCap) cost to every snapshot even when nothing changed.
	pendingSampleTotal uint64

	// pendingModule is the second pending index: the one a PC sample that
	// carries NO correlation value lands in. CUPTI populates a PC record's
	// correlationId only in KERNEL_SERIALIZED collection; in CONTINUOUS mode
	// — the only mode that is a candidate for always-on, because it does not
	// serialize kernels — it is zero on every record.
	//
	// Why a second index at all, rather than letting those samples share
	// pending above. pending keys on the whole CorrelationID, and a
	// correlation-less sample still arrives with Backend and PID filled in
	// (they are context the producer knows regardless — see
	// CorrelationID.Present), so EVERY such sample from one process hashes to
	// the single key {backend, pid, ""}. Two things then go wrong at once and
	// neither is visible as what it is: pendingSampleCap bounds an entire
	// process's PC samples to that one entry and evicts the rest, and
	// Snapshot's t.pending[exec.Correlation] lookup matches none of them
	// anyway, because no execution ever carries an empty correlation value.
	// The observable symptom is "PC sampling produces almost nothing" with an
	// eviction counter as the only clue — spec §6.1's pathology for
	// correlation-less launches, reached here through a different door.
	//
	// The key is {Backend, PID, CubinCRC, FunctionIndex}: the coarsest
	// identity a continuous-mode sample actually carries, and the one the
	// module store can resolve back to a device function. The PID is IN the
	// key rather than in a check performed somewhere else, which is issue
	// #52's discipline: two processes running the identical binary produce
	// the identical cubin CRC and the identical function index, so the only
	// thing separating their samples is the PID, and a structural refusal
	// cannot be forgotten by a later edit the way a guard can. Backend is
	// present for the same reason CorrelationID carries it — nothing today
	// emits PC samples from two backends into one Timeline, and this costs
	// nothing to make impossible.
	//
	// pendingModule holds pendingSamples values, identical in shape and
	// contract to pending's: the seq/generation pairing with an orderedFIFO
	// (see the pending field's doc comment for the deleted-then-reused hazard
	// it exists to solve, which applies here unchanged) and anchorNs as the
	// O(1) horizon anchor. Reusing orderedFIFO rather than writing a third
	// eviction walk is deliberate — LaunchCache and pending each grew a
	// hand-written copy of it and each hit the same liveness bug separately.
	// (ModuleStore deliberately does not reuse it, but for a reason specific
	// to LRU touching: an LRU has to move an entry to the front on every
	// *read*, which orderedFIFO has no notion of. This store is pure FIFO by
	// arrival like pending, so that reason does not apply.)
	//
	// Snapshot consumes from this store exactly as it consumes from pending,
	// via joinPendingModuleLocked: a group whose (CRC, functionIndex) names a
	// device function that an execution in the snapshot carries as its
	// KernelName is handed over whole and deleted. A group that cannot be
	// named, or that finds no execution, is left here untouched and stays
	// eligible for a later Snapshot - never attached to a plausible
	// neighbour.
	pendingModule            map[pendingModuleKey]pendingSamples
	pendingModuleOrder       *orderedFIFO[pendingModuleKey]
	pendingModuleCap         int
	pendingModuleSampleTotal uint64

	// modstore resolves a pendingModule key's (CRC, functionIndex) to a
	// device function name at Snapshot time. Nil is supported - see
	// TimelineConfig.Modules. Named apart from the modules ring below, which
	// records THAT a module loaded; this one holds what a module IS.
	modstore *ModuleStore

	// devicesByPID is the multi-GPU guard's whole state: for each process
	// that has produced an execution naming a device, the first device id
	// seen and whether a second, different one has since arrived.
	//
	// It exists because gpu_pc_sample_batch_v1 carries no device id and two
	// devices running the same binary produce the same cubin CRC, so a
	// process's PC samples are indistinguishable BETWEEN its devices on the
	// wire. There is no way to make the join right in that case; there is
	// only a way to stop it from looking right, which is what
	// PCAttribKernelMultiDevice does. Detection is on executions, which do
	// carry a device id.
	//
	// It is cumulative rather than per-snapshot on purpose. The condition is
	// a property of the process, and a process that used two devices in an
	// earlier snapshot has not stopped having done so; per-snapshot detection
	// would clear the mark on exactly the snapshots where only one device
	// happened to report, which is the reading-green-when-worst failure this
	// project keeps hitting.
	//
	// It is bounded, because a system-wide profile has no a-priori bound on
	// distinct pids. Past maxTrackedDeviceProcesses a new process is not
	// admitted and is therefore treated as single-device - the right guess,
	// and still a guess, which is why the refusal is counted in
	// deviceTrackingCapped and raised in joinhealth rather than absorbed.
	devicesByPID         map[uint32]processDevices
	multiDeviceProcesses uint64
	deviceTrackingCapped uint64

	events  *ring[GPUTimelineEvent]
	modules *ring[GPUModule]

	// windows is the serialization disclosure's evidence store: the
	// PC-sampling bursts this Timeline has been told about, per process. It is
	// consulted at Snapshot rather than at EmitExec because a burst's closing
	// record routinely arrives after the executions it covers — the producer
	// drains on a timer — so classifying at ingest would mark a burst's own
	// executions "unknown" for the very reason the window exists.
	//
	// serializedSampling is TimelineConfig.SerializedSampling, copied out at
	// construction. When it is false this store is never consulted and every
	// execution is "false", which is unconditionally correct in that
	// configuration.
	windows            *windowStore
	serializedSampling bool

	dropped TimelineDropStats
}

// processDevices is one process's entry in Timeline.devicesByPID: the first
// device id observed for it, and whether a different one has since been seen.
// Only two states matter to the join (one device, or more than one), so the
// set itself is never retained - the whole point is a decision, not an
// inventory, and retaining the set would make the guard's memory a function of
// the machine's GPU count for no gain.
type processDevices struct {
	first string
	multi bool
}

// maxTrackedDeviceProcesses bounds Timeline.devicesByPID. One entry is a pid,
// a short device id string and a bool; 4,096 of them is a few hundred
// kilobytes at most, and reaching it needs thousands of concurrently profiled
// processes that each run GPU kernels. See the devicesByPID doc comment for
// what happens past it and why that is counted.
const maxTrackedDeviceProcesses = 4096

// pendingSamples is a pending correlation's accumulated samples, tagged with
// the sequence number of its current (live) generation - see the pending
// field's doc comment. anchorNs is the largest PC-sample TimeNs appended to
// samples so far (a running max, updated incrementally by EmitPCSample),
// mirroring cacheEntry's use of its own launch.TimeNs as an O(1) horizon
// anchor - before this refactor, the equivalent value was recomputed by
// latestSampleTimeNs scanning every sample in the entry (up to
// pendingSampleCap of them) on every evictPendingLocked call while the
// horizon was enabled; anchorNs makes that O(1) instead.
type pendingSamples struct {
	samples  []GPUPCSample
	seq      uint64
	anchorNs uint64
}

// defaultMaxPendingSamplesPerCorrelation bounds a single pending
// correlation's accumulated samples when TimelineConfig doesn't set
// MaxPendingSamplesPerCorrelation. Chosen generously relative to a realistic
// PC-sampling batch (tens to low hundreds of samples between exec arrivals)
// while still being a real, enforced bound rather than the previous
// unbounded append.
const defaultMaxPendingSamplesPerCorrelation = 4096

// pendingModuleKey is Timeline.pendingModule's key: the identity a PC sample
// with no correlation value still carries. Every field is comparable and
// sized, so this is a plain map key with no allocation and no hashing helper.
//
// The PID is a field of the key, not a filter applied to it. See the
// pendingModule doc comment.
type pendingModuleKey struct {
	Backend       GPUBackendID
	PID           uint32
	CubinCRC      uint64
	FunctionIndex uint32
}

// pendingModuleKeyFor derives the key from the sample. It reads the PID off
// the Correlation (which carries it even when Value is empty) and the CRC and
// backend off the Module, so a producer that fills in neither still keys
// consistently — into a single group per process, which is the honest answer
// when a sample genuinely carries no module identity, rather than a fabricated
// distinction.
func pendingModuleKeyFor(p GPUPCSample) pendingModuleKey {
	backend := p.Module.Backend
	if backend == "" {
		backend = p.Correlation.Backend
	}
	return pendingModuleKey{
		Backend:       backend,
		PID:           p.Correlation.PID,
		CubinCRC:      p.Module.CRC,
		FunctionIndex: p.FunctionIndex,
	}
}

// defaultMaxPendingModuleGroups bounds Timeline.pendingModule's cardinality
// when TimelineConfig doesn't set MaxPendingModuleGroups. One group is one
// device function in one process; a real workload has tens to low hundreds,
// and even a template- or JIT-heavy one that loads thousands of cubins is
// covered here with room to spare. It is not sized off launch volume — see
// TimelineConfig.MaxPendingModuleGroups for why that would be the wrong
// dial.
//
// Worst case with the defaults, and it is worth stating plainly: 4,096 groups
// x 4,096 samples per group of GPUPCSample. That ceiling is only reachable by
// a producer emitting thousands of distinct device functions whose executions
// never arrive at all, which is itself the condition
// EvictedPendingModuleSamples exists to report.
const defaultMaxPendingModuleGroups = 4096

// NewTimeline constructs a Timeline. cfg.LaunchCache is passed through to
// NewLaunchCache unmodified (including its own zero-value defaulting); the
// cache's own normalized capacity is reused for the exec/module rings and
// the pending-sample cardinality bound rather than re-deriving it, so the
// default can't drift between LaunchCache and Timeline. EventCapacity,
// MaxPendingSamplesPerCorrelation and PendingSampleHorizonNs each get their
// own defaulting - see their doc comments on TimelineConfig.
func NewTimeline(cfg TimelineConfig) *Timeline {
	cache := NewLaunchCache(cfg.LaunchCache)
	capacity := cache.cfg.Capacity

	eventCapacity := cfg.EventCapacity
	if eventCapacity <= 0 {
		eventCapacity = capacity
	}

	sampleCap := cfg.MaxPendingSamplesPerCorrelation
	if sampleCap <= 0 {
		sampleCap = defaultMaxPendingSamplesPerCorrelation
	}

	moduleGroupCap := cfg.MaxPendingModuleGroups
	if moduleGroupCap <= 0 {
		moduleGroupCap = defaultMaxPendingModuleGroups
	}

	return &Timeline{
		cache:            cache,
		joinWindowNs:     cfg.LaunchEventJoinWindowNs,
		execs:            newRing[GPUKernelExec](capacity),
		events:           newRing[GPUTimelineEvent](eventCapacity),
		modules:          newRing[GPUModule](capacity),
		pending:          make(map[CorrelationID]pendingSamples),
		pendingOrder:     newOrderedFIFO[CorrelationID](0),
		pendingCap:       capacity,
		pendingSampleCap: sampleCap,
		pendingHorizonNs: cfg.PendingSampleHorizonNs,

		pendingModule: make(map[pendingModuleKey]pendingSamples),
		// No capacity hint, matching pending's orderedFIFO: the store is
		// bounded by cardinality but a workload that never uses continuous-
		// mode sampling should not pay for a preallocated slice.
		pendingModuleOrder: newOrderedFIFO[pendingModuleKey](0),
		pendingModuleCap:   moduleGroupCap,
		modstore:           cfg.Modules,

		devicesByPID: make(map[uint32]processDevices),

		windows:            newWindowStore(cfg.MaxSamplingWindowsPerPID, cfg.MaxSamplingWindowPIDs),
		serializedSampling: cfg.SerializedSampling,
	}
}

// EmitSamplingWindow records one PC-sampling burst.
//
// It is accepted in EVERY configuration, not only when SerializedSampling is
// set. A producer that is emitting windows is a producer that is bursting, and
// dropping the evidence because the agent's own config disagrees would leave
// the two ends silently out of step. What SerializedSampling gates is the
// ANSWER, not the ingest.
func (t *Timeline) EmitSamplingWindow(w GPUSamplingWindow) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	before := t.windows.evicted
	t.windows.add(w)
	t.dropped.EvictedSamplingWindows += t.windows.evicted - before
	return nil
}

// isPendingLiveLocked answers orderedFIFO's isLive callback for pendingOrder:
// a position is live only if t.pending still holds an entry for id stamped
// with exactly seq. The caller must hold t.mu.
func (t *Timeline) isPendingLiveLocked(id CorrelationID, seq uint64) bool {
	cur, ok := t.pending[id]
	return ok && cur.seq == seq
}

// isPendingModuleLiveLocked is isPendingLiveLocked for pendingModuleOrder. The
// hazard it answers is identical and is described on the pending field; the
// stores are separate only so their bounds and their loss counters are.
// The caller must hold t.mu.
func (t *Timeline) isPendingModuleLiveLocked(k pendingModuleKey, seq uint64) bool {
	cur, ok := t.pendingModule[k]
	return ok && cur.seq == seq
}

func (t *Timeline) EmitLaunch(l GPUKernelLaunch) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.launchesSinceSnapshot++
	t.cache.Put(l)
	return nil
}

func (t *Timeline) EmitExec(e GPUKernelExec) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.observeExecDeviceLocked(e)
	if t.execs.push(e) {
		t.dropped.EvictedExecutions++
	}
	return nil
}

// execDeviceID is the device an execution ran on. GPUExecutionRef is the
// authoritative place (it is the execution's own identity); GPUQueueRef's
// device is the fallback for a producer that fills in the queue and not the
// execution ref. Empty means the producer named no device at all, which is not
// evidence of one device or of two and is therefore not an observation.
func execDeviceID(e GPUKernelExec) string {
	if e.Execution.DeviceID != "" {
		return e.Execution.DeviceID
	}
	return e.Queue.Device.DeviceID
}

// observeExecDeviceLocked feeds the multi-GPU guard. It runs on every
// execution, not only when PC sampling is on: the executions that reveal a
// second device are not necessarily the ones whose kernels get sampled, and a
// guard that only watched while sampling was running would learn the fact too
// late to mark anything. Caller holds t.mu.
func (t *Timeline) observeExecDeviceLocked(e GPUKernelExec) {
	device := execDeviceID(e)
	if device == "" {
		return
	}
	pid := e.Correlation.PID

	cur, ok := t.devicesByPID[pid]
	if !ok {
		if len(t.devicesByPID) >= maxTrackedDeviceProcesses {
			t.deviceTrackingCapped++
			return
		}
		t.devicesByPID[pid] = processDevices{first: device}
		return
	}
	if cur.multi || cur.first == device {
		return
	}
	cur.multi = true
	t.devicesByPID[pid] = cur
	t.multiDeviceProcesses++
}

// isMultiDeviceLocked reports whether this process has been observed running
// kernels on more than one device. An untracked process answers false: it has
// produced no execution naming a device, or the tracker was full, and in both
// cases there is no evidence of a second device. The absence of evidence is
// reported honestly by deviceTrackingCapped rather than by turning every
// unknown into a multidevice mark, which would bury the real ones. Caller
// holds t.mu.
func (t *Timeline) isMultiDeviceLocked(pid uint32) bool {
	return t.devicesByPID[pid].multi
}

func (t *Timeline) EmitPCSample(p GPUPCSample) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.observePendingTimestampLocked(p.TimeNs)

	// Correlation.Present() — the presence of a vendor correlation VALUE, not
	// of a non-zero CorrelationID — is the whole of the routing decision, and
	// it is the same gate Snapshot already uses to route a correlation-less
	// execution to the heuristic join. A sample that carries a value keeps
	// taking the exact-correlation path below, unchanged; one that does not
	// would otherwise collapse onto {backend, pid, ""} there. See the
	// pendingModule field's doc comment.
	if !p.Correlation.Present() {
		t.emitPendingModuleLocked(p)
		return nil
	}

	entry, exists := t.pending[p.Correlation]
	if !exists {
		// Absent-to-present transition: either a brand new correlation, or
		// this ID being reused after its previous generation was consumed
		// or evicted. Either way it is a new generation and gets a new
		// sequence number plus a new order position - see the pending
		// field's doc comment for why this must not also happen on every
		// append to an already-live entry. This is the divergence from
		// LaunchCache.Put (which bumps on every call): pending accumulates
		// samples into one live generation across many EmitPCSample calls,
		// so re-stamping per append would falsely orphan the entry's own
		// live order position.
		entry.seq = t.pendingOrder.insert(p.Correlation)
	}

	if len(entry.samples) >= t.pendingSampleCap {
		// Review Critical 5: evictPendingLocked's cardinality bound only
		// limits the number of distinct pending correlations, not the
		// samples within any single one of them. Without this check, one
		// orphaned (or stale-ID-reused) correlation below that bound could
		// still accumulate samples forever. The entry (and its order
		// position/generation/anchor) is left exactly as it was; only the
		// new sample is dropped and counted.
		t.dropped.EvictedPendingSamples++
		t.pending[p.Correlation] = entry
		t.evictPendingLocked()
		return nil
	}

	entry.samples = append(entry.samples, p)
	if p.TimeNs > entry.anchorNs {
		entry.anchorNs = p.TimeNs
	}
	t.pending[p.Correlation] = entry
	t.pendingSampleTotal++
	t.evictPendingLocked()
	return nil
}

// observePendingTimestampLocked advances pendingNewestNs the same way
// LaunchCache.observeTimestampLocked advances newestNs: a single jump larger
// than defaultMaxAdvanceNs is treated as anomalous and does not move the
// anchor, so one corrupt PC-sample timestamp can't push every pending
// correlation past the horizon at once. Caller holds t.mu.
func (t *Timeline) observePendingTimestampLocked(timeNs uint64) {
	if timeNs <= t.pendingNewestNs {
		return
	}
	if t.pendingNewestNs != 0 && timeNs-t.pendingNewestNs > defaultMaxAdvanceNs {
		return
	}
	t.pendingNewestNs = timeNs
}

// evictPendingLocked drops pending correlation groups past the horizon
// first (if pendingHorizonNs > 0), then the oldest ones once the map holds
// more distinct correlations than pendingCap. An order position whose
// sequence no longer matches the map's current entry for that id - because
// Snapshot already consumed it, or a later re-insertion superseded it - is
// skipped rather than double-counted or, worse, mistaken for the live
// generation and evicted in its place. This mirrors LaunchCache.evictLocked
// exactly, for the same reason: presence in the map is not a sufficient
// liveness check when a key can be deleted and then reused - both now
// delegate that walk to orderedFIFO. Caller holds t.mu.
func (t *Timeline) evictPendingLocked() {
	if t.pendingHorizonNs > 0 {
		for {
			id, ok := t.pendingOrder.peekOldestLive(t.isPendingLiveLocked)
			if !ok {
				break
			}
			cur := t.pending[id] // guaranteed present: peekOldestLive just confirmed liveness
			if t.pendingNewestNs <= cur.anchorNs || t.pendingNewestNs-cur.anchorNs <= t.pendingHorizonNs {
				break
			}
			t.pendingOrder.evictOldestLive(t.isPendingLiveLocked)
			delete(t.pending, id)
			t.pendingSampleTotal -= uint64(len(cur.samples))
			t.dropped.EvictedPendingSamples += uint64(len(cur.samples))
		}
	}
	for len(t.pending) > t.pendingCap {
		id, ok := t.pendingOrder.evictOldestLive(t.isPendingLiveLocked)
		if !ok {
			break
		}
		cur := t.pending[id]
		delete(t.pending, id)
		t.pendingSampleTotal -= uint64(len(cur.samples))
		t.dropped.EvictedPendingSamples += uint64(len(cur.samples))
	}
}

// emitPendingModuleLocked is EmitPCSample's body for a sample with no
// correlation value: the same three bounds as the correlation-keyed path
// (per-group sample cap, horizon, cardinality), the same generation
// discipline, and its own counter. Deliberately a near-mirror rather than a
// shared generic: the two stores' keys, bounds and loss counters all differ,
// and the only piece worth sharing — the order/liveness walk, which is where
// both of the earlier hand-written copies had a real bug — already is, via
// orderedFIFO. Caller holds t.mu.
func (t *Timeline) emitPendingModuleLocked(p GPUPCSample) {
	key := pendingModuleKeyFor(p)

	entry, exists := t.pendingModule[key]
	if !exists {
		// Absent-to-present transition only, exactly as pending does it: a
		// group accumulates samples into one live generation across many
		// calls, so re-stamping per append would orphan its own live order
		// position. See the pending field's doc comment.
		entry.seq = t.pendingModuleOrder.insert(key)
	}

	if len(entry.samples) >= t.pendingSampleCap {
		// The per-group bound is MaxPendingSamplesPerCorrelation, shared with
		// the correlation-keyed store: it answers the same question ("how
		// many samples may one not-yet-matched thing accumulate") about the
		// same objects over the same drain interval, and a second dial for it
		// would be two names for one decision. The eviction it causes is
		// still counted separately.
		t.dropped.EvictedPendingModuleSamples++
		t.pendingModule[key] = entry
		t.evictPendingModuleLocked()
		return
	}

	entry.samples = append(entry.samples, p)
	if p.TimeNs > entry.anchorNs {
		entry.anchorNs = p.TimeNs
	}
	t.pendingModule[key] = entry
	t.pendingModuleSampleTotal++
	t.evictPendingModuleLocked()
}

// evictPendingModuleLocked is evictPendingLocked for the module-keyed store:
// horizon first, then cardinality, both delegating the superseded-position
// walk to orderedFIFO. pendingNewestNs is shared with the correlation-keyed
// store — one PC-sample stream, one clock domain, one anomalous-jump clamp —
// while pendingHorizonNs is shared by value and the counter is not shared at
// all. Caller holds t.mu.
func (t *Timeline) evictPendingModuleLocked() {
	if t.pendingHorizonNs > 0 {
		for {
			key, ok := t.pendingModuleOrder.peekOldestLive(t.isPendingModuleLiveLocked)
			if !ok {
				break
			}
			cur := t.pendingModule[key] // guaranteed present: peekOldestLive just confirmed liveness
			if t.pendingNewestNs <= cur.anchorNs || t.pendingNewestNs-cur.anchorNs <= t.pendingHorizonNs {
				break
			}
			t.pendingModuleOrder.evictOldestLive(t.isPendingModuleLiveLocked)
			delete(t.pendingModule, key)
			t.pendingModuleSampleTotal -= uint64(len(cur.samples))
			t.dropped.EvictedPendingModuleSamples += uint64(len(cur.samples))
		}
	}
	for len(t.pendingModule) > t.pendingModuleCap {
		key, ok := t.pendingModuleOrder.evictOldestLive(t.isPendingModuleLiveLocked)
		if !ok {
			break
		}
		cur := t.pendingModule[key]
		delete(t.pendingModule, key)
		t.pendingModuleSampleTotal -= uint64(len(cur.samples))
		t.dropped.EvictedPendingModuleSamples += uint64(len(cur.samples))
	}
}

// pcModuleJoin is joinPendingModuleLocked's result: the per-execution
// attribution it decided, the samples it moved, and the accounting for every
// group it looked at.
type pcModuleJoin struct {
	// attrib is parallel to the execs slice the join was given, empty at every
	// index the join did not attach samples to. It is nil when the join did no
	// work at all, which attribAt handles so callers need no length check.
	attrib  []PCAttrib
	samples uint64
	stats   PCJoinStats
}

func (r pcModuleJoin) attribAt(i int) PCAttrib {
	if i < len(r.attrib) {
		return r.attrib[i]
	}
	return ""
}

// execKernelKey is the identity the module join matches on: the process, and
// the kernel's name. Both halves are load-bearing.
//
// The PROCESS is in the key for the reason issue #52 put it in
// candidateGroupKey: two processes running the identical binary produce the
// identical cubin CRC and the identical function index, so the module side of
// this join cannot distinguish them at all, and only the pid can. It is a
// field of the key rather than a check beside it so there is nothing to
// forget.
//
// The NAME is a plain string comparison against the name the module store read
// out of the cubin's symbol table, and it is deliberately exact. No
// demangling, no prefix or suffix trimming, no "close enough" - the design's
// rule is that kernel identity comes from the module and never from guessing
// on the kernel-name string, and every relaxation of this comparison is a way
// to attach a sample to a kernel that merely looks related. Where the two
// spellings do not match, the group stays pending and is counted; a sample
// left pending is recoverable, a sample on the wrong kernel is not.
type execKernelKey struct {
	pid        uint32
	kernelName string
}

// joinPendingModuleLocked is the continuous-mode half of the Snapshot join:
// for every correlation-less pending group, resolve (cubin CRC,
// functionIndex) through the module store to a device function name, and hand
// the group's samples to the execution of that kernel in this snapshot.
//
// It runs only after the exact-correlation pass, and only over executions that
// pass left unserved - exactServed marks the rest. That ordering is the
// design's: Tier A keeps taking the exact index, untouched, and the module
// path is strictly what happens on a miss.
//
// Four outcomes per group, and they partition the groups examined:
//
//	no process        the producer named no pid; refused (see execKernelKey)
//	unnameable        no module store, no module, or an unknown functionIndex
//	no execution      named, but no eligible execution of that kernel is here
//	joined            attached, with the attribution quality below
//
// The first three leave the group in the store, untouched and still eligible
// for a later Snapshot; they are not losses, and the loss - if the group never
// becomes joinable - is counted where it happens, at horizon eviction.
//
// Attribution quality:
//
//	one execution of that kernel in the snapshot     -> kernel
//	more than one                                    -> kernel-ambiguous
//	either, in a process observed on 2+ devices      -> kernel-multidevice
//
// Ambiguity is counted over EVERY execution of that kernel in the snapshot,
// not only the eligible ones, because "how many invocations of this kernel
// could these samples have come from" is a question about the horizon, not
// about which of them the join happened to be allowed to use.
//
// Where more than one execution qualifies, the group's samples go to the
// earliest of them by StartNs, whole. Splitting them across the candidates would
// manufacture a distribution the data does not contain, and it would make each
// resulting execution look individually more certain than the group is; the
// label is what carries the doubt instead. Caller holds t.mu.
func (t *Timeline) joinPendingModuleLocked(execs []GPUKernelExec, execSamples [][]GPUPCSample, exactServed []bool) pcModuleJoin {
	res := pcModuleJoin{
		stats: PCJoinStats{
			MultiDeviceProcesses: t.multiDeviceProcesses,
			DeviceTrackingCapped: t.deviceTrackingCapped,
		},
	}
	if len(t.pendingModule) == 0 {
		return res
	}

	// byKernel indexes every named execution of this snapshot. len(indices) is
	// the ambiguity test; the first index whose exact pass came up empty is
	// the target.
	byKernel := make(map[execKernelKey][]int, len(execs))
	for i, e := range execs {
		if e.KernelName == "" || e.Correlation.PID == 0 {
			continue
		}
		key := execKernelKey{pid: e.Correlation.PID, kernelName: e.KernelName}
		byKernel[key] = append(byKernel[key], i)
	}

	res.attrib = make([]PCAttrib, len(execs))

	// Map iteration order is randomized, and two groups can legitimately
	// resolve to the same kernel name (the same device function in two cubin
	// generations, after a JIT reload). Walking the keys in a fixed order
	// makes the resulting sample order, and therefore the profile, the same
	// for the same inputs.
	keys := slices.SortedFunc(maps.Keys(t.pendingModule), comparePendingModuleKey)
	for _, key := range keys {
		if key.PID == 0 {
			res.stats.GroupsNoProcess++
			continue
		}
		name, ok := "", false
		if t.modstore != nil {
			name, ok = t.modstore.FunctionName(key.CubinCRC, key.FunctionIndex)
		}
		if !ok {
			res.stats.GroupsUnresolvedName++
			continue
		}
		candidates := byKernel[execKernelKey{pid: key.PID, kernelName: name}]
		target := -1
		for _, i := range candidates {
			// Earliest by StartNs, not first in ring order: executions reach
			// the ring in the order the producer drained them, which is not
			// the order they ran, and "which invocation" must not depend on
			// that. Ties break on the ring index, which is stable.
			if exactServed[i] {
				continue
			}
			if target < 0 || execs[i].StartNs < execs[target].StartNs {
				target = i
			}
		}
		if target < 0 {
			res.stats.GroupsNoExecution++
			continue
		}

		attrib := PCAttribKernel
		if len(candidates) > 1 {
			attrib = PCAttribKernelAmbiguous
		}
		if t.isMultiDeviceLocked(key.PID) {
			attrib = PCAttribKernelMultiDevice
		}

		entry := t.pendingModule[key]
		execSamples[target] = append(execSamples[target], entry.samples...)
		res.attrib[target] = worsePCAttrib(res.attrib[target], attrib)
		res.samples += uint64(len(entry.samples))
		res.stats.GroupsJoined++

		delete(t.pendingModule, key)
		t.pendingModuleSampleTotal -= uint64(len(entry.samples))
	}

	// Counted over executions after the fact, not incremented per group: two
	// groups landing on one execution are one execution carrying the label,
	// and incrementing inline would double-count it.
	for _, a := range res.attrib {
		switch a {
		case PCAttribKernelAmbiguous:
			res.stats.AmbiguousAttributions++
		case PCAttribKernelMultiDevice:
			res.stats.MultiDeviceAttributions++
		case PCAttribExact, PCAttribKernel:
		}
	}
	res.stats.AttributedKernel = res.samples
	return res
}

// comparePendingModuleKey orders the pending module store's keys for the
// deterministic walk in joinPendingModuleLocked. The field order is
// arbitrary; only its stability matters.
func comparePendingModuleKey(a, b pendingModuleKey) int {
	if c := cmp.Compare(a.PID, b.PID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.CubinCRC, b.CubinCRC); c != 0 {
		return c
	}
	if c := cmp.Compare(a.FunctionIndex, b.FunctionIndex); c != 0 {
		return c
	}
	return cmp.Compare(string(a.Backend), string(b.Backend))
}

func (t *Timeline) EmitModule(m GPUModule) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.modules.push(m) {
		t.dropped.EvictedModules++
	}
	return nil
}

func (t *Timeline) EmitEvent(e GPUTimelineEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.events.push(cloneTimelineEvent(e)) {
		t.dropped.EvictedEvents++
	}
	return nil
}

// cloneTimelineEvent deep-copies the reference-typed fields of a
// GPUTimelineEvent (Device, Queue, Attributes) so Timeline's stored copy
// never aliases the caller's own memory. A high-throughput producer that
// reuses its event buffer after Emit returns would otherwise corrupt
// already-ingested events out from under the mutex. Events are small and
// these fields are optional, so cloning on ingest (once) is cheap relative
// to the alternative of documenting yet another aliasing contract.
func cloneTimelineEvent(in GPUTimelineEvent) GPUTimelineEvent {
	out := in
	out.Attributes = maps.Clone(in.Attributes)
	if in.Device != nil {
		device := *in.Device
		out.Device = &device
	}
	if in.Queue != nil {
		queue := *in.Queue
		out.Queue = &queue
	}
	return out
}

// Snapshot builds the joined view and CONSUMES everything Timeline itself
// guards: execs, events and modules are drained from their rings (a second
// consecutive Snapshot call returns none of them unless new ones arrived in
// between), and PC samples matched to an execution in this call are removed
// from the pending stores - both of them, correlation-keyed and module-keyed -
// the same way. This is a deliberate, uniform
// lifecycle across all five stores - see review Critical 3: execs used to be
// merely copied (re-reported in every Snapshot until the ring rotated, so a
// polling caller double-, triple-, N-counted the same kernel time) while PC
// samples were already consumed, two different lifecycles in one call. For a
// profiler emitting periodic profiles, each execution/event/module/sample
// belongs to exactly one output; accumulating instead would double-count
// across snapshots exactly like the pre-fix exec bug did.
//
// A caller snapshotting purely for a metrics endpoint (not to emit a
// profile) should be aware this is destructive: values are taken away from
// the profile that received them, not retained for the next call. Only
// LaunchCache (a live join index, not an output) and not-yet-matched pending
// PC samples (which may still match a future execution) survive a Snapshot
// call.
//
// Draining execs/events/modules is done by swapping in a fresh, empty ring
// under the lock (O(1)) rather than copying the old ring's contents while
// holding it (O(capacity)) - review Important 4. The O(capacity) unwrap into
// an ordered slice happens below, against rings this goroutine now
// exclusively owns, so it no longer serializes concurrent EmitLaunch/
// EmitExec/etc calls. Pending PC samples cannot be swapped wholesale the
// same way: most entries typically do not match this round and must survive
// for a future one, so that pass takes a second, much shorter lock
// (O(len(execs)) map operations, not O(capacity)).
func (t *Timeline) Snapshot() Snapshot {
	t.mu.Lock()
	oldExecs := t.execs
	t.execs = newRing[GPUKernelExec](len(oldExecs.buf))
	oldEvents := t.events
	t.events = newRing[GPUTimelineEvent](len(oldEvents.buf))
	oldModules := t.modules
	t.modules = newRing[GPUModule](len(oldModules.buf))
	launchCount := t.launchesSinceSnapshot
	t.launchesSinceSnapshot = 0
	dropped := t.dropped
	t.mu.Unlock()

	execs := oldExecs.items()
	events := oldEvents.items()
	modules := oldModules.items()

	// Only pop the pending samples the executions in this snapshot can
	// actually use, rather than copying the whole map: a correlation with
	// no live exec this round is left untouched (still eligible for a
	// later snapshot, or eventual orphan eviction).
	var attributedExact uint64
	t.mu.Lock()
	execSamples := make([][]GPUPCSample, len(execs))
	exactServed := make([]bool, len(execs))
	for i, exec := range execs {
		if entry, ok := t.pending[exec.Correlation]; ok {
			execSamples[i] = entry.samples
			exactServed[i] = true
			delete(t.pending, exec.Correlation)
			t.pendingSampleTotal -= uint64(len(entry.samples))
			attributedExact += uint64(len(entry.samples))
		}
	}
	// The module-keyed join runs strictly after the loop above and strictly
	// over what it left unserved: the exact-correlation path is unchanged and
	// keeps first claim. It consumes from the module store exactly as the loop
	// above consumes from the correlation-keyed one - a group it can place is
	// deleted, a group it cannot is left alone and stays eligible for a later
	// Snapshot.
	pcJoin := t.joinPendingModuleLocked(execs, execSamples, exactServed)
	pcJoin.stats.AttributedExact = attributedExact
	attributedPCSamples := attributedExact + pcJoin.samples

	pendingCorrelations := len(t.pending)
	pendingSamples := t.pendingSampleTotal
	pendingModuleGroups := len(t.pendingModule)
	pendingModuleSamples := t.pendingModuleSampleTotal
	// The disclosure's evidence, classified under the lock rather than copied
	// out: the store is not drained by Snapshot (a window covers executions
	// that have not arrived yet, and the next snapshot needs it), so the
	// alternative would be to clone every burst on every call.
	//
	// serializedSampling FALSE takes the constant branch. With PC sampling off
	// or in continuous collection nothing is ever serialized, so "false" is
	// correct and unconditional and no store is consulted at all.
	serialization := make([]SerializationState, len(execs))
	if t.serializedSampling {
		for i, exec := range execs {
			serialization[i] = t.windows.classify(exec.Correlation.PID, exec.StartNs, exec.EndNs)
		}
	} else {
		for i := range serialization {
			serialization[i] = SerializationNotSerialized
		}
	}
	windowsReceived := t.windows.received
	windowsHeld, windowsOpen := t.windows.windows()
	t.mu.Unlock()

	// The heuristic's candidate set is built lazily - only once the loop
	// below hits its first exec that actually needs it (see the
	// exec.Correlation.Present() branch), not unconditionally on every
	// Snapshot call - review Important 4. LaunchCache.Entries() and the
	// per-group sort are O(capacity); a CUPTI-style workload where every
	// join is exact never touches this at all. Grouping by (queue, kernel
	// name) and sorting each group by TimeNs turns each miss into a binary
	// search - O(log candidates) - instead of a linear scan: a
	// single-queue, single-kernel-name workload (one submission stream) is
	// not a pathological input, it is what most workloads look like, and a
	// linear scan there is still O(misses x candidates) even without a
	// per-miss allocation.
	//
	// cacheEntries is read once and shared by both indexes below, so a
	// snapshot that needs them both still pays LaunchCache.Entries() once.
	stats := JoinStats{LaunchCount: launchCount}
	var cacheEntries []GPUKernelLaunch
	var cacheRead bool
	entries := func() []GPUKernelLaunch {
		if !cacheRead {
			cacheEntries, cacheRead = t.cache.Entries(), true
		}
		return cacheEntries
	}
	var candidateIndex map[candidateGroupKey][]GPUKernelLaunch
	// anyProcessIndex is the same grouping with the process dropped, and it
	// exists ONLY to answer "would a process-blind join have matched here?"
	// for CrossProcessHeuristicBlockedCount - never to produce a join. It is
	// built lazily on the first refusal, so a snapshot with no
	// correlation-less executions (every snapshot on every shipping backend)
	// never allocates it, and one whose heuristic joins all land in their own
	// process never allocates it either.
	var anyProcessIndex map[anyProcessGroupKey][]GPUKernelLaunch
	// noteBlockedHeuristic counts a refused heuristic join, but only when a
	// candidate genuinely qualified: an execution that would have missed
	// anyway is an ordinary miss, and counting it here would make the
	// "cross-process attributions prevented" figure read high for reasons
	// that have nothing to do with processes.
	noteBlockedHeuristic := func(exec GPUKernelExec) {
		if anyProcessIndex == nil {
			anyProcessIndex = buildAnyProcessCandidateIndex(entries())
		}
		key := anyProcessGroupKey{queue: queueKeyOf(exec.Queue), kernelName: exec.KernelName}
		if match := findLaunchHeuristic(anyProcessIndex[key], exec, t.joinWindowNs); match.launch != nil {
			stats.CrossProcessHeuristicBlockedCount++
		}
	}

	views := make([]ExecutionView, 0, len(execs))
	matched := make(map[CorrelationID]struct{})
	// The three gpu_serialized outcomes, counted where the view is BUILT and
	// before any of the loop's `continue`s, so every execution is counted
	// exactly once on every path through the join. That is what makes the sum
	// identity (the three equal len(Executions)) hold by construction rather
	// than by remembering to count in each branch.
	var serializedCount, notSerializedCount, serializationUnknownCount uint64
	for i, exec := range execs {
		view := ExecutionView{Exec: exec, PCSamples: execSamples[i], Serialized: serialization[i]}
		// gpu_pc_attrib, decided entirely by which index served this
		// execution. It is set independently of view.Join and view.Ambiguous
		// below and never reads or writes either: an execution can be joined
		// to its launch exactly and still have INFERRED PC samples, which is
		// precisely the pair of facts one boolean could not carry. See
		// PCAttrib.
		switch {
		case exactServed[i]:
			view.PCAttrib = PCAttribExact
		case len(view.PCSamples) > 0:
			view.PCAttrib = pcJoin.attribAt(i)
		}
		switch view.Serialized {
		case SerializationSerialized:
			serializedCount++
		case SerializationNotSerialized:
			notSerializedCount++
		default:
			serializationUnknownCount++
		}

		if exec.Correlation.Present() {
			if l, ok := t.cache.Get(exec.Correlation); ok {
				launch := l
				view.Launch = &launch
				view.Join = JoinExact
				stats.ExactExecutionJoinCount++
				matched[exec.Correlation] = struct{}{}
				views = append(views, view)
				continue
			}
			// Review Critical 2: a correlation ID was supplied but missed
			// the cache. That tells us the truth - the launch aged out (or,
			// less commonly, never arrived) - it does not tell us "no
			// correlation was ever available", which is the one situation
			// the heuristic below exists for (DRM, which never supplies a
			// correlation ID at all). Guessing here risks attaching a
			// different, still-live launch's PID and Tags to this
			// execution merely because it shares a kernel name and queue -
			// spec §13 requires degrading to unattributed instead.
			//
			// Since the correlation carries the producing process (issue
			// #36), a launch from ANOTHER process with the same vendor
			// value is now one of the things that legitimately misses here,
			// and lands in UnmatchedExecutionCount rather than being
			// reported as an exact join to the wrong process's call stack.
			stats.UnmatchedExecutionCount++
			views = append(views, view)
			continue
		}

		// No correlation at all: the heuristic path (spec §10). Counted
		// before anything else is decided, because this is the boundary
		// issue #52 is about - see CorrelationlessExecutionCount.
		stats.CorrelationlessExecutionCount++

		// Issue #52: a heuristic join is only allowed within one process,
		// and an execution that does not name its process cannot make one.
		// The pid lives on the correlation even when the correlation carries
		// no value (CorrelationID.Present tests Value alone), so "no vendor
		// correlation" and "no process" are separate facts and a producer
		// that knows the pid can always supply it. When it did not, refuse
		// and degrade to unattributed rather than guess which process this
		// GPU time - and the pod_uid/container_id it would inherit from the
		// chosen launch's Tags - belongs to.
		execPID := exec.Correlation.PID
		if execPID == 0 {
			stats.UnmatchedExecutionCount++
			noteBlockedHeuristic(exec)
			views = append(views, view)
			continue
		}

		if candidateIndex == nil {
			candidateIndex = buildHeuristicCandidateIndex(entries())
		}
		key := candidateGroupKey{
			pid:        execPID,
			queue:      queueKeyOf(exec.Queue),
			kernelName: exec.KernelName,
		}
		if match := findLaunchHeuristic(candidateIndex[key], exec, t.joinWindowNs); match.launch != nil {
			view.Launch = match.launch
			view.Join = JoinHeuristic
			view.Heuristic = true
			view.Ambiguous = match.ambiguous
			stats.HeuristicExecutionJoinCount++
			if match.ambiguous {
				stats.AmbiguousHeuristicMatchCount++
			}
			matched[match.launch.Correlation] = struct{}{}
		} else {
			stats.UnmatchedExecutionCount++
			if match.outOfWindow {
				stats.OutOfWindowDropCount++
			}
			// The execution named its process and its process had nothing
			// for it. Another process might still have held a candidate that
			// a process-blind join would have handed over - the exact
			// misattribution issue #52 describes - so ask, and count it if
			// so. A match found here is necessarily another process's: our
			// own group is a subset of the process-blind one under the same
			// window, so a candidate of ours that qualified there would have
			// qualified above.
			noteBlockedHeuristic(exec)
		}
		views = append(views, view)
	}

	stats.MatchedLaunchCount = uint64(len(matched))
	if stats.LaunchCount >= stats.MatchedLaunchCount {
		stats.UnmatchedLaunchCount = stats.LaunchCount - stats.MatchedLaunchCount
	}

	return Snapshot{
		Executions:          views,
		Events:              events,
		Modules:             modules,
		JoinStats:           stats,
		LaunchCache:         t.cache.Stats(),
		Dropped:             dropped,
		AttributedPCSamples: attributedPCSamples,
		PendingSamples:      int(pendingSamples),
		PendingCorrelations: pendingCorrelations,

		PendingModuleSamples: int(pendingModuleSamples),
		PendingModuleGroups:  pendingModuleGroups,
		PCJoin:               pcJoin.stats,

		SamplingWindowsReceived: windowsReceived,
		SamplingWindowsHeld:     windowsHeld,
		SamplingWindowsOpen:     windowsOpen,

		ExecutionsSerialized:           serializedCount,
		ExecutionsNotSerialized:        notSerializedCount,
		ExecutionsSerializationUnknown: serializationUnknownCount,
	}
}

// launchMatch is the result of the heuristic join: the best candidate found
// (nil on a miss - a miss must never attach a launch), whether more than one
// candidate matched (ambiguous), and - on a miss only - whether the window
// (windowNs) is specifically what excluded it: outOfWindow is true when at
// least one candidate precedes the exec but none fall within the window,
// distinct from a miss with no preceding candidate at all. Feeds
// JoinStats.OutOfWindowDropCount (review Important 3 / minors cleanup).
type launchMatch struct {
	launch      *GPUKernelLaunch
	ambiguous   bool
	outOfWindow bool
}

// queueKey is the subset of GPUQueueRef the heuristic join actually
// compares on (Backend + QueueID, matching the ported filter's original
// condition) - deliberately not the full GPUQueueRef, which also carries
// Device and would otherwise split candidates the original rule treated as
// the same queue.
type queueKey struct {
	backend GPUBackendID
	queueID string
}

func queueKeyOf(q GPUQueueRef) queueKey {
	return queueKey{backend: q.Backend, queueID: q.QueueID}
}

// candidateGroupKey carries the process, and that is issue #52's fix.
//
// It used to carry only (queue, kernel name), on the reasoning that a
// correlation-less execution has no pid to group by - the heuristic runs only
// when no correlation was supplied, and since issue #36 the correlation is
// where an execution's process identity lives. That reasoning was wrong in
// one specific way: CorrelationID.Present() tests Value alone, so PID and
// Value are independent fields and a record can carry a process without
// carrying a correlation (see CorrelationID.Present, and the
// CorrelationID{Backend: BackendLinuxDRM, PID: 4242} shape the tests pin).
// The pid #52 says would need a new field on GPUKernelExec was already there.
//
// So the heuristic groups by process too, and Timeline.Snapshot refuses the
// join outright for an execution whose Correlation.PID is zero. The effect is
// that a correlation-less execution can only ever be handed a launch from its
// own process, and one that names no process is handed nothing at all - it
// degrades to unattributed (spec §13) rather than inheriting another
// process's CPU stack, pod_uid and container_id. Neither refusal is silent:
// see JoinStats.CorrelationlessExecutionCount and
// JoinStats.CrossProcessHeuristicBlockedCount.
//
// The launch's side of the pid is launchProcessID, not Correlation.PID alone,
// because a launch carries LaunchContext.PID as well and producers populate
// them independently.
//
// candidateGroupKey is the exact equivalence launchKernelNamesCompatible
// defines (queue match plus kernel-name equality) turned into a map key.
// Grouping candidates by this key and looking a miss up by its own
// (queue, kernel name) is equivalent to scanning every candidate and
// filtering with launchKernelNamesCompatible - PROVIDED that function stays
// pure string equality on KernelName, which is what it is today (see its
// doc comment: the one case it might have needed to be looser, the AMD/HIP
// fallback, was deliberately left unported). If a future backend reintroduces
// that fallback, this key must change - grouping by queue alone and
// filtering the qualifying (binary-searched) prefix by name would be the
// fallback design, at the cost of an O(prefix) scan per miss instead of
// O(log candidates).
type candidateGroupKey struct {
	pid        uint32
	queue      queueKey
	kernelName string
}

// anyProcessGroupKey is candidateGroupKey with the process dropped: the
// grouping the heuristic used BEFORE issue #52. It exists only so
// Timeline.Snapshot can ask "would the old, process-blind rule have matched
// here?" and count the answer in
// JoinStats.CrossProcessHeuristicBlockedCount. Nothing looked up through this
// key is ever attached to an ExecutionView.
//
// Keeping the old rule executable, rather than deleting it, is what stops the
// guarded path from going dark: a refusal that produced no join and no
// counter would be indistinguishable from a workload that never had a
// candidate in the first place, and the whole point of the guard is to know
// how often it fires.
type anyProcessGroupKey struct {
	queue      queueKey
	kernelName string
}

// launchProcessID is the process a launch is attributed to, for heuristic
// grouping. LaunchContext.PID is preferred because it is the host-side
// observation of who submitted the work and every producer in the tree fills
// it in; Correlation.PID is the fallback for a launch that carries the
// process only on its correlation. Zero means the producer named no process,
// and such a launch can never be a heuristic candidate: Snapshot refuses the
// join for a zero-pid execution, so the zero group is only ever built, never
// queried.
func launchProcessID(l GPUKernelLaunch) uint32 {
	if l.Launch.PID != 0 {
		return l.Launch.PID
	}
	return l.Correlation.PID
}

// buildHeuristicCandidateIndex groups cache entries by candidateGroupKey and
// sorts each group by TimeNs ascending, once per Snapshot call. This is what
// turns each miss's lookup into a binary search (findLaunchHeuristic) rather
// than a linear scan: grouping alone (this function's predecessor,
// groupLaunchesByQueue) removed the per-miss allocation but left an all-miss
// Snapshot at O(misses x candidates-in-queue) - fine for a multi-queue
// workload, but a single submission stream puts every launch in one group,
// and a linear scan of that group per miss is the same quadratic shape this
// phase exists to remove, just without the allocation on top.
func buildHeuristicCandidateIndex(entries []GPUKernelLaunch) map[candidateGroupKey][]GPUKernelLaunch {
	return groupLaunchesByTime(entries, func(l GPUKernelLaunch) candidateGroupKey {
		return candidateGroupKey{
			pid:        launchProcessID(l),
			queue:      queueKeyOf(l.Queue),
			kernelName: l.KernelName,
		}
	})
}

// buildAnyProcessCandidateIndex is buildHeuristicCandidateIndex without the
// process in the key - the pre-#52 grouping, retained solely for counting
// (see anyProcessGroupKey).
func buildAnyProcessCandidateIndex(entries []GPUKernelLaunch) map[anyProcessGroupKey][]GPUKernelLaunch {
	return groupLaunchesByTime(entries, func(l GPUKernelLaunch) anyProcessGroupKey {
		return anyProcessGroupKey{queue: queueKeyOf(l.Queue), kernelName: l.KernelName}
	})
}

// groupLaunchesByTime is the shared body of the two indexes above: bucket by
// keyOf, then sort each bucket by TimeNs ascending so findLaunchHeuristic can
// binary-search it. The two differ only in their key, and keeping one
// implementation is what guarantees the "blocked" count is computed under
// exactly the rule the join itself uses, minus the process.
func groupLaunchesByTime[K comparable](entries []GPUKernelLaunch, keyOf func(GPUKernelLaunch) K) map[K][]GPUKernelLaunch {
	byGroup := make(map[K][]GPUKernelLaunch, len(entries))
	for _, l := range entries {
		k := keyOf(l)
		byGroup[k] = append(byGroup[k], l)
	}
	for _, group := range byGroup {
		// Sorts in place: group's backing array is the same one stored in
		// byGroup, so no re-assignment into the map is needed.
		slices.SortFunc(group, func(a, b GPUKernelLaunch) int {
			return cmp.Compare(a.TimeNs, b.TimeNs)
		})
	}
	return byGroup
}

// findLaunchHeuristic is the fallback path when an exec never carried a
// correlation ID at all (see the exec.Correlation.Present() branch in
// Snapshot). Ported decision from the prototype's findLaunchHeuristic: a
// launch that precedes the exec's start, preferring the most recent such
// launch, and flagging Ambiguous when more than one candidate qualified.
//
// candidates must already be filtered to the exec's exact (queue, kernel
// name) group (see buildHeuristicCandidateIndex) and sorted by TimeNs
// ascending. Because the group is already filtered, every entry satisfies
// the queue/name half of the original filter; sort.Search finds the
// insertion point of exec.StartNs - the count of entries with
// TimeNs <= StartNs, which locates the best candidate (immediately before
// the insertion point, i.e. the most recent one that still precedes the
// exec) in O(log n) instead of an O(n) scan.
//
// windowNs is review Important 3's fix: spec §10 calls the heuristic
// "bounded-time", but this used to accept any candidate preceding the exec
// with no time bound at all, and Ambiguous counted every such candidate -
// for a kernel name reused throughout a workload's life, that count only
// grows, making Ambiguous permanently true and carrying no information.
// windowNs == 0 keeps the historical unbounded behavior (TimelineConfig's
// default); windowNs > 0 restricts both which candidate can match and what
// Ambiguous counts to the [exec.StartNs-windowNs, exec.StartNs] range, via a
// second binary search for the window's lower bound within the same
// already-sorted, already-filtered slice.
func findLaunchHeuristic(candidates []GPUKernelLaunch, exec GPUKernelExec, windowNs uint64) launchMatch {
	insertionPoint := sort.Search(len(candidates), func(i int) bool {
		return candidates[i].TimeNs > exec.StartNs
	})
	if insertionPoint == 0 {
		return launchMatch{}
	}

	lowerBound := 0
	if windowNs > 0 {
		var earliest uint64
		if exec.StartNs > windowNs {
			earliest = exec.StartNs - windowNs
		}
		lowerBound = sort.Search(insertionPoint, func(i int) bool {
			return candidates[i].TimeNs >= earliest
		})
	}

	inWindow := insertionPoint - lowerBound
	if inWindow == 0 {
		// insertionPoint > 0 here (checked above), so at least one candidate
		// precedes the exec - the window is specifically what excluded all
		// of them. Distinct from insertionPoint == 0 above (no preceding
		// candidate existed at all), which is not an out-of-window drop.
		return launchMatch{outOfWindow: true}
	}
	best := candidates[insertionPoint-1]
	return launchMatch{launch: &best, ambiguous: inWindow > 1}
}

// ring is a fixed-capacity FIFO: push is O(1) amortized (no shifting), and
// overflow overwrites the oldest entry while reporting eviction to the
// caller. It exists so the bounded exec/event/module buffers don't
// reintroduce the quadratic behaviour (repeated slice-shift-on-overflow)
// this phase exists to remove.
type ring[T any] struct {
	buf   []T
	start int
	count int
}

func newRing[T any](capacity int) *ring[T] {
	if capacity <= 0 {
		capacity = 1
	}
	return &ring[T]{buf: make([]T, capacity)}
}

// push stores v, evicting the oldest entry (and reporting true) if the ring
// was already full.
func (r *ring[T]) push(v T) (evicted bool) {
	capacity := len(r.buf)
	if r.count < capacity {
		r.buf[(r.start+r.count)%capacity] = v
		r.count++
		return false
	}
	r.buf[r.start] = v
	r.start = (r.start + 1) % capacity
	return true
}

// items returns the stored entries in insertion (oldest-first) order, as a
// fresh slice safe for the caller to retain past the next push.
func (r *ring[T]) items() []T {
	capacity := len(r.buf)
	out := make([]T, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.buf[(r.start+i)%capacity])
	}
	return out
}
