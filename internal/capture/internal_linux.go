package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The reaper's inherited descriptors (see StartReaper).
const (
	specFD    = 3
	cmdFD     = 4
	eventFD   = 5
	lockFD    = 6
	perfOutFD = 7
	perfErrFD = 8
	workOutFD = 9
	workErrFD = 10
)

const (
	// maxSpec bounds the spec read from fd 3.
	maxSpec = 4 << 20
	// selfExe is the reaper's own binary, the launcher: the file it runs
	// from, even if the path it was started by has been replaced since.
	selfExe = "/proc/self/exe"
	// waitPoll backs up SIGCHLD in the wait loop.
	waitPoll = 100 * time.Millisecond
	// settleFloor is the least time the final cleanup waits for SIGKILLed
	// processes to go, so a zero StopGrace still lets the kernel finish.
	settleFloor = 250 * time.Millisecond
	// sweepTick is the pause between sweep rounds and condition polls.
	sweepTick = 20 * time.Millisecond
	// eventWriteTimeout bounds one event write to a server that is not
	// reading; after a failure the reaper stops publishing.
	eventWriteTimeout = 2 * time.Second
	// beforeExitLimit bounds the before-exit fault pause, so a test that
	// forgets its file does not leave a reaper behind for long.
	beforeExitLimit = time.Minute
)

