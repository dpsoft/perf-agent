package perfmcp

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// captureWindowSpan is the length of the rolling windows that bound the
// captures started and the duration requested (§6.1).
const captureWindowSpan = time.Hour

// registry holds the capture jobs of this server (§4.0, CAP-R11): the one
// active job, and the finished jobs and historical answers in an LRU
// bounded by Capture.MaxJobs and Capture.MaxJobBytes. It is used from the
// dispatcher, from the job goroutines and from the reader goroutine
// (capture_cancel is a control message), so mu guards the LRU, the id maps
// and the windows. mu is never held across I/O or a channel wait, and a
// job's own mutex is never taken while it is held. active is read without
// mu, and written only under it.
type registry struct {
	lim   CaptureLimits
	nonce string
	seq   atomic.Uint64
	// now is the clock reconcile reads; admit is given its time.
	now func() time.Time

	active atomic.Pointer[job]

	mu  sync.Mutex
	win window
	// res is the window reservation of each admitted job until it finishes.
	res map[*job]*reservation
	// lru holds *registryEntry, the most recently used first.
	lru       *list.List
	byJob     map[string]*list.Element
	byCapture map[string]*list.Element
	bytes     int
}

// registryEntry is a finished job, or a historical answer read from a
// manifest.
type registryEntry struct {
	j *job
	// answer is what a cancel of the finished job j is told.
	answer    string
	h         *historical
	jobID     string
	captureID string
	bytes     int
}

// newRegistry is an empty registry bounded by lim, whose job ids carry
// nonce so that ids from another server process never match.
func newRegistry(lim CaptureLimits, nonce string) *registry {
	return &registry{
		lim:       lim,
		nonce:     nonce,
		now:       time.Now,
		res:       map[*job]*reservation{},
		lru:       list.New(),
		byJob:     map[string]*list.Element{},
		byCapture: map[string]*list.Element{},
	}
}

// newJobID is the next job id, "j<N>-<nonce>".
func (r *registry) newJobID() string {
	return "j" + strconv.FormatUint(r.seq.Add(1), 10) + "-" + r.nonce
}

// admit gives j the one active slot and reserves j.reserve in the windows.
// A capture already active is busy; windows that cannot take j.reserve are
// budget, and then the slot is left as it was and nothing is reserved
// (PLAN-R3).
func (r *registry) admit(j *job, now time.Time) *capture.Error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.active.Load(); a != nil {
		e := capture.Errorf(capture.CodeBusy, "capture_wait or capture_cancel "+a.id,
			"capture job %s is running; one capture runs at a time", a.id)
		e.RetryAfter = max(a.created.Add(a.reserve).Sub(now), 0)
		return e
	}
	if e := r.win.allow(now, j.reserve, r.lim.WindowCaptures, r.lim.WindowDuration); e != nil {
		return e
	}
	r.res[j] = r.win.reserve(now, j.reserve)
	r.active.Store(j)
	return nil
}

// reconcile replaces j's reservation with its effective duration, once the
// job has resolved its settings. A duration the windows no longer take is
// budget, and j's reservation is then released: the capture will not run.
func (r *registry) reconcile(j *job, effective time.Duration) *capture.Error {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.res[j]
	if res == nil {
		return nil
	}
	if e := r.win.allowReplacing(now, res, effective, r.lim.WindowDuration); e != nil {
		r.win.release(res)
		delete(r.res, j)
		return e
	}
	r.win.replace(res, effective)
	return nil
}

