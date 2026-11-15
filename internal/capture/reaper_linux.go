package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
)

// Reaper is the server's handle on a capture's reaper: a detached process
// in its own session that starts perf-agent and the workload itself, and
// kills them on EOF of its command pipe, at its absolute deadline, or on
// cancel. The server only asks, and hears PIDs back as events.
type Reaper struct {
	proc   *child
	cmds   *os.File
	sendMu sync.Mutex
	// closeCmds closes the command pipe once: the reaper reads it as EOF.
	closeCmds func()
	events    chan Event
	done      chan struct{}
	// The process groups the events reported, cleared when their leader's
	// exit is reported. Kill reads them without a lock.
	perfPGID, workPGID atomic.Int64
}

// eventBuffer bounds the events queued for a server that is not reading;
// a reaper publishes a handful per capture.
const eventBuffer = 64

// StartReaper starts self in reaper mode for spec, handing it f's files
// and its own pipes (see ReaperFiles for the files; the caller still owns
// and closes them). The reaper's environment is exactly InternalEnv=reaper,
// it is in its own session, has no parent-death signal (it must outlive a
// dead server) and its stdio is the null device. A goroutine waits for it.
func StartReaper(self string, spec ReaperSpec, f ReaperFiles) (*Reaper, error) {
	if !filepath.IsAbs(self) {
		return nil, fmt.Errorf("the server's own path %q is not absolute", self)
	}
	if spec.Deadline <= 0 {
		return nil, errors.New("a reaper needs a positive deadline")
	}
	if spec.StopGrace < 0 {
		return nil, errors.New("a reaper's stop grace cannot be negative")
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	var ends [6]*os.File // spec r/w, commands r/w, events r/w
	closeAll := func(fs ...*os.File) {
		for _, f := range fs {
			if f != nil {
				_ = f.Close()
			}
		}
	}
	for i := 0; i < len(ends); i += 2 {
		if ends[i], ends[i+1], err = os.Pipe(); err != nil {
			closeAll(ends[:]...)
			return nil, err
		}
	}
	specR, specW, cmdR, cmdW, evR, evW := ends[0], ends[1], ends[2], ends[3], ends[4], ends[5]
	// fd 3 the spec, 4 commands, 5 events, 6 the lock, 7 and 8 perf-agent's
	// output, 9 and 10 the workload's.
	extra := []*os.File{specR, cmdR, evW, f.Lock, f.PerfOut, f.PerfErr, f.WorkOut, f.WorkErr}
	proc, err := startReaperProcess(self, extra)
	// The reaper has its copies; holding these would hide its EOFs.
	closeAll(specR, cmdR, evW)
	if err != nil {
		closeAll(specW, cmdW, evR)
		return nil, fmt.Errorf("start the reaper: %w", err)
	}
	r := &Reaper{proc: proc, cmds: cmdW, events: make(chan Event, eventBuffer), done: make(chan struct{})}
	r.closeCmds = sync.OnceFunc(func() { _ = cmdW.Close() })
	go r.readEvents(evR)
	go func() {
		_ = proc.wait()
		r.closeCmds()
		close(r.done)
	}()
	_, werr := specW.Write(b)
	if err := errors.Join(werr, specW.Close()); err != nil {
		// A reaper without its whole spec exits by itself; closing its
		// command pipe makes sure it does not wait.
		r.closeCmds()
		return nil, fmt.Errorf("send the reaper its spec: %w", err)
	}
	return r, nil
}

// readEvents decodes the event pipe until the reaper closes it, recording
// the process groups Kill needs before passing each event on.
func (r *Reaper) readEvents(f *os.File) {
	defer close(r.events)
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			return
		}
		r.record(ev)
		r.events <- ev
	}
}

func (r *Reaper) record(ev Event) {
	g := &r.perfPGID
	if ev.Who == WhoWorkload {
		g = &r.workPGID
	}
	switch ev.Op {
	case EventLaunched, EventAttached:
		if ev.PGID > 1 {
			g.Store(int64(ev.PGID))
		}
	case EventExited:
		// The reaper has reaped the leader, so the number may be reused;
		// what is left of the group is the reaper's to clean up.
		g.CompareAndSwap(int64(ev.PGID), 0)
	}
}

// Send writes one command. It fails once the command pipe is closed.
func (r *Reaper) Send(c Command) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	_, err = r.cmds.Write(append(b, '\n'))
	return err
}

// Events delivers the reaper's events in order and is closed when the
// reaper closes its event pipe; reaped is the last.
func (r *Reaper) Events() <-chan Event { return r.events }

// Done is closed when the reaper process has exited.
func (r *Reaper) Done() <-chan struct{} { return r.done }

// PID is the reaper's process ID.
func (r *Reaper) PID() int { return r.proc.pid }

// Kill is the server's emergency end: one SIGKILL per recorded process
// group, then the command pipe is closed. It never signals the reaper,
// which reads the EOF and runs its cleanup (cgroup.kill, or the sweep of
// adopted descendants), reports survivors and only then exits; killing it
// would leave setsid'd descendants nobody sweeps. Kill takes no lock and
// does not block, and is safe from any goroutine and more than once.
func (r *Reaper) Kill() {
	for _, g := range []*atomic.Int64{&r.perfPGID, &r.workPGID} {
		if pgid := int(g.Load()); pgid > 1 && pgid != r.proc.pid {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	r.closeCmds()
}

// closeCommands closes the command pipe without killing anything, as a
// dead server would.
func (r *Reaper) closeCommands() { r.closeCmds() }
