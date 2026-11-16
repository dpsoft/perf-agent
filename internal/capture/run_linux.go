package capture

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// ringFloor is the least each output ring keeps; a ring is otherwise
	// eight tails long.
	ringFloor = 16 << 10
	// maxLogLine and maxLoggedLines bound what one output stream sends to
	// the debug log, so a flood never waits on the server's stderr.
	maxLogLine     = 1 << 10
	maxLoggedLines = 1000
	// readerDrain bounds the wait for the output readers' EOF at the end;
	// a survivor holding a pipe open must not hold Wait.
	readerDrain = 500 * time.Millisecond
	// reaperGone bounds the wait for the event stream to end once the
	// reaper has exited.
	reaperGone = 2 * time.Second
	// shebangRead is how much of a command is read for a #! line.
	shebangRead = 256
	// unresponsiveSlack is how much longer than StopGrace and
	// FinishTimeout a killed capture's reaper gets to report.
	unresponsiveSlack = 5 * time.Second
	// lateReadiness bounds the wait, at perf-agent's exit, for a readiness
	// line still in the stdout pipe.
	lateReadiness = 200 * time.Millisecond
)

// Run is the server's side of one capture: it asks the reaper to start
// the children, watches perf-agent's output for readiness, enforces the
// bounds, rechecks the target, stops early when the target goes, and maps
// how it all ended into an Outcome. The reaper does every signal; Run only
// sends it commands, or closes its command pipe.
type Run struct {
	spec   RunSpec
	b      Bounds
	log    *slog.Logger
	reaper *Reaper

	phase atomic.Pointer[string]
	// killAt is when Kill was first called; killCh wakes the supervisor.
	killAt     atomic.Pointer[time.Time]
	killCh     chan struct{}
	wakeKill   func()
	cancel     chan struct{}
	cancelOnce func()
	done       chan struct{}
	outcome    Outcome

	perfOut, perfErr, workOut, workErr *stream
	ready                              chan time.Time

	// The rest belongs to the supervising goroutine.
	o Outcome
	// evc is the reaper's event stream, nil once it ended or reaped came;
	// reaped is that event.
	evc    <-chan Event
	reaped *Event
	// cgroup is the command's own cgroup, when Start made one.
	cgroup string
	// killNoticed says a Run.Kill was taken up; unresponsiveT bounds the
	// wait for the reaper's report after any kill, and unresponsive says
	// it expired.
	killNoticed       bool
	unresponsiveT     *time.Timer
	unresponsive      bool
	launchers, cmdIDs []fileID
	argvPID           int
	target            TargetState
	targetKnown       bool
	work, perf        instance
	workExited        bool
	// targetGone says the target was seen gone; goneAtExit that it was
	// gone when perf-agent exited, which explains a perf-agent failure.
	targetIsGone, goneAtExit bool
	perfExited               bool
	perfStatus               int
	perfSignal               string
	recheckNoted             bool
	attachFrom               time.Time
	readyAt                  time.Time
	stopped                  bool
	stopSent                 bool
	killSent                 bool
	cancelSent               bool
	finish                   *time.Timer
	durationT                *time.Timer
	recheckT                 *time.Ticker
}

// instance is a process by PID and start time.
type instance struct {
	pid   int
	start uint64
}

// fileID is a file by device and inode.
type fileID struct{ dev, ino uint64 }

func statID(path string) (fileID, bool) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return fileID{}, false
	}
	return fileID{uint64(st.Dev), st.Ino}, true
}

