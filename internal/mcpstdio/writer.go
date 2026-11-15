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
// The backlog is bounded by maxMsgs and maxBytes, but a line always fits an
// empty queue, so one large answer is not mistaken for a dead client. A
// line that does not fit, or a failed write, marks the client dead: the
// queue closes and dead is closed.
type outQueue struct {
	w        io.Writer
	maxMsgs  int
	maxBytes int

	wake chan struct{} // cap 1: the queue may have lines
	stop chan struct{} // closed by shutdown
	dead chan struct{} // closed once the client is dead

	mu       sync.Mutex
	q        [][]byte
	size     int
	writing  bool          // a Write is in progress
	closed   bool          // no more lines are accepted
	idle     chan struct{} // closed and replaced each time a Write ends
	writeErr error         // the failed write that marked the client dead
	stopOnce sync.Once
}

func newOutQueue(w io.Writer, maxMsgs, maxBytes int) *outQueue {
	return &outQueue{
		w: w, maxMsgs: maxMsgs, maxBytes: maxBytes,
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		dead: make(chan struct{}),
		idle: make(chan struct{}),
	}
}

// enqueue adds one line and reports whether it was accepted. It never
// waits. A line that does not fit marks the client dead.
func (o *outQueue) enqueue(line []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return false
	}
	if len(o.q) > 0 && (len(o.q) >= o.maxMsgs || o.size+len(line) > o.maxBytes) {
		o.dieLocked(nil)
		return false
	}
	o.q = append(o.q, line)
	o.size += len(line)
	select {
	case o.wake <- struct{}{}:
	default:
	}
	return true
}

// dieLocked closes the queue and marks the client dead, once.
func (o *outQueue) dieLocked(err error) {
	o.closed = true
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
		o.mu.Unlock()

		_, err := o.w.Write(line)

		o.mu.Lock()
		o.writing = false
		close(o.idle)
		o.idle = make(chan struct{})
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
		idle := o.idle
		o.mu.Unlock()
		select {
		case <-idle:
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
