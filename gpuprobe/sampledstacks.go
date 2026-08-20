package gpuprobe

import (
	"github.com/dpsoft/perf-agent/gpu"
	pp "github.com/dpsoft/perf-agent/pprof"
)

// A sampled launch is not a separate launch.
//
// gpu_launch_sampled_v1 carries the same correlation as the batched
// gpu_launch_v1 record for the same launch (shim/stub/stub.cc emits both for
// a sampled launch, one unbatched and one into the launch batch). Emitting
// the sampled record as its own launch would hand the timeline two launches
// for one, and gpu/timeline.go has no dedup that would catch it. So the
// sampled record is never emitted: its captured stack is attached to the
// launch that arrives on the *batched* probe, and only that launch is
// emitted.
//
// The two halves can arrive in either order, and both happen:
//
//   - Sampled first (the common case). The shim's launch batch flushes only
//     when it fills, so the batched record for launch N usually reaches the
//     ringbuf long after the unbatched sampled record for the same launch.
//     The resolved stack waits in pendingStacks until its twin shows up.
//   - Batched first. When launch N is the record that fills the batch, the
//     flush happens inside the same add() call that precedes the sampler
//     check, so the batch - whose last record is launch N - is queued before
//     N's own sampled record. The launch waits in deferredLaunches, briefly,
//     for the twin that is already on its way.
//
// The batched-first case has a known, counted, rate-dependent way of losing
// the join, and it is the cause of the PendingStacks the demo run reports
// (24 of 250 captures at 2000 launches; none at 500). The producer queues
// the launch batch, then the EXEC batch, then the sampled probe. So when one
// launch is both the record that fills the launch batch and the one the
// sampler picks, the exec batch lands between the twins - and any batch that
// is not a sampled launch releases the deferred queue (Consumer.applyBatch,
// deliberately: the timeline needs launches promptly). The launch goes out
// stackless and its stack, arriving next, parks with nothing to join.
//
// It looks arithmetically impossible in the stub, which is why it went
// unexplained: batches hold 32 records and the sampler takes one in 8, so
// "fills the batch" (i = 0 mod 32) and "is sampled" (i = 1 mod 8) cannot
// both hold. What breaks that is the producer's periodic drain tick, which
// flushes a PARTIAL batch every 100ms and so re-phases the batch boundary to
// wherever the launch loop had got to. About one tick in eight leaves the
// new boundary sitting on a sampled launch, and until the next tick moves it
// again every remaining fill - one per 32 launches - loses its stack this
// way. Hence rate-dependent: more launches, more ticks, more chances to land
// in phase and more fills to spend there.
//
// The cost is attribution, never a record: the launch ships, the execution
// ships, the GPU time is measured and projects as unattributed, and the
// orphaned stack is visible in Stats.PendingStacks. The alternative -
// holding launches past the next batch - trades a rare attribution gain for
// a systematic delay in launch delivery that the timeline's join depends on.
// Reproduced without any privilege by
// TestStackParksUnattachedWhenAnotherBatchSplitsTheTwins.
//
// Both stores are bounded, and both count what they push out. An unbounded
// map on either side is a leak driven by a profiled application's launch
// rate, which is exactly the class of bug the bounded stores in gpu/ exist
// to avoid.

// launchKey identifies one launch. A correlation alone does not.
//
// The probes are attached with uprobe_multi against the shim *file*, so
// every process that maps it fires them, and Config.PID == 0 (system-wide)
// is a supported mode. CUPTI hands out correlation ids per process, each
// sequence starting from a low value, so two profiled processes collide on
// correlation within the first handful of launches. Keying the side tables
// on correlation alone would let process A's stack - symbolized against
// /proc/A/maps - be attached to process B's launch, projecting B's measured
// GPU time under a call path that provably did not produce it. A fabricated
// flame graph is worse than no flame graph, so the pid is part of the key on
// every insert, lookup, eviction and drain path.
type launchKey struct {
	pid  uint32
	corr gpu.CorrelationID
}