// Start checks spec, runs the preflight of a PID target (and, for gpu, that
// it mapped the shim), creates a command's cgroup when the parent allows
// it, and starts the reaper. A failure here started nothing. The capture
// then runs in its own goroutine; Wait returns how it ended.
func Start(spec RunSpec) (*Run, *Error) {
	if e := checkRunSpec(spec); e != nil {
		return nil, e
	}
	r := &Run{
		spec:   spec,
		b:      spec.Bounds,
		log:    spec.Log,
		cancel: make(chan struct{}),
		killCh: make(chan struct{}),
		done:   make(chan struct{}),
		ready:  make(chan time.Time, 1),
		o:      Outcome{PerfAgentExit: -1, Started: time.Now()},
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	r.cancelOnce = sync.OnceFunc(func() { close(r.cancel) })
	r.wakeKill = sync.OnceFunc(func() { close(r.killCh) })
	r.setPhase(PhaseAttaching)
	if spec.PID > 0 {
		st, e := Preflight(spec.Proc, spec.PID, spec.SelfID)
		if e != nil {
			return nil, e
		}
		if slices.Contains(spec.Settings.Modes, ModeGPU) {
			if e := CheckShimMapped(spec.Proc, spec.PID, *spec.GPU); e != nil {
				return nil, e
			}
		}
		r.target, r.targetKnown, r.o.Target = st, true, st
	}
	rspec := ReaperSpec{
		PerfAgent:   spec.Binary.Path,
		PerfAgentID: spec.Binary.ID,
		Deadline:    spec.Bounds.StartTimeout + spec.Settings.Duration + spec.Bounds.FinishTimeout + spec.Bounds.StopGrace,
		StopGrace:   spec.Bounds.StopGrace,
		FaultPause:  spec.FaultPause,
	}
	if spec.Launch != nil {
		r.setPhase(PhaseLaunching)
		r.o.CommandPath, r.o.ArgCount = spec.Launch.Path, len(spec.Launch.Args)
		r.launchers = launcherIDs(spec.Self)
		r.cmdIDs = commandIDs(spec.Launch.Path)
		rspec.Launch = spec.Launch
		if spec.Cgroup != "" {
			cg, err := CreateCgroup(spec.Cgroup, cgroupName(spec.CaptureID))
			if err != nil {
				r.o.Notes = append(r.o.Notes, fmt.Sprintf("no cgroup: creating the capture's cgroup under the delegated cgroup failed (%v); the subreaper sweep was used", pathErr(err)))
			}
			rspec.Cgroup, r.cgroup = cg, cg
		}
	}
	if e := r.startReaper(rspec); e != nil {
		if rspec.Cgroup != "" {
			_ = RemoveCgroup(rspec.Cgroup)
		}
		return nil, e
	}
	go r.supervise()
	return r, nil
}

// checkRunSpec refuses a spec Start cannot run.
func checkRunSpec(s RunSpec) *Error {
	bad := func(format string, args ...any) *Error {
		return Errorf(CodeInvalidArgument, "", format, args...)
	}
	b := s.Bounds
	switch {
	case !filepath.IsAbs(s.Self):
		return bad("the server's own path %q is not absolute", s.Self)
	case (s.PID > 0) == (s.Launch != nil):
		return bad("a capture needs exactly one target: a pid or a command")
	case s.PID < 0:
		return bad("pid %d is not a process id", s.PID)
	case s.Launch != nil && !filepath.IsAbs(s.Launch.Path):
		return bad("command path %q is not absolute", echo(s.Launch.Path))
	case s.Staging == nil || !filepath.IsAbs(s.StagingPath):
		return bad("a capture needs its open staging directory and its absolute path")
	case s.CaptureID == "":
		return bad("a capture needs an id")
	case s.Settings.Duration <= 0:
		return bad("duration %v is not positive", s.Settings.Duration)
	case slices.Contains(s.Settings.Modes, ModeGPU) && s.GPU == nil:
		return Errorf(CodeInvalidArgument, "start perf-agent-mcp with --gpu-shim PATH", "a gpu capture needs the CUPTI adapter")
	case b.StartTimeout <= 0 || b.FinishTimeout <= 0 || b.StopGrace <= 0 || b.MaxOutputTail <= 0 ||
		b.MaxProfileBytes <= 0 || b.MaxExeclogBytes <= 0 || b.Recheck <= 0 || b.Poll <= 0:
		return bad("every capture bound must be positive: %+v", b)
	}
	if _, err := BuildArgv(argvSpec(s, 1)); err != nil {
		if e, ok := errors.AsType[*Error](err); ok {
			return e
		}
		return bad("%v", err)
	}
	return nil
}

func argvSpec(s RunSpec, pid int) ArgvSpec {
	a := ArgvSpec{Binary: s.Binary.Path, PID: pid, Settings: s.Settings, Staging: s.StagingPath, CaptureID: s.CaptureID}
	if s.GPU != nil {
		a.GPUShim = s.GPU.Path
	}
	return a
}

// cgroupName is the capture's own cgroup: the prefix, the capture id made
// one safe path element, this server's pid and a nonce.
func cgroupName(id string) string {
	safe := strings.Map(func(c rune) rune {
		if c < utf8.RuneSelf && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return c
		}
		return '-'
	}, cut(id, maxEcho))
	return CgroupPrefix + safe + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// launcherIDs are the files the launcher runs from: the server's binary as
// named, and as this process runs it.
func launcherIDs(self string) []fileID {
	var ids []fileID
	for _, p := range []string{self, "/proc/self/exe"} {
		if id, ok := statID(p); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// commandIDs are the files a launched command's exe may be once it runs:
// the command, or the interpreter its #! line names.
func commandIDs(path string) []fileID {
	var ids []fileID
	if id, ok := statID(path); ok {
		ids = append(ids, id)
	}
	f, err := os.Open(path)
	if err != nil {
		return ids
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, shebangRead)
	n, _ := io.ReadFull(f, head)
	line, _, _ := bytes.Cut(head[:n], []byte("\n"))
	if rest, ok := bytes.CutPrefix(line, []byte("#!")); ok {
		if fields := strings.Fields(string(rest)); len(fields) > 0 {
			if id, ok := statID(fields[0]); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// startReaper starts the reaper with the output pipes and the lock, and
// the readers of the pipes.
func (r *Run) startReaper(rs ReaperSpec) *Error {
	var reads, writes [4]*os.File
	closeAll := func(fs []*os.File) {
		for _, f := range fs {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	for i := range reads {
		var err error
		if reads[i], writes[i], err = os.Pipe(); err != nil {
			closeAll(reads[:])
			closeAll(writes[:])
			return Errorf(CodePerfAgentUnstartable, "", "create the capture's pipes: %v", pathErr(err))
		}
	}
	files := ReaperFiles{Lock: r.spec.Lock, PerfOut: writes[0], PerfErr: writes[1]}
	if rs.Launch != nil {
		files.WorkOut, files.WorkErr = writes[2], writes[3]
	}
	reaper, err := StartReaper(r.spec.Self, rs, files)
	closeAll(writes[:])
	if err != nil {
		closeAll(reads[:])
		return Errorf(CodePerfAgentUnstartable, "check that perf-agent-mcp's own binary is intact", "start the capture's reaper (perf-agent-mcp's own binary): %v", pathErr(err))
	}
	r.reaper = reaper
	r.evc = reaper.Events()
	if r.spec.Events != nil {
		r.evc = r.spec.Events(r.evc)
	}
	size := max(ringFloor, 8*r.b.MaxOutputTail)
	r.perfOut = r.read(reads[0], size, "perf-agent stdout", r.readiness)
	r.perfErr = r.read(reads[1], size, "perf-agent stderr", nil)
	r.workOut = r.read(reads[2], size, "workload stdout", nil)
	r.workErr = r.read(reads[3], size, "workload stderr", nil)
	return nil
}

// readiness passes the first readiness line's arrival time on.
func (r *Run) readiness(line []byte) bool {
	if !bytes.HasPrefix(line, []byte(ReadinessPrefix)) {
		return false
	}
	r.ready <- time.Now()
	return true
}

// Phase is where the capture is: launching, attaching, profiling,
// finishing or done.
func (r *Run) Phase() string { return *r.phase.Load() }

func (r *Run) setPhase(p string) { r.phase.Store(&p) }

// Cancel asks the reaper to cancel: SIGINT to perf-agent, then SIGKILL
// after StopGrace, and the capture is discarded. It never blocks, and is
// safe from any goroutine and more than once.
func (r *Run) Cancel() { r.cancelOnce() }

// Kill is the emergency end: it closes the reaper's command pipe, which
// the reaper answers by killing both children and sweeping, as on a dead
// server. The capture is then cancelled, stopped when Kill was called,
// whatever order the reaper's exits arrive in. It never blocks, and is
// safe from any goroutine and more than once.
func (r *Run) Kill() {
	now := time.Now()
	r.killAt.CompareAndSwap(nil, &now)
	r.reaper.Kill()
	r.wakeKill()
}

// Reaper is the capture's reaper by its PID and start time, which the
// server records in the capture's intent and the lock file, so recovery
// and a busy answer can tell the capture's owner still runs. It is read
// once the capture has started, while the reaper is this process's
// unreaped child; the start time is 0 when it cannot be read.
func (r *Run) Reaper() (pid int, start uint64) {
	pid = r.reaper.PID()
	start, _ = StartTime(r.spec.Proc, pid)
	return pid, start
}

// Wait blocks until the reaper reports its cleanup, or exits without
// doing so, or does not answer a kill in time, and returns how the
// capture ended. The captures lock may stay held by the reaper after Wait
// returns, while a child it could not reap lives; it is free once the
// reaper exits.
func (r *Run) Wait() Outcome {
	<-r.done
	return r.outcome
}

// fail records the capture's first failure; later ones are logged.
func (r *Run) fail(e *Error) {
	if r.o.Err == nil {
		r.o.Err = e
		return
	}
	r.log.Debug("capture: a further failure", "capture", r.spec.CaptureID, "err", e.Error())
}

// send writes a command to the reaper; a command that cannot be sent is
// answered by closing the pipe, which ends the capture.
func (r *Run) send(c Command) {
	if err := r.reaper.Send(c); err != nil {
		r.log.Debug("capture: the reaper took no command", "capture", r.spec.CaptureID, "op", c.Op, "err", err)
		r.reaper.Kill()
	}
}

// stop asks perf-agent to stop, once, while it may still be running.
func (r *Run) stop() {
	if r.stopSent || r.perfExited {
		return
	}
	r.stopSent = true
	r.send(Command{Op: OpStop})
}

// earlyStop ends profiling before the duration: the stop point is now.
func (r *Run) earlyStop(by string) {
	r.markStopped(by, time.Now())
	r.stop()
}

// failStop is a stop for a failure: the error is the cause, so StoppedBy
// stays empty, and the stop point is now.
func (r *Run) failStop(e *Error) {
	r.markStopped("", time.Now())
	r.fail(e)
	r.stop()
}

// markStopped records the stop, once: the first stop wins. by is one of
// the StoppedBy values, or empty for a failure. at is when it happened:
// Profiled runs to it from the readiness line, and FinishTimeout counts
// from it.
func (r *Run) markStopped(by string, at time.Time) {
	if r.stopped {
		return
	}
	r.stopped = true
	r.o.StoppedBy = by
	if !r.readyAt.IsZero() && at.After(r.readyAt) {
		r.o.Profiled = at.Sub(r.readyAt)
	}
	r.setPhase(PhaseFinishing)
	if r.durationT != nil {
		r.durationT.Stop()
	}
	if r.recheckT != nil {
		r.recheckT.Stop()
	}
	r.finish = time.NewTimer(max(0, r.b.FinishTimeout-time.Since(at)))
}

// goneFirst says the target ended before perf-agent did. A launched
// command is judged only by its exit event: the reaper publishes exits in
// the order it reaps them, and it ends the command itself once perf-agent
// is done, so a dead command found in /proc may be the reaper's doing. A
// PID target is nobody's here, so /proc is the truth.
func (r *Run) goneFirst() bool {
	if r.spec.Launch != nil {
		return r.workExited
	}
	return r.targetIsGone || r.pidGone()
}

// ranItsCourse is the stop of a perf-agent left to its duration: the
// duration stopped it, unless the target was already gone, which then did.
// It profiled at most its duration, whatever the delay in hearing of it.
func (r *Run) ranItsCourse(gone bool, at time.Time) {
	if end := r.readyAt.Add(r.spec.Settings.Duration); at.After(end) {
		at = end
	}
	by := StoppedByDuration
	switch {
	case gone && r.spec.Launch != nil:
		by = StoppedByCommandExit
	case gone:
		by = StoppedByTargetExit
	}
	r.markStopped(by, at)
}

// kill ends the capture through the reaper for a failed bound, once. The
// error is the cause, so StoppedBy stays empty.
func (r *Run) kill(e *Error) {
	if r.killSent {
		return
	}
	r.killSent = true
	now := time.Now()
	r.markStopped("", now)
	r.fail(e)
	r.reaper.Kill()
	r.armUnresponsive(now)
}

// noticeKill takes up a Run.Kill: a cancel, stopped at the moment of the
// call. It runs before anything else the loop handles, so an exit the
// kill caused never decides the stop.
func (r *Run) noticeKill() {
	at := r.killAt.Load()
	if at == nil || r.killNoticed {
		return
	}
	r.killNoticed = true
	r.markStopped(StoppedByCancel, *at)
	r.fail(Errorf(CodeCancelled, "", "the capture was killed"))
	r.armUnresponsive(*at)
}

// armUnresponsive bounds the wait for the reaper's report after a kill: it
// should take StopGrace, and the reaper gets FinishTimeout and 5s more.
func (r *Run) armUnresponsive(at time.Time) {
	if r.unresponsiveT == nil {
		r.unresponsiveT = time.NewTimer(max(0, r.unresponsiveAfter()-time.Since(at)))
	}
}

func (r *Run) unresponsiveAfter() time.Duration {
	return r.b.StopGrace + r.b.FinishTimeout + unresponsiveSlack
}

// profiling says the readiness line came and no stop yet.
func (r *Run) profiling() bool { return !r.readyAt.IsZero() && !r.stopped }

// supervise is the capture, in one goroutine: every event, bound and check
// until the reaper reports its cleanup.
func (r *Run) supervise() {
	defer close(r.done)
	defer r.setPhase(PhaseDone)
	start := time.NewTimer(time.Until(r.o.Started.Add(r.b.StartTimeout)))
	defer start.Stop()
	poll := time.NewTicker(r.b.Poll)
	defer poll.Stop()
	defer func() {
		for _, t := range []*time.Timer{r.finish, r.durationT, r.unresponsiveT} {
			if t != nil {
				t.Stop()
			}
		}
		if r.recheckT != nil {
			r.recheckT.Stop()
		}
	}()
	if r.spec.PID > 0 {
		r.attach(r.spec.PID)
	}
	cancel, killed := r.cancel, r.killCh
	reaperDone := r.reaper.Done()
	var gone <-chan time.Time
	for r.evc != nil {
		r.noticeKill()
		var finish, duration, recheck, unresponsive <-chan time.Time
		if r.finish != nil {
			finish = r.finish.C
		}
		if r.durationT != nil {
			duration = r.durationT.C
		}
		if r.recheckT != nil {
			recheck = r.recheckT.C
		}
		if r.unresponsiveT != nil {
			unresponsive = r.unresponsiveT.C
		}
		select {
		case ev, ok := <-r.evc:
			r.handleEvent(ev, ok)
		case t := <-r.ready:
			r.readied(t)
		case <-start.C:
			r.startExpired()
		case <-duration:
			r.ranItsCourse(r.goneFirst(), r.readyAt.Add(r.spec.Settings.Duration))
		case <-finish:
			r.finishExpired()
		case <-recheck:
			if r.profiling() {
				r.recheckTarget()
			}
		case <-poll.C:
			r.poll()
		case <-cancel:
			cancel = nil
			r.cancelled()
		case <-killed:
			killed = nil
		case <-reaperDone:
			reaperDone = nil
			gone = time.After(reaperGone)
		case <-gone:
			r.drainPending()
			r.evc = nil
		case <-unresponsive:
			r.drainPending()
			if r.evc != nil {
				r.unresponsive, r.evc = true, nil
			}
		}
	}
	if r.reaped != nil {
		r.awaitRelease()
	}
	r.finishOutcome()
	r.outcome = r.o
}

// reaperRelease bounds how long Wait waits, after reaped, for the reaper
// to let go of the captures lock.
const reaperRelease = time.Second

// awaitRelease waits, at most reaperRelease, for the reaper to close its
// event pipe, which it does once it has let go of the captures lock, or to
// exit: so the next capture can take the lock as soon as this one is seen
// to end. A reaper still holding it after that (a known child stuck in
// the kernel) keeps it, and the next capture is told busy.
func (r *Run) awaitRelease() {
	t := time.NewTimer(reaperRelease)
	defer t.Stop()
	select {
	case <-r.reaper.Ended():
	case <-r.reaper.Done():
	case <-t.C:
	}
}

// handleEvent takes one item of the event stream; reaped, or its end,
// ends the loop.
func (r *Run) handleEvent(ev Event, ok bool) {
	// An exit the kill caused must find the kill already decided.
	r.noticeKill()
	switch {
	case !ok:
		r.evc = nil
	case ev.Op == EventReaped:
		r.reaped, r.evc = &ev, nil
	default:
		r.event(ev)
	}
}

// drainPending handles every event and readiness line already queued, so
// a bound that expires decides on what has happened, not on which ready
// channel a select picked first.
func (r *Run) drainPending() {
	for {
		select {
		case t := <-r.ready:
			r.readied(t)
			continue
		default:
		}
		if r.evc == nil {
			return
		}
		select {
		case ev, ok := <-r.evc:
			r.handleEvent(ev, ok)
		default:
			return
		}
	}
}

// startExpired is the start bound: perf-agent not profiling by
// StartTimeout. A capture already stopping is left to its finish bound.
func (r *Run) startExpired() {
	r.drainPending()
	if r.readyAt.IsZero() && !r.perfExited && !r.stopping() {
		r.kill(Errorf(CodeStartTimeout, "raise --limit Capture.StartTimeout, or profile a smaller target",
			"perf-agent did not start profiling within StartTimeout=%v", r.b.StartTimeout))
	}
}

// finishExpired is the finish bound: perf-agent not done by FinishTimeout
// after the stop.
func (r *Run) finishExpired() {
	r.drainPending()
	if !r.perfExited && r.evc != nil {
		r.kill(Errorf(CodeFinishTimeout, "raise --limit Capture.FinishTimeout, or capture for less time",
			"perf-agent did not finish within FinishTimeout=%v of the stop", r.b.FinishTimeout))
	}
}

// attach builds perf-agent's argv for pid and asks the reaper to start it.
func (r *Run) attach(pid int) {
	argv, err := BuildArgv(argvSpec(r.spec, pid))
	if err != nil {
		r.failStop(Errorf(CodeInvalidArgument, "", "%v", err))
		return
	}
	r.o.Argv, r.argvPID = argv, pid
	if r.attachFrom.IsZero() {
		r.attachFrom = time.Now()
	}
	r.setPhase(PhaseAttaching)
	r.send(Command{Op: OpAttach, Argv: argv, Dir: r.spec.StagingPath})
}

// stopping says the capture is ending before perf-agent ran its course.
func (r *Run) stopping() bool {
	return r.o.Err != nil || r.stopped || r.stopSent || r.cancelSent || r.killSent || r.killAt.Load() != nil
}

func (r *Run) event(ev Event) {
	switch {
	case ev.Op == EventLaunched && ev.Who == WhoWorkload:
		r.launched(ev)
	case ev.Op == EventAttached && ev.Who == WhoPerfAgent:
		r.perf = instance{ev.PID, ev.StartTime}
	case ev.Op == EventExited && ev.Who == WhoPerfAgent:
		r.perfDone(ev)
	case ev.Op == EventExited && ev.Who == WhoWorkload:
		r.o.WorkloadExited, r.o.WorkloadStatus, r.o.WorkloadSignal = true, ev.Status, ev.Signal
		r.workDone()
	case ev.Op == EventError:
		r.reaperError(ev)
	case ev.Op == EventDeadline:
		r.deadlinePassed()
	}
}

// deadlinePassed is the reaper ending the capture at its absolute
// deadline, which only a server that did not act on its own bounds (one
// stopped or starved) lets come: a bound, start_timeout before readiness
// and finish_timeout after, decided before the exits the kills cause.
// After perf-agent's exit it is only a note: perf-agent's exit decides.
func (r *Run) deadlinePassed() {
	if r.perfExited {
		// perf-agent had finished: the deadline came while the reaper ended
		// the command, and perf-agent's own exit decides the capture.
		r.o.Notes = append(r.o.Notes, "the capture's reaper ended the command at its deadline, after perf-agent had finished")
		return
	}
	const next = "capture again; if this repeats, check that perf-agent-mcp is not stopped or starved of CPU"
	e := Errorf(CodeFinishTimeout, next,
		"the capture's reaper ended it at its deadline, after FinishTimeout=%v: this server did not stop it in time", r.b.FinishTimeout)
	if r.readyAt.IsZero() {
		e = Errorf(CodeStartTimeout, next,
			"the capture's reaper ended it at its deadline, after StartTimeout=%v: this server did not stop it in time", r.b.StartTimeout)
	}
	e.Retryable, e.RetryAfter = true, 0
	r.markStopped("", time.Now())
	r.fail(e)
}

// launched proves the command runs, then attaches. launched means only
// that the launcher reached its exec: the PID must pass the preflight and
// run the command (or its interpreter), not the launcher.
func (r *Run) launched(ev Event) {
	r.work = instance{ev.PID, ev.StartTime}
	r.o.Target = TargetState{PID: ev.PID, StartTime: ev.StartTime}
	// The command runs from here: its attach latency is what the profile
	// misses of it, the preflight included.
	r.attachFrom = time.Now()
	if r.stopping() {
		return
	}
	st, e := Preflight(r.spec.Proc, ev.PID, r.spec.SelfID)
	if e == nil && st.StartTime != ev.StartTime {
		e = targetExited(ev.PID)
	}
	if e == nil {
		e = r.execProven(ev.PID, ev.StartTime)
	}
	if e != nil {
		// A pid gone or not found is the command that ended on its own; a
		// retryable target_exited (the launcher never ran it) keeps its
		// own message and verdict.
		if (e.Code == CodeTargetExited || e.Code == CodeTargetNotFound) && !e.Retryable {
			e = r.commandEnded(ev.PID, "before it could be profiled: it exited at once, or never ran the command")
		}
		r.failStop(e)
		return
	}
	r.target, r.targetKnown, r.o.Target, r.o.Launched = st, true, st, true
	r.attach(ev.PID)
}

// execProven checks that pid ran the exec: its exe is the command or its
// interpreter, or at least no longer the launcher (a command may exec
// another program at once, and that case is noted).
func (r *Run) execProven(pid int, start uint64) *Error {
	id, ok := statID(r.spec.Proc.path(pid, "exe"))
	if !ok || !Alive(r.spec.Proc, pid, start) {
		return targetExited(pid)
	}
	switch {
	case slices.Contains(r.cmdIDs, id):
		return nil
	case slices.Contains(r.launchers, id):
		r.o.Notes = append(r.o.Notes, fmt.Sprintf("pid %d never ran the command: it was still the launcher", pid))
		return notStarted(Errorf(CodeTargetExited, "capture it again",
			"pid %d still runs the launcher, not %s: the command was never executed", pid, echo(r.spec.Launch.Path)))
	}
	// Not the launcher, so the exec happened; but this shows only that, not
	// that it was the command resolved at the start.
	exe, _ := os.Readlink(r.spec.Proc.path(pid, "exe"))
	r.o.Notes = append(r.o.Notes, fmt.Sprintf("pid %d runs %s: it executed a program other than the resolved command %s",
		pid, filepath.Base(exe), filepath.Base(r.spec.Launch.Path)))
	return nil
}

// notStarted marks e, a target_exited of a command the launcher never got
// to run, retryable: unlike a command that ended on its own, the same call
// may succeed.
func notStarted(e *Error) *Error {
	e.Retryable = true
	return e
}

// commandEnded is target_exited for the launched command, with the hint
// for a command that hands its work to another process. It is not
// retryable: the same command ends as fast again.
func (r *Run) commandEnded(pid int, how string) *Error {
	return Errorf(CodeTargetExited,
		"the command ended before perf-agent attached; if it starts a daemon or a server and returns, start that yourself and capture it with pid; otherwise give it more work",
		"the launched command (pid %d) ended %s, %.1fs after the start", pid, how, time.Since(r.o.Started).Seconds())
}

// readied is the readiness line: the attach latency, the duration and
// recheck timers, and the readiness recheck, which runs before the phase
// says profiling. A line that comes once the capture is ending starts no
// profiling: readyAt stays zero.
func (r *Run) readied(t time.Time) {
	if !r.readyAt.IsZero() || r.stopping() {
		return
	}
	r.readyAt = t
	if !r.attachFrom.IsZero() {
		r.o.AttachLatency = t.Sub(r.attachFrom)
	}
	r.durationT = time.NewTimer(r.spec.Settings.Duration - time.Since(t))
	r.recheckT = time.NewTicker(r.b.Recheck)
	r.recheckTarget()
	if !r.stopped {
		r.setPhase(PhaseProfiling)
	}
}

// drainReady takes a readiness line that is already in, so an exit
// handled before it does not miss it.
func (r *Run) drainReady() {
	select {
	case t := <-r.ready:
		r.readied(t)
	default:
	}
}

// lateReadiness waits briefly, at perf-agent's exit, for a readiness line
// still in its stdout pipe: the exit comes by another pipe and may
// overtake it.
func (r *Run) lateReadiness() {
	if !r.readyAt.IsZero() {
		return
	}
	select {
	case t := <-r.ready:
		r.readied(t)
	case <-r.perfOut.done:
		r.drainReady()
	case <-time.After(lateReadiness):
	}
}

// recheckTarget repeats the preflight: Observe, then Compare. A target
// that is gone stops the capture; one that changed fails it.
func (r *Run) recheckTarget() {
	gone, e := r.observe()
	switch {
	case gone:
		r.targetGone()
	case e != nil:
		r.failStop(e)
	}
}

// observe compares the target with the preflight. gone is a target that
// has ended or no longer exists. A failure to read it that is not its end
// is "cannot recheck": noted once, and the capture goes on.
func (r *Run) observe() (gone bool, e *Error) {
	if !r.targetKnown {
		return false, nil
	}
	now, oe := Observe(r.spec.Proc, r.target.PID)
	if oe != nil {
		if oe.Code == CodeTargetExited || oe.Code == CodeTargetNotFound {
			// What has ended at the pid may be another process: the start
			// time decides first.
			if st, err := readStat(r.spec.Proc, r.target.PID); err == nil && st.start != r.target.StartTime {
				return false, r.target.Compare(TargetState{PID: r.target.PID, StartTime: st.start})
			}
			return true, nil
		}
		r.cannotRecheck(oe)
		return false, nil
	}
	return false, r.target.Compare(now)
}

// cannotRecheck notes, once, that the target could not be read.
func (r *Run) cannotRecheck(e *Error) {
	if r.recheckNoted {
		return
	}
	r.recheckNoted = true
	r.o.Notes = append(r.o.Notes, fmt.Sprintf("pid %d could not be rechecked (%s); the capture went on without that check", r.target.PID, e.Message))
}

// pidGone says a PID target is no longer the live process the preflight
// saw: it does not exist, has ended, or is another process now. A read
// error that is not its end says nothing.
func (r *Run) pidGone() bool {
	if !r.targetKnown {
		return false
	}
	s, err := readStat(r.spec.Proc, r.target.PID)
	if err != nil {
		return gone(err)
	}
	return s.start != r.target.StartTime || s.exited()
}

// targetGone handles a PID target that exited: before readiness it is
// target_exited; while profiling it stops the capture early. A launched
// command's end is left to its exit event, which comes in order (see
// goneFirst).
func (r *Run) targetGone() {
	if r.spec.Launch != nil {
		return
	}
	r.targetIsGone = true
	switch {
	case r.readyAt.IsZero():
		if !r.perfExited {
			r.failStop(Errorf(CodeTargetExited, pidGoneNext, "pid %d exited before profiling began", r.spec.PID))
		}
	case r.profiling():
		r.earlyStop(StoppedByTargetExit)
	}
}

// workDone is the launched command's exit.
func (r *Run) workDone() {
	if r.workExited {
		return
	}
	r.workExited = true
	r.drainReady()
	switch {
	case r.readyAt.IsZero():
		if !r.perfExited {
			r.failStop(r.commandEnded(r.work.pid, "before profiling began"))
		}
	case r.profiling():
		r.earlyStop(StoppedByCommandExit)
	}
}

// perfDone is perf-agent's exit; the end recheck runs while the target is
// still there, and not once it was seen gone.
func (r *Run) perfDone(ev Event) {
	r.lateReadiness()
	r.perfExited, r.perfStatus, r.perfSignal = true, ev.Status, ev.Signal
	r.goneAtExit = r.goneFirst()
	if r.profiling() {
		r.ranItsCourse(r.goneAtExit, time.Now())
	}
	if r.finish != nil {
		r.finish.Stop()
	}
	// The end recheck, unless the target was already seen gone: nothing can
	// change after its end, and its pid may since belong to another process.
	// A reuse found only now, with no end seen first, is still pid_reused.
	if !r.readyAt.IsZero() && r.o.Err == nil && !r.targetIsGone && !r.workExited {
		if _, e := r.observe(); e != nil {
			r.fail(e)
		}
	}
	r.setPhase(PhaseFinishing)
}

// reaperError maps an error event. The workload's is a launch that
// failed; perf-agent's is a refused start (a changed binary among them).
// Others are commands refused while the capture ends, and are only logged.
func (r *Run) reaperError(ev Event) {
	r.log.Debug("capture: reaper error", "capture", r.spec.CaptureID, "who", ev.Who, "err", ev.Err)
	switch {
	case strings.HasPrefix(ev.Err, "the capture is ending"), strings.HasPrefix(ev.Err, "perf-agent was already started"):
		return
	case ev.Who == WhoWorkload:
		r.failStop(r.launchError(ev.Err))
	case ev.Who == WhoPerfAgent:
		code, rest, ok := strings.Cut(ev.Err, ": ")
		if _, known := DefaultRetryable[Code(code)]; ok && known {
			r.failStop(Errorf(Code(code), "", "%s", rest))
		} else {
			r.failStop(Errorf(CodePerfAgentUnstartable, "check the perf-agent binary", "perf-agent could not start: %s", cut(ev.Err, maxPathEcho)))
		}
	}
}

// launchError maps the launcher's failure. An exec it reports is the
// command's: invalid_argument, naming it and the error. A launcher that
// never got to the command (it exited, or its handshake failed) is
// target_exited, retryable, with a note.
func (r *Run) launchError(msg string) *Error {
	text := strings.TrimPrefix(msg, "launch: ")
	if rest, ok := strings.CutPrefix(text, "exec "); ok {
		why := rest
		if _, after, found := strings.Cut(rest, ": "); found {
			why = after
		}
		return Errorf(CodeInvalidArgument, "check that command[0] is an executable file this user may run",
			"the command %s could not be executed: %s", echo(r.spec.Launch.Path), cut(why, maxPathEcho))
	}
	r.o.Notes = append(r.o.Notes, "the launcher did not get to the command: "+cut(text, maxPathEcho))
	return notStarted(Errorf(CodeTargetExited, "capture it again", "the command was not started: %s", cut(text, maxPathEcho)))
}

// poll checks the staging sizes while perf-agent writes, and whether a PID
// target is still the process the preflight saw.
func (r *Run) poll() {
	if r.argvPID != 0 && !r.perfExited && !r.killSent {
		if e := r.sizes(); e != nil {
			r.kill(e)
			return
		}
	}
	if r.spec.PID > 0 && !r.targetIsGone && r.pidGone() {
		// Not the recorded live process: ended, or a reused pid.
		gone, e := r.observe()
		switch {
		case e != nil:
			r.targetIsGone = true
			r.failStop(e)
		case gone:
			r.targetGone()
		}
	}
}

// sizes is too_large for any regular file in staging over its limit: the
// execution log's is MaxExeclogBytes, every other file's, an unexpected
// one's included, MaxProfileBytes.
func (r *Run) sizes() *Error {
	dir, err := r.spec.Staging.Open(".")
	if err != nil {
		return nil
	}
	ents, _ := dir.ReadDir(-1)
	_ = dir.Close()
	for _, ent := range ents {
		name := ent.Name()
		fi, err := r.spec.Staging.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		limit, which := r.b.MaxProfileBytes, "MaxProfileBytes"
		if name == ExeclogFile {
			limit, which = r.b.MaxExeclogBytes, "MaxExeclogBytes"
		}
		if fi.Size() > limit {
			return Errorf(CodeTooLarge, "capture for less time, or at a lower sample rate",
				"%s grew to %d bytes, over Capture.%s=%d", name, fi.Size(), which, limit)
		}
	}
	return nil
}

// cancelled relays the cancel to the reaper.
func (r *Run) cancelled() {
	r.markStopped(StoppedByCancel, time.Now())
	r.fail(Errorf(CodeCancelled, "", "the capture was cancelled"))
	if !r.cancelSent {
		r.cancelSent = true
		r.send(Command{Op: OpCancel})
	}
}

// finishOutcome turns the end into the Outcome: the tails, the exit
// mapping, the survivors and the notes. The readers get readerDrain to
// reach EOF, then their pipes are closed, so a survivor holding one open
// keeps no reader behind; a readiness line they passed on by then still
// counts.
func (r *Run) finishOutcome() {
	end := time.Now().Add(readerDrain)
	streams := []*stream{r.perfOut, r.perfErr, r.workOut, r.workErr}
	for _, s := range streams {
		s.wait(end)
	}
	for _, s := range streams {
		s.detach()
	}
	r.drainReady()
	stderr := r.perfErr.tail(r.b.MaxOutputTail)
	if r.spec.Launch != nil {
		r.o.Workload = Tail{Stdout: r.workOut.tail(r.b.MaxOutputTail), Stderr: r.workErr.tail(r.b.MaxOutputTail)}
	}
	if r.perfExited && r.perfSignal == "" {
		r.o.PerfAgentExit = r.perfStatus
	}
	switch {
	case r.unresponsive:
		r.lost(fmt.Sprintf("the capture's reaper is unresponsive: it reported no cleanup within %v of the kill", r.unresponsiveAfter()))
	case r.reaped == nil:
		r.lost("containment lost: the capture's reaper exited before it reported its cleanup")
	default:
		r.o.Survivors, r.o.Cleanup = r.reaped.Survivors, r.reaped.Cleanup
	}
	if r.o.Err == nil {
		r.o.Err = r.exitError(stderr)
	}
	switch {
	case len(r.o.Survivors) == 0:
	case r.o.Err != nil && r.o.Err.Code == CodeStuckChild:
		r.o.Err.Survivors = r.o.Survivors
	default:
		r.stuck(Errorf(CodeStuckChild, "do not retry; tell the user which processes are left",
			"%d process(es) survived the stop grace: %s", len(r.o.Survivors), survivorList(r.o.Survivors)))
	}
	if r.o.Err != nil && r.argvPID != 0 {
		r.o.StderrTail = stderr
	}
	if !CleanupIsComplete(r.o.Cleanup) {
		r.o.Notes = append(r.o.Notes, "descendants may survive: "+r.o.Cleanup)
	}
	if r.o.Launched {
		if n := attachNote(r.o.AttachLatency); !r.readyAt.IsZero() && n != "" {
			r.o.Notes = append(r.o.Notes, n)
		}
		r.o.Notes = append(r.o.Notes, fmt.Sprintf("only pid %d was profiled, not processes it started", r.target.PID))
	}
	if r.o.Err == nil {
		r.o.StderrTail = ""
	}
}

// attachNote says how much of a command's run the profile misses; nothing
// when that rounds to zero.
func attachNote(latency time.Duration) string {
	lat := latency.Round(100 * time.Millisecond)
	if lat == 0 {
		return ""
	}
	return fmt.Sprintf("profiling began %v after the command started; the first %v are not in the profile", lat, lat)
}

// lost is a reaper that gave no report, gone or unresponsive: whatever it
// started may still run, and that is never a success. The survivors are
// what is known: perf-agent, the command and the command's cgroup.
func (r *Run) lost(why string) {
	r.markStopped("", time.Now())
	r.o.Cleanup = "incomplete: " + why
	r.o.Notes = append(r.o.Notes, why+"; perf-agent or the command may still be running")
	add := func(pid int, start uint64) {
		for _, s := range r.o.Survivors {
			if s.PID == pid {
				return
			}
		}
		st, err := readStat(r.spec.Proc, pid)
		if err != nil || st.start != start || !present(r.spec.Proc, pid, start) {
			return
		}
		r.o.Survivors = append(r.o.Survivors, Survivor{PID: pid, Comm: st.comm, StartTime: start, State: string(st.state)})
	}
	for _, p := range []instance{r.perf, r.work} {
		if p.pid > 0 {
			add(p.pid, p.start)
		}
	}
	if r.cgroup != "" && !r.unresponsive {
		// A reaper gone without its cleanup leaves its cgroup: once the
		// cgroup holds no process, nothing else will remove it. rmdir
		// refuses a cgroup that still holds one.
		_ = os.Remove(r.cgroup)
	}
	if r.cgroup != "" {
		if _, err := os.Stat(r.cgroup); err == nil {
			r.o.Notes = append(r.o.Notes, fmt.Sprintf("the command's cgroup %s under the delegated cgroup was not removed; it may still hold processes", filepath.Base(r.cgroup)))
			for _, pid := range cgroupProcs(r.cgroup) {
				if st, err := readStat(r.spec.Proc, pid); err == nil {
					add(pid, st.start)
				}
			}
		}
	}
	r.stuck(Errorf(CodeStuckChild, "do not retry; tell the user to look for leftover perf-agent or command processes", "%s", why))
}

// stuck makes e, a stuck_child, the capture's error with its survivors;
// an earlier failure becomes a note.
func (r *Run) stuck(e *Error) {
	e.Survivors = r.o.Survivors
	if r.o.Err != nil {
		r.o.Notes = append(r.o.Notes, "the capture had already failed: "+r.o.Err.Error())
	}
	r.o.Err = e
}

func survivorList(ss []Survivor) string {
	var parts []string
	for _, s := range ss[:min(len(ss), 8)] {
		parts = append(parts, fmt.Sprintf("pid %d (%s, %s)", s.PID, s.Comm, s.State))
	}
	if len(ss) > 8 {
		parts = append(parts, fmt.Sprintf("and %d more", len(ss)-8))
	}
	return strings.Join(parts, ", ")
}

// exitError is §5.4's mapping of how perf-agent ended, for a capture that
// has not failed otherwise; nil is a capture whose files may be kept.
func (r *Run) exitError(stderr string) *Error {
	withTail := func(e *Error) *Error {
		e.StderrTail = stderr
		if r.perfExited {
			e.PerfAgentExit = strconv.Itoa(r.perfStatus)
			if r.perfSignal != "" {
				e.PerfAgentExit = r.perfSignal
			}
		}
		return e
	}
	// A target gone before readiness explains a perf-agent that failed.
	if r.readyAt.IsZero() && r.targetKnown && r.goneAtExit {
		if r.spec.Launch != nil {
			return withTail(r.commandEnded(r.target.PID, "before profiling began"))
		}
		return withTail(Errorf(CodeTargetExited, pidGoneNext, "pid %d exited before profiling began", r.target.PID))
	}
	switch {
	case r.argvPID == 0 || r.perf.pid == 0:
		return withTail(Errorf(CodePerfAgentUnstartable, "check the perf-agent binary", "perf-agent was never started"))
	case !r.perfExited:
		return withTail(Errorf(CodePerfAgentFailed, "", "perf-agent's exit was never reported"))
	case r.perfSignal != "":
		return withTail(Errorf(CodePerfAgentFailed, "", "perf-agent was ended by a signal: %s", r.perfSignal))
	}
	switch r.perfStatus {
	case 0:
	case 2:
		return withTail(Errorf(CodePerfAgentRefusedArgs, "use a perf-agent that matches this perf-agent-mcp",
			"perf-agent refused its arguments; it may be older than perf-agent-mcp expects (found %s)", cmp.Or(r.spec.Binary.Version, "an unknown version")))
	case 1:
		e := Errorf(CodePerfAgentFailed, "", "the capture failed")
		if strings.Contains(stderr, "set capabilities") || strings.Contains(stderr, "cap_checkpoint_restore") {
			e.Next = "give perf-agent its capabilities: " + strings.ReplaceAll(SetcapLine, "<path>", r.spec.Binary.Path)
			e.Retryable = false
		}
		return withTail(e)
	case 126, 127:
		e := Errorf(CodePerfAgentUnstartable, "", "perf-agent could not start: %s", lastLine(stderr))
		if strings.Contains(stderr, "error while loading shared libraries") && strings.Contains(stderr, "blazesym") {
			e.Next = "link blazesym statically into perf-agent: a setcap'd binary ignores LD_LIBRARY_PATH"
		}
		return withTail(e)
	default:
		return withTail(Errorf(CodePerfAgentFailed, "", "perf-agent exited %d", r.perfStatus))
	}
	if r.readyAt.IsZero() {
		return withTail(Errorf(CodePerfAgentFailed, "", "perf-agent exited before profiling began"))
	}
	if e := r.sizes(); e != nil {
		return withTail(e)
	}
	for _, m := range r.spec.Settings.Canonical().Modes {
		names := []string{OutputFile(m)}
		if m == ModeGPU {
			names = append(names, ExeclogFile)
		}
		for _, name := range names {
			if fi, err := r.spec.Staging.Lstat(name); err != nil || fi.Size() == 0 {
				return withTail(Errorf(CodeNoOutput, "capture again", "perf-agent exited 0 but wrote no %s profile (%s)", m, name))
			}
		}
	}
	return nil
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

// stream is one output pipe's reader: a bounded ring of its last bytes,
// and its lines at debug level. It never blocks on anything but the pipe.
type stream struct {
	mu   sync.Mutex
	ring []byte
	size int
	f    *os.File
	done chan struct{}
}

// read starts the reader of f. onLine sees each line until it returns true.
func (r *Run) read(f *os.File, size int, name string, onLine func([]byte) bool) *stream {
	s := &stream{size: size, f: f, done: make(chan struct{})}
	debug := r.log.Enabled(context.Background(), slog.LevelDebug)
	go func() {
		defer close(s.done)
		defer func() { _ = f.Close() }()
		var partial []byte
		logged := 0
		line := func(l []byte) {
			if onLine != nil && onLine(l) {
				onLine = nil
			}
			if !debug {
				return
			}
			switch {
			case logged < maxLoggedLines:
				r.log.Debug(name, "capture", r.spec.CaptureID, "line", strings.ToValidUTF8(string(l), string(utf8.RuneError)))
			case logged == maxLoggedLines:
				r.log.Debug(name+": further output is not logged", "capture", r.spec.CaptureID)
			}
			logged++
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			chunk := buf[:n]
			s.add(chunk)
			for len(chunk) > 0 {
				i := bytes.IndexByte(chunk, '\n')
				if i < 0 {
					partial = appendCapped(partial, chunk)
					break
				}
				line(appendCapped(partial, chunk[:i]))
				partial, chunk = partial[:0], chunk[i+1:]
			}
			if err != nil {
				if len(partial) > 0 {
					line(partial)
				}
				return
			}
		}
	}()
	return s
}

func appendCapped(dst, b []byte) []byte {
	return append(dst, b[:min(len(b), max(0, maxLogLine-len(dst)))]...)
}

func (s *stream) add(b []byte) {
	if len(b) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(b) >= s.size {
		s.ring = append(s.ring[:0], b[len(b)-s.size:]...)
		return
	}
	if over := len(s.ring) + len(b) - s.size; over > 0 {
		s.ring = s.ring[:copy(s.ring, s.ring[over:])]
	}
	s.ring = append(s.ring, b...)
}

// detach closes the read end, which ends a reader still blocked on a pipe
// that something else holds open.
func (s *stream) detach() { _ = s.f.Close() }

// wait waits for the reader's EOF until end.
func (s *stream) wait(end time.Time) {
	select {
	case <-s.done:
	case <-time.After(time.Until(end)):
	}
}

// tail is the ring's last n bytes as valid UTF-8, cut on a rune boundary:
// what the ring holds is the program's, and may be anything.
func (s *stream) tail(n int) string {
	s.mu.Lock()
	b := bytes.Clone(s.ring)
	s.mu.Unlock()
	return validTail(b, n)
}

// validTail is the end of b, at most n bytes, as valid UTF-8 starting on a
// rune boundary; an invalid byte becomes U+FFFD, which may cut more.
func validTail(b []byte, n int) string {
	cutFront := func(s string) string {
		if len(s) <= n {
			return s
		}
		s = s[len(s)-n:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		return s
	}
	return cutFront(strings.ToValidUTF8(cutFront(string(b)), string(utf8.RuneError)))
}