// RunInternal runs an internal mode when this process was started as one:
// InternalEnv is reaper or launcher and fd 3 is open. It returns handled
// false otherwise, and the caller carries on as the server; when handled,
// the caller exits with code.
func RunInternal() (code int, handled bool) {
	mode := os.Getenv(InternalEnv)
	if mode != modeReaper && mode != modeLauncher {
		return 0, false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(specFD, &st); err != nil {
		return 0, false
	}
	spec := os.NewFile(specFD, "spec")
	if mode == modeLauncher {
		var l LaunchSpec
		if err := readSpec(spec, &l); err != nil {
			fmt.Fprintf(os.Stderr, "perf-agent-mcp launcher: read the spec: %v\n", err)
			return 126, true
		}
		return launch(l), true
	}
	var w reaperWire
	err := readSpec(spec, &w)
	return runReaper(w, err), true
}

// reaperWire is what fd 3 carries to a reaper: the spec, and which of the
// slots from fd 6 on (lock, perf-agent stdout and stderr, workload stdout
// and stderr) carry a real file rather than the null device.
type reaperWire struct {
	Spec    ReaperSpec
	Present uint8
}

// slot returns fd as a file when the wire says it carries one.
func (w reaperWire) slot(fd int, name string) *os.File {
	if w.Present&(1<<(fd-lockFD)) == 0 {
		return nil
	}
	return os.NewFile(uintptr(fd), name)
}

func readSpec(f *os.File, v any) error {
	defer func() { _ = f.Close() }()
	return json.NewDecoder(io.LimitReader(f, maxSpec)).Decode(v)
}

// oCloexec is O_CLOEXEC as /proc/<pid>/fdinfo prints it, in octal.
const oCloexec = 0o2000000

// closeOnExecInherited marks the inherited descriptors 4 to 10
// close-on-exec, so no child inherits the lock or another child's pipes;
// each child is handed exactly its own through os/exec. A descriptor
// above 10 without close-on-exec was leaked to the reaper by whatever
// started the server, and is closed; the runtime's own all have the flag.
func closeOnExecInherited() {
	for fd := cmdFD; fd <= workErrFD; fd++ {
		syscall.CloseOnExec(fd)
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= workErrFD {
			continue
		}
		if flags, ok := fdFlags(fd); ok && flags&oCloexec == 0 {
			_ = syscall.Close(fd)
		}
	}
}

// fdFlags reads a descriptor's open flags from /proc/self/fdinfo.
func fdFlags(fd int) (int64, bool) {
	b, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return 0, false
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, "flags:"); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 8, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// reaper is the reaper process's state. The main goroutine owns everything
// but live, which the wait loop shares under mu.
type reaper struct {
	spec ReaperSpec
	self int
	proc Proc
	out  eventWriter
	lock *os.File
	// The output pipe ends not yet handed to a child.
	perfOut, perfErr, workOut, workErr *os.File

	// mu is held while the wait loop reaps and while a recorded PID or
	// group is checked and signalled: a child of this process keeps its
	// PID until it is reaped, so under mu the check cannot go stale.
	mu sync.Mutex
	// live holds the known children not reaped yet.
	live map[int]bool

	perf, work *child
	exits      chan exitNote
	cmds       <-chan Command
	deadline   <-chan time.Time
	// cancelKill fires StopGrace after a cancel's SIGINT.
	cancelKill <-chan time.Time
	stopping   bool
	subreaper  error
	inCgroup   bool
	// starts records the start time of each child the sweep listed.
	starts  map[int]uint64
	reasons []string
}

// exitNote is a known child the wait loop reaped.
type exitNote struct {
	pid int
	ws  syscall.WaitStatus
}

// eventWriter publishes events from the main goroutine. Each write has a
// deadline, so a server that stops reading cannot stall the cleanup.
type eventWriter struct {
	f      *os.File
	broken bool
}

func (w *eventWriter) publish(ev Event) {
	if w.f == nil || w.broken {
		return
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_ = w.f.SetWriteDeadline(time.Now().Add(eventWriteTimeout))
	if _, err := w.f.Write(append(b, '\n')); err != nil {
		w.broken = true
	}
}

func runReaper(w reaperWire, specErr error) int {
	closeOnExecInherited()
	// A non-blocking descriptor goes through the poller, which gives event
	// writes their deadline.
	_ = syscall.SetNonblock(eventFD, true)
	spec := w.Spec
	r := &reaper{
		spec:    spec,
		self:    os.Getpid(),
		proc:    DefaultProc(),
		out:     eventWriter{f: os.NewFile(eventFD, "events")},
		lock:    w.slot(lockFD, "lock"),
		perfOut: w.slot(perfOutFD, "perf-agent stdout"),
		perfErr: w.slot(perfErrFD, "perf-agent stderr"),
		workOut: w.slot(workOutFD, "workload stdout"),
		workErr: w.slot(workErrFD, "workload stderr"),
		live:    map[int]bool{},
		exits:   make(chan exitNote, 4),
		starts:  map[int]uint64{},
	}
	if specErr == nil && spec.Deadline <= 0 {
		specErr = errors.New("no deadline")
	}
	if specErr != nil {
		r.out.publish(Event{Op: EventError, Err: "reaper spec: " + specErr.Error()})
		return 2
	}
	return r.run()
}

// run is the reaper, in order: the subreaper, the launcher, the commands
// and exits until perf-agent is done or the server is gone, the end of the
// workload, the final cleanup and survivor report, and the lock last.
func (r *reaper) run() int {
	if r.spec.FaultPause == PauseNoSubreaper {
		r.subreaper = errors.New("skipped by the no-subreaper fault pause")
	} else {
		r.subreaper = setChildSubreaper()
	}
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, syscall.SIGCHLD)
	go r.waitLoop(sigchld)
	r.cmds = readCommands(os.NewFile(cmdFD, "commands"))
	deadline := time.NewTimer(r.spec.Deadline)
	defer deadline.Stop()
	r.deadline = deadline.C

	if r.spec.Launch != nil {
		r.startWorkload()
	}
	if aborted := r.serve(); aborted || !r.endWorkload() {
		r.killAll()
	}
	survivors := r.settle()
	cleanup := CleanupComplete
	if len(r.reasons) > 0 {
		cleanup = "incomplete: " + strings.Join(r.reasons, "; ")
	}
	r.out.publish(Event{Op: EventReaped, PID: r.self, Survivors: survivors, Cleanup: cleanup})
	if r.spec.Cgroup != "" {
		r.removeCgroup()
	}
	r.beforeExit()
	for _, f := range []*os.File{r.perfOut, r.perfErr, r.workOut, r.workErr, r.out.f} {
		if f != nil {
			_ = f.Close()
		}
	}
	for _, c := range []*child{r.perf, r.work} {
		if c != nil {
			c.release()
		}
	}
	// Last: the captures directory stays locked until both children are
	// reaped and the cleanup is done.
	if r.lock != nil {
		_ = r.lock.Close()
	}
	return 0
}

// readCommands decodes the command pipe; the channel is closed on EOF or
// on a malformed command, which the reaper treats alike.
func readCommands(f *os.File) <-chan Command {
	ch := make(chan Command)
	go func() {
		defer close(ch)
		if f == nil {
			return
		}
		dec := json.NewDecoder(f)
		for {
			var c Command
			if err := dec.Decode(&c); err != nil {
				return
			}
			ch <- c
		}
	}()
	return ch
}

// waitLoop is the one place children are reaped: wait4(-1) until nothing
// is left to reap, woken by SIGCHLD or the poll. It also reaps the orphans
// this subreaper adopted. Known children go to the main goroutine.
func (r *reaper) waitLoop(sigchld <-chan os.Signal) {
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		var got []exitNote
		r.mu.Lock()
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil || pid <= 0 {
				break
			}
			if r.live[pid] {
				delete(r.live, pid)
				got = append(got, exitNote{pid, ws})
			}
		}
		r.mu.Unlock()
		for _, e := range got {
			r.exits <- e
		}
		select {
		case <-sigchld:
		case <-tick.C:
		}
	}
}

