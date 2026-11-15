package capture

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
)

// slack absorbs scheduling on a loaded test machine. Bounds are asserted
// as "within the grace plus slack", never as an exact duration.
const slack = 3 * time.Second

// procRef is a process a test saw, by PID and start time, so a later
// check is about that process and not a reused PID.
type procRef struct {
	pid   int
	start uint64
}

func (p procRef) alive() bool { return Alive(DefaultProc(), p.pid, p.start) }

// refOf records pid with its current start time.
func refOf(t *testing.T, pid int) procRef {
	t.Helper()
	start, err := StartTime(DefaultProc(), pid)
	if err != nil {
		t.Fatalf("start time of %d: %v", pid, err)
	}
	return procRef{pid, start}
}

// harness drives one reaper: it owns the lock, the output pipes and the
// event stream, and on cleanup checks that nothing the test saw is left.
type harness struct {
	t         *testing.T
	r         *Reaper
	root      *os.Root
	lease     *Lease
	reaper    procRef
	perfLines chan string
	workLines chan string

	mu      sync.Mutex
	stderr  strings.Builder
	events  []Event
	seen    []procRef
	exitArm string
}

// harnessOpts varies how the reaper is started: noFiles passes no output
// pipes and no lock, and more are extra descriptors from fd 11 on.
type harnessOpts struct {
	noFiles bool
	more    []*os.File
}

func startHarness(t *testing.T, spec ReaperSpec) *harness {
	t.Helper()
	return startHarnessOpts(t, spec, harnessOpts{})
}

func startHarnessOpts(t *testing.T, spec ReaperSpec, o harnessOpts) *harness {
	t.Helper()
	capturetest.SkipIfPrivileged(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	lease, e := TryLock(root, Holder{ServerPID: os.Getpid(), CaptureID: "cap/test"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = lease.Close() })

	h := &harness{t: t, root: root, lease: lease,
		perfLines: make(chan string, 256), workLines: make(chan string, 256)}
	if path, ok := strings.CutPrefix(spec.FaultPause, PauseBeforeExit); ok {
		h.exitArm = path
	}
	var reads, writes [4]*os.File
	for i := range reads {
		if reads[i], writes[i], err = os.Pipe(); err != nil {
			t.Fatal(err)
		}
	}
	files := ReaperFiles{Lock: lease.File(),
		PerfOut: writes[0], PerfErr: writes[1], WorkOut: writes[2], WorkErr: writes[3]}
	if o.noFiles {
		files = ReaperFiles{}
	}
	r, err := startReaper(self, spec, files, o.more)
	for _, w := range writes {
		_ = w.Close()
	}
	if err != nil {
		for _, rd := range reads {
			_ = rd.Close()
		}
		t.Fatal(err)
	}
	h.r = r
	h.reaper = refOf(t, r.PID())
	t.Cleanup(h.cleanup)
	go h.lines(reads[0], h.perfLines)
	go h.drain(reads[1])
	go h.lines(reads[2], h.workLines)
	go h.drain(reads[3])
	return h
}

func (h *harness) lines(f *os.File, out chan<- string) {
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		select {
		case out <- sc.Text():
		default:
		}
	}
}

func (h *harness) drain(f *os.File) {
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		h.mu.Lock()
		if h.stderr.Len() < 16<<10 {
			h.stderr.WriteString(sc.Text() + "\n")
		}
		h.mu.Unlock()
	}
}

// watch adds a process to the end-of-test leftover check.
func (h *harness) watch(p procRef) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen = append(h.seen, p)
}

