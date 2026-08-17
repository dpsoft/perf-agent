package gpu

import (
	"maps"
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
}

// TimelineConfig configures Timeline's storage bounds.
type TimelineConfig struct {
	// LaunchCache bounds the launch cache (see LaunchCacheConfig). Its
	// normalized Capacity also bounds Timeline's exec/event/module ring
	// buffers and the pending-PC-sample map: there is no separate capacity
	// field for them, and their volume tracks launch volume closely enough
	// to share one dial.
	LaunchCache LaunchCacheConfig
	// LaunchEventJoinWindowNs is reserved for a future launch/event join
	// (the prototype's findLaunchForEvent). Timeline does not join
	// GPUTimelineEvent to launches yet, so this field is currently inert;
	// kept here so callers don't need to change TimelineConfig's shape later.
	LaunchEventJoinWindowNs uint64
}

// Timeline is the indexed join point: it ingests launches, executions, PC
// samples, modules and lifecycle events, and produces a Snapshot that joins
// executions back to the launch that produced them. Correlation lookups go
// through LaunchCache (O(1)/O(capacity)) rather than the earlier prototype's
// linear scan of every launch ever seen.
type Timeline struct {
	mu sync.Mutex

	cache *LaunchCache

	launchCount uint64

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

// NewTimeline constructs a Timeline. cfg.LaunchCache is passed through to
// NewLaunchCache unmodified (including its own zero-value defaulting); the
// cache's own normalized capacity is reused for the exec/event/module rings
// and the pending-sample bound rather than re-deriving it, so the default
// can't drift between LaunchCache and Timeline.
func NewTimeline(cfg TimelineConfig) *Timeline {
	cache := NewLaunchCache(cfg.LaunchCache)
	capacity := cache.cfg.Capacity
	return &Timeline{
		cache:      cache,
		execs:      newRing[GPUKernelExec](capacity),
		events:     newRing[GPUTimelineEvent](capacity),
		modules:    newRing[GPUModule](capacity),
		pending:    make(map[CorrelationID]pendingSamples),
		pendingCap: capacity,
	}
}

func (t *Timeline) EmitLaunch(l GPUKernelLaunch) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.launchCount++
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
	entry.samples = append(entry.samples, p)
	t.pending[p.Correlation] = entry
	t.evictPendingLocked()
	return nil
}

// evictPendingLocked drops the oldest pending correlation groups once the
// map holds more distinct correlations than pendingCap. An order position
// whose sequence no longer matches the map's current entry for that id -
// because Snapshot already consumed it, or a later re-insertion superseded
// it - is skipped rather than double-counted or, worse, mistaken for the
// live generation and evicted in its place. This mirrors
// LaunchCache.evictLocked/currentIfLiveLocked exactly, for the same reason:
// presence in the map is not a sufficient liveness check when a key can be
// deleted and then reused. Caller holds t.mu.
func (t *Timeline) evictPendingLocked() {
	for len(t.pending) > t.pendingCap && t.pendingHead < len(t.pendingOrder) {
		e := t.pendingOrder[t.pendingHead]
		t.pendingHead++
		cur, live := t.pending[e.id]
		if !live || cur.seq != e.seq {
			continue
		}
		delete(t.pending, e.id)
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

// Snapshot builds the joined view. It copies/consumes everything Timeline
// itself guards (execs, events, modules, matched pending samples, counters)
// while holding the lock, then releases it before touching LaunchCache -
// which guards itself - so the join work below never runs under Timeline's
// own mutex.
//
// PC samples are consumed, not cached: once a sample is attached to an
// execution's PCSamples here, it is removed from Timeline's pending store.
// A second consecutive Snapshot call for a still-live execution therefore
// returns no PC samples for it unless new ones arrived since the previous
// call. A caller polling Snapshot for metrics sees each sample exactly
// once - it belongs to whichever profile it was reported in, not to every
// later one - so samples should be taken away from the profile that
// received them, not expected to reappear in the next.
func (t *Timeline) Snapshot() Snapshot {
	t.mu.Lock()
	execs := t.execs.items()
	events := t.events.items()
	modules := t.modules.items()

	// Only pop the pending samples the executions in this snapshot can
	// actually use, rather than copying the whole map: a correlation with
	// no live exec this round is left untouched (still eligible for a
	// later snapshot, or eventual orphan eviction).
	execSamples := make([][]GPUPCSample, len(execs))
	for i, exec := range execs {
		if entry, ok := t.pending[exec.Correlation]; ok {
			execSamples[i] = entry.samples
			delete(t.pending, exec.Correlation)
		}
	}

	launchCount := t.launchCount
	dropped := t.dropped
	t.mu.Unlock()

	// The heuristic's candidate set is materialized once per Snapshot call
	// (not once per miss): LaunchCache.Entries() allocates, and calling it
	// inside the per-execution loop was exactly the quadratic-with-
	// allocation behaviour this phase exists to remove. Grouping by queue
	// also means each miss only scans the launches that could possibly
	// match its queue, not the whole cache.
	candidatesByQueue := groupLaunchesByQueue(t.cache.Entries())

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
		}

		if match := findLaunchHeuristic(candidatesByQueue[queueKeyOf(exec.Queue)], exec); match.launch != nil {
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
		}
		views = append(views, view)
	}

	stats.MatchedLaunchCount = uint64(len(matched))
	if stats.LaunchCount >= stats.MatchedLaunchCount {
		stats.UnmatchedLaunchCount = stats.LaunchCount - stats.MatchedLaunchCount
	}

	return Snapshot{
		Executions:  views,
		Events:      events,
		Modules:     modules,
		JoinStats:   stats,
		LaunchCache: t.cache.Stats(),
		Dropped:     dropped,
	}
}

