package procmap

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// The warmer against a real /proc and a real process that really exits.
//
// The fake-/proc tests prove the logic; this proves the thing the logic is for.
// It spawns a process, lets the warmer sweep while it lives, kills it, waits
// for /proc/<pid> to actually disappear, and then asks the resolver to place an
// address inside its executable mapping. Issue #56.
func TestWarmerKeepsARealProcessResolvableAfterItExits(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn: %v", err)
	}
	pid := uint32(cmd.Process.Pid)

	r := NewResolver()
	defer r.Close()
	w := NewWarmer(r, 20*time.Millisecond)
	w.Start()

	// Give the sweep a chance to see it. Poll rather than sleep a fixed span:
	// a fixed sleep here would be a race dressed up as a constant.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !r.isCached(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	w.Stop()
	if !r.isCached(pid) {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Skipf("warmer never saw pid %d (no permission to read its maps?)", pid)
	}

	// What we warmed, so the post-exit assertion has something concrete to
	// compare against rather than merely being non-empty.
	before, err := r.Mappings(pid)
	if err != nil || len(before) == 0 {
		t.Fatalf("warmed mappings: %v (%d)", err, len(before))
	}
	probe := before[0].Start

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	// The kernel removes /proc/<pid> asynchronously after reap; wait for it,
	// so "still resolvable" cannot be true merely because the process is
	// somehow still there.
	gone := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + itoa(int(pid))); os.IsNotExist(err) {
			gone = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Skipf("pid %d did not leave /proc in time", pid)
	}

	m, ok := r.Lookup(pid, probe)
	if !ok {
		t.Fatalf("a warmed PID must stay resolvable after it exits: lost mapping at %#x", probe)
	}
	if m.Path != before[0].Path {
		t.Fatalf("resolved to %q, want %q", m.Path, before[0].Path)
	}
	t.Logf("pid %d exited; %#x still resolves to %s", pid, probe, m.Path)
}

// The control: a process that exits before anything warms it is unresolvable.
// Without this, the test above could pass on a resolver that read /proc late
// and got lucky.
func TestAnUnwarmedRealProcessIsLostAfterItExits(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn: %v", err)
	}
	pid := uint32(cmd.Process.Pid)

	r := NewResolver() // no warmer
	defer r.Close()

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + itoa(int(pid))); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if m, ok := r.Lookup(pid, 0x400000); ok {
		t.Fatalf("expected nothing for an unwarmed, exited pid; got %+v", m)
	}
}
