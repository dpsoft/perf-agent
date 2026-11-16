//go:build linux

package perfmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// Two stdio servers over one captures directory: the lock lets one capture
// run at a time, the other's capture fails busy naming the holder, and the
// series' ids are never handed out twice.
func TestTwoServersShareOneCapturesDir(t *testing.T) {
	e := newProcEnv(t, capturetest.FakeOptions{})
	a := startServerProcess(t, e.config())
	b := startServerProcess(t, e.config())

	jobA := a.startCapture(e.args("30s"))
	a.waitProfiling(jobA)
	a.awaitGrandchild()
	text, isErr := b.waitEnd(b.startCapture(e.args("1s")))
	if !isErr {
		t.Fatalf("B captured while A held the directory: %s", text)
	}
	w := toolErrorOf(t, text)
	if w.Code != string(capture.CodeBusy) || !w.Retryable ||
		!strings.Contains(w.Message, "capture adhoc/001") || !strings.Contains(w.Message, fmt.Sprintf("perf-agent-mcp pid %d", a.pid())) {
		t.Errorf("B's capture: %+v, want busy naming capture adhoc/001 of pid %d", w, a.pid())
	}
	a.cancel(jobA)
	if text, isErr := a.waitEnd(jobA); !isErr || toolErrorOf(t, text).Code != string(capture.CodeCancelled) {
		t.Fatalf("A's cancelled capture: %s", text)
	}

	// Round after round, one server captures and the other, asking while
	// it profiles, is told busy; then the roles swap. Each capture starts
	// as soon as the round before ended: a finished capture's reaper has
	// let go of the lock by then. Every capture has its own id.
	ids := map[string]string{}
	for round := range 4 {
		lead, other := a, b
		if round%2 == 1 {
			lead, other = b, a
		}
		job := lead.startCapture(e.args("2s"))
		lead.waitProfiling(job)
		text, isErr := other.waitEnd(other.startCapture(e.args("1s")))
		if w := toolErrorOf(t, text); !isErr || w.Code != string(capture.CodeBusy) || !strings.Contains(w.Message, fmt.Sprintf("perf-agent-mcp pid %d", lead.pid())) {
			t.Fatalf("round %d: the other server's capture: %s", round, text)
		}
		res := lead.waitResult(job)
		if prev, ok := ids[res.Capture.ID]; ok {
			t.Fatalf("id %s handed out twice: %s and %s", res.Capture.ID, prev, job)
		}
		ids[res.Capture.ID] = job
	}
	for _, c := range []*procClient{a, b} {
		var l listing
		c.callOK("capture_list", map[string]any{"series": "adhoc"}, &l)
		if len(l.Captures) != len(ids)+1 {
			t.Errorf("pid %d lists %d captures, want %d: %+v", c.pid(), len(l.Captures), len(ids)+1, l.Captures)
		}
	}
	e.invariant()
}

