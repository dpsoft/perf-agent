package pyunwind

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// interpretersUnderTest returns every CPython this machine has that the
// offsets table claims to support, so the live checks run against each rather
// than against whichever one happens to be first.
//
// This matters more than it looks: 3.12, 3.13 and 3.14 carry the SAME
// code-object offsets, so a test that exercised only one would pass just as
// happily if the table returned that one's values for every version.
func interpretersUnderTest(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	candidates := []string{
		"/home/diego/pytorch-profile-test/.venv/bin/python",
		"python3.12", "python3.13", "python3.14",
	}
	for _, home := range []string{os.Getenv("HOME")} {
		if home == "" {
			continue
		}
		matches, _ := filepath.Glob(home + "/.local/share/uv/python/cpython-3.1[234].*-linux-x86_64-gnu/bin/python3.1[234]")
		candidates = append(candidates, matches...)
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		out, err := exec.Command(path, "-c", "import sys;print(f\"{sys.version_info.major}.{sys.version_info.minor}\")").Output()
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(out))
		if _, seen := found[v]; !seen {
			found[v] = path
		}
	}
	if len(found) == 0 {
		t.Skip("no supported CPython on this machine")
	}
	return found
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
func startNamedTarget(t *testing.T, py string) (*exec.Cmd, map[string]uint64) {
	t.Helper()
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

// The whole point: a code object becomes a name a person recognises, on every
// interpreter this build claims to support.
func TestResolvesCodeObjectsToQualifiedNames(t *testing.T) {
	for version, py := range interpretersUnderTest(t) {
		t.Run(version, func(t *testing.T) {
			cmd, addrs := startNamedTarget(t, py)
			minor, _ := strconv.Atoi(strings.TrimPrefix(version, "3."))
			off, err := TableFor(Version{Major: 3, Minor: minor})
			if err != nil {
				t.Skipf("no offsets table for %s: %v", version, err)
			}
			r := NewResolver(cmd.Process.Pid, off.Code)
			if r == nil {
				t.Fatalf("%s: resolver declined despite a measured table", version)
			}

			// co_qualname, not co_name: a method must carry its class, or a
			// flame graph row reading "method_here" is unidentifiable among
			// the dozens of classes that have one.
			info, ok := r.Resolve(addrs["method_here"])
			if !ok {
				t.Fatalf("%s: could not resolve the method's code object", version)
			}
			if info.Qualname != "Widget.method_here" {
				t.Errorf("%s: Qualname = %q, want %q", version, info.Qualname, "Widget.method_here")
			}
			if !strings.HasSuffix(info.Filename, "target.py") {
				t.Errorf("%s: Filename = %q, want it to end in target.py", version, info.Filename)
			}
			if info.FirstLine == 0 {
				t.Errorf("%s: FirstLine = 0, want the def's line", version)
			}
			if got := r.Name(addrs["outer_function"]); !strings.HasPrefix(got, "outer_function (target.py:") {
				t.Errorf("%s: Name = %q", version, got)
			}
		})
	}
}

// Reads must happen once. The name has to outlive the process, and re-reading
// per sample would both cost a syscall per frame and stop working the moment
// the target exits -- which is exactly when a profiler builds its output.
func TestResolutionIsCachedPerCodeObject(t *testing.T) {
	cmd, addrs := startNamedTarget(t, anyInterpreter(t))
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
	cmd, addrs := startNamedTarget(t, anyInterpreter(t))
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

// anyInterpreter returns one supported CPython, for the checks that are about
// the resolver's behaviour rather than about a version's layout.
func anyInterpreter(t *testing.T) string {
	t.Helper()
	for _, py := range interpretersUnderTest(t) {
		return py
	}
	return ""
}