// done reports that perf-agent is finished: it exited, or it was never
// started and the server said stop or cancel.
func (r *reaper) done() bool {
	if r.perf != nil {
		return r.perf.reaped
	}
	return r.stopping
}

// serve handles commands and exits until perf-agent is done. It returns
// aborted on EOF of the command pipe or at the deadline.
func (r *reaper) serve() (aborted bool) {
	for !r.done() {
		select {
		case c, ok := <-r.cmds:
			if !ok {
				return true
			}
			r.handle(c)
		case e := <-r.exits:
			r.exited(e)
		case <-r.deadline:
			return true
		case <-r.cancelKill:
			r.cancelKill = nil
			r.signalGroup(r.perf, syscall.SIGKILL)
		}
	}
	return false
}

func (r *reaper) handle(c Command) {
	running := r.perf != nil && !r.perf.reaped
	switch c.Op {
	case OpAttach:
		r.attach(c)
	case OpStop:
		r.stopping = true
		if running {
			r.signalGroup(r.perf, syscall.SIGINT)
		}
	case OpCancel:
		r.stopping = true
		if running && r.cancelKill == nil {
			r.signalGroup(r.perf, syscall.SIGINT)
			r.cancelKill = time.After(r.spec.StopGrace)
		}
	default:
		r.fail("", fmt.Sprintf("unknown command %q", c.Op))
	}
}

func (r *reaper) fail(who, msg string) {
	r.out.publish(Event{Op: EventError, Who: who, Err: msg})
}

// report publishes a child's first event, unless the fault pause holds it
// back; a child never reported gets no exited event either.
func (r *reaper) report(c *child, op, pause string, ev Event) {
	if r.spec.FaultPause == pause {
		return
	}
	c.reported = true
	r.out.publish(ev)
}

// startTime reads a child's start time; the caller holds mu, so the child
// is not reaped and its /proc entry exists.
func (r *reaper) startTime(pid int) uint64 {
	s, _ := StartTime(r.proc, pid)
	return s
}

