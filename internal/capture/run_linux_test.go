package capture

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
)

// testBounds are generous for a loaded -race machine; a test that is about
// one bound sets it.
func testBounds() Bounds {
	return Bounds{
		StartTimeout:    10 * time.Second,
		FinishTimeout:   10 * time.Second,
		StopGrace:       500 * time.Millisecond,
		MaxOutputTail:   2 << 10,
		MaxProfileBytes: 64 << 20,
		MaxExeclogBytes: 64 << 20,
		Recheck:         time.Second,
		Poll:            100 * time.Millisecond,
	}
}

// runSpec stages a fake perf-agent with o and returns a cpu capture of
// 500ms with no target yet: the test sets PID or Launch.
func runSpec(t *testing.T, o capturetest.FakeOptions) RunSpec {
	t.Helper()
	capturetest.SkipIfPrivileged(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fake := capturetest.FakePerfAgent(t, o)
	id, err := IdentityOf(fake)
	if err != nil {
		t.Fatal(err)
	}
	stagingPath := t.TempDir()
	staging, err := os.OpenRoot(stagingPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staging.Close() })
	lockDir, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockDir.Close() })
	lease, e := TryLock(lockDir, Holder{ServerPID: os.Getpid(), CaptureID: "cap/run"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = lease.Close() })
	return RunSpec{
		Self:        self,
		Binary:      Binary{Path: fake, ID: id, Version: "v0.0.0-test"},
		Proc:        DefaultProc(),
		SelfID:      ourSelf(),
		Settings:    Settings{Modes: []Mode{ModeCPU}, Duration: 500 * time.Millisecond, SampleRate: 99, Unwind: "fp"},
		Staging:     staging,
		StagingPath: stagingPath,
		CaptureID:   "cap-run",
		Bounds:      testBounds(),
		Lock:        lease.File(),
		Log:         slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

// launchOf is a capworkload command with args.
func launchOf(t *testing.T, args ...string) *LaunchSpec {
	t.Helper()
	w := capturetest.Workload(t)
	return &LaunchSpec{Path: w, Args: append([]string{w}, args...), Env: []string{"PATH=/usr/bin:/bin", "LANG=C"}, Dir: t.TempDir()}
}

// startRun starts spec and, at cleanup, kills it, waits for it and fails
// the test for a launched target still alive.
func startRun(t *testing.T, spec RunSpec) *Run {
	t.Helper()
	r, e := Start(spec)
	if e != nil {
		t.Fatalf("Start: %v", e)
	}
	t.Cleanup(func() {
		r.Kill()
		o := waitRun(t, r, 30*time.Second)
		if spec.Launch != nil && o.Target.PID > 0 {
			if p := (procRef{o.Target.PID, o.Target.StartTime}); p.alive() {
				t.Errorf("the launched command %d was left running", p.pid)
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
	})
	return r
}

// waitRun is Wait, failing the test past timeout.
func waitRun(t *testing.T, r *Run, timeout time.Duration) Outcome {
	t.Helper()
	ch := make(chan Outcome, 1)
	go func() { ch <- r.Wait() }()
	select {
	case o := <-ch:
		return o
	case <-time.After(timeout):
		t.Fatalf("Wait did not return within %v (phase %s)", timeout, r.Phase())
	}
	return Outcome{}
}

// waitPhase waits until r reports phase.
func waitPhase(t *testing.T, r *Run, phase string, timeout time.Duration) {
	t.Helper()
	if !eventually(timeout, func() bool { return r.Phase() == phase }) {
		t.Fatalf("phase %s, want %s within %v", r.Phase(), phase, timeout)
	}
}

func errCode(e *Error) Code {
	if e == nil {
		return ""
	}
	return e.Code
}

// readRecord reads the fake's record, or reports it was never written.
func readRecord(t *testing.T, path string) (fakemode.Record, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fakemode.Record{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var rec fakemode.Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec, true
}

// grandchildOf reads the pid a double-fork-setsid workload printed first.
func grandchildOf(t *testing.T, o Outcome) int {
	t.Helper()
	first, _, _ := strings.Cut(o.Workload.Stdout, "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("the workload printed %q, not a grandchild pid", o.Workload.Stdout)
	}
	return pid
}

// requireGone fails when pid is still a live capworkload, and kills it
// then (by its comm, so not a reused pid).
func requireGone(t *testing.T, pid int) {
	t.Helper()
	s, err := readStat(DefaultProc(), pid)
	if err != nil || s.exited() || s.comm != "capworkload" {
		return
	}
	t.Errorf("grandchild %d survived", pid)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func stagingHas(t *testing.T, spec RunSpec, name string) bool {
	t.Helper()
	fi, err := spec.Staging.Lstat(name)
	return err == nil && fi.Size() > 0
}

func TestRunCapturesAPIDTarget(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{ReadyAfter: 100 * time.Millisecond})
	target := startChild(t, "sleep")
	spec.PID = target.PID
	r := startRun(t, spec)
	o := waitRun(t, r, 30*time.Second)
	if o.Err != nil {
		t.Fatalf("Err = %v", o.Err)
	}
	if o.StoppedBy != StoppedByDuration || o.Launched || o.Target.PID != target.PID || o.PerfAgentExit != 0 {
		t.Errorf("outcome = %+v", o)
	}
	if o.AttachLatency < 100*time.Millisecond {
		t.Errorf("AttachLatency = %v, want at least the fake's 100ms", o.AttachLatency)
	}
	if d := o.Profiled - spec.Settings.Duration; d < -250*time.Millisecond || d > 250*time.Millisecond {
		t.Errorf("Profiled = %v, want %v ± 250ms", o.Profiled, spec.Settings.Duration)
	}
	if !stagingHas(t, spec, OutputFile(ModeCPU)) {
		t.Error("no cpu profile in staging")
	}
	if !CleanupIsComplete(o.Cleanup) || o.StderrTail != "" || len(o.Argv) == 0 || o.Started.IsZero() {
		t.Errorf("cleanup %q, stderr tail %q, argv %q, started %v", o.Cleanup, o.StderrTail, o.Argv, o.Started)
	}
	if again := waitRun(t, r, time.Second); again.StoppedBy != o.StoppedBy || again.Profiled != o.Profiled {
		t.Error("a second Wait returned another outcome")
	}
}

func TestRunLaunchesACommand(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Launch = launchOf(t, "sleep", "1m")
	r := startRun(t, spec)
	o := waitRun(t, r, 30*time.Second)
	if o.Err != nil {
		t.Fatalf("Err = %v", o.Err)
	}
	if !o.Launched || o.StoppedBy != StoppedByDuration || o.CommandPath != spec.Launch.Path || o.ArgCount != 3 {
		t.Errorf("outcome = %+v", o)
	}
	if o.Target.PID <= 0 || o.Target.Comm != "capworkload" {
		t.Errorf("target = %+v, want the launched workload", o.Target)
	}
	notes := strings.Join(o.Notes, "\n")
	if attached := strings.Contains(notes, "after the command started"); attached != (o.AttachLatency.Round(100*time.Millisecond) > 0) ||
		!strings.Contains(notes, "only pid "+strconv.Itoa(o.Target.PID)+" was profiled") {
		t.Errorf("notes = %q (attach latency %v)", o.Notes, o.AttachLatency)
	}
	if (procRef{o.Target.PID, o.Target.StartTime}).alive() {
		t.Error("the command still runs after Wait")
	}
	if r.Phase() != PhaseDone {
		t.Errorf("phase %s after Wait", r.Phase())
	}
}

// A single-threaded command shows its exit window (PF_EXITING, no mm, the
// exe link gone, still S) when the reaper ends it after perf-agent; the
// end recheck meets it there and must not discard the capture.
func TestRunASingleThreadedCommandThatOutlivesTheDurationIsOK(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	if python, err = filepath.EvalSymlinks(python); err != nil {
		t.Skip(err)
	}
	for i := range 4 {
		spec := runSpec(t, capturetest.FakeOptions{})
		spec.Launch = &LaunchSpec{Path: python, Args: []string{python, "-c", "import time; time.sleep(60)"},
			Env: []string{"PATH=/usr/bin:/bin"}, Dir: t.TempDir()}
		o := waitRun(t, startRun(t, spec), 30*time.Second)
		if o.Err != nil || o.StoppedBy != StoppedByDuration {
			t.Fatalf("run %d: Err %+v, stopped by %q", i, o.Err, o.StoppedBy)
		}
	}
}

// A PID target that exits by itself mid-capture, with a recheck often
// enough to meet its exit window, stops the capture as target_exit.
func TestRunAPIDTargetExitingMidCaptureIsTargetExit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	for i := range 4 {
		spec := runSpec(t, capturetest.FakeOptions{})
		spec.Settings.Duration = 20 * time.Second
		spec.Bounds.Recheck = 5 * time.Millisecond
		// A touched 1 GiB makes the exit, from exit_mm on, long enough to
		// be seen.
		cmd := exec.Command(python, "-c", "import time; b = b'x' * (1 << 30); print('ready', flush=True); time.sleep(1)")
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		if line, _ := bufio.NewReader(out).ReadString('\n'); line != "ready\n" {
			t.Fatalf("python printed %q", line)
		}
		spec.PID = cmd.Process.Pid
		o := waitRun(t, startRun(t, spec), 30*time.Second)
		if o.Err != nil || o.StoppedBy != StoppedByTargetExit {
			t.Fatalf("run %d: Err %+v, stopped by %q", i, o.Err, o.StoppedBy)
		}
	}
}

func TestRunStopsWhenTheCommandExits(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = 20 * time.Second
	spec.Launch = launchOf(t, "exit-after", "500ms", "0")
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err != nil || o.StoppedBy != StoppedByCommandExit {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
	// The command lives 500ms; the stop follows its exit event.
	if o.Profiled <= 0 || o.Profiled >= time.Second {
		t.Errorf("Profiled = %v, want under 1s", o.Profiled)
	}
	if !stagingHas(t, spec, OutputFile(ModeCPU)) {
		t.Error("the stopped perf-agent wrote no profile")
	}
}

// The launched command's own exit is recorded: its status, or the signal
// that killed it.
func TestRunRecordsHowTheCommandExited(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = 20 * time.Second
	spec.Launch = launchOf(t, "exit-after", "500ms", "3")
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err != nil || !o.WorkloadExited || o.WorkloadStatus != 3 || o.WorkloadSignal != "" {
		t.Fatalf("Err %v, exited %v, status %d, signal %q", o.Err, o.WorkloadExited, o.WorkloadStatus, o.WorkloadSignal)
	}

	r := testRun(t, new(0))
	r.spec.Launch = &LaunchSpec{Path: "/x/bench"}
	r.event(Event{Op: EventExited, Who: WhoWorkload, PID: 11, Status: -1, Signal: "killed"})
	if !r.o.WorkloadExited || r.o.WorkloadSignal != "killed" {
		t.Errorf("exited %v, signal %q", r.o.WorkloadExited, r.o.WorkloadSignal)
	}
}

func TestRunStopsWhenThePIDExits(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = 20 * time.Second
	target := startChild(t, "sleep")
	spec.PID = target.PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	target.exit() // left a zombie: not alive, as Alive sees it
	o := waitRun(t, r, 30*time.Second)
	if o.Err != nil || o.StoppedBy != StoppedByTargetExit {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
	if o.Profiled >= 10*time.Second {
		t.Errorf("Profiled = %v", o.Profiled)
	}
}

// A perf-agent that stops by itself after its target is gone was stopped
// by the target, even when no poll saw the exit first.
func TestRunPerfAgentExitAfterTheTargetIsTargetExit(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	// Longer than the helper's exit: under -race it lingers 1s.
	spec.Settings.Duration = 3 * time.Second
	spec.Bounds.Poll, spec.Bounds.Recheck = time.Hour, time.Hour
	target := startChild(t, "sleep")
	spec.PID = target.PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	target.exit()
	o := waitRun(t, r, 30*time.Second)
	if o.Err != nil || o.StoppedBy != StoppedByTargetExit {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
}

func TestRunTargetExitedBeforeReadiness(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{ReadyAfter: 3 * time.Second})
	target := startChild(t, "sleep")
	spec.PID = target.PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseAttaching, 10*time.Second)
	target.exit()
	o := waitRun(t, r, 30*time.Second)
	if errCode(o.Err) != CodeTargetExited || o.StoppedBy != "" {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
}

// Review Focus 2: a wrapper that forks its real work and returns.
func TestRunForkAndExitIsTargetExitedWithAPIDHint(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{ReadyAfter: time.Second})
	spec.Launch = launchOf(t, "fork-and-exit", "1m")
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if errCode(o.Err) != CodeTargetExited || !strings.Contains(o.Err.Next, "capture it with pid") {
		t.Fatalf("Err = %v", o.Err)
	}
	if !strings.Contains(o.Err.Message, "pid "+strconv.Itoa(o.Target.PID)) {
		t.Errorf("message %q does not name the launched pid %d", o.Err.Message, o.Target.PID)
	}
	child := grandchildOf(t, o)
	requireGone(t, child)
	if !strings.Contains(o.Workload.Stdout, strconv.Itoa(child)) {
		t.Errorf("tail %q lacks the output", o.Workload.Stdout)
	}
}

func TestRunTimeouts(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		record := filepath.Join(t.TempDir(), "record.json")
		spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeNeverReady, Record: record})
		spec.Bounds.StartTimeout = 300 * time.Millisecond
		target := startChild(t, "sleep")
		spec.PID = target.PID
		began := time.Now()
		o := waitRun(t, startRun(t, spec), 30*time.Second)
		if errCode(o.Err) != CodeStartTimeout || !strings.Contains(o.Err.Message, "StartTimeout=300ms") {
			t.Fatalf("Err = %v", o.Err)
		}
		// The bound kills; the reaper's own deadline is 10s further out.
		if d := time.Since(began); d > spec.Bounds.StartTimeout+slack {
			t.Errorf("Wait returned %v after Start, want within StartTimeout + %v", d, slack)
		}
		rec, ok := readRecord(t, record)
		if !ok || !groupGone(rec.PGID) {
			t.Errorf("perf-agent's group %d (recorded %v) is not gone after Wait", rec.PGID, ok)
		}
	})
	t.Run("finish", func(t *testing.T) {
		record := filepath.Join(t.TempDir(), "record.json")
		spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeHangAfterSIGINT, Record: record})
		spec.Settings.Duration = time.Minute
		spec.Bounds.FinishTimeout = 500 * time.Millisecond
		spec.Launch = launchOf(t, "exit-after", "1s", "0")
		o := waitRun(t, startRun(t, spec), 30*time.Second)
		if errCode(o.Err) != CodeFinishTimeout || !strings.Contains(o.Err.Message, "FinishTimeout=500ms") {
			t.Fatalf("Err = %v", o.Err)
		}
		rec, ok := readRecord(t, record)
		if !ok || !groupGone(rec.PGID) {
			t.Errorf("perf-agent's group %d (recorded %v) is not gone after Wait", rec.PGID, ok)
		}
	})
}

