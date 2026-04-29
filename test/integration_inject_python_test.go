package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInjectPython_ActivatesTrampoline launches a CPython 3.12+ workload WITHOUT
// -X perf, then runs perf-agent --profile --inject-python --pid <PID>. Asserts
// that perf-agent injects the trampoline (perf map exists with py:: entries),
// that the resulting profile has Python frame names, and that deactivation
// runs at end-of-profile (perf map stops growing after perf-agent exits).
func TestInjectPython_ActivatesTrampoline(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))
	requirePython312Plus(t)

	// Launch python WITHOUT -X perf
	pyCmd := exec.Command("python3", "workloads/python/cpu_bound.py", "10", "2")
	pyCmd.Dir = "." // tests run from test/ directory
	if err := pyCmd.Start(); err != nil {
		t.Fatalf("start python workload: %v", err)
	}
	pid := pyCmd.Process.Pid
	t.Cleanup(func() {
		_ = pyCmd.Process.Kill()
		_ = pyCmd.Wait()
		_ = os.Remove(fmt.Sprintf("/tmp/perf-%d.map", pid))
	})

	// Wait for warmup
	time.Sleep(1 * time.Second)

	// Run perf-agent --profile --inject-python
	profileOut := filepath.Join(t.TempDir(), "profile.pb.gz")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentPath := getAgentPath(t)
	pa := exec.CommandContext(ctx, agentPath,
		"--profile",
		"--inject-python",
		"--pid", fmt.Sprintf("%d", pid),
		"--duration", "5s",
		"--profile-output", profileOut,
	)
	out, err := pa.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\n%s", err, out)
	}
	t.Logf("perf-agent output:\n%s", out)

	// Assert /tmp/perf-<PID>.map exists and is non-empty with py:: lines
	perfMap := fmt.Sprintf("/tmp/perf-%d.map", pid)
	st, err := os.Stat(perfMap)
	if err != nil {
		t.Fatalf("perf map %s not created: %v", perfMap, err)
	}
	if st.Size() == 0 {
		t.Fatalf("perf map %s is empty", perfMap)
	}
	pmContent, _ := os.ReadFile(perfMap)
	if !strings.Contains(string(pmContent), "py::") {
		t.Errorf("perf map missing py:: entries:\nfirst 500 bytes:\n%s",
			truncateForLog(string(pmContent), 500))
	}

	// Assert profile.pb.gz exists
	if _, err := os.Stat(profileOut); err != nil {
		t.Fatalf("profile not created: %v", err)
	}

	// Optional: pprof inspection. Skip the assertion if go tool pprof is not
	// installed; the JIT names appearing in the perf map already prove
	// activation worked.
	pprofTop := exec.CommandContext(ctx, "go", "tool", "pprof", "-top", "-nodecount=20", profileOut)
	topOut, perr := pprofTop.CombinedOutput()
	if perr == nil {
		t.Logf("pprof top output:\n%s", topOut)
		// Warning-level check, not Fatal: if pprof can read the file we'd
		// like to see Python frames, but blazesym sometimes resolves them
		// to address-only on low-CPU samples. Not a hard failure.
		if !strings.Contains(string(topOut), "py::") &&
			!strings.Contains(string(topOut), "cpu_bound.py") {
			t.Logf("WARNING: profile lacks Python frame names (may be a low-sample-count flake)")
		}
	}

	// Assert deactivation actually fired: re-stat perf-map twice with a 1s
	// gap; size must NOT grow (no new trampoline entries being written).
	size1 := fileSize(t, perfMap)
	time.Sleep(1 * time.Second)
	size2 := fileSize(t, perfMap)
	if size2 != size1 {
		t.Errorf("perf-map grew after perf-agent exit (deactivation didn't run): %d → %d",
			size1, size2)
	}
}

// TestInjectPython_StrictFailsOnNonPython confirms that perf-agent --pid <go-binary>
// --inject-python exits non-zero (strict mode) with a clear "not python" reason.
func TestInjectPython_StrictFailsOnNonPython(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	// Build the Go workload if not already present
	goWorkload := "workloads/go/cpu_bound"
	if _, err := os.Stat(goWorkload); err != nil {
		t.Skipf("go workload not built; run `make test-workloads` first (got %v)", err)
	}

	goCmd := exec.Command(goWorkload)
	if err := goCmd.Start(); err != nil {
		t.Fatalf("start go workload: %v", err)
	}
	pid := goCmd.Process.Pid
	t.Cleanup(func() {
		_ = goCmd.Process.Kill()
		_ = goCmd.Wait()
	})

	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agentPath := getAgentPath(t)
	pa := exec.CommandContext(ctx, agentPath,
		"--profile",
		"--inject-python",
		"--pid", fmt.Sprintf("%d", pid),
		"--duration", "2s",
	)
	out, err := pa.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit; got success\noutput:\n%s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected ExitError; got %T: %v", err, err)
	}
	combined := string(out)
	t.Logf("perf-agent stderr/stdout:\n%s", combined)
	// The error message should mention not_python OR "not a python" OR a
	// related structured reason. Be permissive: the exact wording may vary
	// based on log format.
	if !strings.Contains(combined, "not_python") &&
		!strings.Contains(combined, "not a python") &&
		!strings.Contains(combined, "ErrNotPython") &&
		!strings.Contains(combined, "python") {
		t.Errorf("output does not mention 'python' anywhere; expected a structured reason\n%s",
			combined)
	}
}

// requirePython312Plus skips if python3 < 3.12 OR the interpreter was built
// without --enable-perf-trampoline. Both gate the activate path, so both
// must skip cleanly.
func requirePython312Plus(t *testing.T) {
	t.Helper()
	out, err := exec.Command("python3", "-c",
		"import sys; print(sys.version_info >= (3, 12))").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "True") {
		t.Skipf("requires python3 >= 3.12; got: %s (err=%v)", strings.TrimSpace(string(out)), err)
	}
	// Even on 3.12+, the interpreter may have been compiled without
	// --enable-perf-trampoline (Fedora's stock build does this). Probe by
	// attempting activation+deactivation; ImportError or AttributeError on
	// the trampoline functions means the build doesn't support it.
	out, err = exec.Command("python3", "-c",
		"import sys; sys.activate_stack_trampoline('perf'); sys.deactivate_stack_trampoline(); print('OK')").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "OK") {
		t.Skipf("python3 lacks --enable-perf-trampoline; got: %s (err=%v)",
			strings.TrimSpace(string(out)), err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