// finish moves j, which has ended, from the slot to the LRU with
// resultBytes counted against MaxJobBytes, and evicts the least recently
// used entries over the bounds. The newest entry is always kept, so the
// wait that follows a job finds it even when its result alone is over
// MaxJobBytes.
func (r *registry) finish(j *job, resultBytes int) {
	j.mu.Lock()
	status, captureID := j.status, j.captureID
	j.mu.Unlock()
	answer := status.cancelAnswer()
	if !status.terminal() {
		// The job calls finish only once it has ended; one that has not is
		// answered as ended, rather than left looking cancellable.
		answer = cancelAnswerEnded
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active.Load() == j {
		r.active.Store(nil)
	}
	delete(r.res, j)
	if _, ok := r.byJob[j.id]; ok {
		return
	}
	if el, ok := r.byCapture[captureID]; ok && captureID != "" {
		r.remove(el)
	}
	r.push(&registryEntry{j: j, answer: answer, jobID: j.id, captureID: captureID, bytes: resultBytes})
}

// put keeps the historical answer for captureID, read from its manifest,
// unless a job of this server already answers for that id.
func (r *registry) put(captureID string, h *historical) {
	n := historicalBytes(h)
	if a := r.active.Load(); a != nil && a.captureIDIs(captureID) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.byCapture[captureID]; ok {
		if el.Value.(*registryEntry).j != nil {
			return
		}
		r.remove(el)
	}
	r.push(&registryEntry{h: h, captureID: captureID, bytes: n})
}

// get is the job with the job or capture id id, active or finished. A
// finished one becomes the most recently used.
func (r *registry) get(id string) (*job, bool) {
	if a := r.active.Load(); a != nil && (a.id == id || a.captureIDIs(id)) {
		return a, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.byJob[id]
	if !ok {
		el, ok = r.byCapture[id]
	}
	if !ok || el.Value.(*registryEntry).j == nil {
		return nil, false
	}
	r.lru.MoveToFront(el)
	return el.Value.(*registryEntry).j, true
}

// historical is the historical answer kept for captureID, which becomes
// the most recently used.
func (r *registry) historical(captureID string) (*historical, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.byCapture[captureID]
	if !ok || el.Value.(*registryEntry).h == nil {
		return nil, false
	}
	r.lru.MoveToFront(el)
	return el.Value.(*registryEntry).h, true
}

// list is the active job, then the finished jobs, the most recently used
// first. It changes no recency.
func (r *registry) list() []*job {
	r.mu.Lock()
	defer r.mu.Unlock()
	var js []*job
	if a := r.active.Load(); a != nil {
		js = append(js, a)
	}
	for el := r.lru.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*registryEntry); e.j != nil && !slices.Contains(js, e.j) {
			js = append(js, e.j)
		}
	}
	return js
}

// cancel asks the job with the job or capture id id to stop, and says what
// will happen; ok is false for an id this registry holds no job for. A
// finished job is answered from the registry with its final state, and is
// never touched. It runs on the control path: no I/O, never a wait.
func (r *registry) cancel(id string) (answer string, ok bool) {
	if a := r.active.Load(); a != nil && (a.id == id || a.captureIDIs(id)) {
		return a.requestCancel(), true
	}
	r.mu.Lock()
	el, ok := r.byJob[id]
	if !ok {
		el, ok = r.byCapture[id]
	}
	var e *registryEntry
	if ok {
		e = el.Value.(*registryEntry)
	}
	r.mu.Unlock()
	if e == nil || e.j == nil {
		return "", false
	}
	return e.answer, true
}

// push adds e as the most recently used entry and evicts the least
// recently used ones over MaxJobs or MaxJobBytes, never e itself. r.mu is
// held.
func (r *registry) push(e *registryEntry) {
	el := r.lru.PushFront(e)
	if e.jobID != "" {
		r.byJob[e.jobID] = el
	}
	if e.captureID != "" {
		r.byCapture[e.captureID] = el
	}
	r.bytes += e.bytes
	for r.lru.Len() > 1 && (r.lru.Len() > r.lim.MaxJobs || r.bytes > r.lim.MaxJobBytes) {
		r.remove(r.lru.Back())
	}
}

// remove drops el from the LRU and the id maps. r.mu is held.
func (r *registry) remove(el *list.Element) {
	e := r.lru.Remove(el).(*registryEntry)
	if e.jobID != "" && r.byJob[e.jobID] == el {
		delete(r.byJob, e.jobID)
	}
	if e.captureID != "" && r.byCapture[e.captureID] == el {
		delete(r.byCapture, e.captureID)
	}
	r.bytes -= e.bytes
}

