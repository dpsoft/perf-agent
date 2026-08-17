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
	// LaunchCacheStats.AnomalousTimestamp is incremented instead. Zero
	// disables clamping entirely, for callers who genuinely want that. 60
	// seconds (60_000_000_000 ns) is a reasonable default for callers that
	// want protection.
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

// cacheEntry is a stored launch tagged with the Put sequence number that
// produced it, so a stale order position (superseded by a later replace) can
// be told apart from the entry currently live for that correlation ID.
type cacheEntry struct {
	launch GPUKernelLaunch
	seq    uint64
}

// orderEntry names one FIFO position: the correlation ID inserted and the
// sequence number it was inserted (or replaced) with. Comparing seq against
// the current cacheEntry.seq for the same id is how eviction tells a live
// position from one a later Put has superseded.
type orderEntry struct {
	id  CorrelationID
	seq uint64
}

// LaunchCache is a bounded FIFO of recent launches indexed by correlation ID.
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
	order    []orderEntry // insertion order; entries before head are dead
	head     int
	seq      uint64
	newestNs uint64
	stats    LaunchCacheStats
}

func NewLaunchCache(cfg LaunchCacheConfig) *LaunchCache {
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultLaunchCacheCapacity
	}
	return &LaunchCache{
		cfg:    cfg,
		byCorr: make(map[CorrelationID]cacheEntry, cfg.Capacity),
		order:  make([]orderEntry, 0, cfg.Capacity),
	}
}

// Put inserts or replaces the launch for its correlation ID. A repeated
// correlation ID takes the newer launch without growing the cache: see the
// LaunchCache doc comment for the aliasing contract.
func (c *LaunchCache) Put(l GPUKernelLaunch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.observeTimestampLocked(l.TimeNs)

	c.seq++
	seq := c.seq
	if _, exists := c.byCorr[l.Correlation]; exists {
		c.stats.Replaced++
	}
	c.byCorr[l.Correlation] = cacheEntry{launch: l, seq: seq}
	c.order = append(c.order, orderEntry{id: l.Correlation, seq: seq})
	c.evictLocked()
}

// observeTimestampLocked advances the horizon anchor (newestNs) to l's
// timestamp, unless that would be an anomalous jump larger than
// MaxAdvanceNs, in which case the anchor is left alone and the anomaly is
// counted. The anchor is observed time, not wall-clock: wall-clock is wrong
// for replay and out-of-order input. The caller must hold c.mu.
func (c *LaunchCache) observeTimestampLocked(timeNs uint64) {
	if timeNs <= c.newestNs {
		return
	}
	if c.cfg.MaxAdvanceNs > 0 && c.newestNs != 0 && timeNs-c.newestNs > c.cfg.MaxAdvanceNs {
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
		for c.head < len(c.order) {
			e := c.order[c.head]
			cur, superseded := c.currentIfLiveLocked(e)
			if superseded {
				c.head++
				continue
			}
			if c.newestNs <= cur.TimeNs || c.newestNs-cur.TimeNs <= c.cfg.HorizonNs {
				break
			}
			delete(c.byCorr, e.id)
			c.head++
			c.stats.EvictedHorizon++
		}
	}
	for len(c.byCorr) > c.cfg.Capacity && c.head < len(c.order) {
		e := c.order[c.head]
		c.head++
		if _, superseded := c.currentIfLiveLocked(e); superseded {
			continue
		}
		delete(c.byCorr, e.id)
		c.stats.EvictedCapacity++
	}
	c.compactLocked()
}

// currentIfLiveLocked resolves an order position to the launch currently
// live for its correlation ID, and reports whether that position is
// superseded rather than real. A position is superseded in two cases: the id
// has since been evicted outright (superseded && zero value; this should be
// unreachable given eviction always consumes the order position it deletes
// through, but is kept as a defensive guard), or — the case this exists to
// fix — a later Put replaced the entry, so e.seq no longer matches the
// sequence number the map holds for that id. Only a non-superseded position
// is a real, currently-live entry eligible for eviction. The caller must
// hold c.mu.
func (c *LaunchCache) currentIfLiveLocked(e orderEntry) (GPUKernelLaunch, bool) {
	cur, live := c.byCorr[e.id]
	if !live || cur.seq != e.seq {
		return GPUKernelLaunch{}, true
	}
	return cur.launch, false
}

// compactLocked reclaims the dead prefix of order once it dominates the
// slice, so a long-running agent's order slice does not grow without bound.
// It is not tight: compaction only fires once head >= 1024 && head*2 >=
// len(order), so in steady state under sustained load order oscillates
// between roughly capacity and roughly 2x capacity. That is bounded, but
// len(order) should not be read as a proxy for live count.
func (c *LaunchCache) compactLocked() {
	if c.head < 1024 || c.head*2 < len(c.order) {
		return
	}
	rest := c.order[c.head:]
	c.order = append(c.order[:0], rest...)
	c.head = 0
}
