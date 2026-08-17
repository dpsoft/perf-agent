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
}

// LaunchCacheStats reports what the cache holds and what it dropped. Every
// eviction path is counted: a cache that loses attributions silently is
// indistinguishable from a correlation bug.
type LaunchCacheStats struct {
	Live            int    `json:"live"`
	EvictedCapacity uint64 `json:"evicted_capacity,omitempty"`
	EvictedHorizon  uint64 `json:"evicted_horizon,omitempty"`
	Replaced        uint64 `json:"replaced,omitempty"`
}

const defaultLaunchCacheCapacity = 65536

// LaunchCache is a bounded FIFO of recent launches indexed by correlation ID.
// Put and Get are O(1) amortized. It replaces the unbounded slice plus linear
// scan the earlier prototype used, which was quadratic at snapshot time.
type LaunchCache struct {
	mu       sync.Mutex
	cfg      LaunchCacheConfig
	byCorr   map[CorrelationID]GPUKernelLaunch
	order    []CorrelationID // insertion order; entries before head are dead
	head     int
	newestNs uint64
	stats    LaunchCacheStats
}

func NewLaunchCache(cfg LaunchCacheConfig) *LaunchCache {
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultLaunchCacheCapacity
	}
	return &LaunchCache{
		cfg:    cfg,
		byCorr: make(map[CorrelationID]GPUKernelLaunch, cfg.Capacity),
		order:  make([]CorrelationID, 0, cfg.Capacity),
	}
}

func (c *LaunchCache) Put(l GPUKernelLaunch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if l.TimeNs > c.newestNs {
		c.newestNs = l.TimeNs
	}
	if _, exists := c.byCorr[l.Correlation]; exists {
		// Same correlation seen again: take the newer launch without growing
		// the FIFO. The stale order entry is skipped when it reaches the head.
		c.byCorr[l.Correlation] = l
		c.stats.Replaced++
		c.evictLocked()
		return
	}
	c.byCorr[l.Correlation] = l
	c.order = append(c.order, l.Correlation)
	c.evictLocked()
}

func (c *LaunchCache) Get(id CorrelationID) (GPUKernelLaunch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.byCorr[id]
	return l, ok
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
			id := c.order[c.head]
			l, live := c.byCorr[id]
			if !live {
				c.head++ // stale order entry from a replacement
				continue
			}
			if c.newestNs <= l.TimeNs || c.newestNs-l.TimeNs <= c.cfg.HorizonNs {
				break
			}
			delete(c.byCorr, id)
			c.head++
			c.stats.EvictedHorizon++
		}
	}
	for len(c.byCorr) > c.cfg.Capacity && c.head < len(c.order) {
		id := c.order[c.head]
		c.head++
		if _, live := c.byCorr[id]; !live {
			continue
		}
		delete(c.byCorr, id)
		c.stats.EvictedCapacity++
	}
	c.compactLocked()
}

// compactLocked reclaims the dead prefix of order once it dominates the slice,
// so a long-running agent does not grow order without bound.
func (c *LaunchCache) compactLocked() {
	if c.head < 1024 || c.head*2 < len(c.order) {
		return
	}
	rest := c.order[c.head:]
	c.order = append(c.order[:0], rest...)
	c.head = 0
}