// Each server-side kill still lets the reaper sweep: a setsid grandchild
// that is in neither group is gone by the time Wait returns.
func TestRunKillsKeepTheSweep(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode capturetest.FakeMode
		args []string
		set  func(*RunSpec)
		end  func(*Run)
		want Code
	}{
		{"Kill", capturetest.FakeNormal, []string{"double-fork-setsid", "1m"}, nil, (*Run).Kill, CodeCancelled},
		{"size", capturetest.FakeHugeFileThenHang, []string{"double-fork-setsid", "1m"},
			func(s *RunSpec) { s.Bounds.MaxProfileBytes = 1 << 20 }, nil, CodeTooLarge},
		{"finish timeout", capturetest.FakeHangAfterSIGINT, []string{"double-fork-setsid", "2s", "1m"},
			func(s *RunSpec) { s.Bounds.FinishTimeout = 500 * time.Millisecond }, nil, CodeFinishTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The readiness delay lets the workload fork its grandchild
			// before any kill; Kill also waits for the reaper to adopt it.
			spec := runSpec(t, capturetest.FakeOptions{Mode: tc.mode, ReadyAfter: 500 * time.Millisecond})
			spec.Settings.Duration = time.Minute
			spec.Launch = launchOf(t, tc.args...)
			if tc.set != nil {
				tc.set(&spec)
			}
			r := startRun(t, spec)
			if tc.end != nil {
				waitPhase(t, r, PhaseProfiling, 10*time.Second)
				waitAdopted(t, r.reaper.PID())
				tc.end(r)
			}
			o := waitRun(t, r, 30*time.Second)
			if errCode(o.Err) != tc.want {
				t.Errorf("Err = %v, want %s", o.Err, tc.want)
			}
			if !CleanupIsComplete(o.Cleanup) || len(o.Survivors) != 0 {
				t.Errorf("cleanup %q survivors %v", o.Cleanup, o.Survivors)
			}
			requireGone(t, grandchildOf(t, o))
		})
	}
}

