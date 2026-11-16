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
	// launchHandshake bounds the spec write to the launcher and the wait
	// for its exec.
	launchHandshake = 10 * time.Second
	// beforeExitLimit bounds the before-exit fault pause, so a test that
	// forgets its file does not leave a reaper behind for long.
	beforeExitLimit = time.Minute
)

// RunInternal runs an internal mode when this process was started as one:
// InternalEnv is reaper or launcher and fd 3 is a pipe, as the server's
// starter passes the spec. fd 3 being open is not enough: in any Go
// process the runtime may have opened something there itself. It returns
// handled false otherwise, and the caller carries on as the server; when
// handled, the caller exits with code.
func RunInternal() (code int, handled bool) {
	mode := os.Getenv(InternalEnv)
	if mode != modeReaper && mode != modeLauncher {
		return 0, false
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(specFD, &st); err != nil || st.Mode&syscall.S_IFMT != syscall.S_IFIFO {
		return 0, false
	}
	spec := os.NewFile(specFD, "spec")
	if mode == modeLauncher {
		var l launcherWire
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

// launcherWire is what fd 3 carries to a launcher: the command, and
// DieAfterMarker, the fault pause that kills the launcher between the exec
// marker and the exec.
type launcherWire struct {
	Spec           LaunchSpec
	DieAfterMarker bool `json:",omitzero"`
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
	// term delivers SIGTERM, SIGINT and SIGHUP, which end the capture as
	// EOF does instead of killing the reaper.
	term <-chan os.Signal
	// cancelKill fires StopGrace after a cancel's SIGINT.
	cancelKill <-chan time.Time
	stopping   bool
	subreaper  error
	inCgroup   bool
	// wait4 is wait4(-1, ws, WNOHANG); a test replaces it so that it never
	// reaps the test process's own children.
	wait4 func(ws *syscall.WaitStatus) (int, error)
	// launches delivers the launcher's handshake: nil once it reached the
	// exec (attempted, not proven), or why it did not.
	launches chan error
	// launchPending holds the workload's exited event while the handshake
	// is pending, so it follows launched.
	launchPending bool
	heldExit      *Event
	// starts records the start time of each child the sweep listed.
	starts  map[int]uint64
	reasons []string
	// notes qualify a complete cleanup, such as a cgroup fallback.
	notes []string
}

// reason records why the cleanup is incomplete, on one line.
func (r *reaper) reason(why string) {
	r.reasons = append(r.reasons, strings.ReplaceAll(why, "\n", "; "))
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
		spec:     spec,
		self:     os.Getpid(),
		proc:     DefaultProc(),
		out:      eventWriter{f: os.NewFile(eventFD, "events")},
		lock:     w.slot(lockFD, "lock"),
		perfOut:  w.slot(perfOutFD, "perf-agent stdout"),
		perfErr:  w.slot(perfErrFD, "perf-agent stderr"),
		workOut:  w.slot(workOutFD, "workload stdout"),
		workErr:  w.slot(workErrFD, "workload stderr"),
		live:     map[int]bool{},
		exits:    make(chan exitNote, 4),
		launches: make(chan error, 1),
		starts:   map[int]uint64{},
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
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	r.term = term
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
	if r.spec.Cgroup != "" {
		r.removeCgroup()
	}
	cleanup := CleanupComplete
	if len(r.reasons) > 0 {
		cleanup = "incomplete: " + strings.Join(r.reasons, "; ")
	}
	if len(r.notes) > 0 {
		cleanup += " (" + strings.Join(r.notes, "; ") + ")"
	}
	r.out.publish(Event{Op: EventReaped, PID: r.self, Survivors: survivors, Cleanup: cleanup})
	r.awaitKnown()
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

// reap is the one place children are reaped: wait4(-1) until nothing is
// left to reap, under mu. It also reaps the orphans this subreaper
// adopted. It returns the known children it reaped, and none when the
// kernel says this process has no child at all (ECHILD): as a subreaper,
// then, it has no descendant either.
func (r *reaper) reap() (got []exitNote, none bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var ws syscall.WaitStatus
		var pid int
		var err error
		if r.wait4 != nil {
			pid, err = r.wait4(&ws)
		} else {
			pid, err = syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		}
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ECHILD):
			return got, true
		case err != nil || pid <= 0:
			return got, false
		}
		if r.live[pid] {
			delete(r.live, pid)
			got = append(got, exitNote{pid, ws})
		}
	}
}

// waitLoop reaps whenever SIGCHLD or the poll wakes it, and hands the
// known children to the main goroutine.
func (r *reaper) waitLoop(sigchld <-chan os.Signal) {
	tick := time.NewTicker(waitPoll)
	defer tick.Stop()
	for {
		got, _ := r.reap()
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
		case <-r.term:
			return true
		case err := <-r.launches:
			r.launchDone(err)
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
	b, err := json.Marshal(launcherWire{Spec: *r.spec.Launch, DieAfterMarker: r.spec.FaultPause == PauseLauncherDiesAfterMarker})
	if err != nil {
		r.fail(WhoWorkload, err.Error())
		return
	}
	specR, specW, err := os.Pipe()
	if err != nil {
		r.fail(WhoWorkload, err.Error())
		return
	}
	syncR, syncW, err := os.Pipe()
	if err != nil {
		_, _ = specR.Close(), specW.Close()
		r.fail(WhoWorkload, err.Error())
		return
	}
	var cg *os.File
	fallback := func(why error) {
		r.notes = append(r.notes, fmt.Sprintf("no cgroup: %v; the subreaper sweep was used", why))
	}
	switch {
	case r.spec.Cgroup == "":
	case !strings.HasPrefix(filepath.Base(r.spec.Cgroup), CgroupPrefix):
		// A cgroup this server would not have created may hold anything:
		// the workload never starts in it, so it is never killed, proven
		// or removed either.
		fallback(fmt.Errorf("%s is not a %s cgroup", r.spec.Cgroup, CgroupPrefix))
	default:
		if cg, err = os.OpenFile(r.spec.Cgroup, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0); err != nil {
			cg = nil
			fallback(err)
		}
	}
	r.mu.Lock()
	ch, err := startLauncher(selfExe, specR, syncW, r.spec.Launch.Dir, r.workOut, r.workErr, cg)
	inCgroup := cg != nil
	if err != nil && cg != nil && cgroupRetry(err) {
		fallback(fmt.Errorf("start in %s: %w", r.spec.Cgroup, err))
		ch, err = startLauncher(selfExe, specR, syncW, r.spec.Launch.Dir, r.workOut, r.workErr, nil)
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
	_, _ = specR.Close(), syncW.Close()
	if err != nil {
		_, _ = specW.Close(), syncR.Close()
		r.fail(WhoWorkload, "start the launcher: "+err.Error())
		return
	}
	r.workOut, r.workErr = nil, nil
	r.work, r.inCgroup = ch, inCgroup
	r.launchPending = true
	// The handshake runs beside the main loop, which keeps serving EOF and
	// the deadline; the launcher is a known child from here on.
	go func() { r.launches <- handshake(specW, syncR, b) }()
}

// handshake writes the launcher its spec, then reads the sync pipe to EOF:
// exactly the exec marker means the launcher got as far as the exec and
// the pipe then closed, which a successful exec does, but so does a
// launcher killed in between: no signal reaches user space after an exec
// succeeds, so this is "exec attempted", never proof that the command
// runs. The server proves it from /proc on launched. The marker and text,
// or text alone, is the launcher's failure; nothing is a launcher that
// exited (or was killed) before it got to the exec. Both steps have a
// deadline.
func handshake(specW, syncR *os.File, spec []byte) error {
	defer func() { _ = syncR.Close() }()
	_ = specW.SetWriteDeadline(time.Now().Add(launchHandshake))
	_, werr := specW.Write(spec)
	if err := errors.Join(werr, specW.Close()); err != nil {
		return fmt.Errorf("send the launcher its spec: %w", err)
	}
	_ = syncR.SetReadDeadline(time.Now().Add(launchHandshake))
	msg, err := io.ReadAll(io.LimitReader(syncR, 4096))
	switch {
	case err != nil:
		return fmt.Errorf("the launcher did not confirm its exec: %w", err)
	case len(msg) == 0:
		return errors.New("the launcher exited before executing the command")
	case len(msg) == 1 && msg[0] == execMarker:
		return nil
	case msg[0] == execMarker:
		return errors.New(string(msg[1:]))
	}
	return errors.New(string(msg))
}

// launchDone publishes launched once the launcher reached its exec, then
// any exit held back meanwhile; a failed exec is an error event, and the
// workload is never reported. launched says the exec was attempted; the
// server checks that the PID really runs the command.
func (r *reaper) launchDone(err error) {
	r.launchPending = false
	w := r.work
	if err != nil {
		r.heldExit = nil
		r.fail(WhoWorkload, "launch: "+err.Error())
		return
	}
	r.report(w, EventLaunched, PauseAfterLauncherFork,
		Event{Op: EventLaunched, Who: WhoWorkload, PID: w.pid, PGID: w.pid, StartTime: w.start, Cgroup: r.inCgroup})
	if r.heldExit != nil && w.reported {
		r.out.publish(*r.heldExit)
	}
	r.heldExit = nil
}

// exited handles a known child's reaping: its event, then its handle and
// pipe ends are released.
func (r *reaper) exited(e exitNote) {
	for _, c := range []*child{r.perf, r.work} {
		if c == nil || c.pid != e.pid || c.reaped {
			continue
		}
		c.reaped = true
		ev := Event{Op: EventExited, Who: c.who, PID: c.pid, PGID: c.pid, StartTime: c.start, Status: e.ws.ExitStatus()}
		if e.ws.Signaled() {
			ev.Signal = e.ws.Signal().String()
		}
		switch {
		case c == r.work && r.launchPending:
			r.heldExit = &ev
		case c.reported:
			r.out.publish(ev)
		}
		c.release()
		return
	}
}

// signalGroup signals c's process group, and only while c is an unreaped
// child: under mu the wait loop cannot reap it, so its PID, and with it
// the group's number, is still c's. Once c is reaped the number may be
// reused by anyone; what is left of the group is reached only through the
// sweep of adopted children, by verified PID, or the cgroup.
func (r *reaper) signalGroup(c *child, sig syscall.Signal) {
	if c == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live[c.pid] {
		_ = syscall.Kill(-c.pid, sig)
	}
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
		r.reason("cgroup.kill: " + err.Error())
	}
	// Members left after the leader are adopted children by then, and the
	// sweep ends them.
	r.signalGroup(w, syscall.SIGTERM)
	ok, aborted := r.waitUntil(time.Now().Add(r.spec.StopGrace), true, func() bool { return w.reaped })
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
			r.reason("cgroup.kill: " + err.Error())
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
	cmds, deadline, term := r.cmds, r.deadline, r.term
	if !abortable {
		cmds, deadline, term = nil, nil, nil
	}
	for {
		if cond() {
			return true, false
		}
		select {
		case e := <-r.exits:
			r.exited(e)
		case c, open := <-cmds:
			if !open {
				return false, true
			}
			who := ""
			if c.Op == OpAttach {
				who = WhoPerfAgent
			}
			r.fail(who, fmt.Sprintf("the capture is ending: %s ignored", c.Op))
		case <-deadline:
			return false, true
		case <-term:
			return false, true
		case <-timer.C:
			return cond(), false
		case <-tick.C:
		}
	}
}

// settle is the final cleanup, all within StopGrace. The sweep reaps,
// lists this process's children (adopted descendants included) and
// SIGKILLs them by verified PID, round after round, until the kernel
// reports no child left (ECHILD) and a fresh /proc scan agrees: only then
// is the tree quiescent. Then the cgroup must be proven empty, and both
// known children reaped. What is left becomes survivors, with the reasons
// in r.reasons.
//
// Survivors are listed only from what the reaper can see: its children
// and the cgroup. Without a cgroup, a descendant that is still a child of
// another descendant when the grace runs out (a fork chain racing the
// kills) makes the cleanup incomplete, but is not itself listed.
func (r *reaper) settle() []Survivor {
	grace := max(r.spec.StopGrace, settleFloor)
	end := time.Now().Add(grace)
	left, quiet := sweep(r.round, r.killChild, func(pid int) bool { return present(r.proc, pid, r.starts[pid]) }, grace, time.Now)

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
		r.reason(fmt.Sprintf("pid %d %s", pid, why))
	}
	for _, pid := range left {
		add(pid, "not reaped")
	}
	if !quiet {
		r.reason("the process tree was not quiescent at the stop grace")
	}
	if r.inCgroup {
		// populated in cgroup.events covers the whole subtree, so it is the
		// proof; an unreadable file proves nothing.
		var perr error
		empty := func() bool {
			p, err := cgroupPopulated(r.spec.Cgroup)
			perr = err
			return err == nil && !p
		}
		if ok, _ := r.waitUntil(end, false, empty); !ok {
			if perr != nil {
				r.reason(fmt.Sprintf("cannot prove cgroup %s empty: %v", r.spec.Cgroup, perr))
			} else {
				r.reason(fmt.Sprintf("cgroup %s still populated", r.spec.Cgroup))
			}
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
	switch {
	case r.subreaper != nil && r.inCgroup:
		r.reason("process group only for descendants outside the cgroup (no subreaper: " + r.subreaper.Error() + ")")
	case r.subreaper != nil:
		r.reason("process group only (no subreaper: " + r.subreaper.Error() + ")")
	}
	return survivors
}

// round is one sweep round: reap what has exited, handle the known
// children among it, then list the children left, zombies included. quiet
// is ECHILD from the reaping and an empty listing after it.
func (r *reaper) round() (pids []int, quiet bool) {
	got, none := r.reap()
	for _, e := range got {
		r.exited(e)
	}
	for drained := false; !drained; {
		select {
		case e := <-r.exits:
			r.exited(e)
		default:
			drained = true
		}
	}
	pids = r.children()
	return pids, none && len(pids) == 0
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
		r.reason(fmt.Sprintf("kill pid %d: %v", pid, err))
	}
	return err
}

// sweep runs children's rounds, SIGKILLing every listed process by PID
// (a zombie is harmless to signal, and its orphans are adopted as it
// exits, so it keeps the sweep going), until a round reports quiet or
// grace has passed. It returns the listed processes alive says are still
// alive when grace ran out, and whether the tree went quiet.
func sweep(children func() (pids []int, quiet bool), kill func(pid int, sig syscall.Signal) error, alive func(pid int) bool, grace time.Duration, now func() time.Time) (left []int, quiet bool) {
	end := now().Add(grace)
	for {
		pids, quiet := children()
		if quiet {
			return nil, true
		}
		for _, pid := range pids {
			_ = kill(pid, syscall.SIGKILL)
		}
		if !now().Before(end) {
			for _, pid := range pids {
				if alive(pid) {
					left = append(left, pid)
				}
			}
			return left, false
		}
		time.Sleep(sweepTick)
	}
}

// removeCgroup removes the capture's cgroup and any cgroup the workload
// made below it, depth first, before reaped is published, retrying
// briefly while the kernel still counts an exiting process. A failure is
// a reason. A cgroup this server would not have created is left alone,
// and noted.
func (r *reaper) removeCgroup() {
	cg := r.spec.Cgroup
	if !strings.HasPrefix(filepath.Base(cg), CgroupPrefix) {
		r.notes = append(r.notes, fmt.Sprintf("cgroup %s left in place: not a %s cgroup", cg, CgroupPrefix))
		return
	}
	var err error
	for range 10 {
		if err = RemoveCgroup(cg); err == nil || !errors.Is(err, syscall.EBUSY) {
			break
		}
		time.Sleep(sweepTick)
	}
	if err != nil {
		r.reason("remove cgroup: " + err.Error())
	}
}

// awaitKnown waits, without a bound, until both known children are
// reaped: the captures directory stays locked for their sake even after a
// reaped event that reported one as a survivor (say, stuck in D state).
// The server must not assume the lock is free once reaped arrives; it is
// free when the reaper exits, which is not before.
func (r *reaper) awaitKnown() {
	for (r.perf != nil && !r.perf.reaped) || (r.work != nil && !r.work.reaped) {
		r.exited(<-r.exits)
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
