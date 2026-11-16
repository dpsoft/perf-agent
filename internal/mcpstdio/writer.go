package mcpstdio

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// outQueue is the only writer of the client's stream. Producers enqueue
// whole encoded lines and never wait; one goroutine (run) writes them in
// order, so lines never interleave.
//
// maxMsgs and maxBytes bound the backlog (the lines not yet taken by a
// write). A response is refused only when the backlog has already reached
// a bound, so one line of any size fits behind a small backlog; a refused
// response marks the client dead. A notification is dropped instead,
// without harm, while the backlog is at half a bound or more. The
// dispatcher waits for the backlog to
// fall under half the bounds before it starts a request (room), so a live
// client is not declared dead for reading slowly. The client is dead too
// when a write fails, or when no write ends for stall while lines wait.
type outQueue struct {
	w        io.Writer
	maxMsgs  int
	maxBytes int
	stall    time.Duration

	wake chan struct{} // cap 1: the queue may have lines
	stop chan struct{} // closed by shutdown
	dead chan struct{} // closed once the client is dead

	mu        sync.Mutex
	q         [][]byte
	size      int
	writing   bool          // a Write is in progress
	closed    bool          // no more lines are accepted
	changed   chan struct{} // closed and replaced when a line is taken or a write ends
	writeErr  error         // the failed write that marked the client dead
	stallFrom time.Time     // the last write's end, or when the backlog stopped being empty
	stallT    *time.Timer
	stopOnce  sync.Once
}

func newOutQueue(w io.Writer, maxMsgs, maxBytes int, stall time.Duration) *outQueue {
	o := &outQueue{
		w: w, maxMsgs: max(maxMsgs, 1), maxBytes: max(maxBytes, 1), stall: stall,
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		dead:    make(chan struct{}),
		changed: make(chan struct{}),
	}
	o.stallT = time.AfterFunc(time.Hour, o.checkStall)
	o.stallT.Stop()
	return o
}

// enqueue adds a response and reports whether it was accepted. It never
// waits. A response refused because the backlog is at a bound marks the
// client dead.
func (o *outQueue) enqueue(line []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	if len(o.q) >= o.maxMsgs || o.size >= o.maxBytes {
		o.dieLocked(nil)
		return false
	}
	o.addLocked(line)
	return true
}

// enqueueNote adds a notification and reports whether it was accepted.
// Notifications are lossy: one is dropped while the backlog is at half a
// bound or more, and that never marks the client dead.
func (o *outQueue) enqueueNote(line []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.roomLocked() {
		return false
	}
	o.addLocked(line)
	return true
}

func (o *outQueue) addLocked(line []byte) {
	if len(o.q) == 0 {
		o.stallFrom = time.Now()
		o.stallT.Reset(o.stall)
	}
	o.q = append(o.q, line)
	o.size += len(line)
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// roomLocked reports whether the backlog is under half of both bounds.
func (o *outQueue) roomLocked() bool {
	return 2*len(o.q) < o.maxMsgs && 2*o.size < o.maxBytes
}

// room reports whether the backlog is under half of both bounds and, when
// it is not, returns a channel closed when it may have changed.
func (o *outQueue) room() (bool, <-chan struct{}) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.roomLocked(), o.changed
}

// checkStall runs on stallT: lines that waited stall with no write ending
// mean the client stopped reading.
func (o *outQueue) checkStall() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || len(o.q) == 0 {
		return
	}
	if left := o.stall - time.Since(o.stallFrom); left > 0 {
		o.stallT.Reset(left)
		return
	}
	o.dieLocked(nil)
}

func (o *outQueue) changedLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// dieLocked closes the queue and marks the client dead, once.
func (o *outQueue) dieLocked(err error) {
	o.closed = true
	o.stallT.Stop()
	select {
	case <-o.dead:
	default:
		o.writeErr = err
		close(o.dead)
	}
}

// run writes the queued lines until shutdown or a failed write. A Write in
// progress at shutdown finishes (or fails) on its own; nothing waits for it.
func (o *outQueue) run() {
	for {
		o.mu.Lock()
		if len(o.q) == 0 {
			o.mu.Unlock()
			select {
			case <-o.wake:
				continue
			case <-o.stop:
				return
			}
		}
		select {
		case <-o.stop:
			o.mu.Unlock()
			return
		default:
		}
		line := o.q[0]
		o.q[0] = nil
		o.q = o.q[1:]
		o.size -= len(line)
		o.writing = true
		o.changedLocked()
		o.mu.Unlock()

		_, err := o.w.Write(line)

		o.mu.Lock()
		o.writing = false
		o.changedLocked()
		o.stallFrom = time.Now()
		if len(o.q) > 0 {
			o.stallT.Reset(o.stall)
		}
		if err != nil {
			o.dieLocked(err)
			o.mu.Unlock()
			return
		}
		o.mu.Unlock()
	}
}

// drain waits until every accepted line is written, the client is dead, or
// deadline passes, and reports whether everything was written.
func (o *outQueue) drain(deadline time.Time) bool {
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	for {
		o.mu.Lock()
		select {
		case <-o.dead:
			o.mu.Unlock()
			return false
		default:
		}
		if len(o.q) == 0 && !o.writing {
			o.mu.Unlock()
			return true
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-o.dead:
			return false
		case <-t.C:
			return false
		}
	}
}

// shutdown refuses further lines and stops run before its next write.
func (o *outQueue) shutdown() {
	o.mu.Lock()
	o.closed = true
	o.stallT.Stop()
	o.mu.Unlock()
	o.stopOnce.Do(func() { close(o.stop) })
}

// goneErr is ErrClientGone, carrying the write error when one marked the
// client dead.
func (o *outQueue) goneErr() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.writeErr != nil {
		return fmt.Errorf("%w: %w", ErrClientGone, o.writeErr)
	}
	return ErrClientGone
}