// A server SIGKILLed while it profiles leaves its reaper, which kills the
// children at once and keeps the captures lock until it is done. Another
// server's capture fails busy, naming the reaper, for as long as the
// reaper lives; then it captures, and recovery, judging the dead server's
// intent by the recorded server and reaper (PID and start time), wrote
// it an interrupted manifest.
func TestAKilledServerLeavesTheLockWithItsReaper(t *testing.T) {
	e := newProcEnv(t, capturetest.FakeOptions{})
	hold := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cfg.ReaperPause = capture.PauseBeforeExit + hold
	a := startServerProcess(t, cfg)
	t.Cleanup(func() { _ = os.Remove(hold) })
	b := startServerProcess(t, e.config())

	job := a.startCapture(e.args("60s"))
	a.waitProfiling(job)
	a.awaitGrandchild()
	h := e.holder(func(h capture.Holder) bool { return h.ReaperPID > 0 })
	reaper, ok := procOf(h.ReaperPID)
	if !ok || reaper.start != h.ReaperStart || reaper.ppid != a.pid() {
		t.Fatalf("the lock names reaper %d (start %d), and /proc has %+v", h.ReaperPID, h.ReaperStart, reaper)
	}
	if in := e.intent(1, "run"); in.ReaperPID != h.ReaperPID || in.ReaperStart != h.ReaperStart ||
		in.ServerPID != a.pid() || in.ServerStart != a.self.start {
		t.Fatalf("the intent records server %d/%d and reaper %d/%d", in.ServerPID, in.ServerStart, in.ReaperPID, in.ReaperStart)
	}
	rec, ok := e.fakeRecord()
	if !ok {
		t.Fatal("the fake left no record")
	}
	watched := a.watch()
	a.kill9()

	// The reaper ends every child on the EOF and stays, holding the lock.
	waitGone(t, "the fake's group", func() bool { return groupGone(rec.PGID) })
	for _, p := range watched {
		if p.pid != reaper.pid {
			waitGone(t, p.comm, p.gone)
		}
	}
	busy := func() {
		t.Helper()
		text, isErr := b.waitEnd(b.startCapture(e.args("1s")))
		if !isErr {
			t.Fatalf("B captured while A's reaper holds the lock: %s", text)
		}
		w := toolErrorOf(t, text)
		if w.Code != string(capture.CodeBusy) || !strings.Contains(w.Message, fmt.Sprintf("(reaper pid %d)", reaper.pid)) ||
			!strings.Contains(w.Message, "capture adhoc/001") {
			t.Fatalf("B's capture: %+v, want busy naming reaper pid %d", w, reaper.pid)
		}
		if reaper.gone() {
			t.Fatal("the reaper exited while its barrier file exists")
		}
	}
	busy()
	busy()
	// The dead server's heartbeat goes stale while the reaper still holds
	// the lock: recovery cannot run before the reaper is gone.
	e.waitStale()
	busy()

	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	waitGone(t, "the reaper", reaper.gone)
	res := b.waitResult(b.startCapture(e.args("1s")))
	if res.Capture.ID != "002" {
		t.Errorf("B's capture is %s, want 002", res.Capture.ID)
	}
	if m, ok := e.manifest(1, "run"); !ok || m.Outcome != captureInterrupted {
		t.Errorf("capture 001: %+v (found %v), want interrupted", m, ok)
	}
	text, isErr := b.call("capture_wait", map[string]any{"id": "adhoc/001", "timeout": "5s"})
	if w := toolErrorOf(t, text); !isErr || w.Status != captureInterrupted || !w.Retryable {
		t.Errorf("capture_wait adhoc/001: %s", text)
	}
	e.invariant()
}

