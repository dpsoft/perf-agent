//go:build linux && amd64

package pyunwind

import (
	"fmt"
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
	defer func() { _ = unix.PtraceDetach(r.tid) }()

	if err := unix.PtraceInterrupt(r.tid); err != nil {
		return 0, fmt.Errorf("pyunwind: ptrace interrupt tid %d: %w", r.tid, err)
	}
	if err := waitForTraceStop(r.tid, tlsStopTimeout); err != nil {
		return 0, err
	}

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
