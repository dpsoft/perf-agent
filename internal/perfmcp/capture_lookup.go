package perfmcp

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// errLookupClosed is the answer of a lookup the pool was closed before it
// ran.
var errLookupClosed = errors.New("the lookup pool is closed")

// lookupPool reads historical answers from disk on a fixed number of
// workers with a bounded queue (CAP-R14). A lookup for a key already
// queued or running is shared, and a caller waits on it only to its own
// deadline: no goroutine is started per lookup or per wait.
//
// A load has no timeout of its own: a read that never returns holds its
// worker for good, and with every worker held the queue fills and get
// answers (nil, false), which callers report as "loading". The loads are
// small bounded reads of manifests (readManifest reads at most 1 MiB), so
// a stuck one means a stuck filesystem, which a timeout here could not
// free: Go cannot interrupt a blocked read.
type lookupPool struct {
	queue     chan *lookup
	quit      chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	pending map[string]*lookup
	closed  bool
}

// lookup is one historical read, shared by everyone who asked for its key.
// done is closed once val and err are set.
type lookup struct {
	key  string
	load func() (any, error)
	done chan struct{}
	val  any
	err  error
}

// newLookupPool starts workers goroutines reading from a queue of queue
// lookups.
func newLookupPool(workers, queue int) *lookupPool {
	p := &lookupPool{
		queue:   make(chan *lookup, queue),
		quit:    make(chan struct{}),
		pending: map[string]*lookup{},
	}
	for range workers {
		go p.work()
	}
	return p
}

// get is the lookup of key, the pending one when there is one, or a new
// one that runs load. It is (nil, false) when the queue is full or the
// pool is closed; the caller answers "loading" with a retry_after.
func (p *lookupPool) get(key string, load func() (any, error)) (*lookup, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false
	}
	if l, ok := p.pending[key]; ok {
		return l, true
	}
	l := &lookup{key: key, load: load, done: make(chan struct{})}
	select {
	case p.queue <- l:
		p.pending[key] = l
		return l, true
	default:
		return nil, false
	}
}

// close stops the workers once their current lookup returns and fails the
// queued ones with errLookupClosed. It does not wait for a running load.
func (p *lookupPool) close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		close(p.quit)
		for {
			select {
			case l := <-p.queue:
				p.complete(l, nil, errLookupClosed)
			default:
				return
			}
		}
	})
}

// work runs queued lookups until the pool is closed.
func (p *lookupPool) work() {
	for {
		select {
		case <-p.quit:
			return
		case l := <-p.queue:
			select {
			case <-p.quit:
				p.complete(l, nil, errLookupClosed)
			default:
				v, err := runLoad(l.load)
				p.complete(l, v, err)
			}
		}
	}
}

// complete records l's answer, forgets it as pending and wakes its waiters.
func (p *lookupPool) complete(l *lookup, v any, err error) {
	l.val, l.err = v, err
	p.mu.Lock()
	if p.pending[l.key] == l {
		delete(p.pending, l.key)
	}
	p.mu.Unlock()
	close(l.done)
}

// runLoad runs load, turning a panic into an error so the worker survives.
func runLoad(load func() (any, error)) (v any, err error) {
	defer func() {
		if x := recover(); x != nil {
			v, err = nil, fmt.Errorf("lookup panicked: %v", x)
		}
	}()
	return load()
}

// wait waits for l's answer until ctx is done or d has passed, whichever
// is first; ready is false when it has not come by then, and the lookup
// goes on.
func (l *lookup) wait(ctx context.Context, d time.Duration) (val any, ready bool, err error) {
	select {
	case <-l.done:
		return l.val, true, l.err
	default:
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-l.done:
		return l.val, true, l.err
	case <-ctx.Done():
	case <-t.C:
	}
	return nil, false, nil
}
