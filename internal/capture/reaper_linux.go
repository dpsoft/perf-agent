package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	// ended is closed when the event pipe reaches EOF: the reaper closes
	// it after it has let go of the captures lock.
	ended chan struct{}
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
	return startReaper(self, spec, f, nil)
}

// startReaper is StartReaper with more descriptors from fd 11 on, which a
// test uses to check that the reaper closes strays.
func startReaper(self string, spec ReaperSpec, f ReaperFiles, more []*os.File) (*Reaper, error) {
	if !filepath.IsAbs(self) {
		return nil, fmt.Errorf("the server's own path %q is not absolute", self)
	}
	if spec.Deadline <= 0 {
		return nil, errors.New("a reaper needs a positive deadline")
	}
	if spec.StopGrace < 0 {
		return nil, errors.New("a reaper's stop grace cannot be negative")
	}
	// No slot is left empty: a closed one would be taken by the reaper's
	// runtime for its own files (cpu.max, an eventfd) before the reaper
	// runs, and look inherited. A missing file is the null device, and the
	// wire says which slots carry a real one.
	slots := []*os.File{f.Lock, f.PerfOut, f.PerfErr, f.WorkOut, f.WorkErr}
	w := reaperWire{Spec: spec}
	for i, sf := range slots {
		if sf != nil {
			w.Present |= 1 << i
			continue
		}
		null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		defer func() { _ = null.Close() }()
		slots[i] = null
	}
	b, err := json.Marshal(w)
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
	extra := append(append([]*os.File{specR, cmdR, evW}, slots...), more...)
	proc, err := startReaperProcess(self, extra)
	// The reaper has its copies; holding these would hide its EOFs.
	closeAll(specR, cmdR, evW)
	if err != nil {
		closeAll(specW, cmdW, evR)
		return nil, fmt.Errorf("start the reaper: %w", err)
	}
	r := &Reaper{proc: proc, cmds: cmdW, events: make(chan Event, eventBuffer), done: make(chan struct{}), ended: make(chan struct{})}
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

// readEvents decodes the event pipe until the reaper closes it.
func (r *Reaper) readEvents(f *os.File) {
	defer close(r.ended)
	defer close(r.events)
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	for {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			return
		}
		r.events <- ev
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

// Ended is closed when the reaper has closed its event pipe, which it
// does last, after it has let go of the captures lock.
func (r *Reaper) Ended() <-chan struct{} { return r.ended }

// Done is closed when the reaper process has exited.
func (r *Reaper) Done() <-chan struct{} { return r.done }

// PID is the reaper's process ID.
func (r *Reaper) PID() int { return r.proc.pid }

// Kill is the server's emergency end: it closes the command pipe, which
// the reaper reads as EOF and answers with SIGKILL to both children, the
// cgroup kill or the sweep, and the survivor report, before it exits.
// Kill signals nothing itself. A process group number the server heard of
// may already have been reaped and reused by the time Kill runs; only the
// reaper, which reaps, knows when a number is still its child's. Nor does
// Kill signal the reaper: killing it would leave setsid'd descendants that
// nobody sweeps. Kill takes no lock, does not block, and is safe from any
// goroutine and more than once.
func (r *Reaper) Kill() { r.closeCmds() }

// closeCommands closes the command pipe without killing anything, as a
// dead server would.
func (r *Reaper) closeCommands() { r.closeCmds() }