// Killing the server in each phase of a capture leaves no child: the
// reaper kills them on the EOF, exits and frees the lock, and the next
// server recovers the capture as interrupted.
func TestKillingTheServerInEachPhaseKillsTheChildren(t *testing.T) {
	for _, tc := range []struct {
		name string
		fake capturetest.FakeOptions
		// pause blocks the job at a fault point; phase is the job's phase
		// to reach otherwise.
		pause, phase string
		duration     string
		// exitAfter, when set, is how long the workload runs before it
		// exits; otherwise it leaves a setsid grandchild and sleeps.
		exitAfter string
		children  bool
	}{
		{name: "preparing", pause: "after-staging", duration: "60s"},
		{name: "attaching", fake: capturetest.FakeOptions{Mode: capturetest.FakeNeverReady}, phase: capture.PhaseAttaching, duration: "60s", children: true},
		{name: "profiling", phase: capture.PhaseProfiling, duration: "60s", children: true},
		// The command's exit stops perf-agent, which hangs on the stop.
		{name: "finishing", fake: capturetest.FakeOptions{Mode: capturetest.FakeHangAfterSIGINT}, phase: capture.PhaseFinishing, duration: "60s", exitAfter: "3s", children: true},
		{name: "committing", pause: "after-validate", duration: "1s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newProcEnv(t, tc.fake)
			cfg := e.config()
			cfg.Pause = tc.pause
			cfg.Capture.StartTimeout = 2 * time.Minute
			cfg.Capture.FinishTimeout = 2 * time.Minute
			a := startServerProcess(t, cfg)
			args := e.args(tc.duration)
			if tc.exitAfter != "" {
				args["command"] = []any{capturetest.Workload(t), "exit-after", tc.exitAfter, "0"}
			}
			job := a.startCapture(args)
			if tc.pause != "" {
				a.waitLine(pausedLine + tc.pause)
			} else {
				a.waitPhase(job, tc.phase)
			}
			var pgid int
			if tc.children {
				if tc.exitAfter == "" {
					a.awaitGrandchild()
				}
				rec, ok := e.fakeRecord()
				if !ok {
					t.Fatal("the fake left no record")
				}
				pgid = rec.PGID
			}
			watched := a.watch()
			a.kill9()
			for _, p := range watched {
				waitGone(t, p.comm, p.gone)
			}
			if pgid > 0 {
				waitGone(t, "the fake's group", func() bool { return groupGone(pgid) })
			}
			waitGone(t, "the lock", e.lockFree)
			e.waitStale()
			c := startServerProcess(t, e.plainConfig())
			if res := c.waitResult(c.startCapture(e.args("1s"))); res.Capture.ID != "002" {
				t.Errorf("the next capture is %s, want 002", res.Capture.ID)
			}
			if m, ok := e.manifest(1, "run"); !ok || m.Outcome != captureInterrupted {
				t.Errorf("capture 001: %+v (found %v), want interrupted", m, ok)
			}
			e.invariant()
		})
	}
}

// A server that is alive but hung never stops its capture: the reaper's
// absolute deadline kills the children while the server is still stopped.
func TestAHungServerIsBoundedByTheDeadline(t *testing.T) {
	e := newProcEnv(t, capturetest.FakeOptions{Mode: capturetest.FakeNeverReady})
	cfg := e.config()
	cfg.Capture.StartTimeout = 15 * time.Second
	cfg.Capture.FinishTimeout = 2 * time.Second
	a := startServerProcess(t, cfg)
	job := a.startCapture(e.args("1s"))
	a.waitPhase(job, capture.PhaseAttaching)
	a.awaitGrandchild()
	rec, ok := e.fakeRecord()
	if !ok {
		t.Fatal("the fake left no record")
	}
	h := e.holder(func(h capture.Holder) bool { return h.ReaperPID > 0 })
	reaper, _ := procOf(h.ReaperPID)
	watched := a.watch()
	a.stop()
	if v := a.state(); v != 'T' {
		t.Fatalf("the server's state is %c, not stopped", v)
	}
	// StartTimeout + duration + FinishTimeout + StopGrace from the
	// reaper's start, with room for a loaded machine.
	waitGoneWithin(t, "the fake's group", 2*time.Minute, func() bool { return groupGone(rec.PGID) })
	for _, p := range watched {
		waitGoneWithin(t, p.comm, 2*time.Minute, p.gone)
	}
	waitGoneWithin(t, "the reaper", 2*time.Minute, reaper.gone)
	if v := a.state(); v != 'T' {
		t.Fatalf("the server ran (state %c) before the children were gone: the deadline was not what ended them", v)
	}
	a.cont()
	// Resumed, the server decides by its own start bound or by the
	// reaper's deadline event, whichever it reads first: either way the
	// capture is start_timeout, never the exits the deadline's kills caused.
	text, isErr := a.waitEnd(job)
	if w := toolErrorOf(t, text); !isErr || w.Code != string(capture.CodeStartTimeout) {
		t.Fatalf("the capture the reaper's deadline ended before readiness: %s", text)
	}
	e.invariant()
}

