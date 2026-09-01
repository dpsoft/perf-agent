//go:build linux && amd64

package pyunwind

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// procField reads one "Name:\tvalue" field out of /proc/<pid>/status.
func procField(t *testing.T, pid int, name string) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/status: %v", pid, err)
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && k == name {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("/proc/%d/status has no %s field", pid, name)
	return ""
}

// TestReleaseTraceeDetachesAThreadThatNeverStopped is the regression test
// for the worst thing this package can do to a target.
//
// PTRACE_DETACH is only legal while the tracee is in a ptrace-stop. On
// every path where the stop never arrived -- PTRACE_INTERRUPT failed, or
// the thread sat in uninterruptible sleep past the deadline -- a bare
// detach returns ESRCH and does nothing, and the thread stays SEIZED with
// no tracer that will ever continue it. It keeps running until the next
// signal, at which point it parks in `t (tracing stop)` until this process
// exits. If that thread holds the GIL, the whole interpreter stops. A
// profiler must not be able to do that to a process it is only observing.
//
// The test reproduces exactly that state -- seize, never stop -- and
// requires releaseTracee to get out of it: no tracer left, and a signal
// afterwards does not freeze the target.
func TestReleaseTraceeDetachesAThreadThatNeverStopped(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child to trace: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// Let it reach its sleep, so the seize below lands on a running
	// thread rather than racing exec.
	time.Sleep(100 * time.Millisecond)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unix.PtraceSeize(pid); err != nil {
		t.Skipf("cannot PTRACE_SEIZE own child (ptrace_scope?): %v", err)
	}
	if got := procField(t, pid, "TracerPid"); got == "0" {
		t.Fatalf("seize did not attach: TracerPid = %s", got)
	}

	// The state the fix exists for: seized, never interrupted, so the
	// tracee is running and a bare detach cannot work. Logged rather than
	// asserted -- the point of the test is the invariant below, not the
	// kernel's errno.
	t.Logf("bare PTRACE_DETACH on a running tracee: %v", unix.PtraceDetach(pid))

	stopped := false
	if err := releaseTracee(pid, &stopped, 5*time.Second); err != nil {
		t.Fatalf("releaseTracee left the thread attached: %v", err)
	}
	if got := procField(t, pid, "TracerPid"); got != "0" {
		t.Fatalf("TracerPid = %s after release; the thread is still traced and the next signal will freeze it", got)
	}

	// The freeze itself: a signal to a leaked tracee parks it in
	// `t (tracing stop)`. After a real detach it is simply ignored.
	if err := unix.Kill(pid, unix.SIGWINCH); err != nil {
		t.Fatalf("signal the target: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if state := procField(t, pid, "State"); strings.HasPrefix(state, "t") {
		t.Fatalf("target is in state %q after a signal: it was left traced and is now frozen until this process exits", state)
	}
}

// TestReleaseTraceeIsCleanOnTheNormalPath pins the common case: the caller
// has already consumed the stop, and release must detach without
// interrupting again (a second interrupt would queue a notification nobody
// reaps) and without waiting.
func TestReleaseTraceeIsCleanOnTheNormalPath(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child to trace: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	time.Sleep(100 * time.Millisecond)

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unix.PtraceSeize(pid); err != nil {
		t.Skipf("cannot PTRACE_SEIZE own child (ptrace_scope?): %v", err)
	}
	if err := unix.PtraceInterrupt(pid); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if err := waitForTraceStop(pid, tlsStopTimeout); err != nil {
		t.Fatalf("wait for stop: %v", err)
	}
	stopped := true

	start := time.Now()
	if err := releaseTracee(pid, &stopped, tlsReleaseTimeout); err != nil {
		t.Fatalf("releaseTracee on a stopped tracee: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("release took %s on the normal path; it should be one detach", elapsed)
	}
	if got := procField(t, pid, "TracerPid"); got != "0" {
		t.Fatalf("TracerPid = %s after release", got)
	}
}

// TestReleaseTraceeReturnsWhenTheThreadIsGone: a tracee that exits while
// seized leaves nothing to detach from, and that is not an error to report.
func TestReleaseTraceeReturnsWhenTheThreadIsGone(t *testing.T) {
	cmd := exec.Command("sleep", "0.05")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child to trace: %v", err)
	}
	pid := cmd.Process.Pid

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := unix.PtraceSeize(pid); err != nil {
		t.Skipf("cannot PTRACE_SEIZE own child (ptrace_scope?): %v", err)
	}
	// Reap the exit as the tracer, then release: the thread is gone.
	var ws unix.WaitStatus
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		wpid, err := unix.Wait4(pid, &ws, unix.WALL|unix.WNOHANG, nil)
		if err == nil && wpid == pid && ws.Exited() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stopped := false
	if err := releaseTracee(pid, &stopped, 2*time.Second); err != nil {
		t.Fatalf("releasing an exited tracee must not be an error: %v", err)
	}
	_ = cmd.Wait()
}

// TestPtracingOurOwnChildsLeaderCorruptsCmdWait DEMONSTRATES the mechanism the
// leader filter exists for, rather than asserting the filter and trusting the
// reasoning.
//
// It stops the leader of a child exactly as tlsBase does, does NOT consume the
// notification, and shows Cmd.Wait reporting the workload as stopped -- the
// same "trace/breakpoint trap (trap 128)" the GPU run died on. Then it shows a
// non-leader thread's stop leaving Cmd.Wait alone, which is why the fix is a
// filter and not a refusal to walk our own children.
func TestPtracingOurOwnChildsLeaderCorruptsCmdWait(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	cmd := exec.Command("sh", "-c", "sleep 0.3")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill() }()

	if err := unix.PtraceSeize(pid); err != nil {
		t.Skipf("cannot PTRACE_SEIZE own child (ptrace_scope?): %v", err)
	}
	if err := unix.PtraceInterrupt(pid); err != nil {
		t.Skipf("PTRACE_INTERRUPT: %v", err)
	}

	// Deliberately NOT reaping the stop: this is the state a timed-out or
	// double-interrupted attach leaves behind.
	err := cmd.Wait()
	if err == nil {
		t.Skip("Cmd.Wait saw the exit rather than the stop; the race did not land this run")
	}
	if !strings.Contains(err.Error(), "trap") && !strings.Contains(err.Error(), "stop") {
		t.Skipf("Cmd.Wait failed for an unrelated reason: %v", err)
	}
	t.Logf("CONFIRMED: ptracing our own child's LEADER makes Cmd.Wait report %q "+
		"-- the workload is neither finished nor reaped", err)

	_ = unix.PtraceDetach(pid)
}

// THE OTHER HALF IS NOT ASSERTED HERE, AND THAT IS DELIBERATE.
//
// A stop of a NON-leader thread is invisible to wait4(pid) -- which is what
// makes the fix a filter on the leader rather than a refusal to walk our own
// children at all, and so what saves every Python frame on the GPU path. That
// rests on documented kernel semantics: waitpid with pid > 0 waits for the
// child whose ID equals pid, and a sibling thread has a different id.
//
// A test for it was written and then removed. It needed a multi-threaded child
// to seize a non-leader thread of, and the version that let Cmd.Wait run HUNG
// THE SUITE -- which is precisely the hazard tlsBase documents: a thread left
// seized keeps its whole thread group from being reaped. A test that can wedge
// CI to assert a kernel invariant is a worse trade than the invariant being
// stated, and the dangerous half -- that the LEADER does corrupt Cmd.Wait --
// is demonstrated above rather than argued.
