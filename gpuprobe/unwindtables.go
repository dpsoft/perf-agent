package gpuprobe

import (
	"container/list"
	"errors"
	"log"
	"sync"

	"github.com/dpsoft/perf-agent/pyunwind"
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
	// python is gpu_usdt's copy of py_procs and py_eval_ranges. The
	// interpreter arm lives in walk_step, which this program shares with
	// perf_dwarf and offcpu_dwarf, so a CUDA launch made from Python
	// carries Python frames here for free -- but only once these two maps
	// are populated for the producing process. Nothing else populates them
	// on this path.
	python *pyunwind.BPFMaps
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
		python:  &pyunwind.BPFMaps{PyProcs: objs.PyProcs, EvalRanges: objs.PyEvalRanges},
	}
}

func (r *ehmapsRegistrar) Register(pid uint32) (int, error) {
	n, err := ehmaps.AttachAllMappings(r.tracker, pid)
	if err != nil {
		return n, err
	}
	// CPython frames at the launch site. This is the case the whole design
	// exists for -- a torch/numpy program whose cudaLaunchKernel is reached
	// from Python -- and it gets them from the same walk_step arm the CPU
	// profilers use, so all that is needed here is the per-process record
	// and the eval range.
	//
	// AFTER AttachAllMappings, never before: the eval range is keyed by
	// table_id, and a table_id only resolves to a PC once that binary has a
	// pid_mappings row.
	//
	// Best-effort and never fatal to CFI registration: a producer whose
	// interpreter cannot be walked must still get native GPU stacks. Every
	// outcome is logged, including the refusals, because "not a Python
	// process" and "Python we decline to walk" are different answers to the
	// only question a user with no Python frames has.
	r.enrollPython(pid)
	return n, nil
}

// enrollPython installs the CPython walker's per-process state for a GPU
// producer. Mirrors dwarfagent.enrollPython; both are thin wrappers over
// pyunwind.EnrollTarget so the two paths cannot drift into enrolling
// different sets of processes.
//
// UNVALIDATED ON HARDWARE. The wiring is symmetric with the DWARF path and
// the BPF side is literally the same code, but no machine in CI has a GPU,
// so no test anywhere has yet seen a Python frame on a gpu_launch_sampled
// stack. See the task report; this needs a run on the 3090.
func (r *ehmapsRegistrar) enrollPython(pid uint32) {
	if r.python == nil || r.python.PyProcs == nil || r.python.EvalRanges == nil {
		return
	}
	libPath, found, res, err := pyunwind.EnrollTarget(pid, r.python)
	switch {
	case !found && err == nil:
		return
	case err != nil:
		log.Printf("gpuprobe: python frames: pid %d: REFUSED %s: %v", pid, libPath, err)
	case res.Refused != "":
		log.Printf("gpuprobe: python frames: pid %d: REFUSED %s (CPython %s): %s",
			pid, libPath, res.Version, res.Refused)
	default:
		log.Printf("gpuprobe: python frames: pid %d: attached %s (CPython %s)", pid, libPath, res.Version)
	}
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
	// enrolled records that this PID came through the startup rendezvous
	// (enrollListener), i.e. that the producer blocked in its own init
	// waiting for these tables. It exists so a walk that still found no
	// tables can be told apart from one whose process never enrolled at
	// all - see Stats.StacksNoTablesAfterEnroll, which is zero by
	// construction unless the handshake failed to do its job.
	enrolled bool
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
	registered           uint64
	failed               uint64
	evicted              uint64
	enrolledEvicted      uint64
	enrolledMarksDropped uint64
	requestsDropped      uint64
	releaseFailed        uint64
	binariesAttached     uint64
	lastErr              string
}

// enrollOutcome is what the rendezvous decided for one connecting producer.
type enrollOutcome uint8

const (
	// enrollInstalled: the tables were compiled and installed by this call.
	enrollInstalled enrollOutcome = iota
	// enrollAlreadyHeld: the walker already had tables for this PID, so
	// nothing was recompiled. ehmaps.PIDTracker.Attach accumulates mappings
	// and takes a table reference every time it is called, so a second
	// registration for the same PID would duplicate both.
	enrollAlreadyHeld
	// enrollInFlight: another path (the lazy worker) is registering this PID
	// right now. The producer is released without a promise, because the
	// registry cannot honestly make one.
	enrollInFlight
	// enrollFailed: registration ran and installed nothing.
	enrollFailed
)

