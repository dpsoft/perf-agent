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
	Ambiguous bool             `json:"ambiguous,omitempty"`
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
	// Nothing joins out of this store yet — that is the Snapshot join, and it
	// is deliberately NOT part of this change. Until it lands, a continuous-
	// mode run shows these two gauges rising and AttributedPCSamples flat,
	// which is the honest reading of "collected, correctly grouped, not yet
	// attributed" rather than the previous "collapsed onto one key and
	// silently evicted".
	PendingModuleSamples int `json:"pending_module_samples,omitempty"`
	PendingModuleGroups  int `json:"pending_module_groups,omitempty"`
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
	// Nothing joins out of this store yet. Getting samples into a correctly
	// keyed index and getting them attached to an execution are separate
	// changes on purpose; the second one is where the module store, the
	// kernel-name match and the attribution-quality label live.
	pendingModule            map[pendingModuleKey]pendingSamples
	pendingModuleOrder       *orderedFIFO[pendingModuleKey]
	pendingModuleCap         int
	pendingModuleSampleTotal uint64

	events  *ring[GPUTimelineEvent]
	modules *ring[GPUModule]

	dropped TimelineDropStats
}

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
	}
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
	if t.execs.push(e) {
		t.dropped.EvictedExecutions++
	}
	return nil
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
// from the pending store the same way. This is a deliberate, uniform
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
	var attributedPCSamples uint64
	t.mu.Lock()
	execSamples := make([][]GPUPCSample, len(execs))
	for i, exec := range execs {
		if entry, ok := t.pending[exec.Correlation]; ok {
			execSamples[i] = entry.samples
			delete(t.pending, exec.Correlation)
			t.pendingSampleTotal -= uint64(len(entry.samples))
			attributedPCSamples += uint64(len(entry.samples))
		}
	}
	pendingCorrelations := len(t.pending)
	pendingSamples := t.pendingSampleTotal
	// Read, never drained: nothing joins out of the module-keyed store yet,
	// so every group here survives this Snapshot. When the join lands it
	// consumes from this store the same way the loop above consumes from the
	// correlation-keyed one.
	pendingModuleGroups := len(t.pendingModule)
	pendingModuleSamples := t.pendingModuleSampleTotal
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
	for i, exec := range execs {
		view := ExecutionView{Exec: exec, PCSamples: execSamples[i]}

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
