//go:build linux && amd64

package pyunwind

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// tlsStopTimeout bounds how long the attach path waits for a thread to
// reach its ptrace stop.
//
// It is polled rather than waited on because a blocking waitpid here can
// wait arbitrarily long: PTRACE_INTERRUPT is delivered like a signal, and a
// thread in an uninterruptible sleep (a slow disk read, a hung NFS mount)
// does not reach its stop until that sleep ends. A profiler that hangs at
// startup because the target was mid-IO is a far worse failure than one
// that declines to walk its Python frames, so this gives up and refuses by
// name instead.
const tlsStopTimeout = 2 * time.Second

// tlsReleaseTimeout bounds releaseTracee. It is longer than tlsStopTimeout
// on purpose: giving up on READING the FS base costs a refusal, while
// giving up on DETACHING leaves a thread that the next signal freezes. The
// second is worth waiting longer to avoid, and the wait only happens on a
// path that has already gone wrong.
const tlsReleaseTimeout = 10 * time.Second

// releaseTracee undoes a PTRACE_SEIZE, and does not return until it has --
// or until the thread is gone, or the deadline passes.
//
// PTRACE_DETACH requires a ptrace-stop, so on any path where the tracee was
// never stopped (PTRACE_INTERRUPT failed, or the stop never arrived because
// the thread sat in uninterruptible sleep) a bare detach fails with ESRCH
// and silently leaves the thread traced. This drives it into a stop first:
// interrupt, reap the stop notification, then detach, retrying until one of
// them takes.
//
// stopped is a POINTER because the caller learns it mid-function: the
// normal path has already consumed a stop by the time the deferred release
// runs, and interrupting an already-stopped tracee again would leave a
// second notification queued.
func releaseTracee(tid int, stopped *bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !*stopped {
			if err := unix.PtraceInterrupt(tid); err != nil {
				if !threadExists(tid) {
					return nil // exited: there is nothing left to detach from
				}
			}
			var ws unix.WaitStatus
			wpid, err := unix.Wait4(tid, &ws, unix.WALL|unix.WNOHANG, nil)
			if err == nil && wpid == tid {
				if !ws.Stopped() {
					return nil // exited under us
				}
				*stopped = true
			}
		}
		if err := unix.PtraceDetach(tid); err == nil {
			return nil
		} else if !threadExists(tid) {
			return nil
		} else {
			// ESRCH here means "not in a ptrace-stop", not "no such
			// thread" -- threadExists just said otherwise. Go round again.
			*stopped = false
			if time.Now().After(deadline) {
				return fmt.Errorf("pyunwind: detach tid %d: %w", tid, err)
			}
		}
		time.Sleep(time.Millisecond)
	}
}

// threadExists reports whether a tid is still live, so ESRCH from ptrace
// can be told apart from "not stopped" -- the two are indistinguishable
// from the errno alone and mean opposite things here.
func threadExists(tid int) bool {
	_, err := os.Stat(fmt.Sprintf("/proc/%d", tid))
	return err == nil
}

// waitForTraceStop polls for the tracee's ptrace stop.
//
// __WALL: the tracee is a thread, not a child process, and waitpid without
// it does not report clone-children that do not deliver SIGCHLD.
func waitForTraceStop(tid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		var ws unix.WaitStatus
		wpid, err := unix.Wait4(tid, &ws, unix.WALL|unix.WNOHANG, nil)
		if err != nil {
			return fmt.Errorf("pyunwind: wait for tid %d to stop: %w", tid, err)
		}
		if wpid == tid {
			if !ws.Stopped() {
				return fmt.Errorf("pyunwind: tid %d did not stop (wait status %#x)", tid, uint32(ws))
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pyunwind: tid %d did not reach a ptrace stop within %s (uninterruptible sleep?)", tid, timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// tlsBase reads the target thread's FS base -- the address glibc's
// `struct pthread` starts at, and so the base py_tss_get indexes the TSD
// block from.
//
// It is read with ptrace because there is no other way from userspace: the
// FS base is a register, not memory, and /proc exposes it nowhere. (The BPF
// side has it for free -- task_struct.thread.fsbase -- which is why this
// replica is only needed at attach time, once per process, and never per
// sample.)
//
// PTRACE_SEIZE + PTRACE_INTERRUPT rather than PTRACE_ATTACH: SEIZE does not
// inject a SIGSTOP the tracee can observe, and INTERRUPT's stop is undone
// exactly by the DETACH below. The target thread is stopped for the length
// of one GETREGS.
//
// THE DETACH IS THE DANGEROUS PART, not the stop. PTRACE_DETACH is only
// legal while the tracee is in a ptrace-stop; called on a seized-but-running
// thread it returns ESRCH and does nothing, leaving the thread traced with
// no tracer that will ever continue it. The next signal delivered to such a
// thread parks it in `t (tracing stop)` until this process exits -- and if
// that thread holds the GIL, the whole interpreter stops. A profiler must
// not be able to do that. releaseTracee therefore never assumes the stop:
// it drives the tracee into one and refuses to return until the detach has
// actually succeeded or the thread is gone.
//
// runtime.LockOSThread is not optional. ptrace is a per-THREAD relationship
// in the kernel: every request for a tracee must come from the same OS
// thread that seized it, and Go will otherwise reschedule this goroutine
// onto another thread between calls, at which point PTRACE_GETREGS fails
// with ESRCH.
func (r *ProcReader) tlsBase() (uint64, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unix.PtraceSeize(r.tid); err != nil {
		return 0, fmt.Errorf("pyunwind: ptrace seize tid %d: %w", r.tid, err)
	}
	stopped := false
	defer func() {
		if err := releaseTracee(r.tid, &stopped, tlsReleaseTimeout); err != nil {
			// Loud, not swallowed: this is the state in which a target
			// thread can be frozen by the next signal it receives, and
			// nothing else in the system will report it.
			log.Printf("pyunwind: WARNING: tid %d is still ptrace-attached after %s of trying to detach: %v -- "+
				"a signal delivered to it will stop it until perf-agent exits",
				r.tid, tlsReleaseTimeout, err)
		}
	}()

	if err := unix.PtraceInterrupt(r.tid); err != nil {
		return 0, fmt.Errorf("pyunwind: ptrace interrupt tid %d: %w", r.tid, err)
	}
	if err := waitForTraceStop(r.tid, tlsStopTimeout); err != nil {
		return 0, err
	}
	stopped = true

	var regs unix.PtraceRegs
	if err := unix.PtraceGetRegs(r.tid, &regs); err != nil {
		return 0, fmt.Errorf("pyunwind: ptrace getregs tid %d: %w", r.tid, err)
	}
	if regs.Fs_base == 0 {
		// Zero is not a plausible FS base for a live glibc thread, and
		// hostTSSGet would happily index a TSD block off address zero and
		// report the read failure from there instead of from here.
		return 0, fmt.Errorf("pyunwind: tid %d reports a zero FS base", r.tid)
	}
	return regs.Fs_base, nil
}