// pidRegistry decides which PIDs get CFI tables, and bounds the set.
//
// Two lifecycle points feed it, and they are different on purpose:
//
//   - Config.PID != 0. Attach calls registerNow before the uprobe link exists,
//     so the tables are in place before the probe can fire even once. This is
//     the eager path, and it is what unwind/dwarfagent does for a per-PID
//     profiler (mode is forced to ModeEager there for the same reason).
//
//   - Config.PID == 0 (system-wide, the documented default). The target
//     process may not exist at Attach time - the gate's stub is launched after
//     Attach, because the shim only emits once the uprobe's semaphore says
//     someone is listening. Two things can feed the registry here, and only
//     one of them is in time:
//
//     enroll(), from the startup rendezvous (enroll.go). The producer blocks
//     in its own initialisation - before its first launch, and therefore
//     before its first probe - until this call has installed its tables. This
//     is the #49 fix, and it is the only path that puts tables in the maps
//     before the kernel-side walk needs them.
//
//     note(), on the first batch from a PID. The fallback, for a producer
//     that never reached the rendezvous (it was already running when the
//     consumer attached, or the socket was unavailable). Everything sampled
//     during the compile it starts is walked without tables; that is the
//     ~38% loss issue #49 measured, and it is what Stats.StacksWalkedNoTables
//     still counts, truthfully.
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

	mu    sync.Mutex
	byPID map[uint32]*pidRegEntry
	lru   *list.List // front = most recently seen
	// wasEnrolled remembers PIDs that completed the startup rendezvous and
	// were later evicted from byPID.
	//
	// Without it the enrolled mark dies with the entry, and the one case
	// StacksNoTablesAfterEnroll exists to catch - "we promised a producer its
	// tables and then took them back" - reads as zero while
	// StacksWalkedNoTables climbs. That is this project's signature defect
	// (a counter green exactly when things are worst) inside the counter
	// added to prevent it.
	//
	// Bounded FIFO, same order of size as the registry itself, so it cannot
	// grow with a profiled machine's process churn. The bound's cost is a
	// false NEGATIVE for a PID whose mark has aged out, counted in
	// Stats.UnwindEnrolledMarksDropped so the under-report is never silent;
	// PID recycling can produce a false POSITIVE, which UnwindEnrolledPIDsEvicted
	// is the cross-check for. See rememberEnrolledLocked for why it fills per
	// enrolment rather than per eviction.
	wasEnrolled     map[uint32]struct{}
	wasEnrolledFIFO *list.List
	// wasEnrolledCap bounds it. Deliberately larger than the registry's own
	// capacity: the set is fed once per ENROLMENT and once per eviction of an
	// enrolled PID, so sizing it equal to the LRU made the two churn against
	// each other and aged marks out roughly twice as fast as evictions
	// happened. A multiple leaves an evicted PID's mark comfortably alive
	// while it is the thing the counter needs to see.
	wasEnrolledCap int
	stats          unwindStats
	closed         bool
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

	// enrolledMarkOvercommit sizes the evicted-enrolment memory relative to
	// the PID capacity. See pidRegistry.wasEnrolledCap: at 1x the set churned
	// against the LRU itself and dropped marks about twice as fast as
	// evictions occurred, which is under-reporting in the one counter that
	// must not under-report silently. Four PIDs' worth of marks per tracked
	// PID is a few kilobytes at the default capacity.
	enrolledMarkOvercommit = 4

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
		reg:             reg,
		capacity:        capacity,
		work:            make(chan pidWork, unwindWorkDepth),
		byPID:           map[uint32]*pidRegEntry{},
		lru:             list.New(),
		wasEnrolled:     map[uint32]struct{}{},
		wasEnrolledFIFO: list.New(),
		wasEnrolledCap:  capacity * enrolledMarkOvercommit,
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
	e := &pidRegEntry{pid: pid, state: pidPending, enrolled: r.wasEnrolledLocked(pid)}
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
		if ve.enrolled {
			// A producer we released on the promise that its tables were
			// installed, whose tables we are now taking back. Counted, and
			// remembered, so its next tableless walk is not read as an
			// ordinary startup transient.
			r.stats.enrolledEvicted++
			r.rememberEnrolledLocked(ve.pid)
		}
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
		e = &pidRegEntry{pid: pid, state: pidPending, enrolled: r.wasEnrolledLocked(pid)}
		e.elem = r.lru.PushFront(e)
		r.byPID[pid] = e
	}
	r.recordLocked(e, n, err)
	r.evictLocked()
	return n, err
}

// wasEnrolledLocked reports whether pid completed the rendezvous before it was
// evicted. Caller holds mu.
func (r *pidRegistry) wasEnrolledLocked(pid uint32) bool {
	_, ok := r.wasEnrolled[pid]
	return ok
}