// cleanup ends the reaper the way the server would, then fails the test
// for any process it saw that is still alive, killing it by verified PID.
func (h *harness) cleanup() {
	t := h.t
	if h.exitArm != "" {
		_ = os.Remove(h.exitArm)
	}
	h.r.Kill()
	select {
	case <-h.r.Done():
	case <-time.After(20 * time.Second):
		t.Errorf("reaper %d did not exit after Kill", h.reaper.pid)
	}
	h.mu.Lock()
	seen := append([]procRef{h.reaper}, h.seen...)
	h.mu.Unlock()
	for _, p := range seen {
		if !p.alive() {
			continue
		}
		t.Errorf("pid %d was left running", p.pid)
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
	if t.Failed() {
		h.mu.Lock()
		t.Logf("children's stderr:\n%s", h.stderr.String())
		h.mu.Unlock()
	}
}

func (h *harness) send(c Command) {
	h.t.Helper()
	if err := h.r.Send(c); err != nil {
		h.t.Fatalf("send %s: %v", c.Op, err)
	}
}

// next returns the next event, recording it and watching any child it
// names; ok is false when the stream ended.
func (h *harness) next(timeout time.Duration) (Event, bool) {
	h.t.Helper()
	select {
	case ev, ok := <-h.r.Events():
		if !ok {
			return Event{}, false
		}
		h.mu.Lock()
		h.events = append(h.events, ev)
		h.mu.Unlock()
		if (ev.Op == EventLaunched || ev.Op == EventAttached) && ev.PID > 0 {
			h.watch(procRef{ev.PID, ev.StartTime})
		}
		return ev, true
	case <-time.After(timeout):
		h.t.Fatalf("no event within %v; so far %+v", timeout, h.all())
	}
	return Event{}, false
}

// waitFor skips to the next event with op (and who, when set).
func (h *harness) waitFor(op, who string, timeout time.Duration) Event {
	h.t.Helper()
	end := time.Now().Add(timeout)
	for {
		ev, ok := h.next(max(time.Until(end), time.Millisecond))
		if !ok {
			h.t.Fatalf("event stream ended before %s %s; got %+v", op, who, h.all())
		}
		if ev.Op == EventError {
			h.t.Logf("error event: %+v", ev)
		}
		if ev.Op == op && (who == "" || ev.Who == who) {
			return ev
		}
	}
}

// rest reads the stream to its end.
func (h *harness) rest(timeout time.Duration) []Event {
	h.t.Helper()
	for {
		if _, ok := h.next(timeout); !ok {
			return h.all()
		}
	}
}

func (h *harness) all() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.events)
}

func (h *harness) waitDone(timeout time.Duration) {
	h.t.Helper()
	select {
	case <-h.r.Done():
	case <-time.After(timeout):
		h.t.Fatalf("reaper did not exit within %v", timeout)
	}
}

// line returns the next line on ch.
func (h *harness) line(ch <-chan string, timeout time.Duration) string {
	h.t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(timeout):
		h.t.Fatalf("no output line within %v", timeout)
	}
	return ""
}

// waitReady waits for the fake perf-agent's readiness line.
func (h *harness) waitReady() {
	h.t.Helper()
	for {
		if strings.HasPrefix(h.line(h.perfLines, 10*time.Second), ReadinessPrefix) {
			return
		}
	}
}

// grandchild reads the PID a double-fork-setsid workload prints and
// watches it.
func (h *harness) grandchild() procRef {
	h.t.Helper()
	l := h.line(h.workLines, 10*time.Second)
	pid, err := strconv.Atoi(strings.TrimSpace(l))
	if err != nil {
		h.t.Fatalf("workload printed %q, not a pid", l)
	}
	p := refOf(h.t, pid)
	h.watch(p)
	return p
}

