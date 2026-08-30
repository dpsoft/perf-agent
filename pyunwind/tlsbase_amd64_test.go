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