// resolvedStack is a capture that has already been read out of the stackmap
// and symbolized, waiting for the batched launch it belongs to.
type resolvedStack struct {
	frames []pp.Frame
	period uint32
	// gen distinguishes this entry from an earlier one for the same
	// correlation whose order position is still in the FIFO. Without it,
	// evicting the stale position would delete the live entry - the same
	// trap gpu.orderedFIFO documents at length for LaunchCache and
	// Timeline.pending. (gpu.orderedFIFO itself is unexported, so this is a
	// deliberately minimal re-implementation rather than a shared one.)
	gen uint64
}

// pendingStacks is the bounded (pid, correlation) -> stack side table for
// captures that arrived before their batched twin. Eviction is FIFO: the
// oldest
// parked capture is the one whose twin is least likely to still be coming
// (its batch was dropped, or the producer stopped), so it is the right one
// to give up on first.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type pendingStacks struct {
	byKey    map[launchKey]resolvedStack
	order    []stackPos
	head     int
	gen      uint64
	capacity int
}

type stackPos struct {
	key launchKey
	gen uint64
}

func newPendingStacks(capacity int) *pendingStacks {
	if capacity <= 0 {
		capacity = defaultSampledStackCapacity
	}
	return &pendingStacks{
		byKey:    make(map[launchKey]resolvedStack),
		capacity: capacity,
	}
}

// park stores a resolved capture and returns how many entries had to be
// evicted to make room (0 or 1 in practice; the caller counts them). A
// second capture for a correlation that is already parked replaces it and
// is not itself an eviction of a *pending* stack - the caller counts the
// replaced one as evicted all the same, because its stack will never reach
// a launch either.
func (p *pendingStacks) park(key launchKey, frames []pp.Frame, period uint32) (evicted int) {
	if _, replaced := p.byKey[key]; replaced {
		evicted++
	}
	p.gen++
	p.byKey[key] = resolvedStack{frames: frames, period: period, gen: p.gen}
	p.order = append(p.order, stackPos{key: key, gen: p.gen})
	p.reclaimOrder()
	for len(p.byKey) > p.capacity {
		if !p.evictOldest() {
			break
		}
		evicted++
	}
	return evicted
}

// evictOldest drops the oldest live entry, skipping order positions that a
// later park superseded. It reports whether anything was actually evicted.
func (p *pendingStacks) evictOldest() bool {
	for p.head < len(p.order) {
		pos := p.order[p.head]
		p.head++
		cur, ok := p.byKey[pos.key]
		if !ok || cur.gen != pos.gen {
			continue // superseded or already taken: not an eviction
		}
		delete(p.byKey, pos.key)
		p.compact()
		return true
	}
	p.compact()
	return false
}

// reclaimOrder drops the order positions that no longer describe a live
// entry, keeping the survivors in their original order.
//
// This is what stops the FIFO outgrowing the work it tracks. park is the
// only thing that appends, and take deliberately leaves a position behind
// (evictOldest tells a dead position apart from a live one by generation,
// so no scan is needed on the hot take path). In the ordinary sampled-first
// steady state the map hovers near empty and evictOldest therefore never
// runs, so nothing else would ever reclaim those positions: the slice would
// grow by one position plus a retained correlation string per sampled
// launch, for the life of the process. Head-advancing alone is not enough
// either, because a single stack parked and never taken - its launch batch
// was dropped - sits at the head forever while the dead positions pile up
// behind it.
//
// Live entries are at most capacity, so the slice is rebuilt only once the
// dead outnumber any possible live set, which makes the O(n) rebuild
// amortized O(1) per park and bounds the slice at 2*capacity + 1.
// Eviction order is preserved exactly: the filter keeps survivors in place,
// so the oldest live position stays the oldest.
func (p *pendingStacks) reclaimOrder() {
	if len(p.order)-p.head <= 2*p.capacity {
		return
	}
	n := 0
	for _, pos := range p.order[p.head:] {
		cur, ok := p.byKey[pos.key]
		if !ok || cur.gen != pos.gen {
			continue
		}
		// Safe in place: n never runs ahead of the read index, because it
		// advances at most once per position read and starts no later.
		p.order[n] = pos
		n++
	}
	clear(p.order[n:])
	p.order = p.order[:n]
	p.head = 0
}

// take removes and returns the capture parked for key, if any.
func (p *pendingStacks) take(key launchKey) (resolvedStack, bool) {
	st, ok := p.byKey[key]
	if !ok {
		return resolvedStack{}, false
	}
	delete(p.byKey, key)
	// The order position is left behind deliberately: evictOldest tells it
	// apart from a live entry by generation, so no scan is needed here.
	return st, true
}

