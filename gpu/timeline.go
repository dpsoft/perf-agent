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
	// cardinality bound.
	PendingSampleHorizonNs uint64
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

	// pending holds PC samples keyed by correlation ID until an exec with a
	// matching Correlation is joined at Snapshot time. A correlation's
	// samples are deleted from pending the moment they are attached to an
	// execution's view - they are consumed, not cached indefinitely. As a
	// direct consequence, if the same still-live execution is included in a
	// later Snapshot call, that later call sees only samples that arrived
	// since the previous attach, not the full history: PCSamples is a
	// once-delivered view, not an accumulating one. Samples that are never
	// claimed (exec never arrives, or ages out of the ring first) are
	// orphans; pendingCap/pendingOrder/pendingHead bound how many distinct
	// correlations' worth of orphans can accumulate, evicting the oldest
	// and counting it in Dropped.EvictedPendingSamples.
	//
	// Both pending's entries and pendingOrder's positions carry a sequence
	// number, the same way LaunchCache pairs cacheEntry.seq with
	// orderEntry.seq (see currentIfLiveLocked there). This exists to solve
	// the identical hazard: a correlation ID consumed by Snapshot leaves its
	// old pendingOrder position behind with no map entry; if that same ID is
	// then reused by a later EmitPCSample, presence-only eviction cannot
	// tell that stale position apart from the ID's new, live generation, and
	// would delete the freshly re-inserted entry instead of the position it
	// actually corresponds to. A new sequence is assigned only on the
	// absent-to-present transition (a brand new correlation, or a reused one
	// after consumption/eviction), not on every append to an already-live
	// entry - unlike LaunchCache.Put, which always replaces the whole value,
	// pending accumulates samples across many EmitPCSample calls for the
	// same still-live generation, so an in-place append must not look like a
	// new generation.
	pending      map[CorrelationID]pendingSamples
	pendingOrder []pendingOrderEntry
	pendingHead  int
	pendingSeq   uint64
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

	events  *ring[GPUTimelineEvent]
	modules *ring[GPUModule]

	dropped TimelineDropStats
}

// pendingSamples is a pending correlation's accumulated samples, tagged with
// the sequence number of its current (live) generation - see the pending
// field's doc comment.
type pendingSamples struct {
	samples []GPUPCSample
	seq     uint64
}

// pendingOrderEntry names one FIFO position: the correlation ID inserted and
// the sequence number it was inserted with. Comparing seq against the
// current pendingSamples.seq for the same id is how eviction tells a live
// position from one a later re-insertion (after consumption or eviction) has
// superseded. Mirrors LaunchCache's orderEntry.
type pendingOrderEntry struct {
	id  CorrelationID
	seq uint64
}

// defaultMaxPendingSamplesPerCorrelation bounds a single pending
// correlation's accumulated samples when TimelineConfig doesn't set
// MaxPendingSamplesPerCorrelation. Chosen generously relative to a realistic
// PC-sampling batch (tens to low hundreds of samples between exec arrivals)
// while still being a real, enforced bound rather than the previous
// unbounded append.
const defaultMaxPendingSamplesPerCorrelation = 4096

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

	return &Timeline{
		cache:            cache,
		joinWindowNs:     cfg.LaunchEventJoinWindowNs,
		execs:            newRing[GPUKernelExec](capacity),
		events:           newRing[GPUTimelineEvent](eventCapacity),
		modules:          newRing[GPUModule](capacity),
		pending:          make(map[CorrelationID]pendingSamples),
		pendingCap:       capacity,
		pendingSampleCap: sampleCap,
		pendingHorizonNs: cfg.PendingSampleHorizonNs,
	}
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

	entry, exists := t.pending[p.Correlation]
	if !exists {
		// Absent-to-present transition: either a brand new correlation, or
		// this ID being reused after its previous generation was consumed
		// or evicted. Either way it is a new generation and gets a new
		// sequence number plus a new order position - see the pending
		// field's doc comment for why this must not also happen on every
		// append to an already-live entry.
		t.pendingSeq++
		entry.seq = t.pendingSeq
		t.pendingOrder = append(t.pendingOrder, pendingOrderEntry{id: p.Correlation, seq: entry.seq})
	}

	if len(entry.samples) >= t.pendingSampleCap {
		// Review Critical 5: evictPendingLocked's cardinality bound only
		// limits the number of distinct pending correlations, not the
		// samples within any single one of them. Without this check, one
		// orphaned (or stale-ID-reused) correlation below that bound could
		// still accumulate samples forever. The entry (and its order
		// position/generation) is left exactly as it was; only the new
		// sample is dropped and counted.
		t.dropped.EvictedPendingSamples++
		t.pending[p.Correlation] = entry
		t.evictPendingLocked()
		return nil
	}

	entry.samples = append(entry.samples, p)
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

