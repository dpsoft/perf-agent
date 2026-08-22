package gpu

import "sync"

// LaunchCacheConfig bounds the cache two ways. Capacity caps memory; HorizonNs
// caps how far back an attribution can reach.
//
// HorizonNs is the load-bearing tunable for PC sampling: samples arrive
// asynchronously, sometimes seconds after the launch that produced them, and a
// launch evicted before its samples land makes those samples unattributable.
// Too small silently loses attribution; too large costs memory. Zero disables
// horizon eviction and leaves only the capacity bound.
type LaunchCacheConfig struct {
	Capacity  int
	HorizonNs uint64

	// MaxAdvanceNs bounds how far a single Put may advance the cache's
	// observed-time horizon anchor (newestNs) in one step. Timestamps arrive
	// out of order and asynchronously from potentially multiple producers;
	// without a clamp, one corrupt sample (clock-domain bug, overflow, bad
	// backend) can push the anchor far enough forward to evict every other
	// live entry at once, which defeats the entire purpose of a horizon meant
	// to survive late arrival. A Put whose TimeNs would advance the anchor by
	// more than MaxAdvanceNs is treated as anomalous: the launch is still
	// stored (no data is dropped), the anchor does not advance, and
	// LaunchCacheStats.AnomalousTimestamp is incremented instead.
	//
	// Zero means "use the default" (defaultMaxAdvanceNs, 60s): the callers
	// exposed to this hazard are exactly the ones who set HorizonNs, so the
	// guard must be on unless a caller explicitly turns it off. A caller who
	// genuinely wants unclamped advance sets MaxAdvanceNs to math.MaxUint64.
	MaxAdvanceNs uint64
}

// LaunchCacheStats reports what the cache holds and what it dropped. Every
// eviction path is counted: a cache that loses attributions silently is
// indistinguishable from a correlation bug.
type LaunchCacheStats struct {
	Live               int    `json:"live"`
	EvictedCapacity    uint64 `json:"evicted_capacity,omitempty"`
	EvictedHorizon     uint64 `json:"evicted_horizon,omitempty"`
	Replaced           uint64 `json:"replaced,omitempty"`
	AnomalousTimestamp uint64 `json:"anomalous_timestamp,omitempty"`
}

const defaultLaunchCacheCapacity = 65536

// defaultMaxAdvanceNs bounds how far one Put may move the horizon anchor when
// the caller has not chosen a bound (LaunchCacheConfig.MaxAdvanceNs == 0). A
// corrupt or out-of-range timestamp would otherwise push every live entry
// past the horizon in a single call.
const defaultMaxAdvanceNs = 60 * 1e9 // 60s

// cacheEntry is a stored launch tagged with the Put sequence number that
// produced it, so a stale order position (superseded by a later replace) can
// be told apart from the entry currently live for that correlation ID. The
// horizon anchor is the launch's own TimeNs (cacheEntry.launch.TimeNs) - a
// single scalar, already O(1) to read - so unlike Timeline.pending (whose
// value is a slice needing its own tracked anchor; see pendingSamples),
// cacheEntry does not need a separate anchor field.
type cacheEntry struct {
	launch GPUKernelLaunch
	seq    uint64
}

// LaunchCache is a bounded FIFO of recent launches indexed by correlation ID.
// The correlation carries the producing process (see CorrelationID), so the
// index is process-qualified: two processes reusing the same vendor value -
// the norm system-wide, since vendor counters restart per process - occupy
// two entries and neither can be handed out for the other's execution.
// Put and Get are O(1) amortized. It replaces the unbounded slice plus linear
// scan the earlier prototype used, which was quadratic at snapshot time.
//
// Put and Get do not deep-copy. A GPUKernelLaunch's LaunchContext.CPUStack
// slice and Tags map are stored, and later returned, by reference: the value
// Get returns shares backing arrays with the cache's own copy. This is the
// ingestion hot path, so callers must treat both the value passed to Put
// (after the call) and the value returned by Get as read-only rather than
// pay for a per-launch clone.
type LaunchCache struct {
	mu       sync.Mutex
	cfg      LaunchCacheConfig
	byCorr   map[CorrelationID]cacheEntry
	order    *orderedFIFO[CorrelationID]
	newestNs uint64
	stats    LaunchCacheStats
}