// rememberEnrolledLocked records a PID's rendezvous so the mark can outlive
// its registry entry. Bounded FIFO. Caller holds mu.
//
// Note that it fills once per ENROLMENT, not once per eviction: enroll()
// records the PID before it starts compiling, because that entry can be
// evicted while its tables are still being built and the mark has to survive
// that. So the set holds the last `capacity` enrolled PIDs, and a mark ages
// out after `capacity` further enrolments rather than after `capacity`
// evictions. Every mark that falls off the end is counted
// (Stats.UnwindEnrolledMarksDropped): the consequence is that
// StacksNoTablesAfterEnroll under-reports for that PID, and a silent
// under-report in this particular counter is exactly the failure it exists
// to prevent.
func (r *pidRegistry) rememberEnrolledLocked(pid uint32) {
	if _, ok := r.wasEnrolled[pid]; ok {
		return
	}
	r.wasEnrolled[pid] = struct{}{}
	r.wasEnrolledFIFO.PushFront(pid)
	for r.wasEnrolledFIFO.Len() > r.wasEnrolledCap {
		back := r.wasEnrolledFIFO.Back()
		if back == nil {
			return
		}
		r.wasEnrolledFIFO.Remove(back)
		if old, ok := back.Value.(uint32); ok {
			delete(r.wasEnrolled, old)
			r.stats.enrolledMarksDropped++
		}
	}
}

// enroll installs pid's tables for the startup rendezvous and reports what it
// found. Called on the enrollListener's goroutine, with a producer blocked in
// its own initialisation waiting for the answer, which is the whole point:
// the compile happens while nothing has launched yet.
//
// Not the same call as registerNow, for two reasons that both bite:
//
//   - It claims the registry entry BEFORE the compile, so a batch that
//     arrives mid-compile (Consumer.applyBatch -> note) sees a known PID and
//     does not queue a second registration for it. Two registrations for one
//     PID would duplicate its pid_mappings rows and double the reference it
//     holds on every shared CFI table.
//   - It never recompiles a PID the walker already has tables for. Repeat
//     connections - a producer that re-enrolls, or anything else that reaches
//     the socket - therefore cost a map lookup, not a libcuda compile.
//
// The entry is marked enrolled either way, so a later walk that still finds
// no tables is counted as the contradiction it is.
func (r *pidRegistry) enroll(pid uint32) (enrollOutcome, error) {
	if r == nil || pid == 0 {
		return enrollFailed, errors.New("gpuprobe: no unwind registry")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return enrollFailed, errors.New("gpuprobe: consumer is closing")
	}
	if e, ok := r.byPID[pid]; ok {
		e.enrolled = true
		r.lru.MoveToFront(e.elem)
		state := e.state
		r.mu.Unlock()
		switch state {
		case pidReady:
			return enrollAlreadyHeld, nil
		case pidPending:
			return enrollInFlight, nil
		default:
			return enrollFailed, errors.New("gpuprobe: registration already failed for this pid")
		}
	}
	e := &pidRegEntry{pid: pid, state: pidPending, enrolled: true}
	e.elem = r.lru.PushFront(e)
	r.byPID[pid] = e
	// Recorded before the compile, not after: this entry can be evicted while
	// its tables are still being built, and the mark has to outlive it.
	r.rememberEnrolledLocked(pid)
	r.evictLocked()
	r.mu.Unlock()

	n, err := r.reg.Register(pid)

	r.mu.Lock()
	cur := r.byPID[pid]
	r.recordLocked(cur, n, err)
	// The bound pushed this PID out while its tables were being compiled;
	// same hazard, and same remedy, as the worker's evictedMidFlight arm.
	evictedMidFlight := cur == nil && err == nil && n > 0
	r.mu.Unlock()
	if evictedMidFlight {
		if uerr := r.reg.Unregister(pid); uerr != nil {
			r.mu.Lock()
			r.stats.releaseFailed++
			r.mu.Unlock()
		}
		return enrollFailed, errors.New("gpuprobe: pid evicted while its tables were compiling")
	}
	if err != nil {
		return enrollFailed, err
	}
	if n == 0 {
		return enrollFailed, errors.New("gpuprobe: no binary with usable .eh_frame")
	}
	return enrollInstalled, nil
}

// enrolled reports whether pid went through the startup rendezvous. Used only
// to make StacksNoTablesAfterEnroll mean what it says; see pidRegEntry.
func (r *pidRegistry) enrolled(pid uint32) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byPID[pid]; ok {
		return e.enrolled
	}
	// Not tracked right now, but it may have been evicted after a successful
	// rendezvous - which is precisely the case this counter must not miss.
	return r.wasEnrolledLocked(pid)
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

// snapshotTracked is the tracked-PID gauge on its own, for tests that need to
// observe the moment an entry is claimed.
func (r *pidRegistry) snapshotTracked() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byPID)
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