// The reaper creates both children and owns them from fork: one whose
// first report the server never got (the server dies in that gap) is
// still killed on the EOF.
func TestTheSpawnReportGapStillKills(t *testing.T) {
	for _, pause := range []string{capture.PauseAfterLauncherFork, capture.PauseAfterPerfAgentFork} {
		t.Run(pause, func(t *testing.T) {
			t.Parallel()
			e := newProcEnv(t, capturetest.FakeOptions{})
			cfg := e.config()
			cfg.ReaperPause = pause
			cfg.Capture.StartTimeout = 2 * time.Minute
			a := startServerProcess(t, cfg)
			job := a.startCapture(e.args("60s"))
			a.awaitGrandchild()
			var pgid int
			if pause == capture.PauseAfterPerfAgentFork {
				waitGone(t, "the fake's start", func() bool {
					r, ok := e.fakeRecord()
					pgid = r.PGID
					return ok && pgid > 0
				})
			}
			// Without the launched report the server never asks for
			// perf-agent. Without the attached one it still reads
			// perf-agent's output, which it holds itself, and may profile.
			if v := a.status(job); pause == capture.PauseAfterLauncherFork && v.Phase != capture.PhaseLaunching {
				t.Fatalf("the capture is %+v", v)
			}
			watched := a.watch()
			a.kill9()
			for _, p := range watched {
				waitGone(t, p.comm, p.gone)
			}
			if pgid > 0 {
				waitGone(t, "the fake's group", func() bool { return groupGone(pgid) })
			}
			waitGone(t, "the lock", e.lockFree)
		})
	}
}

// A real crash (the process exits) at each protocol boundary leaves what
// recovery finishes: before the manifest link the capture is interrupted,
// after it committed, with its files verified and its leftovers removed by
// identity; before its intent exists, nothing of it is left.
func TestRealCrashesAtCommitBoundaries(t *testing.T) {
	skip := []faultPoint{faultRetentionHash, faultInRollback, faultInRecovery, faultBeforeOpen, faultBeforeDiff}
	type crashCase struct {
		name        string
		crash, fail string
		// recoverCrash crashes the first recovery at in-recovery.
		recoverCrash bool
		postLink     bool
	}
	var cases []crashCase
	for _, f := range faultPoints {
		if !slices.Contains(skip, f.p) {
			cases = append(cases, crashCase{name: f.name, crash: f.name, postLink: f.postLink})
		}
	}
	cases = append(cases,
		crashCase{name: "in-rollback", crash: "in-rollback", fail: "after-record"},
		crashCase{name: "in-recovery", crash: "after-record", recoverCrash: true})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newProcEnv(t, capturetest.FakeOptions{})
			cfg := e.config()
			cfg.Crash, cfg.Fail = tc.crash, tc.fail
			a := startServerProcess(t, cfg)
			args := e.args("1s")
			args["modes"] = []any{"cpu", "offcpu"}
			// The crash may come before the answer is written: only the
			// request is sent.
			a.sendTool("capture", args, nil)
			a.waitLine(crashLine + tc.crash)
			if code := a.exitCode(); code != crashExit {
				t.Fatalf("the server exited %d, not at the crash", code)
			}
			// The crash comes too soon after the request to watch the
			// server's children first. The proof that none is left is the
			// lock (a reaper holds it until its children are reaped) and,
			// at cleanup, the delegated cgroup subtree.
			waitGone(t, "the lock", e.lockFree)
			hadIntent := e.exists(intentName(1, "run"))
			e.waitStale()
			if tc.recoverCrash {
				rc := e.config()
				rc.Crash = "in-recovery"
				// It crashes in its start-up maintenance, maybe before it
				// could answer initialize: nothing is asked of it.
				r := spawnServerProcess(t, rc)
				r.waitLine(crashLine + "in-recovery")
				if code := r.exitCode(); code != crashExit {
					t.Fatalf("the recovering server exited %d", code)
				}
				e.waitStale()
			}
			c := startServerProcess(t, e.config())
			res := c.waitResult(c.startCapture(e.args("1s")))
			m, found := e.manifest(1, "run")
			switch {
			case tc.postLink:
				if !found || m.Outcome != captureOK {
					t.Errorf("crash after the manifest link: capture 001 is %+v (found %v), want committed", m, found)
				}
			case hadIntent:
				if !found || m.Outcome != captureInterrupted && m.Outcome != captureFailed {
					t.Errorf("crash before the manifest link: capture 001 is %+v (found %v), want interrupted", m, found)
				}
				if tc.fail == "" && m.Outcome != captureInterrupted {
					t.Errorf("crash before the manifest link: outcome %s, want interrupted", m.Outcome)
				}
			case found && res.Capture.ID != "001":
				t.Errorf("a crash before the intent left capture 001: %+v", m)
			}
			t.Logf("intent at the crash %v; capture 001 %s; next capture %s", hadIntent, m.Outcome, res.Capture.ID)
			e.invariant()
		})
	}
}

