package perfmcp

import (
	"container/list"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

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