// waitAdopted waits until the reaper has the workload and its orphaned
// setsid grandchild as children: the grandchild has printed its pid and
// its parent has exited by then.
func waitAdopted(t *testing.T, reaper int) {
	t.Helper()
	ok := eventually(10*time.Second, func() bool {
		ents, _ := os.ReadDir("/proc")
		n := 0
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if st, err := readStat(DefaultProc(), pid); err == nil && st.ppid == reaper && st.comm == "capworkload" && !st.exited() {
				n++
			}
		}
		return n >= 2
	})
	if !ok {
		t.Fatalf("the reaper %d never adopted the workload's grandchild", reaper)
	}
}

func TestRunExitCodes(t *testing.T) {
	for _, tc := range []struct {
		mode      capturetest.FakeMode
		code      Code
		retryable bool
		message   string
		next      string
	}{
		{capturetest.FakeExit2, CodePerfAgentRefusedArgs, false, "v0.0.0-test", ""},
		{capturetest.FakeExit1Stderr, CodePerfAgentFailed, false, "the capture failed", "setcap"},
		{capturetest.FakeExit127Loader, CodePerfAgentUnstartable, false, "could not start", "link blazesym statically"},
		{capturetest.FakeNoFile, CodeNoOutput, true, "wrote no cpu profile", ""},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			spec := runSpec(t, capturetest.FakeOptions{Mode: tc.mode})
			target := startChild(t, "sleep")
			spec.PID = target.PID
			o := waitRun(t, startRun(t, spec), 30*time.Second)
			e := o.Err
			if errCode(e) != tc.code || e.Retryable != tc.retryable || !strings.Contains(e.Message, tc.message) || !strings.Contains(e.Next, tc.next) {
				t.Fatalf("Err = %+v", e)
			}
			if o.StderrTail != e.StderrTail || tc.mode != capturetest.FakeNoFile && e.StderrTail == "" {
				t.Errorf("stderr tail %q, outcome's %q", e.StderrTail, o.StderrTail)
			}
			if e.PerfAgentExit == "" {
				t.Error("no perf-agent exit in the error")
			}
		})
	}
}

func TestRunSizeKill(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeHugeFileThenHang})
	spec.Settings.Duration = time.Minute
	spec.Bounds.MaxProfileBytes = 1 << 20
	target := startChild(t, "sleep")
	spec.PID = target.PID
	// The test watches the file itself, for when it passes the limit.
	crossed := make(chan time.Time, 1)
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		for {
			if fi, err := os.Lstat(filepath.Join(spec.StagingPath, OutputFile(ModeCPU))); err == nil && fi.Size() > spec.Bounds.MaxProfileBytes {
				crossed <- time.Now()
				return
			}
			select {
			case <-stopWatch:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	returned := time.Now()
	if errCode(o.Err) != CodeTooLarge || !strings.Contains(o.Err.Message, OutputFile(ModeCPU)) {
		t.Fatalf("Err = %v", o.Err)
	}
	select {
	case at := <-crossed:
		// One poll to see it, the kill and the reaper's report: within 1s.
		if d := returned.Sub(at); d > time.Second {
			t.Errorf("Wait returned %v after the file passed its limit, want within 1s", d)
		}
	default:
		t.Fatal("the file never passed its limit")
	}
}

// Any regular file in staging is bounded: an unexpected name by
// MaxProfileBytes, the execution log by MaxExeclogBytes.
func TestSizesBoundEveryFileInStaging(t *testing.T) {
	kills := 0
	r := testRun(t, &kills)
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	r.spec.Staging = root
	r.b.MaxExeclogBytes = 2 * r.b.MaxProfileBytes
	grow := func(name string, size int64) {
		t.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	grow(OutputFile(ModeCPU), 10)
	grow(ExeclogFile, r.b.MaxProfileBytes+1)
	if e := r.sizes(); e != nil {
		t.Errorf("an execution log within MaxExeclogBytes: %v", e)
	}
	grow("stray.bin", r.b.MaxProfileBytes+1)
	if e := r.sizes(); errCode(e) != CodeTooLarge || !strings.Contains(e.Message, "stray.bin") {
		t.Errorf("an unexpected file over MaxProfileBytes: %v", e)
	}
}

func TestRunCancel(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeIgnoreSIGINT})
	spec.Settings.Duration = time.Minute
	spec.Bounds.StopGrace = time.Second
	target := startChild(t, "sleep")
	spec.PID = target.PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	cancelled := time.Now()
	go r.Cancel()
	go r.Cancel()
	o := waitRun(t, r, 30*time.Second)
	gone := time.Since(cancelled)
	r.Cancel()
	if errCode(o.Err) != CodeCancelled || o.StoppedBy != StoppedByCancel {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
	// perf-agent ignores SIGINT, so the reaper's SIGKILL at StopGrace ends it.
	if gone < spec.Bounds.StopGrace-100*time.Millisecond || gone > spec.Bounds.StopGrace+slack {
		t.Errorf("gone %v after Cancel, want about StopGrace %v", gone, spec.Bounds.StopGrace)
	}
}

func TestRunRechecksTheTarget(t *testing.T) {
	for _, tc := range []struct{ mode, changed string }{
		{"drop-dumpable-after", "dumpable"},
		{"setresgid-after", "gid"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			spec := runSpec(t, capturetest.FakeOptions{})
			spec.Settings.Duration = 20 * time.Second
			spec.Bounds.Recheck = 100 * time.Millisecond
			spec.Launch = launchOf(t, tc.mode, "500ms", "1m")
			o := waitRun(t, startRun(t, spec), 30*time.Second)
			if strings.Contains(o.Workload.Stdout, "skip") {
				t.Skip("the workload could not change its gid here")
			}
			if errCode(o.Err) != CodeTargetChanged || !strings.Contains(o.Err.Changed, tc.changed) {
				t.Fatalf("Err = %+v, want target_changed naming %s", o.Err, tc.changed)
			}
			if o.Profiled >= 10*time.Second {
				t.Errorf("profiled %v: the change did not stop the capture", o.Profiled)
			}
		})
	}
}

