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
	return m.Run()
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
	FakeNormal         = fakemode.Normal
	FakeNeverReady     = fakemode.NeverReady
	FakeIgnoreSIGINT   = fakemode.IgnoreSIGINT
	FakeExit1Stderr    = fakemode.Exit1Stderr
	FakeExit2          = fakemode.Exit2
	FakeExit127Loader  = fakemode.Exit127Loader
	FakeNoFile         = fakemode.NoFile
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
)

// FakeOptions configures one fake perf-agent.
type FakeOptions struct {
	Mode       FakeMode
	ReadyAfter time.Duration
	// Record is a path the fake writes its argv, environment, pid and
	// pgid to as JSON.
	Record string
}

// FakePerfAgent stages a fake perf-agent in tb.TempDir() and returns its
// path. The executable keeps the basename fakeperfagent so its comm is
// recognisable. Beside it are fake-mode.json and fixtures/, which the fake
// copies from.
func FakePerfAgent(tb testing.TB, o FakeOptions) string {
	tb.Helper()
	built, _ := Binaries(tb)
	dir := tb.TempDir()
	path := filepath.Join(dir, fakeName)
	if err := os.Link(built, path); err != nil {
		if err := copyFile(built, path); err != nil {
			tb.Fatal(err)
		}
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