// A captures directory belongs to one kind of server: a stdio server
// refuses one whose .owner says serve, as serve writes it, and a serve
// server one a stdio server made. The refusal is owner_mismatch, answered
// at once, without any I/O, once the server has read .owner, and neither
// server writes anything in the other's directory.
func TestStdioAndServeRefuseEachOthersDir(t *testing.T) {
	refuses := func(t *testing.T, e *procEnv, c *procClient, theirs, mine string) {
		t.Helper()
		sync := false
		for range 3 {
			text, isErr := c.call("capture", e.args("1s"))
			if !isErr {
				var a acceptedAnswer
				if err := json.Unmarshal([]byte(text), &a); err != nil || a.Job == "" {
					t.Fatalf("capture: %s", text)
				}
				text, isErr = c.waitEnd(a.Job)
				if !isErr {
					t.Fatalf("a %s server captured in a %s directory: %s", mine, theirs, text)
				}
			} else {
				sync = true
			}
			w := toolErrorOf(t, text)
			if w.Code != string(capture.CodeOwnerMismatch) || !strings.Contains(w.Message, "belongs to a perf-agent-mcp "+theirs+" server") ||
				!strings.Contains(w.Message, "this one is "+mine) {
				t.Fatalf("%+v", w)
			}
		}
		if !sync {
			t.Error("owner_mismatch was never answered synchronously from the cached .owner")
		}
	}

	t.Run("stdio refuses serve's", func(t *testing.T) {
		t.Parallel()
		e := newProcEnv(t, capturetest.FakeOptions{})
		if err := os.Mkdir(e.captures(), 0o700); err != nil {
			t.Fatal(err)
		}
		owner := []byte(`{"kind":"hub","format":1}` + "\n")
		if err := os.WriteFile(filepath.Join(e.captures(), ownerFile), owner, 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := e.config()
		cfg.Captures = "captures"
		c := startServerProcess(t, cfg)
		refuses(t, e, c, ownerHub, ownerStdio)
		if names := e.listCaptures(); !slices.Equal(names, []string{ownerFile}) {
			t.Errorf("the serve directory holds %v", names)
		}
		if b, _ := os.ReadFile(filepath.Join(e.captures(), ownerFile)); string(b) != string(owner) {
			t.Errorf(".owner is now %q", b)
		}
	})

	t.Run("serve refuses stdio's", func(t *testing.T) {
		t.Parallel()
		e := newProcEnv(t, capturetest.FakeOptions{})
		cfg := e.config()
		cfg.Captures = "captures"
		s := startServerProcess(t, cfg)
		s.waitResult(s.startCapture(e.args("1s")))
		before := e.listCaptures()
		cfg.Owner = ownerHub
		c := startServerProcess(t, cfg)
		refuses(t, e, c, ownerStdio, ownerHub)
		if after := e.listCaptures(); !slices.Equal(after, before) {
			t.Errorf("the serve server changed the stdio directory: %v, then %v", before, after)
		}
		var rec ownerRecord
		if b, err := os.ReadFile(filepath.Join(e.captures(), ownerFile)); err != nil || json.Unmarshal(b, &rec) != nil || rec.Kind != ownerStdio {
			t.Errorf(".owner: %s (%v)", b, err)
		}
	})
}

// groupGone reports that no process is left in the process group pgid.
func groupGone(pgid int) bool { return syscall.Kill(-pgid, 0) == syscall.ESRCH }

// inProgress is capture_wait's answer for a capture whose intent is on
// disk and whose manifest is not yet.
type inProgress struct {
	ID         string `json:"id"`
	Capture    string `json:"capture_id"`
	Status     string `json:"status"`
	RetryAfter string `json:"retry_after"`
	Next       string `json:"next"`
	Holder     string `json:"holder"`
}

// A server SIGKILLed while it profiles, and another started at once: the
// dead server's reaper still holds the captures lock, so the new server's
// start-up recovery cannot run. A wait by capture id answers in_progress,
// naming the dead server, never unknown_capture. Once the reaper lets go,
// the new server's start-up pass, re-armed while the lock was held, runs
// on its own, with no capture to trigger it, and the wait answers
// interrupted, retryable.
func TestARestartRightAfterAKillAnswersTheCaptureInProgress(t *testing.T) {
	e := newProcEnv(t, capturetest.FakeOptions{})
	hold := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := e.config()
	cfg.ReaperPause = capture.PauseBeforeExit + hold
	a := startServerProcess(t, cfg)
	t.Cleanup(func() { _ = os.Remove(hold) })

	job := a.startCapture(e.args("60s"))
	a.waitProfiling(job)
	a.awaitGrandchild()
	h := e.holder(func(h capture.Holder) bool { return h.ReaperPID > 0 })
	reaper, ok := procOf(h.ReaperPID)
	if !ok || reaper.start != h.ReaperStart {
		t.Fatalf("the lock names reaper %d (start %d), and /proc has %+v", h.ReaperPID, h.ReaperStart, reaper)
	}
	a.kill9()
	b := startServerProcess(t, e.config())

	for range 2 {
		text, isErr := b.call("capture_wait", map[string]any{"id": "adhoc/001", "timeout": "1s"})
		var ip inProgress
		if isErr || json.Unmarshal([]byte(text), &ip) != nil || ip.Status != "in_progress" {
			t.Fatalf("capture_wait adhoc/001 while the dead server's reaper holds the lock: %s", text)
		}
		if d, err := time.ParseDuration(ip.RetryAfter); err != nil || d < time.Second || d > 5*time.Second || d%time.Second != 0 {
			t.Errorf("retry_after %q, want whole seconds from 1s to 5s", ip.RetryAfter)
		}
		if ip.Capture != "adhoc/001" || ip.Next == "" || !strings.Contains(ip.Holder, fmt.Sprintf("pid %d", a.pid())) {
			t.Errorf("the answer does not name the capture, what to do, or the dead server pid %d: %s", a.pid(), text)
		}
		if !strings.Contains(ip.Holder, fmt.Sprintf("its reaper pid %d still runs", h.ReaperPID)) || strings.Contains(ip.Next, "another perf-agent-mcp is running") {
			t.Errorf("the answer does not say the server is gone and its reaper %d runs: %s", h.ReaperPID, text)
		}
		if reaper.gone() {
			t.Fatal("the reaper exited while its barrier file exists")
		}
	}

	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	waitGone(t, "the reaper", reaper.gone)
	var last string
	if !eventuallyTrue(time.Minute, func() bool {
		text, isErr := b.call("capture_wait", map[string]any{"id": "adhoc/001", "timeout": "1s"})
		last = text
		if !isErr {
			return false
		}
		w := toolErrorOf(t, text)
		return w.Status == captureInterrupted && w.Retryable && w.Code == string(capture.CodeCancelled)
	}) {
		t.Fatalf("capture_wait adhoc/001 never answered interrupted: %s", last)
	}
	if m, ok := e.manifest(1, "run"); !ok || m.Outcome != captureInterrupted {
		t.Errorf("capture 001: %+v (found %v), want interrupted", m, ok)
	}
	e.invariant()
}