func TestRunStartTimeHookIsPIDReuse(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = 20 * time.Second
	target := startChild(t, "sleep")
	var reused atomic.Bool
	spec.Proc.StartTimeHook = func(pid int, start uint64) uint64 {
		if pid == target.PID && reused.Load() {
			return start + 1
		}
		return start
	}
	spec.PID = target.PID
	spec.Bounds.Recheck = 100 * time.Millisecond
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	reused.Store(true)
	o := waitRun(t, r, 30*time.Second)
	if errCode(o.Err) != CodePIDReused {
		t.Fatalf("Err = %v, want pid_reused", o.Err)
	}
}

// §6.3's documented limit: a change that reverts between two rechecks is
// not seen. The control subtest holds the change across a recheck.
func TestRunTransitionAndRevertWithinOneIntervalIsMissed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		recheck time.Duration
		hold    time.Duration
		slow    bool
		want    Code
	}{
		{"missed", time.Hour, 500 * time.Millisecond, true, ""},
		{"held across a recheck", 100 * time.Millisecond, 500 * time.Millisecond, false, CodeTargetChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := runSpec(t, capturetest.FakeOptions{})
			spec.Settings.Duration = 2 * time.Second
			spec.Bounds.Recheck = tc.recheck
			target := startChild(t, "flip-dumpable")
			spec.PID = target.PID
			// With slow, every read of the target from here on takes 200ms,
			// the readiness recheck's included: a phase that said profiling
			// before that recheck finished would let the change below land
			// inside it.
			var slow atomic.Bool
			spec.Proc.StartTimeHook = func(pid int, start uint64) uint64 {
				if pid == target.PID && slow.Load() {
					time.Sleep(200 * time.Millisecond)
				}
				return start
			}
			r := startRun(t, spec)
			slow.Store(tc.slow)
			waitPhase(t, r, PhaseProfiling, 10*time.Second)
			_ = syscall.Kill(target.PID, syscall.SIGUSR1)
			if l := target.next(t); l != "dropped" {
				t.Fatalf("child said %q", l)
			}
			time.Sleep(tc.hold)
			_ = syscall.Kill(target.PID, syscall.SIGUSR2)
			if l := target.next(t); l != "restored" {
				t.Fatalf("child said %q", l)
			}
			if o := waitRun(t, r, 30*time.Second); errCode(o.Err) != tc.want {
				t.Fatalf("Err = %v, want %q", o.Err, tc.want)
			}
		})
	}
}

// Review Focus 3.
func TestRunWorkloadFloodNeverStallsAndTailIsValidUTF8(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = time.Second
	spec.Launch = launchOf(t, "flood-then-sleep", "16", "1m")
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err != nil || o.StoppedBy != StoppedByDuration {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
	if d := o.Profiled - spec.Settings.Duration; d < -250*time.Millisecond || d > 250*time.Millisecond {
		t.Errorf("Profiled = %v, want about %v", o.Profiled, spec.Settings.Duration)
	}
	// The marker follows 16 MiB on each stream: had a reader stalled, the
	// workload would still be writing.
	if !strings.Contains(o.Workload.Stdout, "flooded") {
		t.Errorf("stdout tail %q lacks the marker after the flood", o.Workload.Stdout)
	}
	for name, s := range map[string]string{"stdout": o.Workload.Stdout, "stderr": o.Workload.Stderr} {
		if !utf8.ValidString(s) || len(s) > spec.Bounds.MaxOutputTail || len(s) < spec.Bounds.MaxOutputTail/2 {
			t.Errorf("%s tail: %d bytes, valid UTF-8 %v", name, len(s), utf8.ValidString(s))
		}
	}
	b, err := json.Marshal(o)
	if err != nil || !json.Valid(b) {
		t.Errorf("the outcome is not valid JSON: %v", err)
	}
}

func TestRunSpamStdoutStaysInTheRings(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeSpamStdout})
	target := startChild(t, "sleep")
	spec.PID = target.PID
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err != nil || o.StoppedBy != StoppedByDuration {
		t.Fatalf("Err %v, stopped by %q", o.Err, o.StoppedBy)
	}
	if o.StderrTail != "" || o.Workload != (Tail{}) {
		t.Errorf("perf-agent's output leaked into the outcome: stderr tail %d bytes, workload %+v", len(o.StderrTail), o.Workload)
	}
}