// launchMatch is the result of the heuristic join: the best candidate found
// (nil on a miss - a miss must never attach a launch), and whether more than
// one candidate matched (Ambiguous).
type launchMatch struct {
	launch    *GPUKernelLaunch
	ambiguous bool
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

// groupLaunchesByQueue indexes entries by queueKey once, so Snapshot's
// per-execution heuristic lookup is a map access plus a scan of only that
// queue's candidates, not a scan of the entire cache per miss.
func groupLaunchesByQueue(entries []GPUKernelLaunch) map[queueKey][]GPUKernelLaunch {
	out := make(map[queueKey][]GPUKernelLaunch, len(entries))
	for _, l := range entries {
		k := queueKeyOf(l.Queue)
		out[k] = append(out[k], l)
	}
	return out
}

// findLaunchHeuristic is the fallback path when an exec's Correlation misses
// the cache (never arrived, or aged out). Ported from the prototype's
// findLaunchHeuristic: a kernel-name match and a launch that precedes the
// exec's start - picking the most recent such launch as best, and flagging
// Ambiguous when more than one candidate qualified. candidates is expected
// to already be filtered to the exec's queue (see groupLaunchesByQueue);
// this function no longer does that filtering itself.
func findLaunchHeuristic(candidates []GPUKernelLaunch, exec GPUKernelExec) launchMatch {
	var best *GPUKernelLaunch
	var candidateCount int
	for _, l := range candidates {
		if !launchKernelNamesCompatible(l, exec) {
			continue
		}
		if l.TimeNs > exec.StartNs {
			continue
		}
		candidateCount++
		if best == nil || l.TimeNs > best.TimeNs {
			candidate := l
			best = &candidate
		}
	}
	if best == nil {
		return launchMatch{}
	}
	return launchMatch{launch: best, ambiguous: candidateCount > 1}
}

// launchKernelNamesCompatible decides whether launch could plausibly be the
// origin of exec, for the heuristic join's candidate filter.
//
// The prototype this is ported from also accepted a HIP launch's synthetic
// "hip_kernel@..." name for any AMD exec reported under a separate
// execution-side backend id (BackendAMDSample), gated on the launch's CPU
// stack showing a HIP entrypoint. That launch-side/exec-side backend split
// doesn't exist in this model (only BackendHIP, used on both sides), so
// nothing on the exec side can distinguish "same backend, different name"
// from "different kernel" - porting the special case as-is would accept any
// HIP launch for any exec, looser than the original rule. Left out rather
// than ported incorrectly; exact kernel-name match is the only rule here
// until a future backend reintroduces that split.
func launchKernelNamesCompatible(launch GPUKernelLaunch, exec GPUKernelExec) bool {
	return launch.KernelName == exec.KernelName
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
