package gpuprobe

import (
	"github.com/dpsoft/perf-agent/gpu"
)

// Kernel names arrive on their own probe, out of band from the launches and
// executions that reference them.
//
// gpu_kernel_name_v1 carries kernel_id -> name, emitted once when the
// producer first sees a kernel and replayed in full when a consumer attaches
// late (shim/core/kernelnames.h, spec §6.1's replay contract). Everything
// else refers to a kernel by its numeric id only, so without this table an
// execution reaches the profile with no name and projects as
// [gpu:kernel:] - or, since projectionFrames omits an empty name entirely,
// as nothing at all.
//
// Names are not guaranteed to precede their use. The producer only emits a
// name at intern time if the name probe is armed then, so a consumer that
// attaches between the intern and the next replay tick sees launches for a
// kernel whose name is still to come - up to a full drain interval later.
// Events that arrive without a name are therefore held, the same way a
// launch is held for a late sampled stack, and released when the name lands.
// Both stores are bounded and both count what leaves them unnamed: "one
// entry per distinct kernel" is an assumption about the workload, not a
// guarantee the consumer can rely on.

// truncatedNameSuffix marks a name the producer had to cut off at
// GPU_KERNEL_NAME_MAX. Mangled C++ names routinely exceed 256 bytes, and a
// truncated name that reads as complete is a lie about which kernel ran -
// two distinct kernels can share the first 256 bytes. The marker travels
// with the name so it survives into the [gpu:kernel:<name>] frame, whichever
// side of the join carries it; Stats.KernelNamesTruncated counts it in
// aggregate.
const truncatedNameSuffix = "…"

// kernelName is one interned name and whether it arrived complete.
type kernelName struct {
	name      string
	truncated bool
}

// resolved renders the name as it should appear downstream.
func (k kernelName) resolved() string {
	if k.truncated {
		return k.name + truncatedNameSuffix
	}
	return k.name
}

// kernelNameTable is the bounded kernel_id -> name map.
//
// Eviction is FIFO by first insertion, and a re-intern (the replay case)
// replaces the value in place without touching the order, so an id appears
// at most once in the FIFO and no position can ever go stale. That keeps
// len(order)-head exactly equal to len(byID) - the table cannot grow a
// shadow of dead positions the way a generation-stamped FIFO can.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type kernelNameTable struct {
	byID     map[uint64]kernelName
	order    []uint64
	head     int
	capacity int
}

func newKernelNameTable(capacity int) *kernelNameTable {
	if capacity <= 0 {
		capacity = defaultKernelNameCapacity
	}
	return &kernelNameTable{byID: make(map[uint64]kernelName), capacity: capacity}
}

// put interns a name, returning how many older names had to be evicted to
// make room. An evicted name is not record loss - the launches and
// executions still flow - but every event referring to that kernel from then
// on is unnamed, so the caller counts it.
func (t *kernelNameTable) put(id uint64, k kernelName) (evicted int) {
	if _, exists := t.byID[id]; !exists {
		t.order = append(t.order, id)
	}
	t.byID[id] = k
	for len(t.byID) > t.capacity && t.head < len(t.order) {
		oldest := t.order[t.head]
		t.head++
		delete(t.byID, oldest)
		evicted++
	}
	t.compact()
	return evicted
}

func (t *kernelNameTable) get(id uint64) (kernelName, bool) {
	k, ok := t.byID[id]
	return k, ok
}

func (t *kernelNameTable) len() int { return len(t.byID) }

func (t *kernelNameTable) compact() {
	if t.head == 0 || t.head*2 < len(t.order) {
		return
	}
	n := copy(t.order, t.order[t.head:])
	t.order = t.order[:n]
	t.head = 0
}

// unnamedEvent is one normalized event held until its kernel's name
// arrives. Exactly one of launch/exec is set; they are pointers so a held
// event costs one allocation rather than the size of both structs.
type unnamedEvent struct {
	kernelID uint64
	launch   *gpu.GPUKernelLaunch
	exec     *gpu.GPUKernelExec
}

// pendingNames is the bounded FIFO of events waiting for a name. As with
// deferredLaunches, overflow releases the oldest event rather than dropping
// it: a missing name must never cost a record.
//
// Not internally synchronized; Consumer calls it under its own mutex.
type pendingNames struct {
	buf      []unnamedEvent
	head     int
	capacity int
}

func newPendingNames(capacity int) *pendingNames {
	if capacity <= 0 {
		capacity = defaultPendingNamedEventCapacity
	}
	return &pendingNames{capacity: capacity}
}

// push queues an event, returning the oldest one for immediate release if
// the queue was already full.
func (p *pendingNames) push(ev unnamedEvent) (released unnamedEvent, ok bool) {
	if p.len() >= p.capacity {
		released, ok = p.pop()
	}
	p.buf = append(p.buf, ev)
	return released, ok
}

func (p *pendingNames) pop() (unnamedEvent, bool) {
	if p.head >= len(p.buf) {
		return unnamedEvent{}, false
	}
	ev := p.buf[p.head]
	p.buf[p.head] = unnamedEvent{}
	p.head++
	p.compact()
	return ev, true
}

// takeByKernel removes every event waiting on this kernel id, oldest first.
func (p *pendingNames) takeByKernel(id uint64) []unnamedEvent {
	var out []unnamedEvent
	n := p.head
	for i := p.head; i < len(p.buf); i++ {
		if p.buf[i].kernelID == id {
			out = append(out, p.buf[i])
			continue
		}
		p.buf[n] = p.buf[i]
		n++
	}
	if len(out) == 0 {
		return nil
	}
	clear(p.buf[n:])
	p.buf = p.buf[:n]
	return out
}

// drain empties the queue in arrival order.
func (p *pendingNames) drain() []unnamedEvent {
	if p.len() == 0 {
		return nil
	}
	out := make([]unnamedEvent, p.len())
	copy(out, p.buf[p.head:])
	clear(p.buf)
	p.buf = p.buf[:0]
	p.head = 0
	return out
}

func (p *pendingNames) len() int { return len(p.buf) - p.head }

func (p *pendingNames) compact() {
	if p.head == 0 || p.head*2 < len(p.buf) {
		return
	}
	n := copy(p.buf, p.buf[p.head:])
	clear(p.buf[n:])
	p.buf = p.buf[:n]
	p.head = 0
}
