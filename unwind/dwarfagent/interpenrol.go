package dwarfagent

import (
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// interpEnroller is the part of *interp.Set the system-wide path drives.
// Narrowed to an interface so the bookkeeping below can be tested without
// loading a BPF object or stopping a real process.
type interpEnroller interface {
	Enroll(pid uint32, logf func(format string, args ...any)) bool
	Detach(pid uint32, logf func(format string, args ...any))
}

// lazyEnrolQueue bounds the PIDs waiting for a first look. A PID that does not
// fit is not marked seen, so its next sample offers it again: a burst of new
// processes delays enrolment rather than losing it.
const lazyEnrolQueue = 1024

// pidInterpState is where one PID is in system-wide interpreter enrolment.
type pidInterpState uint8

const (
	// pidQueued: offered to the worker, not yet looked at.
	pidQueued pidInterpState = iota + 1
	// pidEnrolling: the worker is inside Enroll for it right now.
	pidEnrolling
	// pidEnrollingExited: the process exited while the worker was inside
	// Enroll; the worker detaches it as soon as Enroll returns.
	pidEnrollingExited
	// pidForeign: no module recognised it. Re-armed by an executable mmap,
	// which is how an exec into an interpreter shows up.
	pidForeign
	// pidClaimed: a module recognised it (attached, refused, or retrying).
	// Held until the process exits.
	pidClaimed
)

// lazyInterp enrols interpreter processes in a system-wide capture, issue #194.
//
// THE TRIGGER IS THE FIRST SAMPLE, not startup and not exec. Enrolment costs a
// ptrace stop per interpreter process (measured on #194: ~21µs of the target
// held, ~6ms of the agent's own time per process), and paying it up front for
// every process on a box charges processes that are never sampled. A process
// that is sampled at all gets looked at once, by one background goroutine,
// off the ring-reader's path; its first samples are native-only, every later
// one can carry interpreter frames.
//
// Exec needs no hook of its own. A process first seen before it exec'd into
// an interpreter -- a shell's fork about to become python3 -- is recorded as
// foreign, and the executable mmaps the exec produces re-arm it, so the next
// sample looks again. That keeps this file, like the rest of the package,
// free of any knowledge of which binaries are interpreters.
//
// PID reuse is handled at exit: a claimed process's per-PID module state is
// detached and the PID forgotten, so a recycled PID is looked at afresh rather
// than walked with its previous occupant's offsets.
type lazyInterp struct {
	set  interpEnroller
	logf func(format string, args ...any)

	mu   sync.Mutex
	pids map[uint32]pidInterpState

	queue chan uint32
	stop  chan struct{}
	wg    sync.WaitGroup

	// recognised is whether any module recognised any process, which is what
	// decides whether the shutdown counter line is news. See Set.LogCounters.
	recognised atomic.Bool
	// For the one summary line at close.
	offered  atomic.Uint64
	claimed  atomic.Uint64
	deferred atomic.Uint64
}

func newLazyInterp(set interpEnroller, logf func(format string, args ...any)) *lazyInterp {
	l := &lazyInterp{
		set:   set,
		logf:  logf,
		pids:  map[uint32]pidInterpState{},
		queue: make(chan uint32, lazyEnrolQueue),
		stop:  make(chan struct{}),
	}
	l.wg.Add(1)
	go l.run()
	return l
}

// sampled is called from the ring reader for every sample with a user stack.
// It must stay cheap: one map lookup under a mutex for every PID already seen.
func (l *lazyInterp) sampled(pid uint32) {
	if l == nil || pid == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, seen := l.pids[pid]; seen {
		return
	}
	select {
	case l.queue <- pid:
		l.pids[pid] = pidQueued
	default:
		l.deferred.Add(1)
	}
}

// observe is a PIDTracker observer: exits detach, executable mmaps re-arm.
func (l *lazyInterp) observe(ev ehmaps.MmapEventRecord) {
	if l == nil {
		return
	}
	switch ev.Kind {
	case ehmaps.ExitEvent:
		if ev.TID == ev.PID {
			l.exited(ev.PID)
		}
	case ehmaps.MmapEvent:
		if ev.Prot&unix.PROT_EXEC != 0 && ev.Filename != "" && !strings.HasPrefix(ev.Filename, "[") {
			l.mu.Lock()
			if l.pids[ev.PID] == pidForeign {
				delete(l.pids, ev.PID)
			}
			l.mu.Unlock()
		}
	}
}

func (l *lazyInterp) exited(pid uint32) {
	l.mu.Lock()
	st, seen := l.pids[pid]
	switch {
	case !seen:
	case st == pidEnrolling:
		// The worker owns it until Enroll returns; detaching now would race
		// the record Enroll is about to write.
		l.pids[pid] = pidEnrollingExited
	default:
		delete(l.pids, pid)
	}
	l.mu.Unlock()
	if st == pidClaimed {
		l.set.Detach(pid, l.logf)
	}
}

func (l *lazyInterp) run() {
	defer l.wg.Done()
	for {
		select {
		case <-l.stop:
			return
		case pid := <-l.queue:
			l.enrol(pid)
		}
	}
}

func (l *lazyInterp) enrol(pid uint32) {
	l.mu.Lock()
	// Only a PID still waiting is looked at. One that exited while queued is
	// gone from the map; one queued twice across a reuse is already decided.
	if l.pids[pid] != pidQueued {
		l.mu.Unlock()
		return
	}
	l.pids[pid] = pidEnrolling
	l.mu.Unlock()

	l.offered.Add(1)
	found := l.set.Enroll(pid, l.logf)
	if found {
		l.recognised.Store(true)
		l.claimed.Add(1)
	}

	l.mu.Lock()
	exited := l.pids[pid] == pidEnrollingExited
	switch {
	case exited:
		delete(l.pids, pid)
	case found:
		l.pids[pid] = pidClaimed
	default:
		l.pids[pid] = pidForeign
	}
	l.mu.Unlock()
	if exited && found {
		l.set.Detach(pid, l.logf)
	}
}

// close stops the worker and waits for an enrolment in flight, which writes
// into the driver's maps. Call before the interpreter Set is closed.
func (l *lazyInterp) close() {
	if l == nil {
		return
	}
	close(l.stop)
	l.wg.Wait()
	l.logf("interpreter frames (system-wide): looked at %d sampled processes, %d recognised by a module, "+
		"%d first looks deferred by a full queue", l.offered.Load(), l.claimed.Load(), l.deferred.Load())
}

// recognisedAny reports whether any module recognised any process.
func (l *lazyInterp) recognisedAny() bool {
	return l != nil && l.recognised.Load()
}
