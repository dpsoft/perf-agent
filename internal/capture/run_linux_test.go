package capture

import (
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
	if !strings.Contains(notes, "after the command started") || !strings.Contains(notes, "only pid "+strconv.Itoa(o.Target.PID)+" was profiled") {
		t.Errorf("notes = %q", o.Notes)
	}
	if (procRef{o.Target.PID, o.Target.StartTime}).alive() {
		t.Error("the command still runs after Wait")
	}
	if r.Phase() != PhaseDone {
		t.Errorf("phase %s after Wait", r.Phase())
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
			spec := runSpec(t, capturetest.FakeOptions{Mode: tc.mode})
			spec.Settings.Duration = time.Minute
			spec.Launch = launchOf(t, tc.args...)
			if tc.set != nil {
				tc.set(&spec)
			}
			r := startRun(t, spec)
			if tc.end != nil {
				waitPhase(t, r, PhaseProfiling, 10*time.Second)
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
	began := time.Now()
	o := waitRun(t, startRun(t, spec), 30*time.Second)
	if errCode(o.Err) != CodeTooLarge || !strings.Contains(o.Err.Message, OutputFile(ModeCPU)) {
		t.Fatalf("Err = %v", o.Err)
	}
	// The fake grows the file just after readiness; the poll catches it
	// within one interval and the kill ends it. A second of slack.
	if d := time.Since(began) - o.AttachLatency; d > time.Second+time.Second {
		t.Errorf("too_large %v after readiness, want within 1s (+1s slack)", d)
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
		want    Code
	}{
		{"missed", time.Hour, 0, ""},
		{"held across a recheck", 100 * time.Millisecond, 500 * time.Millisecond, CodeTargetChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := runSpec(t, capturetest.FakeOptions{})
			spec.Settings.Duration = 2 * time.Second
			spec.Bounds.Recheck = tc.recheck
			target := startChild(t, "flip-dumpable")
			spec.PID = target.PID
			r := startRun(t, spec)
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
	if e := proof(w, launcher.PID); errCode(e) != CodeInvalidArgument || !strings.Contains(e.Message, "still runs the launcher") {
		t.Errorf("a launcher passed for the command: %v", e)
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