// latestSampleTimeNs returns the largest TimeNs among samples, for aging a
// pending entry against pendingNewestNs.
func latestSampleTimeNs(samples []GPUPCSample) uint64 {
	var latest uint64
	for _, s := range samples {
		if s.TimeNs > latest {
			latest = s.TimeNs
		}
	}
	return latest
}

// evictPendingLocked drops pending correlation groups past the horizon
// first (if pendingHorizonNs > 0), then the oldest ones once the map holds
// more distinct correlations than pendingCap. An order position whose
// sequence no longer matches the map's current entry for that id - because
// Snapshot already consumed it, or a later re-insertion superseded it - is
// skipped rather than double-counted or, worse, mistaken for the live
// generation and evicted in its place. This mirrors
// LaunchCache.evictLocked/currentIfLiveLocked exactly, for the same reason:
// presence in the map is not a sufficient liveness check when a key can be
// deleted and then reused. Caller holds t.mu.
func (t *Timeline) evictPendingLocked() {
	if t.pendingHorizonNs > 0 {
		for t.pendingHead < len(t.pendingOrder) {
			e := t.pendingOrder[t.pendingHead]
			cur, live := t.pending[e.id]
			if !live || cur.seq != e.seq {
				t.pendingHead++
				continue
			}
			latest := latestSampleTimeNs(cur.samples)
			if t.pendingNewestNs <= latest || t.pendingNewestNs-latest <= t.pendingHorizonNs {
				break
			}
			delete(t.pending, e.id)
			t.pendingHead++
			t.pendingSampleTotal -= uint64(len(cur.samples))
			t.dropped.EvictedPendingSamples += uint64(len(cur.samples))
		}
	}
	for len(t.pending) > t.pendingCap && t.pendingHead < len(t.pendingOrder) {
		e := t.pendingOrder[t.pendingHead]
		t.pendingHead++
		cur, live := t.pending[e.id]
		if !live || cur.seq != e.seq {
			continue
		}
		delete(t.pending, e.id)
		t.pendingSampleTotal -= uint64(len(cur.samples))
		t.dropped.EvictedPendingSamples += uint64(len(cur.samples))
	}
	t.compactPendingOrderLocked()
}

// compactPendingOrderLocked reclaims the dead prefix of pendingOrder once it
// dominates the slice, the same way LaunchCache.compactLocked does, so the
// order slice's backing array doesn't grow without bound under sustained
// load. Caller holds t.mu.
func (t *Timeline) compactPendingOrderLocked() {
	if t.pendingHead < 1024 || t.pendingHead*2 < len(t.pendingOrder) {
		return
	}
	rest := t.pendingOrder[t.pendingHead:]
	t.pendingOrder = append(t.pendingOrder[:0], rest...)
	t.pendingHead = 0
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
	t.mu.Unlock()

	// The heuristic's candidate set is built lazily - only once the loop
	// below hits its first exec that actually needs it (see the
	// exec.Correlation zero-value branch), not unconditionally on every
	// Snapshot call - review Important 4. LaunchCache.Entries() and the
	// per-group sort are O(capacity); a CUPTI-style workload where every
	// join is exact never touches this at all. Grouping by (queue, kernel
	// name) and sorting each group by TimeNs turns each miss into a binary
	// search - O(log candidates) - instead of a linear scan: a
	// single-queue, single-kernel-name workload (one submission stream) is
	// not a pathological input, it is what most workloads look like, and a
	// linear scan there is still O(misses x candidates) even without a
	// per-miss allocation.
	var candidateIndex map[candidateGroupKey][]GPUKernelLaunch

	views := make([]ExecutionView, 0, len(execs))
	stats := JoinStats{LaunchCount: launchCount}
	matched := make(map[CorrelationID]struct{})
	for i, exec := range execs {
		view := ExecutionView{Exec: exec, PCSamples: execSamples[i]}

		if exec.Correlation != (CorrelationID{}) {
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
			stats.UnmatchedExecutionCount++
			views = append(views, view)
			continue
		}

		if candidateIndex == nil {
			candidateIndex = buildHeuristicCandidateIndex(t.cache.Entries())
		}
		key := candidateGroupKey{queue: queueKeyOf(exec.Queue), kernelName: exec.KernelName}
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
	queue      queueKey
	kernelName string
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
	byGroup := make(map[candidateGroupKey][]GPUKernelLaunch, len(entries))
	for _, l := range entries {
		k := candidateGroupKey{queue: queueKeyOf(l.Queue), kernelName: l.KernelName}
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
// correlation ID at all (see the exec.Correlation zero-value branch in
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