// attach re-checks the perf-agent binary and starts it.
func (r *reaper) attach(c Command) {
	if r.perf != nil || r.stopping {
		r.fail(WhoPerfAgent, "perf-agent was already started or the capture is stopping")
		return
	}
	// A relative name would be looked up in PATH; only the verified,
	// absolute path is ever executed.
	if !filepath.IsAbs(r.spec.PerfAgent) {
		r.fail(WhoPerfAgent, fmt.Sprintf("perf-agent path %q is not absolute", r.spec.PerfAgent))
		return
	}
	b := Binary{Path: r.spec.PerfAgent, ID: r.spec.PerfAgentID}
	if e := b.Recheck(); e != nil {
		r.fail(WhoPerfAgent, e.Error())
		return
	}
	r.mu.Lock()
	ch, err := startPerfAgent(r.spec.PerfAgent, c.Argv, c.Dir, r.perfOut, r.perfErr)
	if err == nil {
		ch.start = r.startTime(ch.pid)
		r.live[ch.pid] = true
	}
	r.mu.Unlock()
	if err != nil {
		r.fail(WhoPerfAgent, "start perf-agent: "+err.Error())
		return
	}
	r.perfOut, r.perfErr = nil, nil
	r.perf = ch
	r.report(ch, EventAttached, PauseAfterPerfAgentFork,
		Event{Op: EventAttached, Who: WhoPerfAgent, PID: ch.pid, PGID: ch.pid, StartTime: ch.start})
}

// cgroupRetry are the errors a start into the cgroup may fail with when
// the cgroup cannot take the process; the start is retried outside it.
func cgroupRetry(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EBUSY) || errors.Is(err, syscall.EINVAL)
}

// startWorkload starts the launcher, in the capture's cgroup when it can.
func (r *reaper) startWorkload() {
	if !filepath.IsAbs(r.spec.Launch.Path) {
		r.fail(WhoWorkload, fmt.Sprintf("command path %q is not absolute", r.spec.Launch.Path))
		return
	}
	b, err := json.Marshal(r.spec.Launch)
	if err != nil {
		r.fail(WhoWorkload, err.Error())
		return
	}
	specR, specW, err := os.Pipe()
	if err != nil {
		r.fail(WhoWorkload, err.Error())
		return
	}
	var cg *os.File
	if r.spec.Cgroup != "" {
		if cg, err = os.OpenFile(r.spec.Cgroup, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0); err != nil {
			cg = nil
		}
	}
	r.mu.Lock()
	ch, err := startLauncher(selfExe, specR, r.spec.Launch.Dir, r.workOut, r.workErr, cg)
	inCgroup := cg != nil
	if err != nil && cg != nil && cgroupRetry(err) {
		ch, err = startLauncher(selfExe, specR, r.spec.Launch.Dir, r.workOut, r.workErr, nil)
		inCgroup = false
	}
	if err == nil {
		ch.start = r.startTime(ch.pid)
		r.live[ch.pid] = true
	}
	r.mu.Unlock()
	if cg != nil {
		_ = cg.Close()
	}
	_ = specR.Close()
	if err != nil {
		_ = specW.Close()
		r.fail(WhoWorkload, "start the launcher: "+err.Error())
		return
	}
	r.workOut, r.workErr = nil, nil
	r.work, r.inCgroup = ch, inCgroup
	// The launcher reads its spec to EOF before it executes the command.
	_, _ = specW.Write(b)
	_ = specW.Close()
	r.report(ch, EventLaunched, PauseAfterLauncherFork,
		Event{Op: EventLaunched, Who: WhoWorkload, PID: ch.pid, PGID: ch.pid, StartTime: ch.start, Cgroup: inCgroup})
}

// exited handles a known child's reaping: its event, then its handle and
// pipe ends are released.
func (r *reaper) exited(e exitNote) {
	for _, c := range []*child{r.perf, r.work} {
		if c == nil || c.pid != e.pid || c.reaped {
			continue
		}
		c.reaped = true
		if c.reported {
			ev := Event{Op: EventExited, Who: c.who, PID: c.pid, PGID: c.pid, StartTime: c.start, Status: e.ws.ExitStatus()}
			if e.ws.Signaled() {
				ev.Signal = e.ws.Signal().String()
			}
			r.out.publish(ev)
		}
		c.release()
		return
	}
}

