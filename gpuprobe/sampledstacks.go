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
// The two halves can arrive in either order, and the consumer handles both:
//
//   - Sampled first. The resolved stack waits in pendingStacks until its
//     twin shows up. Nothing can take it but the twin, and any number of
//     unrelated batches may pass in the meantime.
//   - Batched first. The launch waits in deferredLaunches for a twin that
//     had better be the very next thing off the ringbuf, because the first
//     batch of any other kind releases the whole queue (Consumer.applyBatch,
//     deliberately: the timeline wants launches promptly).
//
// Only the first order is safe, and issue #67 is the measurement of that.
// The shim used to add the launch to its batch and only then fire the
// sampled probe, so a launch that both FILLED the batch and was sampled put
// its own batched record on the wire first - with the exec batch of the same
// loop iteration landing between the twins. The exec batch released the
// launch stackless and the stack, arriving next, parked with nothing to
// join: 58 sampled, 57 attached, 1 in PendingStacks on the privileged gate.
//
// At the old fixed sampler stride that collision was arithmetically
// unreachable - batches hold 32 records and the sampler took one in 8, and a
// multiple of 8 is never 31 mod 32 - which is why the join looked sound for
// a whole phase. Issue #50's jittered stride draws each gap from [4,12], so
// a sampled ordinal eventually lands exactly on a batch boundary. #50 did
// not cause this; it removed the arithmetic that was hiding it.
//
// The fix is in the producer, not here: shim/stub/stub.cc and
// shim/nvidia/cupti_adapter.cc fire the sampled probe BEFORE the batched
// add(). A record cannot be in a batch before add() puts it there, so no
// flush - on the launching thread or on the drain thread - can carry the
// twin past the sampled probe, and sampled-first holds unconditionally.
// shim/stub/probe_order_test.cc pins that order by patching the probe sites
// with int3 and reading the wire order back, with no privilege and no
// consumer; it fails on the pre-#67 producer at every sampled launch that
// fills a batch.
//
// deferredLaunches stays, because this consumer does not only see shims this
// repository builds: the ABI is public (spec §6), a vendor bridge or an
// older shim may still emit batched-first, and for those the queue is the
// difference between "usually joins" and "never joins". What it cannot do is
// make batched-first lossless - the launch has to go out before the next
// exec batch, and once it has gone out there is nothing left to attach to
// without re-emitting an event the sink has already been given. So the cost
// there is attribution and never a record: the launch ships, the execution
// ships, the GPU time is measured and projects as unattributed, and the
// orphaned stack is counted in Stats.PendingStacks rather than vanishing.
// That path is held to its documented behaviour by
// TestStackParksUnattachedWhenAnotherBatchSplitsTheTwins, which is now a
// statement about foreign producers rather than about ours.
//
// Both stores are bounded, and both count what they push out. An unbounded
// map on either side is a leak driven by a profiled application's launch
// rate, which is exactly the class of bug the bounded stores in gpu/ exist
// to avoid.

// The side tables below key on gpu.CorrelationID, which carries the
// producing process. A correlation VALUE alone does not identify a launch.
//
// The probes are attached with uprobe_multi against the shim *file*, so
// every process that maps it fires them, and Config.PID == 0 (system-wide)
// is a supported mode. CUPTI hands out correlation ids per process, each
// sequence starting from a low value, so two profiled processes collide on
// correlation within the first handful of launches. Keying these tables on
// the value alone would let process A's stack - symbolized against
// /proc/A/maps - be attached to process B's launch, projecting B's measured
// GPU time under a call path that provably did not produce it. A fabricated
// flame graph is worse than no flame graph.
//
// This used to be a local launchKey{pid, corr} pair, because the core's
// CorrelationID was pid-blind and the timeline underneath these tables had
// the same defect (issue #36). Now that the pid lives in the id itself, the
// pair would be two copies of the same fact that could drift apart, so the
// key IS the correlation: Consumer builds every one of them through
// correlationOf, which cannot be called without naming a process.

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

// pendingStacks is the bounded correlation -> stack side table for
// captures that arrived before their batched twin. Eviction is FIFO: the
// oldest
// parked capture is the one whose twin is least likely to still be coming
// (its batch was dropped, or the producer stopped), so it is the right one
// to give up on first.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type pendingStacks struct {
	byKey    map[gpu.CorrelationID]resolvedStack
	order    []stackPos
	head     int
	gen      uint64
	capacity int
}

type stackPos struct {
	key gpu.CorrelationID
	gen uint64
}

func newPendingStacks(capacity int) *pendingStacks {
	if capacity <= 0 {
		capacity = defaultSampledStackCapacity
	}
	return &pendingStacks{
		byKey:    make(map[gpu.CorrelationID]resolvedStack),
		capacity: capacity,
	}
}

// park stores a resolved capture and returns how many entries had to be
// evicted to make room (0 or 1 in practice; the caller counts them). A
// second capture for a correlation that is already parked replaces it and
// is not itself an eviction of a *pending* stack - the caller counts the
// replaced one as evicted all the same, because its stack will never reach
// a launch either.
func (p *pendingStacks) park(key gpu.CorrelationID, frames []pp.Frame, period uint32) (evicted int) {
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
func (p *pendingStacks) take(key gpu.CorrelationID) (resolvedStack, bool) {
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

// take removes the queued launch with this correlation, if it is still held.
// A process-qualified correlation is unique per launch - a correlation value
// on its own is not, see the note above pendingStacks - so the first match is
// the only one; the scan runs from the newest end because the twin of a
// just-arrived sampled record is the launch that was queued most recently.
func (d *deferredLaunches) take(key gpu.CorrelationID) (deferredLaunch, bool) {
	for i := len(d.buf) - 1; i >= d.head; i-- {
		if d.buf[i].launch.Correlation != key {
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