func TestRunGPUNeedsTheShimMapped(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := runSpec(t, capturetest.FakeOptions{Record: record})
	shim := filepath.Join(t.TempDir(), "libshim.so")
	if err := os.WriteFile(shim, []byte("not mapped"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(shim, &st); err != nil {
		t.Fatal(err)
	}
	spec.GPU = &Shim{Path: shim, Dev: uint64(st.Dev), Ino: st.Ino}
	spec.Settings.Modes = []Mode{ModeGPU}
	spec.PID = startChild(t, "sleep").PID
	r, e := Start(spec)
	if r != nil || errCode(e) != CodeGPUNotInjected {
		t.Fatalf("Start = %v, %v; want gpu_not_injected", r, e)
	}
	if _, ok := readRecord(t, record); ok {
		t.Error("perf-agent ran for a target without the shim")
	}
}

// The reaper's survivors make the capture stuck_child.
func TestRunSurvivorsAreStuckChild(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.PID = startChild(t, "sleep").PID
	spec.Events = func(in <-chan Event) <-chan Event {
		out := make(chan Event)
		go func() {
			defer close(out)
			for ev := range in {
				if ev.Op == EventReaped {
					ev.Survivors = []Survivor{{PID: 42, Comm: "stuck", State: "D"}}
					ev.Cleanup = "incomplete: pid 42 in D state"
				}
				out <- ev
			}
		}()
		return out
	}
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if errCode(o.Err) != CodeStuckChild || len(o.Err.Survivors) != 1 || o.Err.Survivors[0].PID != 42 || len(o.Survivors) != 1 {
		t.Fatalf("outcome = %+v, Err %+v", o, o.Err)
	}
	if !strings.Contains(strings.Join(o.Notes, "\n"), "descendants may survive") {
		t.Errorf("notes = %q", o.Notes)
	}
}

// A reaper that exits without its reaped event lost containment; that is
// never a success.
func TestRunReaperGoneBeforeReapedIsContainmentLost(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.PID = startChild(t, "sleep").PID
	spec.Events = func(in <-chan Event) <-chan Event {
		out := make(chan Event)
		go func() {
			defer close(out)
			for ev := range in {
				if ev.Op != EventReaped {
					out <- ev
				}
			}
		}()
		return out
	}
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err == nil || !strings.Contains(o.Err.Message, "containment") {
		t.Fatalf("Err = %v, want containment lost", o.Err)
	}
	if !strings.Contains(strings.Join(o.Notes, "\n"), "containment") {
		t.Errorf("notes = %q", o.Notes)
	}
}

func TestRunKillIsNonBlocking(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = time.Minute
	spec.PID = startChild(t, "sleep").PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	began := time.Now()
	r.Kill()
	r.Kill()
	if d := time.Since(began); d > 10*time.Millisecond {
		t.Errorf("Kill took %v", d)
	}
	if o := waitRun(t, r, 30*time.Second); errCode(o.Err) != CodeCancelled {
		t.Errorf("Err = %v", o.Err)
	}
}

// launched says only that the exec was attempted: a launcher that dies
// between its marker and the exec must not pass for the command.
func TestRunLauncherDeathAfterTheMarkerIsNoLaunch(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := runSpec(t, capturetest.FakeOptions{Record: record})
	spec.Launch = launchOf(t, "sleep", "1m")
	spec.FaultPause = PauseLauncherDiesAfterMarker
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err == nil || o.Launched {
		t.Fatalf("outcome = %+v, want a failed launch", o)
	}
	if _, ok := readRecord(t, record); ok || stagingHas(t, spec, OutputFile(ModeCPU)) {
		t.Error("perf-agent ran against a command that never executed")
	}
}

// A command the launcher cannot execute is a typed error, never launched.
func TestRunLaunchErrorIsTyped(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := runSpec(t, capturetest.FakeOptions{Record: record})
	spec.Launch = launchOf(t, "sleep", "1m")
	spec.Launch.Path = filepath.Join(t.TempDir(), "missing")
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err == nil || o.Launched || !strings.Contains(o.Err.Message, "no such file") {
		t.Fatalf("outcome = %+v, Err %v", o, o.Err)
	}
	if _, ok := readRecord(t, record); ok {
		t.Error("perf-agent ran without a command")
	}
}

// The command gets a cgroup of its own under a delegated parent, and the
// reaper removes it.
func TestRunGivesACommandItsOwnCgroup(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	parent, why := DelegatedCgroup(selfCgroup(t), cgroupMount)
	if why != "" {
		t.Skip("no delegated cgroup: " + why)
	}
	ours := func() []string {
		m, _ := filepath.Glob(filepath.Join(parent, CgroupPrefix+"cap-run-"+strconv.Itoa(os.Getpid())+"-*"))
		return m
	}
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Cgroup = parent
	spec.Launch = launchOf(t, "double-fork-setsid", "1m")
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	during := ours()
	o := waitRun(t, r, 30*time.Second)
	if o.Err != nil || !CleanupIsComplete(o.Cleanup) || strings.Contains(o.Cleanup, "no cgroup") {
		t.Fatalf("Err %v, cleanup %q, notes %q", o.Err, o.Cleanup, o.Notes)
	}
	if len(during) != 1 {
		t.Errorf("cgroups while profiling = %v, want one", during)
	}
	if left := ours(); len(left) != 0 {
		t.Errorf("cgroups left after Wait: %v", left)
	}
	requireGone(t, grandchildOf(t, o))
}

func TestStartRefusesABadSpec(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	target := startChild(t, "sleep")
	for name, mutate := range map[string]func(*RunSpec){
		"no target":        func(s *RunSpec) {},
		"both targets":     func(s *RunSpec) { s.PID = target.PID; s.Launch = launchOf(t, "sleep", "1m") },
		"zero bound":       func(s *RunSpec) { s.PID = target.PID; s.Bounds.Poll = 0 },
		"relative self":    func(s *RunSpec) { s.PID = target.PID; s.Self = "perf-agent-mcp" },
		"bad unwind":       func(s *RunSpec) { s.PID = target.PID; s.Settings.Unwind = "frames" },
		"no staging":       func(s *RunSpec) { s.PID = target.PID; s.Staging = nil },
		"gpu with no shim": func(s *RunSpec) { s.PID = target.PID; s.Settings.Modes = []Mode{ModeGPU} },
	} {
		s := spec
		mutate(&s)
		if r, e := Start(s); r != nil || errCode(e) != CodeInvalidArgument {
			t.Errorf("%s: Start = %v, %v; want invalid_argument", name, r, e)
			if r != nil {
				r.Kill()
				r.Wait()
			}
		}
	}
	// A PID the preflight refuses fails Start.
	s := spec
	s.PID = os.Getpid()
	if _, e := Start(s); errCode(e) != CodeTargetRefused {
		t.Errorf("Start on this server = %v, want target_refused", e)
	}
}

// The exec proof: a live PID still running the launcher's binary never
// passes for the command; the command itself, or the interpreter its #!
// line names, does.
func TestExecProofNeedsTheCommandNotTheLauncher(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	w := capturetest.Workload(t)
	proof := func(command string, pid int) *Error {
		r := &Run{spec: RunSpec{Proc: DefaultProc(), Launch: &LaunchSpec{Path: command}},
			launchers: launcherIDs(self), cmdIDs: commandIDs(command)}
		start, err := StartTime(DefaultProc(), pid)
		if err != nil {
			t.Fatal(err)
		}
		return r.execProven(pid, start)
	}
	// A helper child is this test binary: what a launcher that never
	// executed would still be.
	launcher := startChild(t, "sleep")
	if e := proof(w, launcher.PID); errCode(e) != CodeTargetExited || !e.Retryable || !strings.Contains(e.Message, "still runs the launcher") {
		t.Errorf("a launcher passed for the command, or not as target_exited: %v", e)
	}
	// Another program is accepted, and noted for what it is.
	other := exec.Command("/bin/sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	or := &Run{spec: RunSpec{Proc: DefaultProc(), Launch: &LaunchSpec{Path: w}}, launchers: launcherIDs(self), cmdIDs: commandIDs(w)}
	ostart, _ := StartTime(DefaultProc(), other.Process.Pid)
	if e := or.execProven(other.Process.Pid, ostart); e != nil || !strings.Contains(strings.Join(or.o.Notes, ""), "executed a program other than the resolved command") {
		t.Errorf("another program: %v, notes %q", e, or.o.Notes)
	}
	cmd := exec.Command(w, "sleep", "1m")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if e := proof(w, cmd.Process.Pid); e != nil {
		t.Errorf("the command itself: %v", e)
	}
	// A script runs as its interpreter.
	script := filepath.Join(t.TempDir(), "bench.sh")
	if err := os.WriteFile(script, []byte("#!"+w+" sleep\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if e := proof(script, cmd.Process.Pid); e != nil {
		t.Errorf("a script's interpreter: %v", e)
	}
	// A pid that is gone is no proof either.
	gone := startChild(t, "sleep")
	start, _ := StartTime(DefaultProc(), gone.PID)
	_ = gone.reap()
	r := &Run{spec: RunSpec{Proc: DefaultProc(), Launch: &LaunchSpec{Path: w}}, launchers: launcherIDs(self), cmdIDs: commandIDs(w)}
	if e := r.execProven(gone.PID, start); errCode(e) != CodeTargetExited {
		t.Errorf("a reaped pid: %v", e)
	}
}

// An error-driven stop records its time: the finish bound counts from it,
// StoppedBy stays empty (the error is the cause) and Profiled is real.
func TestRunAFailureStopArmsTheFinishBound(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeHangAfterSIGINT})
	spec.Settings.Duration = 8 * time.Second
	spec.Bounds.Recheck = 100 * time.Millisecond
	spec.Bounds.FinishTimeout = time.Second
	spec.Launch = launchOf(t, "drop-dumpable-after", "500ms", "1m")
	began := time.Now()
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if errCode(o.Err) != CodeTargetChanged || o.StoppedBy != "" {
		t.Fatalf("Err %v, stopped by %q; want target_changed and no StoppedBy", o.Err, o.StoppedBy)
	}
	// 0.5s to the change, 1s of FinishTimeout, the kill: about 1.5s, not
	// the 9s a bound counted from the duration's end would take.
	if d := time.Since(began); d > 1500*time.Millisecond+slack {
		t.Errorf("Wait took %v, want about 1.5s", d)
	}
	if o.Profiled <= 0 || o.Profiled > time.Second {
		t.Errorf("Profiled = %v, want the real time to the change", o.Profiled)
	}
}

// Run.Kill is a cancel decided when Kill is called, whatever order the
// reaper's exits then arrive in; Profiled runs to the kill.
func TestRunKillIsCancelWithItsOwnStop(t *testing.T) {
	for i := range 3 {
		spec := runSpec(t, capturetest.FakeOptions{})
		spec.Settings.Duration = 20 * time.Second
		spec.PID = startChild(t, "sleep").PID
		r := startRun(t, spec)
		waitPhase(t, r, PhaseProfiling, 10*time.Second)
		time.Sleep(300 * time.Millisecond)
		r.Kill()
		o := waitRun(t, r, 30*time.Second)
		if errCode(o.Err) != CodeCancelled || o.StoppedBy != StoppedByCancel {
			t.Fatalf("run %d: Err %v, stopped by %q; want cancelled, cancel", i, o.Err, o.StoppedBy)
		}
		if o.Profiled < 300*time.Millisecond || o.Profiled > 300*time.Millisecond+slack {
			t.Errorf("run %d: Profiled = %v, want the time to the kill", i, o.Profiled)
		}
	}
}

// After a cancel before readiness, the start bound no longer kills: the
// reaper's SIGINT and StopGrace run their course.
func TestRunAStopDisarmsTheStartBound(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeIgnoreSIGINT, ReadyAfter: 10 * time.Second})
	spec.Bounds.StartTimeout = time.Second
	spec.Bounds.StopGrace = 2 * time.Second
	spec.PID = startChild(t, "sleep").PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseAttaching, 10*time.Second)
	// Let the fake ignore SIGINT first; still well before the start bound.
	time.Sleep(300 * time.Millisecond)
	cancelled := time.Now()
	r.Cancel()
	o := waitRun(t, r, 30*time.Second)
	if errCode(o.Err) != CodeCancelled {
		t.Fatalf("Err = %v", o.Err)
	}
	if d := time.Since(cancelled); d < spec.Bounds.StopGrace-100*time.Millisecond {
		t.Errorf("gone %v after Cancel: the start bound cut StopGrace (%v) short", d, spec.Bounds.StopGrace)
	}
}