// eventually polls cond every 10 ms until it holds or d passes.
func eventually(d time.Duration, cond func() bool) bool {
	for end := time.Now().Add(d); ; {
		if cond() {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// groupGone reports that no process is left in pgid.
func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

// fakeSpec stages a fake perf-agent and returns a spec for it.
func fakeSpec(t *testing.T, o capturetest.FakeOptions) ReaperSpec {
	t.Helper()
	fake := capturetest.FakePerfAgent(t, o)
	id, err := IdentityOf(fake)
	if err != nil {
		t.Fatal(err)
	}
	return ReaperSpec{PerfAgent: fake, PerfAgentID: id, Deadline: time.Minute, StopGrace: 500 * time.Millisecond}
}

// attach is the attach command for a fake perf-agent profiling for d.
func attach(spec ReaperSpec, d, staging string) Command {
	return Command{Op: OpAttach, Dir: staging, Argv: []string{spec.PerfAgent,
		"--pid", strconv.Itoa(os.Getpid()), "--duration", d,
		"--profile", "--profile-output", filepath.Join(staging, OutputFile(ModeCPU))}}
}

// workload launches capworkload with args.
func workload(t *testing.T, args ...string) *LaunchSpec {
	t.Helper()
	w := capturetest.Workload(t)
	return &LaunchSpec{Path: w, Args: append([]string{w}, args...),
		Env: []string{"PATH=/usr/bin:/bin", "LANG=C"}, Dir: t.TempDir()}
}

func ops(evs []Event) []string {
	var out []string
	for _, e := range evs {
		out = append(out, strings.TrimSuffix(e.Op+" "+e.Who, " "))
	}
	return out
}

func TestReaperRunsPerfAgentWithTheExactEnvironment(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := fakeSpec(t, capturetest.FakeOptions{Record: record})
	h := startHarness(t, spec)
	staging := t.TempDir()
	cmd := attach(spec, "200ms", staging)
	h.send(cmd)

	at := h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
	if at.PID <= 0 || at.PGID != at.PID || at.StartTime == 0 {
		t.Errorf("attached = %+v, want its own group and a start time", at)
	}
	ex := h.waitFor(EventExited, WhoPerfAgent, 10*time.Second)
	if ex.PID != at.PID || ex.Status != 0 || ex.Signal != "" {
		t.Errorf("exited = %+v, want status 0", ex)
	}
	rp := h.waitFor(EventReaped, "", 10*time.Second)
	if !CleanupIsComplete(rp.Cleanup) || len(rp.Survivors) != 0 {
		t.Errorf("reaped = %+v, want complete", rp)
	}
	if got, want := ops(h.rest(10*time.Second)), []string{"attached perf-agent", "exited perf-agent", "reaped"}; !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	h.waitDone(10 * time.Second)

	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var rec fakemode.Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.Env, PerfAgentEnv) {
		t.Errorf("env = %q, want exactly %q", rec.Env, PerfAgentEnv)
	}
	if !slices.Equal(rec.Argv, cmd.Argv) {
		t.Errorf("argv = %q, want %q", rec.Argv, cmd.Argv)
	}
	if rec.PID != at.PID || rec.PGID != at.PID {
		t.Errorf("pid %d pgid %d, want both %d", rec.PID, rec.PGID, at.PID)
	}
	wantDir, _ := filepath.EvalSymlinks(staging)
	if gotDir, _ := filepath.EvalSymlinks(rec.Cwd); gotDir != wantDir {
		t.Errorf("cwd = %q, want %q", rec.Cwd, staging)
	}
	if _, err := os.Stat(filepath.Join(staging, OutputFile(ModeCPU))); err != nil {
		t.Errorf("the fake wrote no profile: %v", err)
	}
}

// statFields returns /proc/<pid>/stat's fields after the comm: state,
// ppid, pgrp, session, tty_nr, ...
func statFields(t *testing.T, pid int) []string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
}

func TestReaperIsInItsOwnSession(t *testing.T) {
	h := startHarness(t, fakeSpec(t, capturetest.FakeOptions{}))
	f := statFields(t, h.r.PID())
	pid := strconv.Itoa(h.r.PID())
	if f[2] != pid || f[3] != pid {
		t.Errorf("reaper pgrp %s session %s, want both %s", f[2], f[3], pid)
	}
	if own := statFields(t, os.Getpid()); own[3] == f[3] {
		t.Errorf("reaper shares this process's session %s", own[3])
	}
	if f[4] != "0" {
		t.Errorf("reaper tty_nr = %s, want no controlling terminal", f[4])
	}
	h.r.Kill()
	if rp := h.waitFor(EventReaped, "", 10*time.Second); !CleanupIsComplete(rp.Cleanup) {
		t.Errorf("reaped = %+v", rp)
	}
	h.waitDone(10 * time.Second)
}

// statusField returns a /proc/<pid>/status field.
func statusField(t *testing.T, pid int, key string) string {
	t.Helper()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, key+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatalf("no %s in /proc/%d/status", key, pid)
	return ""
}

func TestTheLauncherSetsNoNewPrivsAfterExec(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(w string) []string
	}{
		{"direct", func(string) []string { return []string{"report"} }},
		{"exec chain", func(w string) []string { return []string{"exec", w, "report"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := fakeSpec(t, capturetest.FakeOptions{})
			spec.Launch = workload(t)
			spec.Launch.Args = append(spec.Launch.Args[:1], tc.args(spec.Launch.Path)...)
			spec.Launch.Env = append(spec.Launch.Env, "PERFMCP_LAUNCH_TEST=1")
			h := startHarness(t, spec)
			if got := statusField(t, h.r.PID(), "NoNewPrivs"); got != "0" {
				t.Errorf("the reaper's NoNewPrivs = %s; it must never set it on itself", got)
			}
			h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)

			var rep struct {
				NoNewPrivs int      `json:"nonewprivs"`
				TTYNr      int      `json:"tty_nr"`
				Env        []string `json:"env"`
			}
			if err := json.Unmarshal([]byte(h.line(h.workLines, 10*time.Second)), &rep); err != nil {
				t.Fatal(err)
			}
			if rep.NoNewPrivs != 1 {
				t.Errorf("workload NoNewPrivs = %d after exec, want 1", rep.NoNewPrivs)
			}
			if rep.TTYNr != 0 {
				t.Errorf("workload tty_nr = %d, want no controlling terminal", rep.TTYNr)
			}
			if want := slices.Sorted(slices.Values(spec.Launch.Env)); !slices.Equal(rep.Env, want) {
				t.Errorf("workload env = %q, want exactly %q", rep.Env, want)
			}
			if ex := h.waitFor(EventExited, WhoWorkload, 10*time.Second); ex.Status != 0 {
				t.Errorf("workload exited %+v", ex)
			}
			h.send(Command{Op: OpStop})
			if rp := h.waitFor(EventReaped, "", 10*time.Second); !CleanupIsComplete(rp.Cleanup) {
				t.Errorf("reaped = %+v", rp)
			}
			h.waitDone(10 * time.Second)
		})
	}
}

