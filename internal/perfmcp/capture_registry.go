package perfmcp

import (
	"cmp"
	"container/list"
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

// maxJobAliases bounds the finished job ids the registry remembers after
// their entries have left the LRU. An alias is a job id, its capture id
// and its final answer: a few dozen bytes.
const maxJobAliases = 4096

// Misuse of the registry by its callers. None reaches an agent as a capture
// error: each means the caller is wrong.
var (
	errNoReservation = errors.New("the job holds no window reservation")
	errNotAdmitted   = errors.New("the job was never admitted")
	errNotFinished   = errors.New("the job has not finished")
	errNegative      = errors.New("a negative size or duration")
)

// registry holds the capture jobs of this server (§4.0, CAP-R11): the one
// active job, and the finished jobs and historical answers in an LRU
// bounded by Capture.MaxJobs and Capture.MaxJobBytes. It is used from the
// dispatcher, from the job goroutines and from the reader goroutine
// (capture_cancel is a control message), so mu guards the LRU, the id maps,
// the aliases and the windows. mu is never held across I/O or a channel
// wait, and a job's own mutex is never taken while it is held. active is
// read without mu, and written only under it. Every method that reads the
// time is given it, so the registry has one clock: its caller's.
type registry struct {
	lim   CaptureLimits
	nonce string
	seq   atomic.Uint64

	active atomic.Pointer[job]

	mu  sync.Mutex
	win window
	// res is the window reservation of the active job, until it finishes
	// or a reconcile refuses it.
	res map[*job]*reservation
	// lru holds *registryEntry, the most recently used first.
	lru       *list.List
	byJob     map[string]*list.Element
	byCapture map[string]*list.Element
	bytes     int
	// aliases outlive the LRU: a job id, and its capture id through
	// captureAliases, still resolve to the job's capture id and final
	// answer after its entry is evicted. aliasRing holds the job ids oldest
	// first from aliasNext, to forget the oldest past maxJobAliases.
	aliases        map[string]jobAlias
	captureAliases map[string]string
	aliasRing      []string
	aliasNext      int
}

// jobAlias is what is remembered of a finished job: its id, its capture
// id, "" when it ended before it had one, what a cancel of it is told, and
// the code it failed with, "" when it did not fail.
type jobAlias struct {
	jobID, captureID, answer string
	code                     capture.Code
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
		lim:            lim,
		nonce:          nonce,
		res:            map[*job]*reservation{},
		lru:            list.New(),
		byJob:          map[string]*list.Element{},
		byCapture:      map[string]*list.Element{},
		aliases:        map[string]jobAlias{},
		captureAliases: map[string]string{},
		aliasRing:      make([]string, maxJobAliases),
	}
}

// newJobID is the next job id, "j<N>-<nonce>".
func (r *registry) newJobID() string {
	return "j" + strconv.FormatUint(r.seq.Add(1), 10) + "-" + r.nonce
}