// testRun is a Run with no reaper process: kills are counted, commands
// are dropped. It is for the supervisor's decisions alone.
func testRun(t *testing.T, kills *int) *Run {
	t.Helper()
	r := &Run{
		spec:  RunSpec{Proc: DefaultProc(), Settings: Settings{Modes: []Mode{ModeCPU}, Duration: time.Minute}, CaptureID: "t"},
		b:     testBounds(),
		log:   slog.New(slog.DiscardHandler),
		ready: make(chan time.Time, 1),
		o:     Outcome{PerfAgentExit: -1, Started: time.Now()},
	}
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = null.Close() })
	r.reaper = &Reaper{cmds: null, closeCmds: func() { *kills++ }}
	r.killCh, r.wakeKill = make(chan struct{}), func() {}
	ended := make(chan struct{})
	close(ended)
	r.perfOut = &stream{done: ended}
	r.setPhase(PhaseAttaching)
	r.argvPID, r.perf = 1, instance{1, 1}
	return r
}

// A bound that expires while what would satisfy it is already queued does
// not kill: the queue is drained first.
func TestBoundsDrainWhatIsQueuedFirst(t *testing.T) {
	kills := 0
	r := testRun(t, &kills)
	r.ready <- time.Now()
	r.startExpired()
	if kills != 0 || r.readyAt.IsZero() {
		t.Errorf("start bound with a readiness line queued: %d kills, ready %v", kills, r.readyAt)
	}

	kills = 0
	r = testRun(t, &kills)
	evs := make(chan Event, 1)
	evs <- Event{Op: EventExited, Who: WhoPerfAgent, PID: 1}
	r.evc = evs
	r.markStopped(StoppedByCancel, time.Now())
	r.finishExpired()
	if kills != 0 || !r.perfExited {
		t.Errorf("finish bound with perf-agent's exit queued: %d kills, exited %v", kills, r.perfExited)
	}
}

// Run.Kill decides the stop when it is called: the exits it causes, in
// whatever order they are handled, never make it "duration".
func TestAKillDecidesTheStopNotTheExitsItCauses(t *testing.T) {
	kills := 0
	r := testRun(t, &kills)
	r.readyAt = time.Now().Add(-time.Second)
	r.setPhase(PhaseProfiling)
	r.Kill()
	at := *r.killAt.Load()
	time.Sleep(50 * time.Millisecond)
	evs := make(chan Event, 1)
	evs <- Event{Op: EventExited, Who: WhoPerfAgent, PID: 1, Status: -1, Signal: "killed"}
	r.evc = evs
	ev, ok := <-r.evc
	r.handleEvent(ev, ok)
	if r.o.StoppedBy != StoppedByCancel || errCode(r.o.Err) != CodeCancelled {
		t.Fatalf("stopped by %q, Err %v; want cancel, cancelled", r.o.StoppedBy, r.o.Err)
	}
	if want := at.Sub(r.readyAt); r.o.Profiled != want {
		t.Errorf("Profiled = %v, want %v, to the kill", r.o.Profiled, want)
	}
}

// The launcher's own failures (CAP-R29): an exec it reports is the
// command's fault, invalid_argument naming it; a launcher that never got
// to the command is target_exited, retryable, with a note.
func TestLaunchErrorsMapToTheirCodes(t *testing.T) {
	for _, tc := range []struct {
		err       string
		code      Code
		retryable bool
		note      bool
	}{
		{"launch: exec /x/bench: no such file or directory", CodeInvalidArgument, false, false},
		{"launch: exec /x/bench: exec format error", CodeInvalidArgument, false, false},
		{"launch: the launcher exited before executing the command", CodeTargetExited, true, true},
		{"launch: the launcher did not confirm its exec: i/o timeout", CodeTargetExited, true, true},
	} {
		kills := 0
		r := testRun(t, &kills)
		r.spec.Launch = &LaunchSpec{Path: "/x/bench"}
		r.argvPID, r.perf = 0, instance{}
		r.reaperError(Event{Op: EventError, Who: WhoWorkload, Err: tc.err})
		e := r.o.Err
		if errCode(e) != tc.code || e.Retryable != tc.retryable || r.o.Launched {
			t.Errorf("%q: Err %+v", tc.err, e)
			continue
		}
		if tc.code == CodeInvalidArgument && (!strings.Contains(e.Message, "command") || !strings.Contains(e.Message, tc.err[len("launch: exec /x/bench: "):])) {
			t.Errorf("%q: message %q does not name the command and the errno", tc.err, e.Message)
		}
		if got := len(r.o.Notes) > 0; got != tc.note {
			t.Errorf("%q: notes %q", tc.err, r.o.Notes)
		}
	}
}

// A command's attach latency is what its profile misses: it runs from
// launched, the preflight included, not from the attach.
func TestRunACommandsAttachLatencyRunsFromItsLaunch(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Launch = launchOf(t, "sleep", "1m")
	var once sync.Once
	spec.Proc.StartTimeHook = func(_ int, start uint64) uint64 {
		once.Do(func() { time.Sleep(300 * time.Millisecond) }) // the launched preflight
		return start
	}
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if o.Err != nil {
		t.Fatalf("Err = %v", o.Err)
	}
	if o.AttachLatency < 300*time.Millisecond {
		t.Errorf("AttachLatency = %v, want at least the 300ms preflight after launched", o.AttachLatency)
	}
}