func TestReaperKillsOnEOF(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode capturetest.FakeMode
		stop bool
	}{
		{"start-up", capturetest.FakeNeverReady, false},
		{"profiling", capturetest.FakeNormal, false},
		{"finalisation", capturetest.FakeHangAfterSIGINT, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := fakeSpec(t, capturetest.FakeOptions{Mode: tc.mode})
			spec.Launch = workload(t, "sleep", "1m")
			h := startHarness(t, spec)
			wl := h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)
			h.send(attach(spec, "1m", t.TempDir()))
			at := h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
			if tc.mode != capturetest.FakeNeverReady {
				h.waitReady()
			}
			if tc.stop {
				h.send(Command{Op: OpStop})
			}

			closed := time.Now()
			h.r.closeCommands()
			rp := h.waitFor(EventReaped, "", spec.StopGrace+slack)
			if !CleanupIsComplete(rp.Cleanup) || len(rp.Survivors) != 0 {
				t.Errorf("reaped = %+v", rp)
			}
			if !groupGone(at.PGID) || !groupGone(wl.PGID) {
				t.Errorf("a group is left after reaped: perf-agent %v, workload %v", !groupGone(at.PGID), !groupGone(wl.PGID))
			}
			if d := time.Since(closed); d > spec.StopGrace+slack {
				t.Errorf("cleanup took %v", d)
			}
			for _, e := range h.rest(10 * time.Second) {
				if e.Op == EventExited && e.Signal != "killed" {
					t.Errorf("exited %+v, want killed", e)
				}
			}
			h.waitDone(10 * time.Second)
		})
	}
}

func TestReaperKillsAtTheAbsoluteDeadline(t *testing.T) {
	spec := fakeSpec(t, capturetest.FakeOptions{})
	spec.Deadline = 500 * time.Millisecond
	spec.Launch = workload(t, "sleep", "1m")
	began := time.Now()
	h := startHarness(t, spec)
	wl := h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)
	h.send(attach(spec, "1m", t.TempDir()))
	at := h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
	rp := h.waitFor(EventReaped, "", spec.Deadline+spec.StopGrace+slack)
	if d := time.Since(began); d < spec.Deadline {
		t.Errorf("reaped after %v, before the deadline %v", d, spec.Deadline)
	}
	if !CleanupIsComplete(rp.Cleanup) {
		t.Errorf("reaped = %+v", rp)
	}
	if !groupGone(at.PGID) || !groupGone(wl.PGID) {
		t.Error("a group survived the deadline")
	}
	h.waitDone(10 * time.Second)
}

func TestReaperCancelIsSIGINTThenSIGKILL(t *testing.T) {
	spec := fakeSpec(t, capturetest.FakeOptions{Mode: capturetest.FakeIgnoreSIGINT})
	spec.StopGrace = 400 * time.Millisecond
	h := startHarness(t, spec)
	h.send(attach(spec, "1m", t.TempDir()))
	h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
	h.waitReady()
	sent := time.Now()
	h.send(Command{Op: OpCancel})
	ex := h.waitFor(EventExited, WhoPerfAgent, spec.StopGrace+slack)
	if ex.Signal != "killed" {
		t.Errorf("exited = %+v, want killed", ex)
	}
	// SIGKILL is sent only once the grace has passed.
	if d := time.Since(sent); d < spec.StopGrace {
		t.Errorf("killed after %v, before the stop grace %v", d, spec.StopGrace)
	}
	h.waitFor(EventReaped, "", 10*time.Second)
	h.waitDone(10 * time.Second)
}