func (p *pendingStacks) len() int { return len(p.byKey) }

// compact reclaims the consumed prefix of the order slice once it is more
// than half stale, so a long-running consumer's slice does not grow without
// bound while its map stays small.
func (p *pendingStacks) compact() {
	if p.head == 0 || p.head*2 < len(p.order) {
		return
	}
	n := copy(p.order, p.order[p.head:])
	p.order = p.order[:n]
	p.head = 0
}

// deferredLaunches holds launches whose sampled twin may still be in flight.
//
// A launch is held only while it could still gain a stack, and only for as
// long as the very next thing off the ringbuf might be that stack: applying
// any batch that is not a sampled launch releases the whole queue (see
// Consumer.applyBatch), as does Run returning, Close, and Flush. Holding
// launches longer would trade a rare attribution gain for a systematic
// delay in launch delivery, which the timeline's join depends on.
//
// The queue is a slice with a moving head rather than a shifted array: the
// release path runs on every launch, and copying the whole queue per launch
// would be real per-launch cost on the hot path.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type deferredLaunches struct {
	buf      []deferredLaunch
	head     int
	capacity int
}

// deferredLaunch is a held launch plus the kernel id from its wire record.
// gpu.GPUKernelLaunch carries the kernel *name*, not the id, and the name
// may not be known yet (see kernelnames.go), so the id has to ride along
// until the launch is finally emitted and can be named.
type deferredLaunch struct {
	launch   gpu.GPUKernelLaunch
	kernelID uint64
}

func newDeferredLaunches(capacity int) *deferredLaunches {
	if capacity <= 0 {
		capacity = defaultDeferredLaunchCapacity
	}
	return &deferredLaunches{capacity: capacity}
}

// push queues a launch. If the queue is already at capacity the oldest
// launch is returned for immediate release, so the bound can never turn
// into dropped launches.
func (d *deferredLaunches) push(l deferredLaunch) (released deferredLaunch, ok bool) {
	if d.len() >= d.capacity {
		released, ok = d.pop()
	}
	d.buf = append(d.buf, l)
	return released, ok
}

// pop removes the oldest launch.
func (d *deferredLaunches) pop() (deferredLaunch, bool) {
	if d.head >= len(d.buf) {
		return deferredLaunch{}, false
	}
	l := d.buf[d.head]
	// Clear the slot so the queue does not pin a launch's stack frames and
	// tag map alive after it has been handed on.
	d.buf[d.head] = deferredLaunch{}
	d.head++
	d.compact()
	return l, true
}

// take removes the queued launch with this (pid, correlation), if it is
// still held. The pair is unique per launch - a correlation on its own is
// not, see launchKey - so the first match is the only one; the scan runs
// from the newest end because the twin of a just-arrived sampled record is
// the launch that was queued most recently.
func (d *deferredLaunches) take(key launchKey) (deferredLaunch, bool) {
	for i := len(d.buf) - 1; i >= d.head; i-- {
		if d.buf[i].launch.Launch.PID != key.pid || d.buf[i].launch.Correlation != key.corr {
			continue
		}
		l := d.buf[i]
		copy(d.buf[i:], d.buf[i+1:])
		d.buf[len(d.buf)-1] = deferredLaunch{}
		d.buf = d.buf[:len(d.buf)-1]
		return l, true
	}
	return deferredLaunch{}, false
}

// drain empties the queue in FIFO order.
func (d *deferredLaunches) drain() []deferredLaunch {
	if d.len() == 0 {
		return nil
	}
	out := make([]deferredLaunch, d.len())
	copy(out, d.buf[d.head:])
	clear(d.buf)
	d.buf = d.buf[:0]
	d.head = 0
	return out
}

func (d *deferredLaunches) len() int { return len(d.buf) - d.head }

func (d *deferredLaunches) compact() {
	if d.head == 0 || d.head*2 < len(d.buf) {
		return
	}
	n := copy(d.buf, d.buf[d.head:])
	clear(d.buf[n:])
	d.buf = d.buf[:n]
	d.head = 0
}
