// Package capturetest builds and stages the test-only binaries the capture
// tests run instead of the privileged perf-agent: a fake perf-agent and a
// scripted workload. Production code never imports it.
package capturetest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

const (
	fakePackage     = "github.com/dpsoft/perf-agent/internal/capture/capturetest/fakeperfagent"
	workloadPackage = "github.com/dpsoft/perf-agent/internal/capture/capturetest/capworkload"
	fakeName        = "fakeperfagent"
	workloadName    = "capworkload"
)

// buildDir is set by Main and holds the built binaries for one test run.
var buildDir string

// Main owns the lifetime of the built binaries. A package that uses
// Binaries, FakePerfAgent or Workload calls it from TestMain.
func Main(m *testing.M) int {
	dir, err := os.MkdirTemp("", "capturetest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "capturetest:", err)
		return 1
	}
	buildDir = dir
	defer func() { _ = os.RemoveAll(dir) }()
	code := m.Run()
	// A capture's cgroup is named with the pid of the process that made
	// it: one this test process made and no reaper removed is a leak the
	// tests failed to clean up. The empty ones are removed here (they are
	// this process's own), and the run fails.
	if left := ownCgroupsLeft(); len(left) > 0 {
		fmt.Fprintln(os.Stderr, "capturetest: cgroups this test process made are left:", strings.Join(left, " "))
		for _, p := range left {
			_ = os.Remove(p)
		}
		code = max(code, 1)
	}
	return code
}

// cgroupPrefix is capture.CgroupPrefix, which this package cannot import.
const cgroupPrefix = "perf-agent-mcp-"

// cgroupWait is how long ownCgroupsLeft waits for the reapers still ending
// to remove their cgroups.
const cgroupWait = 15 * time.Second

// ownCgroupsLeft lists the capture cgroups this process made, under its
// own cgroup v2 directory, that are still there after cgroupWait.
func ownCgroupsLeft() []string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil
	}
	rel := ""
	for line := range strings.Lines(string(b)) {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			rel = p
		}
	}
	if rel == "" {
		return nil
	}
	dir := filepath.Join("/sys/fs/cgroup", rel)
	mark := fmt.Sprintf("-%d-", os.Getpid())
	var left []string
	for end := time.Now().Add(cgroupWait); ; time.Sleep(100 * time.Millisecond) {
		left = left[:0]
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if e.IsDir() && strings.HasPrefix(e.Name(), cgroupPrefix) && strings.Contains(e.Name(), mark) {
				left = append(left, filepath.Join(dir, e.Name()))
			}
		}
		if len(left) == 0 || time.Now().After(end) {
			return left
		}
	}
}

var build = sync.OnceValue(func() error {
	for _, b := range [][2]string{{fakePackage, fakeName}, {workloadPackage, workloadName}} {
		cmd := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(buildDir, b[1]), b[0])
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return &buildError{pkg: b[0], out: string(out), err: err}
		}
	}
	return nil
})

type buildError struct {
	pkg, out string
	err      error
}

func (e *buildError) Error() string { return "go build " + e.pkg + ": " + e.err.Error() + "\n" + e.out }

// Binaries builds the fake perf-agent and the workload once per test run
// and returns their paths. It fails tb when the package's TestMain is not
// driven by Main.
func Binaries(tb testing.TB) (fake, workload string) {
	tb.Helper()
	if runtime.GOOS != "linux" {
		tb.Skip("the capture test binaries are Linux only")
	}
	if buildDir == "" {
		tb.Fatal("capturetest.Main must drive TestMain")
	}
	if err := build(); err != nil {
		tb.Fatal(err)
	}
	return filepath.Join(buildDir, fakeName), filepath.Join(buildDir, workloadName)
}

// Workload returns the path of the scripted workload binary.
func Workload(tb testing.TB) string {
	tb.Helper()
	_, w := Binaries(tb)
	return w
}

// FakeMode selects what the fake perf-agent does.
type FakeMode = fakemode.Mode