func TestReaperEndsTheWorkloadAfterPerfAgent(t *testing.T) {
	for _, tc := range []struct {
		mode, signal string
	}{
		{"sleep", "terminated"},
		{"ignore-sigterm", "killed"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			spec := fakeSpec(t, capturetest.FakeOptions{})
			spec.Launch = workload(t, tc.mode, "1m")
			h := startHarness(t, spec)
			h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)
			h.send(attach(spec, "300ms", t.TempDir()))
			rp := h.waitFor(EventReaped, "", 10*time.Second+spec.StopGrace)
			if !CleanupIsComplete(rp.Cleanup) {
				t.Errorf("reaped = %+v", rp)
			}
			got := ops(h.all())
			if want := []string{"launched workload", "attached perf-agent", "exited perf-agent", "exited workload", "reaped"}; !slices.Equal(got, want) {
				t.Errorf("events = %v, want %v", got, want)
			}
			for _, e := range h.all() {
				if e.Op == EventExited && e.Who == WhoPerfAgent && (e.Status != 0 || e.Signal != "") {
					t.Errorf("perf-agent %+v, want status 0", e)
				}
				if e.Op == EventExited && e.Who == WhoWorkload && e.Signal != tc.signal {
					t.Errorf("workload %+v, want %s", e, tc.signal)
				}
			}
			h.waitDone(10 * time.Second)
		})
	}
}

func TestReaperSweepsSetsidGrandchildren(t *testing.T) {
	for _, pause := range []string{"", PauseNoSubreaper} {
		t.Run(cmp.Or(pause, "subreaper"), func(t *testing.T) {
			spec := fakeSpec(t, capturetest.FakeOptions{})
			spec.Launch = workload(t, "double-fork-setsid", "1m")
			spec.FaultPause = pause
			h := startHarness(t, spec)
			h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)
			gc := h.grandchild()
			h.send(attach(spec, "300ms", t.TempDir()))
			rp := h.waitFor(EventReaped, "", 10*time.Second+2*spec.StopGrace)
			h.waitDone(10 * time.Second)
			if pause == "" {
				if !CleanupIsComplete(rp.Cleanup) || gc.alive() {
					t.Errorf("reaped = %+v; grandchild alive %v", rp, gc.alive())
				}
				return
			}
			if !strings.HasPrefix(rp.Cleanup, "incomplete: ") || !strings.Contains(rp.Cleanup, "process group only") {
				t.Errorf("cleanup = %q, want it to name process group only", rp.Cleanup)
			}
			// Without the subreaper the setsid grandchild is out of reach,
			// which is why the sweep exists.
			if !gc.alive() {
				t.Error("the grandchild died without the subreaper; the test proves nothing")
			}
			_ = syscall.Kill(gc.pid, syscall.SIGKILL)
			if !eventually(5*time.Second, func() bool { return !gc.alive() }) {
				t.Error("grandchild survived SIGKILL")
			}
		})
	}
}

func TestKillLeavesTheReaperToCleanUp(t *testing.T) {
	arm := filepath.Join(t.TempDir(), "hold")
	if err := os.WriteFile(arm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	spec := fakeSpec(t, capturetest.FakeOptions{})
	spec.StopGrace = time.Second
	spec.Launch = workload(t, "double-fork-setsid", "1m")
	spec.FaultPause = PauseBeforeExit + arm
	h := startHarness(t, spec)
	h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)
	gc := h.grandchild()
	h.send(attach(spec, "1m", t.TempDir()))
	h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
	h.waitReady()

	killed := time.Now()
	h.r.Kill()
	rp := h.waitFor(EventReaped, "", spec.StopGrace+slack)
	if d := time.Since(killed); d > spec.StopGrace+500*time.Millisecond {
		t.Logf("reaped %v after Kill (StopGrace %v)", d, spec.StopGrace)
	}
	if !CleanupIsComplete(rp.Cleanup) || len(rp.Survivors) != 0 {
		t.Errorf("reaped = %+v", rp)
	}
	if gc.alive() {
		t.Error("the setsid grandchild survived Kill: the reaper did not sweep")
	}
	// The before-exit barrier holds the reaper after cleanup: Kill left it
	// alive to do the cleanup, and it still holds the lock.
	if !h.reaper.alive() {
		t.Fatal("the reaper is gone; Kill must never kill it")
	}
	if _, e := TryLock(h.root, Holder{}); e == nil || e.Code != CodeBusy {
		t.Errorf("TryLock while the reaper holds the lock = %v, want busy", e)
	}
	if err := os.Remove(arm); err != nil {
		t.Fatal(err)
	}
	h.waitDone(10 * time.Second)
}

// reaperChild finds the reaper's child whose comm is comm.
func reaperChild(t *testing.T, reaper int, comm string) procRef {
	t.Helper()
	var found procRef
	ok := eventually(10*time.Second, func() bool {
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			s, err := readStat(DefaultProc(), pid)
			if err == nil && s.ppid == reaper && s.comm == comm && !s.exited() {
				found = procRef{pid, s.start}
				return true
			}
		}
		return false
	})
	if !ok {
		t.Fatalf("no %s child of the reaper %d", comm, reaper)
	}
	return found
}

