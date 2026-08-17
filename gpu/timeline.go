package gpu

import "sync"

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
// exec and event ring buffers layered on top of it. Modeled after SinkStats:
// nothing bounded is allowed to evict silently.
type TimelineDropStats struct {
	EvictedExecutions uint64 `json:"evicted_executions,omitempty"`
	EvictedEvents     uint64 `json:"evicted_events,omitempty"`
}

// Snapshot is a point-in-time, fully-joined view of everything Timeline
// currently holds.
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
	// normalized Capacity also bounds Timeline's exec/event ring buffers:
	// there is no separate capacity field for them, and exec/event volume
	// tracks launch volume closely enough to share one dial.
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
	// matching Correlation is joined at Snapshot time. Unlike execs/events
	// this is a plain, unbounded map (per the brief's Structure section): a
	// sample whose exec never arrives, or whose exec ages out of the ring,
	// stays here indefinitely. Known, documented limitation - bounding it is
	// a separate design this task's brief and tests do not call for.
	pending map[CorrelationID][]GPUPCSample
	events  *ring[GPUTimelineEvent]
	modules []GPUModule

	dropped TimelineDropStats
}

// NewTimeline constructs a Timeline. cfg.LaunchCache is passed through to
// NewLaunchCache unmodified (including its own zero-value defaulting).
func NewTimeline(cfg TimelineConfig) *Timeline {
	capacity := cfg.LaunchCache.Capacity
	if capacity <= 0 {
		capacity = defaultLaunchCacheCapacity
	}
	return &Timeline{
		cache:   NewLaunchCache(cfg.LaunchCache),
		execs:   newRing[GPUKernelExec](capacity),
		events:  newRing[GPUTimelineEvent](capacity),
		pending: make(map[CorrelationID][]GPUPCSample),
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
	t.pending[p.Correlation] = append(t.pending[p.Correlation], p)
	return nil
}

func (t *Timeline) EmitModule(m GPUModule) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.modules = append(t.modules, m)
	return nil
}

func (t *Timeline) EmitEvent(e GPUTimelineEvent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.events.push(e) {
		t.dropped.EvictedEvents++
	}
	return nil
}

// Snapshot builds the joined view. It copies everything Timeline itself
// guards (execs, events, modules, pending samples, counters) while holding
// the lock, then releases it before touching LaunchCache - which guards
// itself - so the join work below never runs under Timeline's own mutex.
func (t *Timeline) Snapshot() Snapshot {
	t.mu.Lock()
	execs := t.execs.items()
	events := t.events.items()
	modules := append([]GPUModule(nil), t.modules...)
	pending := make(map[CorrelationID][]GPUPCSample, len(t.pending))
	for id, samples := range t.pending {
		pending[id] = append([]GPUPCSample(nil), samples...)
	}
	launchCount := t.launchCount
	dropped := t.dropped
	t.mu.Unlock()

	views := make([]ExecutionView, 0, len(execs))
	stats := JoinStats{LaunchCount: launchCount}
	matched := make(map[CorrelationID]struct{})
	for _, exec := range execs {
		view := ExecutionView{Exec: exec, PCSamples: pending[exec.Correlation]}

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

		if match := t.findLaunchHeuristic(exec); match.launch != nil {
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

// findLaunchHeuristic is the fallback path when an exec's Correlation misses
// the cache (never arrived, or aged out). Ported from the prototype's
// findLaunchHeuristic: same queue, a kernel-name match, and a launch that
// precedes the exec's start - picking the most recent such launch as best,
// and flagging Ambiguous when more than one candidate qualified. Unlike the
// prototype, which scanned every launch ever recorded, this scans only
// LaunchCache.Entries() - bounded by the cache's capacity, not by ingestion
// history.
func (t *Timeline) findLaunchHeuristic(exec GPUKernelExec) launchMatch {
	var best *GPUKernelLaunch
	var candidateCount int
	for _, l := range t.cache.Entries() {
		if l.Queue.Backend != exec.Queue.Backend || l.Queue.QueueID != exec.Queue.QueueID {
			continue
		}
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
// caller. It exists so the bounded exec/event buffers don't reintroduce the
// quadratic behaviour (repeated slice-shift-on-overflow) this phase exists to
// remove.
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