// captureIDIs reports whether id is j's capture id, once it has one.
func (j *job) captureIDIs(id string) bool {
	if id == "" {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.captureID == id
}

// historicalBytes is what a historical answer counts against MaxJobBytes:
// the size of its manifests as JSON.
func historicalBytes(h *historical) int {
	n := len(h.baseGone)
	for _, m := range []*manifest{&h.m, h.base} {
		if m == nil {
			continue
		}
		if b, err := json.Marshal(m); err == nil {
			n += len(b)
		}
	}
	return n
}

// window is the rolling record of the captures admitted in the last
// captureWindowSpan, each with the duration reserved for it.
type window struct {
	rs []*reservation
}

// reservation is one capture's charge to the windows.
type reservation struct {
	at time.Time
	d  time.Duration
}

// prune drops the reservations that have left the window at now.
func (w *window) prune(now time.Time) {
	w.rs = slices.DeleteFunc(w.rs, func(r *reservation) bool {
		return !now.Before(r.at.Add(captureWindowSpan))
	})
}

// allow is nil when one more capture of duration d fits: at most
// maxCaptures captures and maxDuration of duration in the window. When it
// does not, it is budget with the time at which enough of the window will
// have freed.
func (w *window) allow(now time.Time, d time.Duration, maxCaptures int, maxDuration time.Duration) *capture.Error {
	w.prune(now)
	if d > maxDuration || maxCaptures <= 0 {
		return overWindow(d, maxDuration)
	}
	byAge := w.byAge(nil)
	var e *capture.Error
	if n := len(byAge) + 1 - maxCaptures; n > 0 {
		e = budgetError(fmt.Sprintf("%d captures were started in the last hour, the limit is %d (Capture.WindowCaptures)",
			len(byAge), maxCaptures), byAge[n-1].at.Add(captureWindowSpan).Sub(now))
	}
	if t, ok := freedBy(byAge, used(byAge)+d-maxDuration); ok && (e == nil || t.Sub(now) > e.RetryAfter) {
		e = budgetError(fmt.Sprintf("%v of capture time was requested in the last hour; %v more is over the limit of %v (Capture.WindowDuration)",
			used(byAge), d, maxDuration), t.Sub(now))
	}
	return e
}

// allowReplacing is allow for changing res's duration to d: the capture
// count does not change.
func (w *window) allowReplacing(now time.Time, res *reservation, d, maxDuration time.Duration) *capture.Error {
	w.prune(now)
	if d > maxDuration {
		return overWindow(d, maxDuration)
	}
	others := w.byAge(res)
	if t, ok := freedBy(others, used(others)+d-maxDuration); ok {
		return budgetError(fmt.Sprintf("the effective duration %v, with %v requested by other captures in the last hour, is over the limit of %v (Capture.WindowDuration)",
			d, used(others), maxDuration), max(t.Sub(now), 0))
	}
	return nil
}

// reserve records a capture of duration d admitted at now.
func (w *window) reserve(now time.Time, d time.Duration) *reservation {
	r := &reservation{at: now, d: d}
	w.rs = append(w.rs, r)
	return r
}

// replace charges old with the duration d instead.
func (w *window) replace(old *reservation, d time.Duration) {
	old.d = d
}

// release removes res from the window, as if its capture had never been
// admitted.
func (w *window) release(res *reservation) {
	w.rs = slices.DeleteFunc(w.rs, func(r *reservation) bool { return r == res })
}

// byAge is the reservations other than except, the oldest first.
func (w *window) byAge(except *reservation) []*reservation {
	rs := slices.DeleteFunc(slices.Clone(w.rs), func(r *reservation) bool { return r == except })
	slices.SortStableFunc(rs, func(a, b *reservation) int { return a.at.Compare(b.at) })
	return rs
}

// used is the total duration of rs.
func used(rs []*reservation) time.Duration {
	var t time.Duration
	for _, r := range rs {
		t += r.d
	}
	return t
}

// freedBy is when, as the oldest of rs leave the window, need of duration
// will have left it; ok is false when need is not positive.
func freedBy(rs []*reservation, need time.Duration) (time.Time, bool) {
	if need <= 0 {
		return time.Time{}, false
	}
	var freed time.Duration
	for _, r := range rs {
		if freed += r.d; freed >= need {
			return r.at.Add(captureWindowSpan), true
		}
	}
	// Unreachable while d ≤ maxDuration: freeing all of rs leaves room.
	return rs[len(rs)-1].at.Add(captureWindowSpan), true
}

// budgetError is a window refusal that frees after wait.
func budgetError(why string, wait time.Duration) *capture.Error {
	e := capture.Errorf(capture.CodeBudget, "retry after "+wait.String(), "%s", why)
	e.RetryAfter = wait
	return e
}

// overWindow refuses a duration no window could ever take.
func overWindow(d, maxDuration time.Duration) *capture.Error {
	e := capture.Errorf(capture.CodeBudget, "ask for a shorter duration",
		"a capture of %v is over the window's whole limit of %v (Capture.WindowDuration)", d, maxDuration)
	e.Retryable = false
	return e
}

// errLookupClosed is the answer of a lookup the pool was closed before it
// ran.
var errLookupClosed = errors.New("the lookup pool is closed")

// lookupPool reads historical answers from disk on a fixed number of
// workers with a bounded queue (CAP-R14). A lookup for a key already
// queued or running is shared, and a caller waits on it only to its own
// deadline: no goroutine is started per lookup or per wait.
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