// unreported starts a capture whose launch or attach the fault pause holds
// back, finds the child anyway, ends the reaper with end, and checks the
// child is gone and nothing was ever published about it.
func unreported(t *testing.T, pause string, end func(h *harness)) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := fakeSpec(t, capturetest.FakeOptions{Record: record})
	spec.FaultPause = pause
	var who, held string
	if pause == PauseAfterLauncherFork {
		spec.Launch = workload(t, "sleep", "1m")
		who, held = WhoWorkload, EventLaunched
	} else {
		who, held = WhoPerfAgent, EventAttached
	}
	h := startHarness(t, spec)
	var c procRef
	if who == WhoWorkload {
		c = reaperChild(t, h.r.PID(), "capworkload")
	} else {
		h.send(attach(spec, "1m", t.TempDir()))
		var rec fakemode.Record
		if !eventually(10*time.Second, func() bool {
			b, err := os.ReadFile(record)
			return err == nil && json.Unmarshal(b, &rec) == nil
		}) {
			t.Fatal("the fake never started")
		}
		c = refOf(t, rec.PID)
	}
	h.watch(c)

	ended := time.Now()
	end(h)
	evs := h.rest(spec.StopGrace + slack)
	if !eventually(spec.StopGrace+slack-time.Since(ended), func() bool { return !c.alive() }) {
		t.Errorf("%s pid %d survived", who, c.pid)
	}
	h.waitDone(10 * time.Second)
	for _, e := range evs {
		if e.Who == who || e.Op == held {
			t.Errorf("published %+v despite the %s pause", e, pause)
		}
	}
	if got := ops(evs); len(got) == 0 || got[len(got)-1] != EventReaped {
		t.Errorf("events = %v, want reaped last", got)
	}
}

func TestKillDuringAnUnreportedFork(t *testing.T) {
	for _, pause := range []string{PauseAfterPerfAgentFork, PauseAfterLauncherFork} {
		t.Run(pause, func(t *testing.T) { unreported(t, pause, func(h *harness) { h.r.Kill() }) })
	}
}

func TestReaperForkReportGapStillKillsOnEOF(t *testing.T) {
	for _, pause := range []string{PauseAfterPerfAgentFork, PauseAfterLauncherFork} {
		t.Run(pause, func(t *testing.T) { unreported(t, pause, func(h *harness) { h.r.closeCommands() }) })
	}
}

// selfCgroup returns this process's cgroup v2 path relative to the mount.
func selfCgroup(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skip(err)
	}
	return b
}

const cgroupMount = "/sys/fs/cgroup"

func TestReaperKillsTheCgroup(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	parent, why := DelegatedCgroup(selfCgroup(t), cgroupMount)
	if why != "" {
		t.Skip("no delegated cgroup: " + why)
	}
	cg, err := CreateCgroup(parent, "perf-agent-mcp-test-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err != nil {
		t.Fatal(err)
	}
	// The test's own cgroup: whatever a failure left in it is killed, so
	// the directory can go.
	t.Cleanup(func() {
		if _, err := os.Stat(cg); err != nil {
			return
		}
		_ = KillCgroup(cg)
		eventually(5*time.Second, func() bool { p, err := cgroupPopulated(cg); return err != nil || !p })
		if err := RemoveCgroup(cg); err != nil {
			t.Errorf("remove the test cgroup: %v", err)
		}
	})

	spec := fakeSpec(t, capturetest.FakeOptions{})
	spec.Cgroup = cg
	// Without the subreaper only the cgroup can reach the setsid grandchild.
	spec.FaultPause = PauseNoSubreaper
	spec.Launch = workload(t, "double-fork-setsid", "1m")
	h := startHarness(t, spec)
	if l := h.waitFor(EventLaunched, WhoWorkload, 10*time.Second); !l.Cgroup {
		t.Fatalf("launched = %+v, want it in the cgroup", l)
	}
	gc := h.grandchild()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(gc.pid) + "/cgroup")
	if err != nil || !strings.Contains(string(b), strings.TrimPrefix(cg, cgroupMount)) {
		t.Fatalf("grandchild cgroup = %q (%v), want %s", b, err, cg)
	}
	h.send(attach(spec, "300ms", t.TempDir()))
	rp := h.waitFor(EventReaped, "", 10*time.Second+spec.StopGrace)
	if !CleanupIsComplete(rp.Cleanup) || len(rp.Survivors) != 0 {
		t.Errorf("reaped = %+v", rp)
	}
	if gc.alive() {
		t.Error("the grandchild survived cgroup.kill")
	}
	h.waitDone(10 * time.Second)
	if _, err := os.Stat(cg); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cgroup %s still exists after the reaper exited (%v)", cg, err)
	}
}