// The attach-latency note says nothing when the latency rounds to zero.
func TestTheAttachNoteIsOmittedWhenItRoundsToZero(t *testing.T) {
	if n := attachNote(40 * time.Millisecond); n != "" {
		t.Errorf("note for 40ms = %q", n)
	}
	if n := attachNote(1400 * time.Millisecond); !strings.Contains(n, "1.4s after the command started") {
		t.Errorf("note for 1.4s = %q", n)
	}
}

// A target that cannot be read, though it has not ended, is "cannot
// recheck": noted once, never a change and never its exit.
func TestAnUnreadableTargetCannotBeRechecked(t *testing.T) {
	kills := 0
	r := testRun(t, &kills)
	status := fakeStatus(ids(os.Getuid()), ids(os.Getgid()), "0000000000000000")
	fp := fakeProc(t, 11, fakeStat(11, "w", "S", 500), status, "", "/bin/true")
	r.spec.Proc = fp
	r.target, r.targetKnown = TargetState{PID: 11, StartTime: 500}, true
	if err := os.Chmod(filepath.Join(fp.Root, "11", "status"), 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if gone, e := r.observe(); gone || e != nil {
			t.Fatalf("observe = %v, %v; want neither gone nor a change", gone, e)
		}
	}
	if len(r.o.Notes) != 1 || !strings.Contains(r.o.Notes[0], "could not be rechecked") {
		t.Errorf("notes = %q, want one cannot-recheck note", r.o.Notes)
	}
	if err := os.Chmod(filepath.Join(fp.Root, "11", "stat"), 0); err != nil {
		t.Fatal(err)
	}
	if r.pidGone() {
		t.Error("an unreadable stat reads as the target's end")
	}
}

// waitAllStopped waits until every thread of pid is stopped (T): kill(2)
// returns before a group stop completes, and a thread still running could
// read the command pipe's EOF and clean up after all.
func waitAllStopped(t *testing.T, pid int) {
	t.Helper()
	ok := eventually(10*time.Second, func() bool {
		tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
		if err != nil || len(tasks) == 0 {
			return false
		}
		for _, task := range tasks {
			b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "stat"))
			if err != nil {
				return false
			}
			f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
			if len(f) == 0 || f[0] != "T" {
				return false
			}
		}
		return true
	})
	if !ok {
		t.Fatalf("reaper %d did not stop", pid)
	}
}

// A reused PID is pid_reused even when the process now at it has ended:
// the start time is compared before the exit is believed.
func TestAReusedPIDThatHasEndedIsPIDReused(t *testing.T) {
	kills := 0
	status := fakeStatus(ids(os.Getuid()), ids(os.Getgid()), "0000000000000000")
	for name, stat := range map[string]string{
		"zombie":  fakeStat(11, "w", "Z", 600),
		"exiting": strings.Replace(fakeStat(11, "w", "S", 600), " 4194304 ", " 4194308 ", 1),
	} {
		r := testRun(t, &kills)
		r.spec.Proc = fakeProc(t, 11, stat, status, "", "")
		r.spec.PID = 11
		r.target, r.targetKnown = TargetState{PID: 11, StartTime: 500}, true
		if gone, e := r.observe(); gone || errCode(e) != CodePIDReused {
			t.Errorf("%s at a reused pid: gone %v, %v; want pid_reused", name, gone, e)
		}
	}
}

// The same through the poll: the process at the pid is a zombie that, by
// its start time, is another process.
func TestRunAReusedPIDThatIsAZombieIsPIDReused(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = 20 * time.Second
	spec.Bounds.Recheck = time.Hour
	target := startChild(t, "sleep")
	statPath := filepath.Join("/proc", strconv.Itoa(target.PID), "stat")
	spec.Proc.StartTimeHook = func(pid int, start uint64) uint64 {
		// From the moment it is a zombie, the pid reads as another process.
		if b, err := os.ReadFile(statPath); pid == target.PID && err == nil {
			if f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:])); len(f) > 0 && f[0] == "Z" {
				return start + 1
			}
		}
		return start
	}
	spec.PID = target.PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	target.exit()
	if o := waitRun(t, r, 30*time.Second); errCode(o.Err) != CodePIDReused {
		t.Fatalf("Err %v, stopped by %q; want pid_reused", o.Err, o.StoppedBy)
	}
}

// A target already seen gone cannot change after it: a pid reused before
// perf-agent's exit event (symbolization may take seconds) does not turn
// the target's exit into pid_reused. For a PID target and for a command
// whose exit event came first.
func TestAReuseAfterTheTargetEndedKeepsItsExit(t *testing.T) {
	status := fakeStatus(ids(os.Getuid()), ids(os.Getgid()), "0000000000000000")
	for _, launch := range []bool{false, true} {
		kills := 0
		r := testRun(t, &kills)
		// Another live process now holds the pid.
		r.spec.Proc = fakeProc(t, 11, fakeStat(11, "other", "S", 600), status, "", "/bin/true")
		r.target, r.targetKnown = TargetState{PID: 11, StartTime: 500}, true
		r.readyAt = time.Now().Add(-time.Second)
		r.setPhase(PhaseProfiling)
		want := StoppedByTargetExit
		if launch {
			r.spec.Launch = &LaunchSpec{Path: "/x/bench"}
			r.work = instance{11, 500}
			r.workDone()
			want = StoppedByCommandExit
		} else {
			r.spec.PID = 11
			r.targetGone()
		}
		r.perfDone(Event{Op: EventExited, Who: WhoPerfAgent, PID: 1})
		if r.o.Err != nil || r.o.StoppedBy != want {
			t.Errorf("launch %v: Err %v, stopped by %q; want ok, %s", launch, r.o.Err, r.o.StoppedBy, want)
		}
	}
}

// A perf-agent that ran its course profiled for its duration, not for the
// supervisor's delay in hearing of its exit.
func TestRanItsCourseIsCappedAtTheDuration(t *testing.T) {
	kills := 0
	r := testRun(t, &kills)
	r.spec.Settings.Duration = 500 * time.Millisecond
	r.readyAt = time.Now().Add(-1200 * time.Millisecond)
	r.setPhase(PhaseProfiling)
	r.perfDone(Event{Op: EventExited, Who: WhoPerfAgent, PID: 1})
	if r.o.StoppedBy != StoppedByDuration || r.o.Profiled != r.spec.Settings.Duration {
		t.Errorf("stopped by %q, Profiled %v; want duration, %v", r.o.StoppedBy, r.o.Profiled, r.spec.Settings.Duration)
	}
}

// An event stream that ends mid-profiling without reaped is containment
// lost, and its stop is recorded: Profiled is the time profiled.
func TestRunContainmentLostRecordsItsStop(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = 20 * time.Second
	spec.PID = startChild(t, "sleep").PID
	cut := make(chan struct{})
	spec.Events = func(in <-chan Event) <-chan Event {
		out := make(chan Event)
		go func() {
			defer close(out)
			for {
				select {
				case ev, ok := <-in:
					if !ok {
						return
					}
					select {
					case out <- ev:
					case <-cut:
						go func() {
							for range in {
							}
						}()
						return
					}
				case <-cut:
					go func() {
						for range in {
						}
					}()
					return
				}
			}
		}()
		return out
	}
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	close(cut)
	o := waitRun(t, r, 30*time.Second)
	if errCode(o.Err) != CodeStuckChild || !strings.Contains(o.Err.Message, "containment") {
		t.Fatalf("Err = %v", o.Err)
	}
	if o.Profiled < 300*time.Millisecond || o.Profiled > 300*time.Millisecond+slack {
		t.Errorf("Profiled = %v, want the time to the loss", o.Profiled)
	}
}