func NewLaunchCache(cfg LaunchCacheConfig) *LaunchCache {
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultLaunchCacheCapacity
	}
	if cfg.MaxAdvanceNs == 0 {
		cfg.MaxAdvanceNs = defaultMaxAdvanceNs
	}
	return &LaunchCache{
		cfg:    cfg,
		byCorr: make(map[CorrelationID]cacheEntry, cfg.Capacity),
		order:  newOrderedFIFO[CorrelationID](cfg.Capacity),
	}
}

// isLiveLocked answers orderedFIFO's isLive callback: a position is live
// only if byCorr still holds an entry for id stamped with exactly seq. The
// caller must hold c.mu.
func (c *LaunchCache) isLiveLocked(id CorrelationID, seq uint64) bool {
	e, ok := c.byCorr[id]
	return ok && e.seq == seq
}

// Put inserts or replaces the launch for its correlation ID. A repeated
// correlation ID takes the newer launch without growing the cache: see the
// LaunchCache doc comment for the aliasing contract.
func (c *LaunchCache) Put(l GPUKernelLaunch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.observeTimestampLocked(l.TimeNs)

	seq := c.order.insert(l.Correlation)
	if _, exists := c.byCorr[l.Correlation]; exists {
		c.stats.Replaced++
	}
	c.byCorr[l.Correlation] = cacheEntry{launch: l, seq: seq}
	c.evictLocked()
}

// observeTimestampLocked advances the horizon anchor (newestNs) to l's
// timestamp, unless that would be an anomalous jump larger than
// MaxAdvanceNs, in which case the anchor is left alone and the anomaly is
// counted. NewLaunchCache guarantees MaxAdvanceNs is never zero (it defaults
// to defaultMaxAdvanceNs), so the only way to see unclamped advance here is
// the explicit math.MaxUint64 opt-out, against which no real jump can ever
// compare greater. The anchor is observed time, not wall-clock: wall-clock is
// wrong for replay and out-of-order input. The caller must hold c.mu.
func (c *LaunchCache) observeTimestampLocked(timeNs uint64) {
	if timeNs <= c.newestNs {
		return
	}
	if c.newestNs != 0 && timeNs-c.newestNs > c.cfg.MaxAdvanceNs {
		c.stats.AnomalousTimestamp++
		return
	}
	c.newestNs = timeNs
}

// Get returns the launch stored for id, if any. A miss is reported as
// (zero value, false), never fabricated. See the LaunchCache doc comment for
// the aliasing contract on the returned value.
func (c *LaunchCache) Get(id CorrelationID) (GPUKernelLaunch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byCorr[id]
	if !ok {
		return GPUKernelLaunch{}, false
	}
	return e.launch, true
}

// Entries returns a snapshot of the launches currently live in the cache, in
// no particular order. It exists for the heuristic join (Timeline), which
// must consider more than one candidate launch: Get alone can only answer
// "what is stored for this exact correlation ID." Because the cache is
// bounded, scanning Entries costs at most O(capacity), not O(every launch
// ever seen) - the quadratic behaviour this cache exists to remove. See the
// LaunchCache doc comment for the aliasing contract on the returned values.
func (c *LaunchCache) Entries() []GPUKernelLaunch {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]GPUKernelLaunch, 0, len(c.byCorr))
	for _, e := range c.byCorr {
		out = append(out, e.launch)
	}
	return out
}

func (c *LaunchCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byCorr)
}

func (c *LaunchCache) Stats() LaunchCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Live = len(c.byCorr)
	return s
}

// evictLocked drops entries past the horizon first, then past capacity. The
// caller must hold c.mu.
func (c *LaunchCache) evictLocked() {
	if c.cfg.HorizonNs > 0 {
		for {
			id, ok := c.order.peekOldestLive(c.isLiveLocked)
			if !ok {
				break
			}
			cur := c.byCorr[id] // guaranteed present: peekOldestLive just confirmed liveness
			if c.newestNs <= cur.launch.TimeNs || c.newestNs-cur.launch.TimeNs <= c.cfg.HorizonNs {
				break
			}
			c.order.evictOldestLive(c.isLiveLocked)
			delete(c.byCorr, id)
			c.stats.EvictedHorizon++
		}
	}
	for len(c.byCorr) > c.cfg.Capacity {
		id, ok := c.order.evictOldestLive(c.isLiveLocked)
		if !ok {
			break
		}
		delete(c.byCorr, id)
		c.stats.EvictedCapacity++
	}
}