func TestACgroupStartFailureFallsBack(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	rel, ok := strings.CutPrefix(strings.TrimSpace(string(selfCgroup(t))), "0::")
	if !ok {
		t.Skip("no cgroup v2")
	}
	// The nearest ancestor this user may not move processes into: starting
	// a child in it fails with EACCES.
	dir := filepath.Join(cgroupMount, rel)
	for syscall.Access(filepath.Join(dir, "cgroup.procs"), wOK) == nil {
		if dir == cgroupMount {
			t.Skip("every cgroup up to the root is writable")
		}
		dir = filepath.Dir(dir)
	}
	spec := fakeSpec(t, capturetest.FakeOptions{})
	spec.Cgroup = dir
	spec.Launch = workload(t, "sleep", "1m")
	h := startHarness(t, spec)
	if l := h.waitFor(EventLaunched, WhoWorkload, 10*time.Second); l.Cgroup {
		t.Fatalf("launched = %+v, want the fallback outside %s", l, dir)
	}
	h.send(attach(spec, "200ms", t.TempDir()))
	h.waitFor(EventExited, WhoPerfAgent, 10*time.Second)
	rp := h.waitFor(EventReaped, "", 10*time.Second+spec.StopGrace)
	if !CleanupIsComplete(rp.Cleanup) {
		t.Errorf("reaped = %+v", rp)
	}
	h.waitDone(10 * time.Second)
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the reaper removed a cgroup it never used: %v", err)
	}
}

func TestTheLockIsReleasedOnlyAfterReaping(t *testing.T) {
	busy := func(h *harness) bool {
		l, e := TryLock(h.root, Holder{ServerPID: os.Getpid()})
		if e == nil {
			_ = l.Close()
			return false
		}
		return e.Code == CodeBusy
	}
	t.Run("no pause", func(t *testing.T) {
		spec := fakeSpec(t, capturetest.FakeOptions{})
		h := startHarness(t, spec)
		_ = h.lease.Close()
		h.send(attach(spec, "300ms", t.TempDir()))
		h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
		if !busy(h) {
			t.Error("the lock is free while perf-agent runs")
		}
		h.waitFor(EventReaped, "", 10*time.Second)
		h.waitDone(10 * time.Second)
		if busy(h) {
			t.Error("the lock is still held after the reaper exited")
		}
	})
	t.Run("before-exit", func(t *testing.T) {
		arm := filepath.Join(t.TempDir(), "hold")
		if err := os.WriteFile(arm, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		spec := fakeSpec(t, capturetest.FakeOptions{})
		spec.FaultPause = PauseBeforeExit + arm
		h := startHarness(t, spec)
		_ = h.lease.Close()
		h.send(attach(spec, "200ms", t.TempDir()))
		h.waitFor(EventReaped, "", 10*time.Second)
		if !busy(h) {
			t.Error("the lock was released before the before-exit barrier")
		}
		if err := os.Remove(arm); err != nil {
			t.Fatal(err)
		}
		h.waitDone(10 * time.Second)
		if busy(h) {
			t.Error("the lock is still held after the reaper exited")
		}
	})
}

func TestReaperRefusesAChangedPerfAgent(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record.json")
	spec := fakeSpec(t, capturetest.FakeOptions{Record: record})
	h := startHarness(t, spec)
	// A chmod moves the ctime, so the identity no longer matches.
	if err := os.Chmod(spec.PerfAgent, 0o700); err != nil {
		t.Fatal(err)
	}
	h.send(attach(spec, "200ms", t.TempDir()))
	ev := h.waitFor(EventError, WhoPerfAgent, 10*time.Second)
	if !strings.Contains(ev.Err, string(CodeBinaryChanged)) {
		t.Errorf("error = %q, want binary_changed", ev.Err)
	}
	h.send(Command{Op: OpStop})
	h.waitFor(EventReaped, "", 10*time.Second)
	h.waitDone(10 * time.Second)
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Error("the changed binary was started")
	}
	for _, e := range h.all() {
		if e.Op == EventAttached {
			t.Errorf("published %+v", e)
		}
	}
}