// signalGroup signals c's process group, under mu. While the leader is
// unreaped its PID, and so the group's number, cannot be reused. Once it
// is reaped, the group is signalled only while /proc shows a member: the
// kernel does not reuse a number still in use as a process group ID.
func (r *reaper) signalGroup(c *child, sig syscall.Signal) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[c.pid] || r.groupMembers(c.pid, false) {
		_ = syscall.Kill(-c.pid, sig)
	}
}

// groupMembers reports whether a process is in group pgid; liveOnly
// leaves out zombies.
func (r *reaper) groupMembers(pgid int, liveOnly bool) bool {
	ents, err := os.ReadDir(r.proc.Root)
	if err != nil {
		return false
	}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if s, err := readStat(r.proc, pid); err == nil && s.pgrp == pgid && (!liveOnly || !s.exited()) {
			return true
		}
	}
	return false
}

// endWorkload ends a workload still running after perf-agent: cgroup.kill
// when it has a cgroup, otherwise SIGTERM to its group and SIGKILL after
// StopGrace (the final sweep follows). It returns false when EOF or the
// deadline cut the wait short, and the caller kills everything.
func (r *reaper) endWorkload() bool {
	w := r.work
	if w == nil {
		return true
	}
	if r.inCgroup {
		err := KillCgroup(r.spec.Cgroup)
		if err == nil {
			return true
		}
		r.reasons = append(r.reasons, "cgroup.kill: "+err.Error())
	}
	r.signalGroup(w, syscall.SIGTERM)
	ok, aborted := r.waitUntil(time.Now().Add(r.spec.StopGrace), true, func() bool { return !r.groupMembers(w.pid, true) })
	if !ok {
		r.signalGroup(w, syscall.SIGKILL)
	}
	return !aborted
}

// killAll is the end on EOF or at the deadline: SIGKILL to both groups and
// cgroup.kill.
func (r *reaper) killAll() {
	r.signalGroup(r.perf, syscall.SIGKILL)
	if r.inCgroup {
		if err := KillCgroup(r.spec.Cgroup); err != nil {
			r.reasons = append(r.reasons, "cgroup.kill: "+err.Error())
		}
	}
	r.signalGroup(r.work, syscall.SIGKILL)
}

// waitUntil handles exits until cond holds or end passes. With abortable,
// EOF of the command pipe or the deadline ends the wait early, as aborted.
func (r *reaper) waitUntil(end time.Time, abortable bool, cond func() bool) (ok, aborted bool) {
	tick := time.NewTicker(sweepTick)
	defer tick.Stop()
	timer := time.NewTimer(time.Until(end))
	defer timer.Stop()
	cmds, deadline := r.cmds, r.deadline
	if !abortable {
		cmds, deadline = nil, nil
	}
	for {
		if cond() {
			return true, false
		}
		select {
		case e := <-r.exits:
			r.exited(e)
		case _, open := <-cmds:
			if !open {
				return false, true
			}
		case <-deadline:
			return false, true
		case <-timer.C:
			return cond(), false
		case <-tick.C:
		}
	}
}