// admit gives j the one active slot and reserves j.reserve in the windows.
// A capture already active is busy, with retry_after the time to its
// reserved end, from 1 s to 5 s, and next naming that end; windows that cannot take j.reserve are budget, and then the
// slot is left as it was and nothing is reserved (PLAN-R3).
func (r *registry) admit(j *job, now time.Time) *capture.Error {
	if j.reserve < 0 {
		return capture.Errorf(capture.CodeInvalidArgument, "", "a capture cannot reserve %v", j.reserve)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.active.Load(); a != nil {
		d := a.reserve
		if res := r.res[a]; res != nil {
			d = res.d
		}
		end := a.created.Add(d)
		e := capture.Errorf(capture.CodeBusy,
			fmt.Sprintf("capture_wait or capture_cancel %s; its reserved duration ends by %s", a.id, end.UTC().Format(time.RFC3339)),
			"capture job %s is running; one capture runs at a time", a.id)
		e.RetryAfter = capture.BusyRetry(end.Sub(now))
		return e
	}
	if e := r.win.allow(now, j.reserve, r.lim.WindowCaptures, r.lim.WindowDuration); e != nil {
		return e
	}
	r.res[j] = r.win.reserve(now, j.reserve)
	r.active.Store(j)
	return nil
}

// reconcile replaces the active job j's reservation with its effective
// duration, once the job has resolved its settings. A duration the windows
// no longer take is a *capture.Error with the code budget, and j's
// reservation is then released: the capture will not run. A job without a
// reservation (never admitted, finished, or already refused) is
// errNoReservation.
func (r *registry) reconcile(j *job, effective time.Duration, now time.Time) error {
	if effective < 0 {
		return errNegative
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.res[j]
	if res == nil {
		return fmt.Errorf("reconcile %s: %w", j.id, errNoReservation)
	}
	if e := r.win.allowReplacing(now, res, effective, r.lim.WindowDuration); e != nil {
		r.win.release(res)
		delete(r.res, j)
		return e
	}
	r.win.replace(res, effective)
	return nil
}

// finish moves the active job j, which has ended, from the slot to the
// LRU, with resultBytes counted against MaxJobBytes. In the same step it
// charges the windows with what was profiled, at most the reservation:
// a capture that never profiled charges no duration but still counts as
// a capture started (CAP-R59). It never refuses for the windows.
//
// The newest entry is always kept, so the wait that follows a job finds it
// even when its result alone is over MaxJobBytes. A second finish of j is
// a no-op; a job that was never admitted, or negative sizes, are errors
// that change nothing.
func (r *registry) finish(j *job, resultBytes int, profiled time.Duration) error {
	if resultBytes < 0 || profiled < 0 {
		return fmt.Errorf("finish %s with %d bytes, %v profiled: %w", j.id, resultBytes, profiled, errNegative)
	}
	answer, captureID, code := finalAnswer(j)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active.Load() != j {
		if r.finished(j.id) {
			return nil
		}
		return fmt.Errorf("finish %s: %w", j.id, errNotAdmitted)
	}
	if res := r.res[j]; res != nil {
		r.win.replace(res, min(profiled, res.d))
		delete(r.res, j)
	}
	r.active.Store(nil)
	if el, ok := r.byCapture[captureID]; ok && captureID != "" {
		r.remove(el)
	}
	el := r.lru.PushFront(&registryEntry{j: j, answer: answer, jobID: j.id, captureID: captureID, bytes: resultBytes})
	r.index(el)
	r.bytes += resultBytes
	r.remember(jobAlias{jobID: j.id, captureID: captureID, answer: answer, code: code})
	r.evict(el)
	return nil
}

// update records a finished job's state again after it changed, as when a
// commit that ended undecided is settled later: its answer, its capture id
// and the bytes of its result. A job evicted since keeps only its alias,
// which is corrected. The job must have ended.
func (r *registry) update(j *job, resultBytes int) error {
	if resultBytes < 0 {
		return fmt.Errorf("update %s with %d bytes: %w", j.id, resultBytes, errNegative)
	}
	j.mu.Lock()
	terminal := j.status.terminal()
	j.mu.Unlock()
	if !terminal {
		return fmt.Errorf("update %s: %w", j.id, errNotFinished)
	}
	answer, captureID, code := finalAnswer(j)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished(j.id) {
		return fmt.Errorf("update %s: %w", j.id, errNotAdmitted)
	}
	if _, ok := r.aliases[j.id]; ok {
		r.aliases[j.id] = jobAlias{jobID: j.id, captureID: captureID, answer: answer, code: code}
		if captureID != "" {
			r.captureAliases[captureID] = j.id
		}
	}
	el, ok := r.byJob[j.id]
	if !ok {
		return nil
	}
	e := el.Value.(*registryEntry)
	if e.captureID != captureID {
		if old, ok := r.byCapture[captureID]; ok && captureID != "" {
			r.remove(old)
		}
		if r.byCapture[e.captureID] == el {
			delete(r.byCapture, e.captureID)
		}
		e.captureID = captureID
		r.index(el)
	}
	e.answer = answer
	r.bytes += resultBytes - e.bytes
	e.bytes = resultBytes
	r.evict(el)
	return nil
}

// put keeps the historical answer for captureID, read from its manifest,
// unless a job of this server already answers for that id; that is not an
// error.
func (r *registry) put(captureID string, h *historical) error {
	_, err := r.putIf(captureID, h, nil)
	return err
}

// putIf is put, unless fresh, checked under the registry's lock, says the
// answer went stale while it was read; kept is false then. A forget takes the same lock, so a
// removal that changes what fresh checks before its forget either sees
// the answer kept, and forgets it, or keeps it from being kept.
func (r *registry) putIf(captureID string, h *historical, fresh func() bool) (kept bool, err error) {
	if captureID == "" || h == nil {
		return false, fmt.Errorf("put %q: no capture id or no answer", captureID)
	}
	n := historicalBytes(h)
	if a := r.active.Load(); a != nil && a.captureIDIs(captureID) {
		return true, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fresh != nil && !fresh() {
		return false, nil
	}
	if el, ok := r.byCapture[captureID]; ok {
		if el.Value.(*registryEntry).j != nil {
			return true, nil
		}
		r.remove(el)
	}
	el := r.lru.PushFront(&registryEntry{h: h, captureID: captureID, bytes: n})
	r.index(el)
	r.bytes += n
	r.evict(el)
	return true, nil
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

// alias is the capture id of the finished job jobID, "" when it ended
// before it had one, also after its entry has been evicted; ok is false for
// a job id this registry does not remember.
func (r *registry) alias(jobID string) (captureID string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.aliases[jobID]
	return a.captureID, ok
}

// final is the alias of the finished job with the job or capture id id,
// which outlives its entry; ok is false for an id this registry does not
// remember.
func (r *registry) final(id string) (jobAlias, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.aliasOf(id)
}

// aliasOf is the alias of the job or capture id id. r.mu is held.
func (r *registry) aliasOf(id string) (jobAlias, bool) {
	if a, ok := r.aliases[id]; ok {
		return a, true
	}
	if jobID, ok := r.captureAliases[id]; ok {
		a, ok := r.aliases[jobID]
		return a, ok
	}
	return jobAlias{}, false
}

// forget drops the entry of captureID, a finished job's or a historical
// answer, once retention has removed the capture: a later wait reads the
// disk and finds it gone. The job's alias stays, since its final answer is
// still true; a job whose commit is undecided is kept.
func (r *registry) forget(captureID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if el, ok := r.byCapture[captureID]; ok && el.Value.(*registryEntry).answer != cancelAnswerPending {
		r.remove(el)
	}
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
// finished job is answered from the registry with its final state, from
// its entry or, once that is evicted, its alias, and is never touched. It
// runs on the control path: no I/O, never a wait.
func (r *registry) cancel(id string) (answer string, ok bool) {
	if a := r.active.Load(); a != nil && (a.id == id || a.captureIDIs(id)) {
		return a.requestCancel(), true
	}
	answer, pending, ok := r.finalOf(id)
	if pending != nil {
		// A commit nobody could read yet: the job records the cancel,
		// outside r.mu, and the settle reports it.
		return pending.requestCancel(), true
	}
	return answer, ok
}

// finalOf is what a cancel of the finished job or capture id is told, from
// its entry or its alias; pending is the job when its commit is undecided,
// which the cancel must reach.
func (r *registry) finalOf(id string) (answer string, pending *job, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	el, ok := r.byJob[id]
	if !ok {
		el, ok = r.byCapture[id]
	}
	if ok && el.Value.(*registryEntry).j != nil {
		e := el.Value.(*registryEntry)
		if e.answer == cancelAnswerPending {
			return "", e.j, true
		}
		return e.answer, nil, true
	}
	if a, ok := r.aliasOf(id); ok {
		return a.answer, nil, true
	}
	return "", nil, false
}

// finalAnswer is what a cancel of the finished job j is told, its capture
// id, and the code it failed with. A job leaves the slot before it has
// ended only when its commit is undecided: it may have committed, so the
// answer is that the decision is pending until update records how it was
// settled.
func finalAnswer(j *job) (answer, captureID string, code capture.Code) {
	j.mu.Lock()
	defer j.mu.Unlock()
	code = j.endCode
	if j.err != nil {
		code = cmp.Or(code, j.err.Code)
	}
	if j.ending.terminal() {
		return j.ending.cancelAnswer(), j.captureID, code
	}
	if !j.status.terminal() {
		return cancelAnswerPending, j.captureID, ""
	}
	return j.status.cancelAnswer(), j.captureID, code
}

// finished reports whether the job jobID has finished here, by its entry
// or its alias. r.mu is held.
func (r *registry) finished(jobID string) bool {
	_, inLRU := r.byJob[jobID]
	_, aliased := r.aliases[jobID]
	return inLRU || aliased
}

// index adds el to the id maps. r.mu is held.
func (r *registry) index(el *list.Element) {
	e := el.Value.(*registryEntry)
	if e.jobID != "" {
		r.byJob[e.jobID] = el
	}
	if e.captureID != "" {
		r.byCapture[e.captureID] = el
	}
}

// evict removes entries while the LRU is over MaxJobs or MaxJobBytes,
// never keep. Historical answers go first, the least recently used first:
// one can always be read from its manifest again, while a job's derived
// result exists only here (CAP-R60). Then jobs, the least recently used
// first. A job whose commit is undecided is never evicted, as the active
// one is not: a cancel must still reach it until it is settled. r.mu is
// held.
func (r *registry) evict(keep *list.Element) {
	for r.lru.Len() > 1 && (r.lru.Len() > r.lim.MaxJobs || r.bytes > r.lim.MaxJobBytes) {
		var victim, oldestJob *list.Element
		for el := r.lru.Back(); el != nil; el = el.Prev() {
			if el == keep || el.Value.(*registryEntry).answer == cancelAnswerPending {
				continue
			}
			if el.Value.(*registryEntry).j == nil {
				victim = el
				break
			}
			if oldestJob == nil {
				oldestJob = el
			}
		}
		if victim == nil {
			victim = oldestJob
		}
		if victim == nil {
			return
		}
		r.remove(victim)
	}
}

// remove drops el from the LRU and the id maps; a job's alias stays. r.mu
// is held.
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

// remember records the alias of a finished job, by its job id and its
// capture id, forgetting the oldest past maxJobAliases. r.mu is held.
func (r *registry) remember(a jobAlias) {
	if old := r.aliasRing[r.aliasNext]; old != "" {
		if c := r.aliases[old].captureID; c != "" && r.captureAliases[c] == old {
			delete(r.captureAliases, c)
		}
		delete(r.aliases, old)
	}
	r.aliasRing[r.aliasNext] = a.jobID
	r.aliasNext = (r.aliasNext + 1) % len(r.aliasRing)
	r.aliases[a.jobID] = a
	if a.captureID != "" {
		r.captureAliases[a.captureID] = a.jobID
	}
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