func TestTheMountRecheckIgnoresNosuidOnlyWithoutCapabilities(t *testing.T) {
	if err := mountFlagsError("/p", syscall.MS_NOSUID, true); err == nil {
		t.Error("nosuid accepted for a binary with file capabilities")
	}
	if err := mountFlagsError("/p", syscall.MS_NOSUID, false); err != nil {
		t.Errorf("nosuid refused for a binary without file capabilities: %v", err)
	}
	if err := mountFlagsError("/p", syscall.MS_NOEXEC, false); err == nil {
		t.Error("noexec accepted")
	}
}

func TestRunInternalIgnoresAnUnsetMode(t *testing.T) {
	for _, v := range []string{"", "bogus", "Reaper"} {
		t.Setenv(InternalEnv, v)
		if code, ok := RunInternal(); ok || code != 0 {
			t.Errorf("RunInternal with %s=%q = (%d, %v), want (0, false)", InternalEnv, v, code, ok)
		}
	}
	if err := os.Unsetenv(InternalEnv); err != nil {
		t.Fatal(err)
	}
	if _, ok := RunInternal(); ok {
		t.Error("RunInternal ran with the mode unset")
	}
}

func TestStartReaperRefusesABadSpec(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StartReaper(self, ReaperSpec{StopGrace: time.Second}, ReaperFiles{}); err == nil {
		t.Error("a reaper without a deadline was started")
	}
	if _, err := StartReaper(filepath.Base(self), ReaperSpec{Deadline: time.Minute}, ReaperFiles{}); err == nil {
		t.Error("a reaper was started by a relative path")
	}
}

func TestReaperRefusesARelativePath(t *testing.T) {
	spec := fakeSpec(t, capturetest.FakeOptions{})
	spec.PerfAgent = filepath.Base(spec.PerfAgent)
	spec.Launch = workload(t, "sleep", "1m")
	spec.Launch.Path = filepath.Base(spec.Launch.Path)
	h := startHarness(t, spec)
	if ev := h.waitFor(EventError, WhoWorkload, 10*time.Second); !strings.Contains(ev.Err, "not absolute") {
		t.Errorf("error = %+v", ev)
	}
	h.send(attach(spec, "200ms", t.TempDir()))
	if ev := h.waitFor(EventError, WhoPerfAgent, 10*time.Second); !strings.Contains(ev.Err, "not absolute") {
		t.Errorf("error = %+v", ev)
	}
	h.send(Command{Op: OpStop})
	h.waitFor(EventReaped, "", 10*time.Second)
	h.waitDone(10 * time.Second)
}

// A reaper started without output pipes or a lock gets the null device in
// those slots, never a descriptor its own runtime opened there.
func TestReaperWithoutFilesUsesTheNullDevice(t *testing.T) {
	spec := fakeSpec(t, capturetest.FakeOptions{})
	spec.Launch = workload(t, "sleep", "1m")
	h := startHarnessOpts(t, spec, harnessOpts{noFiles: true})
	h.waitFor(EventLaunched, WhoWorkload, 10*time.Second)
	// Each empty slot is the null device, so the reaper's runtime could
	// not open its own files (cpu.max, an eventfd) there and have them
	// taken for the lock or a child's stdout.
	for fd := lockFD; fd <= workErrFD; fd++ {
		l, err := os.Readlink("/proc/" + strconv.Itoa(h.r.PID()) + "/fd/" + strconv.Itoa(fd))
		if err != nil || l != os.DevNull {
			t.Errorf("reaper fd %d = %q (%v), want %s", fd, l, err, os.DevNull)
		}
	}
	h.send(attach(spec, "300ms", t.TempDir()))
	h.waitFor(EventAttached, WhoPerfAgent, 10*time.Second)
	rp := h.waitFor(EventReaped, "", 10*time.Second+spec.StopGrace)
	if !CleanupIsComplete(rp.Cleanup) || len(rp.Survivors) != 0 {
		t.Errorf("reaped = %+v", rp)
	}
	h.waitDone(10 * time.Second)
	for _, e := range h.all() {
		if e.Op == EventError {
			t.Errorf("error event %+v", e)
		}
	}
}

// A descriptor above fd 10 that leaked into the reaper is closed there.
func TestReaperClosesStrayDescriptors(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	h := startHarnessOpts(t, fakeSpec(t, capturetest.FakeOptions{}), harnessOpts{more: []*os.File{w}})
	_ = w.Close()
	// Only the reaper still has the write end; EOF means it closed it.
	_ = r.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Errorf("read = %v, want EOF: the reaper kept fd 11", err)
	}
	if !h.reaper.alive() {
		t.Error("the reaper exited; the test proves nothing")
	}
}