const (
	FakeNormal        = fakemode.Normal
	FakeNeverReady    = fakemode.NeverReady
	FakeIgnoreSIGINT  = fakemode.IgnoreSIGINT
	FakeExit1Stderr   = fakemode.Exit1Stderr
	FakeExit2         = fakemode.Exit2
	FakeExit127Loader = fakemode.Exit127Loader
	FakeNoFile        = fakemode.NoFile
	// FakeHugeFile gives its first output a 256 MiB size without data, and
	// only when it completes; a test of oversize interruption needs a
	// smaller limit.
	FakeHugeFile       = fakemode.HugeFile
	FakeSymlinkOutput  = fakemode.SymlinkOutput
	FakeHardlinkOutput = fakemode.HardlinkOutput
	FakeSpamStdout     = fakemode.SpamStdout
	FakeForkGrandchild = fakemode.ForkGrandchild
	// FakeHangAfterSIGINT ignores the stop request's effect: it never exits
	// after SIGINT until killed.
	FakeHangAfterSIGINT = fakemode.HangAfterSIGINT
	// FakeNoGPUSamples writes a valid profile with zero GPU samples and an
	// execution log consistent with it.
	FakeNoGPUSamples = fakemode.NoGPUSamples
	// FakeHugeFileThenHang gives its first output a 256 MiB size right
	// after readiness, then hangs, SIGINT ignored, until killed: a size
	// limit can only end it by a kill.
	FakeHugeFileThenHang = fakemode.HugeFileThenHang
)

// FakeOptions configures one fake perf-agent.
type FakeOptions struct {
	Mode       FakeMode
	ReadyAfter time.Duration
	// Record is a path the fake writes its argv, environment, pid, pgid
	// and working directory to as JSON.
	Record string
}

// FakePerfAgent stages a fake perf-agent in tb.TempDir() and returns its
// path. The executable keeps the basename fakeperfagent so its comm is
// recognisable. Beside it are fake-mode.json and fixtures/, which the fake
// copies from. Each fake is a copy of its own, never a link to the built
// binary: staging one must not change the identity (links, ctime) of a
// fake another test's server has verified.
func FakePerfAgent(tb testing.TB, o FakeOptions) string {
	tb.Helper()
	built, _ := Binaries(tb)
	dir := tb.TempDir()
	path := filepath.Join(dir, fakeName)
	if err := copyFile(built, path); err != nil {
		tb.Fatal(err)
	}
	mode := o.Mode
	if mode == "" {
		mode = FakeNormal
	}
	b, err := json.Marshal(fakemode.File{Mode: mode, ReadyAfter: int64(o.ReadyAfter), Record: o.Record})
	if err != nil {
		tb.Fatal(err)
	}
	write := func(p string, data []byte) {
		if err := os.WriteFile(p, data, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	write(filepath.Join(dir, "fake-mode.json"), b)
	fixtures := filepath.Join(dir, "fixtures")
	if err := os.Mkdir(fixtures, 0o755); err != nil {
		tb.Fatal(err)
	}
	write(filepath.Join(fixtures, fakemode.FixtureCPU), pmfixture.CPUBasic())
	write(filepath.Join(fixtures, fakemode.FixtureOffCPU), pmfixture.Build("offcpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("wait")}, Value: 1000}))
	write(filepath.Join(fixtures, fakemode.FixtureGPU), pmfixture.GPUBasic())
	write(filepath.Join(fixtures, fakemode.FixtureExeclog), pmfixture.GPUBasicExeclog(tb))
	empty := pmfixture.Build("gpu", "nanoseconds", []string{"perf-agent.capture_id=cap-1"})
	write(filepath.Join(fixtures, fakemode.FixtureGPUEmpty), empty)
	write(filepath.Join(fixtures, fakemode.FixtureExeclogEmpty), pmfixture.Execlog(tb, empty,
		execlog.Header{CaptureID: "cap-1", Backend: "cupti", SampleType: "gpu/nanoseconds"}, nil))
	return path
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// SkipIfPrivileged skips tb when the process runs as root or holds
// capabilities, since the checks under test then behave differently. It
// fails open: when /proc/self/status cannot be read only the euid is
// checked.
func SkipIfPrivileged(tb testing.TB) {
	tb.Helper()
	if os.Geteuid() == 0 {
		tb.Skip("running as root")
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return
	}
	for line := range strings.Lines(string(status)) {
		for _, key := range []string{"CapEff:", "CapPrm:"} {
			if v, ok := strings.CutPrefix(line, key); ok && strings.Trim(strings.TrimSpace(v), "0") != "" {
				tb.Skipf("process holds capabilities (%s)", strings.TrimSpace(line))
			}
		}
	}
}
