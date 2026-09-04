package pyunwind

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pythonFor returns an interpreter this test can measure against, or skips.
// The child-process relationship matters: reading /proc/<pid>/mem of a
// non-child of the same user is refused under the default yama ptrace_scope=1,
// and the profiler holds CAP_SYS_PTRACE where this test does not.
func pythonFor(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		"/home/diego/pytorch-profile-test/.venv/bin/python",
		"python3.12",
	} {
		if path, err := exec.LookPath(p); err == nil {
			out, err := exec.Command(path, "-V").Output()
			if err == nil && strings.Contains(string(out), "3.12") {
				return path
			}
		}
	}
	t.Skip("no CPython 3.12 available; code offsets are only measured for 3.12")
	return ""
}

const namingTarget = `
import sys, time
def outer_function(): pass
class Widget:
    def method_here(self): pass
for name, fn in (("outer_function", outer_function), ("method_here", Widget.method_here)):
    print(f"{name} {id(fn.__code__)}", flush=True)
print("READY", flush=True)
time.sleep(60)
`

// startNamedTarget runs an interpreter that prints the addresses of code
// objects whose names the test already knows, so resolution is checked against
// ground truth rather than against "it returned something".
func startNamedTarget(t *testing.T) (*exec.Cmd, map[string]uint64) {
	t.Helper()
	py := pythonFor(t)
	script := t.TempDir() + "/target.py"
	if err := os.WriteFile(script, []byte(namingTarget), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command(py, script)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	addrs := map[string]uint64{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "READY" {
				return
			}
			f := strings.Fields(line)
			if len(f) == 2 {
				if a, err := strconv.ParseUint(f[1], 0, 64); err == nil {
					addrs[f[0]] = a
				}
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("interpreter did not report its code objects")
	}
	if len(addrs) != 2 {
		t.Fatalf("expected 2 code objects, got %v", addrs)
	}
	return cmd, addrs
}

// The whole point: a code object becomes a name a person recognises.
func TestResolvesCodeObjectsToQualifiedNames(t *testing.T) {
	cmd, addrs := startNamedTarget(t)
	off := CodeOffsets{Qualname: 128, Filename: 112, FirstLine: 68}
	r := NewResolver(cmd.Process.Pid, off)
	if r == nil {
		t.Fatal("resolver declined despite measured offsets")
	}

	// co_qualname, not co_name: a method must carry its class, or a flame
	// graph row reading "method_here" is unidentifiable among the dozens of
	// classes that have one.
	info, ok := r.Resolve(addrs["method_here"])
	if !ok {
		t.Fatalf("could not resolve the method's code object")
	}
	if info.Qualname != "Widget.method_here" {
		t.Errorf("Qualname = %q, want %q (qualified, not bare co_name)", info.Qualname, "Widget.method_here")
	}
	if !strings.HasSuffix(info.Filename, "target.py") {
		t.Errorf("Filename = %q, want it to end in target.py", info.Filename)
	}
	if info.FirstLine == 0 {
		t.Errorf("FirstLine = 0, want the def's line")
	}

	if got := r.Name(addrs["outer_function"]); !strings.HasPrefix(got, "outer_function (target.py:") {
		t.Errorf("Name = %q, want it to start with outer_function (target.py:", got)
	}
}

// Reads must happen once. The name has to outlive the process, and re-reading
// per sample would both cost a syscall per frame and stop working the moment
// the target exits -- which is exactly when a profiler builds its output.
func TestResolutionIsCachedPerCodeObject(t *testing.T) {
	cmd, addrs := startNamedTarget(t)
	r := NewResolver(cmd.Process.Pid, CodeOffsets{Qualname: 128, Filename: 112, FirstLine: 68})

	for range 50 {
		if _, ok := r.Resolve(addrs["method_here"]); !ok {
			t.Fatal("resolution failed")
		}
	}
	s := r.Stats()
	if s.Resolved != 1 {
		t.Errorf("Resolved = %d, want exactly 1 read for 50 lookups", s.Resolved)
	}
	if s.Cached != 49 {
		t.Errorf("Cached = %d, want 49", s.Cached)
	}
}

// A name read after the target exits would be a name read from nothing. The
// cache must answer anyway -- that is what it is for.
func TestCachedNamesSurviveTheProcess(t *testing.T) {
	cmd, addrs := startNamedTarget(t)
	r := NewResolver(cmd.Process.Pid, CodeOffsets{Qualname: 128, Filename: 112, FirstLine: 68})
	want, ok := r.Resolve(addrs["method_here"])
	if !ok {
		t.Fatal("resolution failed while the target was alive")
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	got, ok := r.Resolve(addrs["method_here"])
	if !ok || got.Qualname != want.Qualname {
		t.Fatalf("after exit: got %+v ok=%v, want the cached %+v", got, ok, want)
	}
	// And an address never seen must now fail rather than invent something.
	if _, ok := r.Resolve(addrs["outer_function"]); ok {
		t.Error("resolved an uncached address after the process exited")
	}
}

// An unmeasured interpreter must decline, not read offset 0 and render
// whatever is there as a function name.
func TestUnmeasuredInterpreterDeclines(t *testing.T) {
	if r := NewResolver(os.Getpid(), CodeOffsets{}); r != nil {
		t.Fatal("NewResolver must return nil when offsets are not measured")
	}
	var nilResolver *Resolver
	if _, ok := nilResolver.Resolve(0x1234); ok {
		t.Error("a nil resolver must decline")
	}
	if got := nilResolver.Name(0x1234); got != "python:0x1234" {
		t.Errorf("Name = %q, want the unresolved form python:0x1234", got)
	}
}