// settle is the final cleanup: the sweep of this process's children
// (adopted descendants included), then a wait, all within StopGrace, for
// the cgroup to empty and both known children to be reaped. What is left
// becomes survivors, with the reasons in r.reasons.
func (r *reaper) settle() []Survivor {
	grace := max(r.spec.StopGrace, settleFloor)
	end := time.Now().Add(grace)
	left := sweep(r.children, r.killChild, func(pid int) bool { return Alive(r.proc, pid, r.starts[pid]) }, grace, time.Now)

	var survivors []Survivor
	add := func(pid int, why string) {
		for _, s := range survivors {
			if s.PID == pid {
				return
			}
		}
		s := Survivor{PID: pid}
		if st, err := readStat(r.proc, pid); err == nil {
			s.Comm, s.StartTime, s.State = st.comm, st.start, string(st.state)
			if st.state == 'D' {
				why = "in D state"
			}
		}
		survivors = append(survivors, s)
		r.reasons = append(r.reasons, fmt.Sprintf("pid %d %s", pid, why))
	}
	for _, pid := range left {
		add(pid, "not reaped")
	}
	if r.inCgroup {
		empty := func() bool { p, err := cgroupPopulated(r.spec.Cgroup); return err == nil && !p }
		if ok, _ := r.waitUntil(end, false, empty); !ok {
			for _, pid := range cgroupProcs(r.spec.Cgroup) {
				add(pid, "still in the cgroup")
			}
		}
	}
	reaped := func() bool { return (r.perf == nil || r.perf.reaped) && (r.work == nil || r.work.reaped) }
	if ok, _ := r.waitUntil(end, false, reaped); !ok {
		for _, c := range []*child{r.perf, r.work} {
			if c != nil && !c.reaped {
				add(c.pid, "("+c.who+") not reaped")
			}
		}
	}
	if r.subreaper != nil && !r.inCgroup {
		r.reasons = append(r.reasons, "process group only (no subreaper: "+r.subreaper.Error()+")")
	}
	return survivors
}

// children lists this process's children from /proc, recording each start
// time for the sweep's checks.
func (r *reaper) children() []int {
	ents, err := os.ReadDir(r.proc.Root)
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if s, err := readStat(r.proc, pid); err == nil && s.ppid == r.self {
			r.starts[pid] = s.start
			pids = append(pids, pid)
		}
	}
	return pids
}

// killChild signals pid only while it is still this process's child with
// the start time the sweep listed, checked under mu so it cannot be
// reaped and reused in between.
func (r *reaper) killChild(pid int, sig syscall.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, err := readStat(r.proc, pid)
	if err != nil || s.ppid != r.self || s.start != r.starts[pid] {
		return syscall.ESRCH
	}
	err = syscall.Kill(pid, sig)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		r.reasons = append(r.reasons, fmt.Sprintf("kill pid %d: %v", pid, err))
	}
	return err
}

// sweep SIGKILLs, by PID, every child children lists that alive says is
// alive, round after round (a killed process's orphans are adopted and
// listed next), until none is left or grace has passed. It returns the
// survivors.
func sweep(children func() []int, kill func(pid int, sig syscall.Signal) error, alive func(pid int) bool, grace time.Duration, now func() time.Time) []int {
	end := now().Add(grace)
	for {
		var live []int
		for _, pid := range children() {
			if alive(pid) {
				live = append(live, pid)
			}
		}
		if len(live) == 0 {
			return nil
		}
		for _, pid := range live {
			_ = kill(pid, syscall.SIGKILL)
		}
		if !now().Before(end) {
			return live
		}
		time.Sleep(sweepTick)
	}
}

// removeCgroup removes the capture's cgroup, retrying briefly while the
// kernel still counts an exiting process in it.
func (r *reaper) removeCgroup() {
	for range 10 {
		if err := RemoveCgroup(r.spec.Cgroup); err == nil || !errors.Is(err, syscall.EBUSY) {
			return
		}
		time.Sleep(sweepTick)
	}
}

// beforeExit honours the before-exit fault pause: the reaper, and the lock,
// stay until the named path disappears.
func (r *reaper) beforeExit() {
	path, ok := strings.CutPrefix(r.spec.FaultPause, PauseBeforeExit)
	if !ok || path == "" {
		return
	}
	for end := time.Now().Add(beforeExitLimit); time.Now().Before(end); time.Sleep(sweepTick) {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
	}
}
