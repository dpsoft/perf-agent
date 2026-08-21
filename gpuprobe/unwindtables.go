package gpuprobe

import (
	"container/list"
	"sync"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// The walker needs tables before it can walk, and nothing else installs them.
//
// bpf/gpu_usdt.bpf.c drives walk_step from bpf/unwind_common.h. That walker is
// a *hybrid*: for each frame it looks the PC up in `pid_mappings` (keyed by
// pid), and only if it finds a mapping does it consult that binary's
// classification and CFI tables and unwind by DWARF. A miss is not an error -
// walk_step falls through to MODE_FP_SAFE and follows the frame-pointer chain,
// which is exactly the behaviour Phase 4b exists to replace. So a gpuprobe
// consumer that never populates those maps gets frame-pointer stacks and
// reports nothing unusual: the failure is quiet, which is why
// Stats.StacksWalkedNoTables exists.
//
// Note for anyone following the plan text: the walker does NOT consult the
// `pids` map. `pids` is a sampling *filter*, read by bpf/perf_dwarf.bpf.c and
// bpf/offcpu_dwarf.bpf.c to decide whether to sample at all; it is defined in
// unwind_common.h (so it is present in gpuprobe's object) but no gpuprobe
// program reads it - a uprobe on the shim only fires in processes that mapped
// the shim, which is the filter. Populating it here would be dead work. The
// maps that matter are `pid_mappings` / `pid_mapping_lengths` plus the four
// `cfi_*` tables, and they are populated by exactly the same calls the CPU and
// off-CPU DWARF profilers use:
//
//	store   := ehmaps.NewTableStore(cfi_rules, cfi_lengths,
//	                                cfi_classification, cfi_classification_lengths)
//	tracker := ehmaps.NewPIDTracker(store, pid_mappings, pid_mapping_lengths)
//	ehmaps.AttachAllMappings(tracker, pid)   // per binary: ehcompile.Compile + populate
//
// unwind/dwarfagent wraps that in a session with an mmap watcher and, in lazy
// mode, a CFI-miss drainer. gpuprobe reuses the store/tracker/AttachAllMappings
// core and not the session: the session is built around a perf_event sampler's
// lifecycle (its own ringbuf, its own aggregation, its own Collect), none of
// which this consumer has or wants.

// pidRegistrar installs and removes the per-PID CFI tables the BPF walker
// consults. It exists as an interface for one reason: the real implementation
// writes to BPF maps, which needs CAP_BPF, so without a seam every test of
// *when* registration happens would need privileges it should not need.
// ehmapsRegistrar is the real one.
type pidRegistrar interface {
	// Register compiles and installs CFI for every file-backed executable
	// mapping of pid, and returns how many distinct binaries were installed.
	Register(pid uint32) (int, error)
	// Unregister removes pid's mappings and drops its references to the
	// shared CFI tables, evicting any table no other PID still holds.
	Unregister(pid uint32) error
}

// ehmapsRegistrar is the real registrar: unwind/ehmaps, driving
// unwind/ehcompile, over this program's own copies of the walker's maps.
type ehmapsRegistrar struct {
	tracker *ehmaps.PIDTracker
}

// newEhmapsRegistrar wires a registrar around a loaded gpu_usdt object. The
// maps come from unwind_common.h, so they are the same six maps perf_dwarf and
// offcpu_dwarf use, and the store/tracker pair is constructed exactly as
// dwarfagent.newSession constructs it. The caller owns the maps; neither the
// store nor the tracker closes them.
func newEhmapsRegistrar(objs *gpuusdtObjects) *ehmapsRegistrar {
	store := ehmaps.NewTableStore(
		objs.CfiRules, objs.CfiLengths,
		objs.CfiClassification, objs.CfiClassificationLengths,
	)
	return &ehmapsRegistrar{
		tracker: ehmaps.NewPIDTracker(store, objs.PidMappings, objs.PidMappingLengths),
	}
}

func (r *ehmapsRegistrar) Register(pid uint32) (int, error) {
	return ehmaps.AttachAllMappings(r.tracker, pid)
}

func (r *ehmapsRegistrar) Unregister(pid uint32) error {
	return r.tracker.Detach(pid)
}

// pidRegState is where one PID is in the registration pipeline.
type pidRegState uint8

const (
	// pidPending: the PID has been seen and its registration is queued or
	// running. The walker has no tables for it yet.
	pidPending pidRegState = iota
	// pidReady: AttachAllMappings installed at least one binary. Walks from
	// this PID can use CFI.
	pidReady
	// pidFailed: registration ran and installed nothing (the process exited,
	// /proc was unreadable, or no mapping had usable .eh_frame). Walks stay on
	// the frame-pointer path and are counted, not retried - a retry loop
	// driven by a profiled application's launch rate is a busy loop.
	pidFailed
)

type pidRegEntry struct {
	pid   uint32
	state pidRegState
	elem  *list.Element
}

// pidWork is one unit of registrar I/O, handed to the worker goroutine.
type pidWork struct {
	pid uint32
	// release inverts the request: tear this PID's tables down instead of
	// installing them.
	release bool
}

// unwindStats is the registry's slice of gpuprobe.Stats. Kept separate so the
// registry can be snapshotted under its own lock.
type unwindStats struct {
	registered       uint64
	failed           uint64
	evicted          uint64
	requestsDropped  uint64
	releaseFailed    uint64
	binariesAttached uint64
	lastErr          string
}

// pidRegistry decides which PIDs get CFI tables, and bounds the set.
//
// Two lifecycle points feed it, and they are different on purpose:
//
//   - Config.PID != 0. Attach calls registerNow before the uprobe link exists,
//     so the tables are in place before the probe can fire even once. This is
//     the eager path, and it is what unwind/dwarfagent does for a per-PID
//     profiler (mode is forced to ModeEager there for the same reason).
//   - Config.PID == 0 (system-wide, the documented default). The target
//     process may not exist at Attach time - the gate's stub is launched after
//     Attach, because the shim only emits once the uprobe's semaphore says
//     someone is listening. A PID becomes interesting the first time any batch
//     arrives from it, and note() requests registration then.
//
// Registration is NOT done on the caller's goroutine in the lazy case. It is
// ehcompile.Compile per binary, and that is not free: measured on this branch,
// /lib64/libc.so.6 takes 4.1ms, libcuda.so.610 takes 77.5ms for 135805 CFI
// entries, and a 147MB libLLVM.so takes 318ms. A real CUDA process maps
// several of those, so registering inline would stall the ringbuf drain for
// hundreds of milliseconds and turn a fixable attribution gap into kernel
// ringbuf overflow (Stats.KernelDropped) - record loss, which is strictly
// worse. A single worker goroutine does the I/O; the drain goroutine only ever
// does O(1) map and list work.
//
// The set is bounded because it is fed by whatever processes a profiled
// machine happens to run. Eviction is LRU on last sighting, not FIFO: the
// oldest *registered* PID on a busy machine is typically the long-running GPU
// process that matters most, whereas the least recently seen one is the one
// that has stopped launching. Every eviction is counted
// (Stats.UnwindPIDsEvicted) and releases its tables, so an evicted PID's
// subsequent walks degrade to frame pointers and say so.
type pidRegistry struct {
	reg      pidRegistrar
	capacity int
	work     chan pidWork
	wg       sync.WaitGroup

	mu     sync.Mutex
	byPID  map[uint32]*pidRegEntry
	lru    *list.List // front = most recently seen
	stats  unwindStats
	closed bool
}

const (
	// defaultUnwindPIDCapacity bounds how many PIDs hold CFI tables at once.
	// A ceiling, not a dial: the walker's own pid_mappings map holds 4096
	// PIDs and cfi_rules 1024 distinct binaries (bpf/unwind_common.h), and a
	// machine running 128 distinct GPU processes concurrently is already well
	// past what a sampled profiler is sized for. Each entry costs one
	// pid_mappings inner array (256 rows) plus a reference on each binary's
	// shared CFI table, so the real memory is dominated by the distinct
	// binaries, which PIDs share.
	defaultUnwindPIDCapacity = 128

	// unwindWorkDepth is the registrar work queue. Requests are one per
	// distinct PID, not one per record, so this is deep enough to absorb a
	// burst of new processes while the worker is inside a 300ms compile. A
	// full queue is counted (Stats.UnwindRequestsDropped) and the PID is
	// forgotten so its next batch retries, rather than pinning a slot that
	// would never be filled.
	unwindWorkDepth = 256
)

func newPIDRegistry(reg pidRegistrar, capacity int) *pidRegistry {
	if capacity <= 0 {
		capacity = defaultUnwindPIDCapacity
	}
	r := &pidRegistry{
		reg:      reg,
		capacity: capacity,
		work:     make(chan pidWork, unwindWorkDepth),
		byPID:    map[uint32]*pidRegEntry{},
		lru:      list.New(),
	}
	r.wg.Add(1)
	go r.run()
	return r
}

// note records that a batch arrived from pid and returns whether the walker
// already has tables for it. First sighting of an unknown PID queues its
// registration; every sighting refreshes its LRU position.
//
// O(1) and never blocks: it is called from Consumer.applyBatch, on the
// goroutine draining the ringbuf, once per batch.
//
// A nil registry (every unit test that builds a Consumer directly, and any
// build where registration could not be set up) answers "no tables", which is
// the truth and makes StacksWalkedNoTables count rather than lie.
func (r *pidRegistry) note(pid uint32) bool {
	if r == nil || pid == 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byPID[pid]; ok {
		r.lru.MoveToFront(e.elem)
		return e.state == pidReady
	}
	if r.closed {
		return false
	}
	e := &pidRegEntry{pid: pid, state: pidPending}
	e.elem = r.lru.PushFront(e)
	r.byPID[pid] = e
	r.evictLocked()
	if !r.enqueueLocked(pidWork{pid: pid}) {
		r.stats.requestsDropped++
		// Forget the placeholder: a pending entry whose work was never queued
		// would occupy a bounded slot forever and never become ready. Dropping
		// it means the next batch from this PID tries again.
		r.lru.Remove(e.elem)
		delete(r.byPID, pid)
	}
	return false
}

// evictLocked trims the set back to capacity, oldest sighting first. Caller
// holds mu.
func (r *pidRegistry) evictLocked() {
	for r.lru.Len() > r.capacity {
		back := r.lru.Back()
		if back == nil {
			return
		}
		ve, _ := back.Value.(*pidRegEntry)
		r.lru.Remove(back)
		if ve == nil {
			continue
		}
		delete(r.byPID, ve.pid)
		r.stats.evicted++
		// Only a PID that actually holds tables has anything to release. A
		// pending one is handled by the worker, which finds it gone and
		// releases whatever it just installed (see run).
		if ve.state == pidReady && !r.enqueueLocked(pidWork{pid: ve.pid, release: true}) {
			r.stats.requestsDropped++
		}
	}
}

// enqueueLocked hands one request to the worker without ever blocking. Caller
// holds mu; a non-blocking send cannot deadlock against the worker, which
// takes mu only after its I/O has finished.
func (r *pidRegistry) enqueueLocked(w pidWork) bool {
	if r.closed {
		return false
	}
	select {
	case r.work <- w:
		return true
	default:
		return false
	}
}

// registerNow installs pid's tables synchronously and records the outcome. Used
// only by the eager path (Config.PID != 0), where the point is that the tables
// exist before the uprobe link does. Returns what the registrar returned so
// Attach can decide whether to care; it never fails the attach - a consumer
// without CFI tables still profiles, with frame-pointer stacks, and says so.
func (r *pidRegistry) registerNow(pid uint32) (int, error) {
	if r == nil || pid == 0 {
		return 0, nil
	}
	n, err := r.reg.Register(pid)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byPID[pid]
	if !ok {
		e = &pidRegEntry{pid: pid, state: pidPending}
		e.elem = r.lru.PushFront(e)
		r.byPID[pid] = e
	}
	r.recordLocked(e, n, err)
	r.evictLocked()
	return n, err
}

// recordLocked folds one registration outcome into the entry and the counters.
// Caller holds mu. e may be nil when the entry was evicted while the worker was
// compiling.
func (r *pidRegistry) recordLocked(e *pidRegEntry, n int, err error) {
	if err != nil || n == 0 {
		r.stats.failed++
		if err != nil {
			// Why, not just how many: a registration that fails because the
			// process already exited and one that fails because /proc is
			// unreadable are different problems, and a bare count cannot tell
			// them apart. Same reasoning as decodeFailureTable.
			r.stats.lastErr = err.Error()
		}
		if e != nil {
			e.state = pidFailed
		}
		return
	}
	r.stats.registered++
	r.stats.binariesAttached += uint64(n)
	if e != nil {
		e.state = pidReady
	}
}

// run is the worker: one goroutine, all the registrar I/O. Ends when close
// closes the work channel.
func (r *pidRegistry) run() {
	defer r.wg.Done()
	for w := range r.work {
		if w.release {
			if err := r.reg.Unregister(w.pid); err != nil {
				r.mu.Lock()
				r.stats.releaseFailed++
				r.stats.lastErr = err.Error()
				r.mu.Unlock()
			}
			continue
		}
		n, err := r.reg.Register(w.pid)
		r.mu.Lock()
		e := r.byPID[w.pid]
		r.recordLocked(e, n, err)
		evictedMidFlight := e == nil && err == nil && n > 0
		r.mu.Unlock()
		if evictedMidFlight {
			// The bound pushed this PID out while its tables were being
			// compiled. Nothing will ever release them otherwise, and the
			// whole point of the bound is that nothing accumulates.
			if uerr := r.reg.Unregister(w.pid); uerr != nil {
				r.mu.Lock()
				r.stats.releaseFailed++
				r.mu.Unlock()
			}
		}
	}
}

// ready reports whether the walker has tables for pid *right now*.
//
// Consumer.resolveStackLocked uses it to tell "no tables yet for this PID" from
// "tables exist and the walk still could not use them". The answer is a lower
// bound on the first of those, and deliberately so: an entry only ever moves
// from pending to ready, never back, so "not ready now" proves "not ready when
// the walk ran", while "ready now" may cover a walk that ran a moment before
// registration completed. The error is therefore always in the direction of
// under-counting StacksWalkedNoTables, never of claiming tables were missing
// when they were not.
func (r *pidRegistry) ready(pid uint32) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byPID[pid]
	return ok && e.state == pidReady
}

// snapshot returns the registry's counters plus the current tracked-PID gauge.
func (r *pidRegistry) snapshot() (unwindStats, int) {
	if r == nil {
		return unwindStats{}, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats, len(r.byPID)
}

// close stops the worker and waits for any registration still running.
//
// The wait is not optional and it is not cosmetic: the worker writes to the BPF
// maps, and Consumer.Close closes those maps immediately afterwards. Returning
// before the worker is done would let a compile finish writing into closed file
// descriptors. Waiting can take as long as one binary's compile (hundreds of
// milliseconds for something libcuda-sized), which is the right trade against
// tearing down maps underneath a live writer.
func (r *pidRegistry) close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.work)
	r.mu.Unlock()
	r.wg.Wait()
}