// A reaper that does not answer a kill is not waited for forever: Wait
// says stuck_child, naming what is known to be left.
func TestRunAnUnresponsiveReaperIsStuckChild(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := runSpec(t, capturetest.FakeOptions{Record: record})
	spec.Settings.Duration = time.Minute
	spec.Bounds.StopGrace = 200 * time.Millisecond
	spec.Bounds.FinishTimeout = 300 * time.Millisecond
	spec.PID = startChild(t, "sleep").PID
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	reaper := r.reaper.PID()
	if err := syscall.Kill(reaper, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(reaper, syscall.SIGCONT) })
	waitAllStopped(t, reaper)
	killed := time.Now()
	r.Kill()
	o := waitRun(t, r, 30*time.Second)
	if d, bound := time.Since(killed), spec.Bounds.StopGrace+spec.Bounds.FinishTimeout+5*time.Second; d > bound+slack {
		t.Errorf("Wait took %v after the kill, want within %v", d, bound)
	}
	if errCode(o.Err) != CodeStuckChild || !strings.Contains(o.Err.Message, "unresponsive") {
		t.Fatalf("Err = %v, want stuck_child naming the unresponsive reaper", o.Err)
	}
	rec, _ := readRecord(t, record)
	found := false
	for _, s := range o.Survivors {
		found = found || s.PID == rec.PID
	}
	if !found {
		t.Errorf("survivors %+v do not name perf-agent %d", o.Survivors, rec.PID)
	}
	// The readers did not stay behind on pipes the stopped children hold.
	for _, st := range []*stream{r.perfOut, r.perfErr, r.workOut, r.workErr} {
		select {
		case <-st.done:
		case <-time.After(5 * time.Second):
			t.Error("a reader is still blocked after Wait")
		}
	}
	// Let the reaper go on: it reads the EOF and cleans up.
	_ = syscall.Kill(reaper, syscall.SIGCONT)
	select {
	case <-r.reaper.Done():
	case <-time.After(20 * time.Second):
		t.Error("the reaper did not finish once continued")
	}
}

// With a command in its own cgroup, an unresponsive reaper's outcome names
// the cgroup and lists the command.
func TestRunAnUnresponsiveReaperNamesTheCgroup(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	parent, why := DelegatedCgroup(selfCgroup(t), cgroupMount)
	if why != "" {
		t.Skip("no delegated cgroup: " + why)
	}
	spec := runSpec(t, capturetest.FakeOptions{})
	spec.Settings.Duration = time.Minute
	spec.Bounds.StopGrace = 200 * time.Millisecond
	spec.Bounds.FinishTimeout = 300 * time.Millisecond
	spec.Cgroup = parent
	spec.Launch = launchOf(t, "sleep", "1m")
	r := startRun(t, spec)
	waitPhase(t, r, PhaseProfiling, 10*time.Second)
	reaper := r.reaper.PID()
	if err := syscall.Kill(reaper, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(reaper, syscall.SIGCONT) })
	waitAllStopped(t, reaper)
	r.Kill()
	o := waitRun(t, r, 30*time.Second)
	if errCode(o.Err) != CodeStuckChild {
		t.Fatalf("Err = %v", o.Err)
	}
	if !strings.Contains(strings.Join(o.Notes, "\n"), filepath.Join(parent, CgroupPrefix)) {
		t.Errorf("notes %q do not name the cgroup", o.Notes)
	}
	listed := false
	for _, s := range o.Survivors {
		listed = listed || s.PID == o.Target.PID
	}
	if !listed {
		t.Errorf("survivors %+v do not list the command %d", o.Survivors, o.Target.PID)
	}
	_ = syscall.Kill(reaper, syscall.SIGCONT)
	select {
	case <-r.reaper.Done():
	case <-time.After(20 * time.Second):
		t.Error("the reaper did not finish once continued")
	}
}

// A perf-agent ended by a signal is named by that signal once: the event
// carries the signal's own text ("killed", "terminated").
func TestASignalledPerfAgentNamesItsSignal(t *testing.T) {
	for _, sig := range []string{"killed", "terminated", "segmentation fault"} {
		kills := 0
		r := testRun(t, &kills)
		r.readyAt = time.Now().Add(-time.Second)
		r.perfExited, r.perfStatus, r.perfSignal = true, -1, sig
		e := r.exitError("")
		if errCode(e) != CodePerfAgentFailed || e.Message != "perf-agent was ended by a signal: "+sig || e.PerfAgentExit != sig {
			t.Errorf("%s: %+v", sig, e)
		}
	}
}

// A capture the reaper ended at its absolute deadline (a server that did
// not act on its own bounds) is start_timeout before readiness and
// finish_timeout after, whatever exits the deadline's kills cause.
func TestTheReapersDeadlineIsABound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
		want  Code
	}{
		{"before readiness", false, CodeStartTimeout},
		{"after readiness", true, CodeFinishTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kills := 0
			r := testRun(t, &kills)
			r.spec.Launch = &LaunchSpec{Path: "/x/bench"}
			r.work = instance{11, 11}
			r.targetKnown, r.target = true, TargetState{PID: 11, StartTime: 11}
			if tc.ready {
				r.readyAt = time.Now().Add(-time.Second)
				r.setPhase(PhaseProfiling)
			}
			evs := make(chan Event, 3)
			evs <- Event{Op: EventDeadline}
			evs <- Event{Op: EventExited, Who: WhoWorkload, PID: 11, Status: -1, Signal: "killed"}
			evs <- Event{Op: EventExited, Who: WhoPerfAgent, PID: 1, Status: -1, Signal: "killed"}
			close(evs)
			r.evc = evs
			r.drainPending()
			if errCode(r.o.Err) != tc.want || !strings.Contains(r.o.Err.Message, "deadline") {
				t.Fatalf("Err = %+v, want %s naming the deadline", r.o.Err, tc.want)
			}
			if r.o.StoppedBy == StoppedByCommandExit || r.o.StoppedBy == StoppedByTargetExit {
				t.Errorf("stopped by %q", r.o.StoppedBy)
			}
		})
	}
}

// Wait returns once the reaper has let go of the captures lock, not only
// once it reported reaped: the next capture can take the lock at once.
// The reaper is held a while after reaped, as a slow exit would hold it.
func TestWaitReturnsWithTheLockFree(t *testing.T) {
	spec := runSpec(t, capturetest.FakeOptions{})
	target := startChild(t, "sleep")
	spec.PID = target.PID
	dir, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	lease, e := TryLock(dir, Holder{ServerPID: os.Getpid(), CaptureID: "cap/lock"})
	if e != nil {
		t.Fatal(e)
	}
	spec.Lock = lease.File()
	hold := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(hold) })
	spec.FaultPause = PauseBeforeExit + hold
	spec.Events = func(in <-chan Event) <-chan Event {
		out := make(chan Event)
		go func() {
			defer close(out)
			for ev := range in {
				if ev.Op == EventReaped {
					time.AfterFunc(300*time.Millisecond, func() { _ = os.Remove(hold) })
				}
				out <- ev
			}
		}()
		return out
	}
	r := startRun(t, spec)
	// Only the reaper holds the lock from here, as in a job after Start.
	_ = lease.Close()
	o := waitRun(t, r, 30*time.Second)
	if o.Err != nil {
		t.Fatalf("Err = %v", o.Err)
	}
	l, be := TryLock(dir, Holder{ServerPID: os.Getpid(), CaptureID: "cap/next"})
	if be != nil {
		t.Fatalf("the lock is still held when Wait returns: %v", be)
	}
	_ = l.Close()
}
