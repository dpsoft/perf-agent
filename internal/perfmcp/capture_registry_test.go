package perfmcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// testRegistryLimits are the default capture limits with the given changes.
func testRegistryLimits(change func(*CaptureLimits)) CaptureLimits {
	lim := defaultCaptureLimits(profilemodel.DefaultLimits())
	if change != nil {
		change(&lim)
	}
	return lim
}

var regT0 = time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)

// newTestRegistry is a registry for a test.
func newTestRegistry(lim CaptureLimits, nonce string) *registry {
	return newRegistry(lim, nonce)
}

// fullRun is a profiled duration at least any reservation in these tests,
// so finish charges the whole reservation.
const fullRun = 24 * time.Hour

// finishOK finishes j and reports an error from finish.
func finishOK(t *testing.T, r *registry, j *job, resultBytes int, profiled time.Duration) {
	t.Helper()
	if err := r.finish(j, resultBytes, profiled); err != nil {
		t.Errorf("finish %s: %v", j.id, err)
	}
}

// putOK keeps a historical answer and reports an error from put.
func putOK(t *testing.T, r *registry, captureID string, h *historical) {
	t.Helper()
	if err := r.put(captureID, h); err != nil {
		t.Errorf("put %s: %v", captureID, err)
	}
}

// asCapture is err as a *capture.Error, nil when err is nil.
func asCapture(t *testing.T, err error) *capture.Error {
	t.Helper()
	if err == nil {
		return nil
	}
	e, ok := errors.AsType[*capture.Error](err)
	if !ok {
		t.Fatalf("%v is not a capture error", err)
	}
	return e
}

// testJob is a job admitted at created that reserves reserve.
func testJob(r *registry, created time.Time, reserve time.Duration) *job {
	return newJob(r.newJobID(), created, reserve)
}

// terminate puts j in a terminal state, as the job does before finish.
func terminate(j *job, s jobStatus) {
	j.mu.Lock()
	j.status = s
	j.mu.Unlock()
}

func wantCode(t *testing.T, what string, e *capture.Error, code capture.Code) {
	t.Helper()
	switch {
	case code == "" && e != nil:
		t.Fatalf("%s: %v, want no error", what, e)
	case code != "" && e == nil:
		t.Fatalf("%s: no error, want %s", what, code)
	case code != "" && e.Code != code:
		t.Fatalf("%s: %v, want %s", what, e, code)
	}
}

func TestAdmitIsOneAtATime(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	j1 := testJob(r, regT0, 10*time.Second)
	wantCode(t, "first admit", r.admit(j1, regT0), "")
	if r.active.Load() != j1 {
		t.Fatal("the admitted job does not hold the slot")
	}
	j2 := testJob(r, regT0.Add(time.Second), 10*time.Second)
	e := r.admit(j2, regT0.Add(time.Second))
	wantCode(t, "second admit", e, capture.CodeBusy)
	if !e.Retryable || e.RetryAfter != 5*time.Second {
		t.Errorf("busy: retryable %v, retry_after %v; want true, 5s (the active job's reserved end, 9s away, capped)", e.Retryable, e.RetryAfter)
	}
	if end := regT0.Add(10 * time.Second).UTC().Format(time.RFC3339); !strings.Contains(e.Next, end) {
		t.Errorf("busy: next %q does not name the active job's latest end %s", e.Next, end)
	}
	j3 := testJob(r, regT0.Add(7500*time.Millisecond), 10*time.Second)
	if e := r.admit(j3, regT0.Add(7500*time.Millisecond)); e == nil || e.RetryAfter != 3*time.Second {
		t.Errorf("busy 2.5s before the end: %v, want retry_after 3s (rounded up)", e)
	}
	if r.active.Load() != j1 {
		t.Fatal("a busy admit changed the slot")
	}
	terminate(j1, jobDone)
	finishOK(t, r, j1, 0, fullRun)
	if r.active.Load() != nil {
		t.Fatal("finish left the slot held")
	}
	wantCode(t, "admit after finish", r.admit(j2, regT0.Add(2*time.Second)), "")

	// Many at once: exactly one wins.
	r = newTestRegistry(testRegistryLimits(nil), "n")
	var won atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		wg.Go(func() {
			j := testJob(r, regT0, time.Second)
			<-start
			if r.admit(j, regT0) == nil {
				won.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d concurrent admits won the slot, want 1", won.Load())
	}
}

// TestARejectedBudgetNeverHoldsTheSlot: a budget refusal leaves the slot
// free and records nothing, so a later capture is admitted once the window
// frees.
func TestARejectedBudgetNeverHoldsTheSlot(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.WindowCaptures = 1 }), "n")
	j1 := testJob(r, regT0, 10*time.Second)
	wantCode(t, "j1", r.admit(j1, regT0), "")
	terminate(j1, jobDone)
	finishOK(t, r, j1, 0, fullRun)

	at := regT0.Add(time.Minute)
	j2 := testJob(r, at, 10*time.Second)
	e := r.admit(j2, at)
	wantCode(t, "j2", e, capture.CodeBudget)
	if !e.Retryable || e.RetryAfter != 59*time.Minute {
		t.Errorf("budget: retryable %v, retry_after %v; want true, 59m", e.Retryable, e.RetryAfter)
	}
	if r.active.Load() != nil {
		t.Fatal("a rejected budget holds the slot")
	}
	if _, ok := r.get(j2.id); ok {
		t.Fatal("a rejected job is in the registry")
	}
	// A rejected budget reserved nothing: the window still frees at regT0+1h.
	at = regT0.Add(time.Hour)
	j3 := testJob(r, at, 10*time.Second)
	wantCode(t, "j3 once the window frees", r.admit(j3, at), "")
	if r.active.Load() != j3 {
		t.Fatal("j3 does not hold the slot")
	}

	// The same by requested duration.
	r = newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.WindowDuration = time.Minute; l.MaxDuration = time.Minute }), "n")
	j4 := testJob(r, regT0, 50*time.Second)
	wantCode(t, "j4", r.admit(j4, regT0), "")
	terminate(j4, jobDone)
	finishOK(t, r, j4, 0, fullRun)
	j5 := testJob(r, regT0, 20*time.Second)
	wantCode(t, "j5", r.admit(j5, regT0), capture.CodeBudget)
	if r.active.Load() != nil {
		t.Fatal("a rejected duration budget holds the slot")
	}
}

// TestReconcileMovesTheReservation: admission reserves an upper bound,
// and reconcile replaces it with the effective duration (PLAN-R3).
func TestReconcileMovesTheReservation(t *testing.T) {
	lim := testRegistryLimits(func(l *CaptureLimits) { l.MaxDuration = 30 * time.Minute })

	// Without reconcile the 30m reservation fills the window.
	r := newTestRegistry(lim, "n")
	j := testJob(r, regT0, 30*time.Minute)
	wantCode(t, "reserve 30m", r.admit(j, regT0), "")
	terminate(j, jobDone)
	finishOK(t, r, j, 0, fullRun)
	wantCode(t, "29m after an unreconciled 30m", r.admit(testJob(r, regT0, 29*time.Minute), regT0), capture.CodeBudget)

	r = newTestRegistry(lim, "n")
	j1 := testJob(r, regT0, 30*time.Minute)
	wantCode(t, "reserve 30m", r.admit(j1, regT0), "")
	wantCode(t, "reconcile to 20s", asCapture(t, r.reconcile(j1, 20*time.Second, regT0)), "")
	terminate(j1, jobDone)
	finishOK(t, r, j1, 0, fullRun)

	j2 := testJob(r, regT0, 29*time.Minute)
	wantCode(t, "29m after 20s", r.admit(j2, regT0), "")
	e := asCapture(t, r.reconcile(j2, 31*time.Minute, regT0))
	wantCode(t, "reconcile above the window", e, capture.CodeBudget)
	terminate(j2, jobFailed)
	finishOK(t, r, j2, 0, fullRun)
	// j2's reservation was released: only j1's 20s is charged, by duration
	// and by count.
	j3 := testJob(r, regT0, 29*time.Minute+40*time.Second)
	wantCode(t, "the rest of the window", r.admit(j3, regT0), "")
	if e := r.reconcile(j3, 29*time.Minute+41*time.Second, regT0); e == nil {
		t.Fatal("a reconcile one second over the window fits")
	}
	terminate(j3, jobFailed)
	finishOK(t, r, j3, 0, fullRun)

	r = newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.WindowCaptures = 1 }), "n")
	j4 := testJob(r, regT0, time.Minute)
	wantCode(t, "j4", r.admit(j4, regT0), "")
	wantCode(t, "reconcile over the window", asCapture(t, r.reconcile(j4, time.Hour, regT0)), capture.CodeBudget)
	terminate(j4, jobFailed)
	finishOK(t, r, j4, 0, fullRun)
	wantCode(t, "the released capture's count", r.admit(testJob(r, regT0, time.Minute), regT0), "")

	// Reconciling upwards within the window is allowed and charged.
	r = newTestRegistry(lim, "n")
	j5 := testJob(r, regT0, 10*time.Second)
	wantCode(t, "j5", r.admit(j5, regT0), "")
	wantCode(t, "reconcile up", asCapture(t, r.reconcile(j5, 20*time.Minute, regT0)), "")
	terminate(j5, jobDone)
	finishOK(t, r, j5, 0, fullRun)
	wantCode(t, "11m after 20m", r.admit(testJob(r, regT0, 11*time.Minute), regT0), capture.CodeBudget)
}

func TestWindowsBound(t *testing.T) {
	var w window
	for i := range 3 {
		at := regT0.Add(time.Duration(i) * 10 * time.Minute)
		if e := w.allow(at, time.Minute, 3, time.Hour); e != nil {
			t.Fatalf("capture %d: %v", i, e)
		}
		w.reserve(at, time.Minute)
	}
	// By count: the fourth waits for the oldest to leave the hour.
	at := regT0.Add(25 * time.Minute)
	e := w.allow(at, time.Minute, 3, time.Hour)
	wantCode(t, "fourth", e, capture.CodeBudget)
	if e.RetryAfter != 35*time.Minute {
		t.Errorf("by count: retry_after %v, want 35m", e.RetryAfter)
	}
	// Two must leave when the limit is 2: the second oldest sets the time.
	if e := w.allow(at, time.Minute, 2, time.Hour); e == nil || e.RetryAfter != 45*time.Minute {
		t.Errorf("by count, two to leave: %v, want retry_after 45m", e)
	}
	if e := w.allow(regT0.Add(time.Hour), time.Minute, 3, time.Hour); e != nil {
		t.Errorf("the oldest has left the hour: %v", e)
	}

	// By duration: retry_after is when enough of the oldest has left.
	var d window
	d.reserve(regT0, 10*time.Minute)
	d.reserve(regT0.Add(5*time.Minute), 10*time.Minute)
	d.reserve(regT0.Add(10*time.Minute), 5*time.Minute)
	at = regT0.Add(20 * time.Minute)
	if e := d.allow(at, 5*time.Minute, 60, 30*time.Minute); e != nil {
		t.Fatalf("5m into 25m of 30m: %v", e)
	}
	e = d.allow(at, 15*time.Minute, 60, 30*time.Minute)
	wantCode(t, "15m into 25m of 30m", e, capture.CodeBudget)
	// 10m must leave: the first reservation (at regT0) frees exactly 10m.
	if e.RetryAfter != 40*time.Minute {
		t.Errorf("by duration: retry_after %v, want 40m", e.RetryAfter)
	}
	e = d.allow(at, 25*time.Minute, 60, 30*time.Minute)
	if e == nil || e.RetryAfter != 45*time.Minute {
		t.Errorf("25m into 25m of 30m: %v, want retry_after 45m", e)
	}

	// A request longer than the whole window never fits.
	e = d.allow(at, 31*time.Minute, 60, 30*time.Minute)
	wantCode(t, "over the window", e, capture.CodeBudget)
	if e.Retryable {
		t.Error("a request over the whole window is retryable")
	}

	// replace moves a reservation's duration.
	var x window
	res := x.reserve(regT0, 30*time.Minute)
	x.replace(res, time.Minute)
	if e := x.allow(regT0, 29*time.Minute, 60, 30*time.Minute); e != nil {
		t.Errorf("after replace: %v", e)
	}
	x.release(res)
	if e := x.allow(regT0, 30*time.Minute, 1, 30*time.Minute); e != nil {
		t.Errorf("after release: %v", e)
	}
}

func TestRegistryLRUByCountAndBytes(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.MaxJobs = 3; l.MaxJobBytes = 100 }), "n")
	run := func(bytes int) *job {
		t.Helper()
		j := testJob(r, regT0, time.Second)
		wantCode(t, j.id, r.admit(j, regT0), "")
		terminate(j, jobDone)
		finishOK(t, r, j, bytes, fullRun)
		return j
	}
	// has looks without making j recently used.
	has := func(j *job) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, ok := r.byJob[j.id]
		return ok
	}
	j1, j2, j3 := run(10), run(10), run(10)
	j4 := run(10)
	if has(j1) || !has(j2) || !has(j3) || !has(j4) {
		t.Fatalf("by count: kept j1 %v j2 %v j3 %v j4 %v, want only j1 gone", has(j1), has(j2), has(j3), has(j4))
	}
	// Using j2 makes j3 the least recently used.
	r.get(j2.id)
	j5 := run(10)
	if has(j3) || !has(j2) || !has(j5) {
		t.Fatalf("LRU: kept j2 %v j3 %v j5 %v, want j3 gone", has(j2), has(j3), has(j5))
	}
	// By bytes: 85 more pushes out j4 by count, then j2 by bytes.
	j6 := run(85)
	if !has(j6) || has(j4) || has(j2) || !has(j5) {
		t.Fatalf("by bytes: kept j2 %v j4 %v j5 %v j6 %v, want j5 and j6", has(j2), has(j4), has(j5), has(j6))
	}
	// The newest is kept even when its result alone is over the bound.
	j7 := run(500)
	if !has(j7) || has(j5) || has(j6) {
		t.Fatalf("an oversized newest: kept j5 %v j6 %v j7 %v, want j7 only", has(j5), has(j6), has(j7))
	}
	if n := len(r.list()); n != 1 {
		t.Fatalf("list has %d jobs, want 1", n)
	}

	// Historical answers share the bounds; a job keeps its capture id.
	r = newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.MaxJobs = 2 }), "n")
	j := testJob(r, regT0, time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	j.mu.Lock()
	j.captureID = "s/001"
	j.mu.Unlock()
	if got, ok := r.get("s/001"); !ok || got != j {
		t.Fatal("the active job is not found by its capture id")
	}
	terminate(j, jobDone)
	finishOK(t, r, j, 1, fullRun)
	if a, ok := r.cancel("s/001"); !ok || a != cancelAnswerTooLate {
		t.Fatalf("cancel of a finished job by its capture id: %q %v", a, ok)
	}
	putOK(t, r, "s/001", &historical{m: manifest{ID: "s/001"}})
	if got, ok := r.get("s/001"); !ok || got != j {
		t.Fatal("a historical answer replaced the job holding its capture id")
	}
	if _, ok := r.historical("s/001"); ok {
		t.Fatal("a historical answer was kept for a job's capture id")
	}
	putOK(t, r, "s/002", &historical{m: manifest{ID: "s/002"}})
	if h, ok := r.historical("s/002"); !ok || h.m.ID != "s/002" {
		t.Fatal("the historical answer is not kept")
	}
	// Over MaxJobs, a historical answer goes before any job: it can be read
	// from its manifest again, and a job's derived result cannot.
	putOK(t, r, "s/003", &historical{m: manifest{ID: "s/003"}})
	if _, ok := r.historical("s/002"); ok {
		t.Fatal("historical answers did not count toward MaxJobs")
	}
	if _, ok := r.get(j.id); !ok {
		t.Fatal("a historical answer pushed out a job")
	}
	// Over MaxJobBytes with no other historical answer left, the job goes.
	putOK(t, r, "s/004", &historical{m: manifest{ID: "s/004", Notes: []string{string(make([]byte, 5<<20))}}})
	if _, ok := r.historical("s/003"); ok {
		t.Fatal("historical answers did not count toward MaxJobBytes")
	}
	if _, ok := r.get(j.id); ok {
		t.Fatal("the job was kept over MaxJobBytes")
	}
	if _, ok := r.get("s/001"); ok {
		t.Fatal("an evicted job is still found by its capture id")
	}
	if _, ok := r.historical("s/004"); !ok {
		t.Fatal("the newest historical answer was evicted")
	}
	if cid, ok := r.alias(j.id); !ok || cid != "s/001" {
		t.Fatalf("the evicted job's id resolves to %q %v, want s/001", cid, ok)
	}
}

func TestJobIDsCarryTheProcessNonce(t *testing.T) {
	a, b := newTestRegistry(testRegistryLimits(nil), "4f2a"), newTestRegistry(testRegistryLimits(nil), "9c01")
	re := regexp.MustCompile(`^j[1-9][0-9]*-4f2a$`)
	seen := map[string]bool{}
	for i := range 5 {
		id := a.newJobID()
		if !re.MatchString(id) || id != fmt.Sprintf("j%d-4f2a", i+1) {
			t.Fatalf("id %d is %q, want j%d-4f2a", i, id, i+1)
		}
		seen[id] = true
	}
	for range 5 {
		if id := b.newJobID(); seen[id] {
			t.Fatalf("%s from another process's nonce collides", id)
		}
	}
}

func TestRequestCancelClosesTheChannelOnce(t *testing.T) {
	j := newJob("j1-n", regT0, time.Second)
	if j.status != jobAccepted || j.status.terminal() || j.phase != "" || j.captureID != "" || j.derived != "" ||
		j.err != nil || j.result != nil || j.run.Load() != nil || j.changed == nil {
		t.Fatalf("a new job is not accepted with nothing yet: %+v", j)
	}
	select {
	case <-j.cancel:
		t.Fatal("a new job's cancel channel is closed")
	default:
	}
	for i := range 2 {
		if a := j.requestCancel(); a != cancelAnswerCancelling {
			t.Fatalf("cancel %d answered %q, want %q", i, a, cancelAnswerCancelling)
		}
	}
	select {
	case <-j.cancel:
	default:
		t.Fatal("requestCancel did not close the cancel channel")
	}
	if !j.d.cancelled() {
		t.Fatal("requestCancel did not set the decision word")
	}
	// Concurrent cancels close the channel exactly once (a second close
	// panics).
	j = newJob("j2-n", regT0, time.Second)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { j.requestCancel() })
	}
	wg.Wait()
	<-j.cancel
}

// TestRegistryIsSafeFromTheReaderGoroutine: capture_cancel runs on the
// reader goroutine while the dispatcher and jobs admit, finish and put.
func TestRegistryIsSafeFromTheReaderGoroutine(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.MaxJobs = 4; l.WindowCaptures = 1 << 20 }), "n")
	n := 2000
	if testing.Short() {
		n = 200
	}
	ids := make(chan string, n)
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(ids)
		for i := range n {
			j := testJob(r, regT0, time.Millisecond)
			if r.admit(j, regT0) != nil {
				t.Error("admit refused with the slot free")
				return
			}
			ids <- j.id
			j.mu.Lock()
			j.captureID = fmt.Sprintf("s/%03d", i)
			j.mu.Unlock()
			if e := r.reconcile(j, time.Microsecond, regT0); e != nil {
				t.Errorf("reconcile down: %v", e)
			}
			terminate(j, jobCancelled)
			finishOK(t, r, j, 10, fullRun)
			putOK(t, r, fmt.Sprintf("h/%03d", i), &historical{m: manifest{ID: "h"}})
		}
	})
	wg.Go(func() {
		for id := range ids {
			if j, ok := r.get(id); ok {
				j.requestCancel()
			}
			r.cancel(id)
			r.get("s/001")
			r.historical("h/001")
			for _, j := range r.list() {
				j.requestCancel()
			}
			if a := r.active.Load(); a != nil {
				a.requestCancel()
			}
		}
	})
	wg.Wait()
}

func TestLookupsAreSharedAndBounded(t *testing.T) {
	p := newLookupPool(2, 16)
	defer p.close()
	gate := make(chan struct{})
	var loads atomic.Int32
	load := func(v string) func() (any, error) {
		return func() (any, error) {
			loads.Add(1)
			<-gate
			return v, nil
		}
	}
	a1, ok1 := p.get("a", load("a"))
	a2, ok2 := p.get("a", load("other"))
	if !ok1 || !ok2 || a1 != a2 {
		t.Fatal("two lookups of one key are not shared")
	}
	// Two run, sixteen wait; the nineteenth distinct key is refused.
	p.get("k0", load("k"))
	waitFor(t, func() bool { return loads.Load() == 2 })
	for i := range 16 {
		if _, ok := p.get(fmt.Sprintf("k%d", i+1), load("k")); !ok {
			t.Fatalf("lookup %d refused below the bound", i+3)
		}
	}
	if l, ok := p.get("over", load("over")); ok || l != nil {
		t.Fatal("a lookup over workers + queue was accepted")
	}
	if l, ok := p.get("a", load("a")); !ok || l != a1 {
		t.Fatal("a full pool does not share a pending key")
	}
	close(gate)
	v, ready, err := a1.wait(t.Context(), 5*time.Second)
	if !ready || err != nil || v != "a" {
		t.Fatalf("wait: %v %v %v, want a, ready", v, ready, err)
	}
	waitFor(t, func() bool { return loads.Load() == 18 })
	// A finished lookup is not reused: the answer lives in the registry.
	b, _ := p.get("a", load("again"))
	if b == a1 {
		t.Fatal("a finished lookup was handed out again")
	}
	if v, _, _ := b.wait(t.Context(), 5*time.Second); v != "again" {
		t.Fatalf("the second lookup of a answered %v", v)
	}

	// A panicking load is an error, and the worker survives.
	pl, _ := p.get("panic", func() (any, error) { panic("boom") })
	if _, ready, err := pl.wait(t.Context(), 5*time.Second); !ready || err == nil {
		t.Fatalf("a panicking load: ready %v, err %v", ready, err)
	}
	ok, _ := p.get("after", func() (any, error) { return 1, nil })
	if v, _, err := ok.wait(t.Context(), 5*time.Second); v != 1 || err != nil {
		t.Fatalf("after a panic: %v %v", v, err)
	}

	// close fails what is queued and refuses new lookups.
	q := newLookupPool(1, 2)
	block := make(chan struct{})
	defer close(block)
	q.get("run", func() (any, error) { <-block; return nil, nil })
	queued, _ := q.get("queued", func() (any, error) { return 1, nil })
	q.close()
	if _, ready, err := queued.wait(t.Context(), 5*time.Second); !ready || !errors.Is(err, errLookupClosed) {
		t.Fatalf("a queued lookup after close: ready %v, err %v", ready, err)
	}
	if l, ok := q.get("new", func() (any, error) { return 1, nil }); ok || l != nil {
		t.Fatal("a closed pool accepted a lookup")
	}
	q.close()
}

// TestWaitingDoesNotAddGoroutines: a wait that times out starts nothing
// and leaves nothing running; the lookup goes on and answers later.
// Goroutine creations are counted, since the live count also moves with
// the runtime's own short-lived goroutines.
func TestWaitingDoesNotAddGoroutines(t *testing.T) {
	gate := make(chan struct{})
	p := newLookupPool(2, 16)
	defer p.close()
	l, _ := p.get("slow", func() (any, error) { <-gate; return "v", nil })
	waitFor(t, func() bool { return len(p.queue) == 0 }) // a worker took it
	base, created := runtime.NumGoroutine(), goroutinesCreated()

	const waits = 1000
	for range waits {
		if _, ready, _ := l.wait(t.Context(), time.Microsecond); ready {
			t.Fatal("the lookup finished before its gate opened")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range waits {
		if _, ready, _ := l.wait(ctx, time.Hour); ready {
			t.Fatal("a cancelled wait reported ready")
		}
	}
	const waiters, each = 8, 100
	var wg sync.WaitGroup
	for range waiters {
		wg.Go(func() {
			for range each {
				_, _, _ = l.wait(t.Context(), 50*time.Microsecond)
			}
		})
	}
	wg.Wait()
	// Each wait starting a goroutine would add 2800; allow the waiters and
	// the runtime's own.
	if n := goroutinesCreated() - created; n > waiters+waits/10 {
		t.Errorf("%d goroutines were created during %d waits", n, 2*waits+waiters*each)
	}
	// The waiters take a moment to exit after wg.Wait.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("%d goroutines after the waits, %d before", n, base)
	}
	close(gate)
	if v, ready, _ := l.wait(t.Context(), 5*time.Second); !ready || v != "v" {
		t.Fatalf("the lookup did not continue past the timed-out waits: %v %v", v, ready)
	}
}

// goroutinesCreated is how many goroutines the process has created.
func goroutinesCreated() int {
	s := []metrics.Sample{{Name: "/sched/goroutines-created:goroutines"}}
	metrics.Read(s)
	return int(s[0].Value.Uint64())
}

// TestDecisionWord: the job's cancel goes through Task 10's decision word,
// and a job that has ended answers from the registry without moving it.
func TestDecisionWord(t *testing.T) {
	// Before the manifest: a cancel wins and the commit is refused.
	j := newJob("j1-n", regT0, time.Second)
	if a := j.requestCancel(); a != cancelAnswerCancelling {
		t.Fatalf("open: %q", a)
	}
	if j.d.beginManifest() {
		t.Fatal("the manifest began after a cancel")
	}

	// After a successful link: too late.
	j = newJob("j2-n", regT0, time.Second)
	if !j.d.beginManifest() {
		t.Fatal("beginManifest on an open job")
	}
	j.d.settle(true)
	if a := j.requestCancel(); a != cancelAnswerTooLate {
		t.Fatalf("committed: %q", a)
	}

	// A failed link leaves the word open. While the job runs a cancel
	// still flips it; once the job is terminal the registry answers.
	r := newTestRegistry(testRegistryLimits(nil), "n")
	for _, c := range []struct {
		status jobStatus
		answer string
	}{
		{jobFailed, cancelAnswerEnded},
		{jobCancelled, cancelAnswerCancelled},
		{jobDone, cancelAnswerTooLate},
	} {
		j := testJob(r, regT0, time.Second)
		wantCode(t, j.id, r.admit(j, regT0), "")
		j.d.beginManifest()
		j.d.settle(false)
		terminate(j, c.status)
		if a, ok := r.cancel(j.id); !ok || a != c.answer {
			t.Errorf("%s, still active: %q %v, want %q", c.status, a, ok, c.answer)
		}
		finishOK(t, r, j, 0, fullRun)
		if a, ok := r.cancel(j.id); !ok || a != c.answer {
			t.Errorf("%s, finished: %q %v, want %q", c.status, a, ok, c.answer)
		}
		if j.d.cancelled() {
			t.Errorf("%s: a cancel after the end moved the decision word", c.status)
		}
		select {
		case <-j.cancel:
			t.Errorf("%s: a cancel after the end closed the cancel channel", c.status)
		default:
		}
	}
	j = testJob(r, regT0, time.Second)
	wantCode(t, j.id, r.admit(j, regT0), "")
	if a, ok := r.cancel(j.id); !ok || a != cancelAnswerCancelling || !j.d.cancelled() {
		t.Fatalf("the active job: %q %v, cancelled %v", a, ok, j.d.cancelled())
	}
	if _, ok := r.cancel("j99-n"); ok {
		t.Fatal("an unknown id was cancelled")
	}
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestFinishChargesWhatWasProfiled: finish charges the window with what the
// capture profiled, at most its reservation, in the same step that frees
// the slot; the capture still counts (CAP-R59).
func TestFinishChargesWhatWasProfiled(t *testing.T) {
	lim := testRegistryLimits(func(l *CaptureLimits) { l.MaxDuration = 30 * time.Minute })
	r := newTestRegistry(lim, "n")
	j1 := testJob(r, regT0, 30*time.Minute)
	wantCode(t, "j1", r.admit(j1, regT0), "")
	terminate(j1, jobFailed)
	finishOK(t, r, j1, 0, 5*time.Minute)
	wantCode(t, "25m+1s after 5m", r.admit(testJob(r, regT0, 25*time.Minute+time.Second), regT0), capture.CodeBudget)
	j2 := testJob(r, regT0, 25*time.Minute)
	wantCode(t, "25m after 5m", r.admit(j2, regT0), "")
	// More profiled than reserved is charged the reservation.
	terminate(j2, jobDone)
	finishOK(t, r, j2, 0, time.Hour)
	wantCode(t, "1s after 30m", r.admit(testJob(r, regT0, time.Second), regT0), capture.CodeBudget)

	// Nothing profiled still counts as a capture started.
	r = newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.WindowCaptures = 1 }), "n")
	j := testJob(r, regT0, 10*time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	terminate(j, jobFailed)
	finishOK(t, r, j, 0, 0)
	wantCode(t, "after a capture that profiled nothing", r.admit(testJob(r, regT0, time.Second), regT0), capture.CodeBudget)
}

// TestRegistryRefusesMisuse: a reconcile without a reservation, a finish of
// a job that was never admitted and negative sizes are errors that change
// nothing; a second finish is a no-op.
func TestRegistryRefusesMisuse(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	stranger := testJob(r, regT0, time.Second)
	if err := r.reconcile(stranger, time.Second, regT0); err == nil {
		t.Error("reconcile of a job never admitted is nil")
	}
	wantCode(t, "a negative reserve", r.admit(testJob(r, regT0, -time.Second), regT0), capture.CodeInvalidArgument)
	if r.active.Load() != nil {
		t.Fatal("a refused admit holds the slot")
	}

	j := testJob(r, regT0, 10*time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	terminate(stranger, jobDone)
	if err := r.finish(stranger, 0, 0); err == nil {
		t.Error("finish of a job never admitted is nil")
	}
	if _, ok := r.get(stranger.id); ok || r.active.Load() != j {
		t.Fatal("finish of a job never admitted changed the registry")
	}
	terminate(j, jobDone)
	for _, c := range []struct {
		bytes    int
		profiled time.Duration
	}{{-1, 0}, {0, -time.Second}} {
		if err := r.finish(j, c.bytes, c.profiled); err == nil {
			t.Errorf("finish with %d bytes, %v profiled is nil", c.bytes, c.profiled)
		}
		if r.active.Load() != j {
			t.Fatal("a refused finish freed the slot")
		}
	}
	finishOK(t, r, j, 7, fullRun)
	if err := r.finish(j, 99, 0); err != nil {
		t.Errorf("a second finish: %v", err)
	}
	if r.bytes != 7 || r.lru.Len() != 1 {
		t.Errorf("after a second finish: %d bytes, %d entries; want 7, 1", r.bytes, r.lru.Len())
	}
	// finish drops the reservation: nothing is left to reconcile.
	if err := r.reconcile(j, time.Second, regT0); err == nil {
		t.Error("reconcile after finish is nil")
	}

	// A second reconcile after a refusal has no reservation either.
	j = testJob(r, regT0, 10*time.Second)
	wantCode(t, "j again", r.admit(j, regT0), "")
	if err := r.reconcile(j, time.Hour, regT0); err == nil {
		t.Fatal("reconcile over the window is nil")
	}
	if err := r.reconcile(j, time.Second, regT0); err == nil {
		t.Error("reconcile after a refused reconcile is nil")
	}

	if err := r.put("", &historical{}); err == nil {
		t.Error("put with an empty capture id is nil")
	}
	if err := r.put("s/001", nil); err == nil {
		t.Error("put of no answer is nil")
	}
}

// TestAnEvictedJobIDResolvesByCaptureID: a job id outlives its LRU entry
// as an alias to its capture id and final answer (CAP-R60), up to
// maxJobAliases of them.
func TestAnEvictedJobIDResolvesByCaptureID(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.MaxJobs = 1; l.WindowCaptures = 1 << 20 }), "n")
	run := func(captureID string, s jobStatus) *job {
		t.Helper()
		j := testJob(r, regT0, 0)
		wantCode(t, j.id, r.admit(j, regT0), "")
		j.mu.Lock()
		j.captureID = captureID
		j.mu.Unlock()
		terminate(j, s)
		finishOK(t, r, j, 0, 0)
		return j
	}
	j1 := run("s/001", jobDone)
	j2 := run("", jobFailed)
	run("s/003", jobCancelled)
	if _, ok := r.get(j1.id); ok {
		t.Fatal("j1 was not evicted")
	}
	if cid, ok := r.alias(j1.id); !ok || cid != "s/001" {
		t.Fatalf("j1 resolves to %q %v", cid, ok)
	}
	if cid, ok := r.alias(j2.id); !ok || cid != "" {
		t.Fatalf("a job that ended before its capture id resolves to %q %v", cid, ok)
	}
	if a, ok := r.cancel(j1.id); !ok || a != cancelAnswerTooLate {
		t.Fatalf("cancel of an evicted done job: %q %v", a, ok)
	}
	if a, ok := r.cancel(j2.id); !ok || a != cancelAnswerEnded {
		t.Fatalf("cancel of an evicted failed job: %q %v", a, ok)
	}
	if _, ok := r.alias("j99-n"); ok {
		t.Fatal("an unknown job id has an alias")
	}
	for range maxJobAliases {
		run("", jobFailed)
	}
	if _, ok := r.alias(j1.id); ok {
		t.Fatal("the aliases are not bounded")
	}
	if n := len(r.aliases); n > maxJobAliases {
		t.Fatalf("%d aliases, over %d", n, maxJobAliases)
	}
}

// TestHistoricalAnswersNeverPushOutAnUndeliveredJob is the reviewer's
// probe: a job finished with a 1 MiB result, then a capture_list read
// four old captures before anyone waited on the job.
func TestHistoricalAnswersNeverPushOutAnUndeliveredJob(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	j := testJob(r, regT0, 0)
	wantCode(t, "j", r.admit(j, regT0), "")
	terminate(j, jobDone)
	finishOK(t, r, j, 1<<20, 0)
	for i := range 4 {
		putOK(t, r, fmt.Sprintf("old/%03d", i), &historical{m: manifest{ID: fmt.Sprintf("%03d", i), Notes: []string{strings.Repeat("x", 900<<10)}}})
	}
	if _, ok := r.get(j.id); !ok {
		t.Fatalf("job %s, finished and never waited on, was evicted by 4 historical answers", j.id)
	}
	if r.bytes > r.lim.MaxJobBytes {
		t.Fatalf("%d bytes kept, over %d", r.bytes, r.lim.MaxJobBytes)
	}
}

// TestUpdateReplacesASettledJobEntry: a job that ended undecided is
// corrected once its commit is settled.
func TestUpdateReplacesASettledJobEntry(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	j := testJob(r, regT0, time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	if err := r.update(j, 1); err == nil {
		t.Fatal("update of the active job is nil")
	}
	j.mu.Lock()
	j.captureID = "s/001"
	j.mu.Unlock()
	terminate(j, jobFailed)
	finishOK(t, r, j, 10, 0)
	terminate(j, jobDone)
	if err := r.update(j, 50); err != nil {
		t.Fatal(err)
	}
	if a, ok := r.cancel(j.id); !ok || a != cancelAnswerTooLate {
		t.Fatalf("after update, cancel answers %q %v", a, ok)
	}
	if r.bytes != 50 {
		t.Fatalf("after update, %d bytes, want 50", r.bytes)
	}
	if err := r.update(newJob("j9-n", regT0, 0), 1); err == nil {
		t.Fatal("update of an unknown job is nil")
	}
	if err := r.update(j, -1); err == nil {
		t.Fatal("update with negative bytes is nil")
	}
	// An evicted job is settled through its alias.
	r.lim.MaxJobs = 1
	j2 := testJob(r, regT0, 0)
	wantCode(t, "j2", r.admit(j2, regT0), "")
	terminate(j2, jobDone)
	finishOK(t, r, j2, 0, 0)
	terminate(j, jobCancelled)
	if err := r.update(j, 1); err != nil {
		t.Fatalf("update of an evicted job: %v", err)
	}
	if a, _ := r.cancel(j.id); a != cancelAnswerCancelled {
		t.Fatalf("the evicted job's alias answers %q after update", a)
	}
	if _, ok := r.get(j.id); ok {
		t.Fatal("update brought an evicted job back")
	}
}

func TestCancelOfTheActiveJobByCaptureID(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	j := testJob(r, regT0, time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	j.mu.Lock()
	j.captureID = "s/001"
	j.mu.Unlock()
	if a, ok := r.cancel("s/001"); !ok || a != cancelAnswerCancelling || !j.d.cancelled() {
		t.Fatalf("cancel by capture id: %q %v, cancelled %v", a, ok, j.d.cancelled())
	}
	select {
	case <-j.cancel:
	default:
		t.Fatal("the cancel channel is open")
	}
}

func TestPutNeverShadowsTheActiveJob(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	j := testJob(r, regT0, time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	j.mu.Lock()
	j.captureID = "s/001"
	j.mu.Unlock()
	putOK(t, r, "s/001", &historical{m: manifest{ID: "s/001"}})
	if _, ok := r.historical("s/001"); ok {
		t.Fatal("a historical answer was kept for the active job's capture id")
	}
}

// TestFinishReplacesAHistoricalAnswer: an answer read from disk before the
// job named its capture id gives way to the job, and its bytes go with it.
func TestFinishReplacesAHistoricalAnswer(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(nil), "n")
	j := testJob(r, regT0, time.Second)
	wantCode(t, "j", r.admit(j, regT0), "")
	putOK(t, r, "s/001", &historical{m: manifest{ID: "s/001"}})
	j.mu.Lock()
	j.captureID = "s/001"
	j.mu.Unlock()
	terminate(j, jobDone)
	finishOK(t, r, j, 3, 0)
	if _, ok := r.historical("s/001"); ok {
		t.Fatal("the historical answer outlived the job's finish")
	}
	if got, ok := r.get("s/001"); !ok || got != j {
		t.Fatal("the capture id does not find the job")
	}
	if r.bytes != 3 || r.lru.Len() != 1 {
		t.Fatalf("%d bytes in %d entries, want 3 in 1", r.bytes, r.lru.Len())
	}
}

func TestListLeavesRecencyAlone(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.MaxJobs = 2 }), "n")
	var js []*job
	for range 3 {
		if len(js) == 2 {
			for range 3 {
				r.list()
			}
		}
		j := testJob(r, regT0, 0)
		wantCode(t, j.id, r.admit(j, regT0), "")
		terminate(j, jobDone)
		finishOK(t, r, j, 0, 0)
		js = append(js, j)
	}
	r.mu.Lock()
	_, kept0 := r.byJob[js[0].id]
	_, kept1 := r.byJob[js[1].id]
	r.mu.Unlock()
	if kept0 || !kept1 {
		t.Fatalf("after list: kept j1 %v j2 %v, want j2 (list must not reorder)", kept0, kept1)
	}
}

func TestBusyRetryAfter(t *testing.T) {
	r := newTestRegistry(testRegistryLimits(func(l *CaptureLimits) { l.MaxDuration = 30 * time.Minute }), "n")
	j := testJob(r, regT0, 30*time.Minute)
	wantCode(t, "j", r.admit(j, regT0), "")
	if err := r.reconcile(j, 20*time.Second, regT0); err != nil {
		t.Fatal(err)
	}
	e := r.admit(testJob(r, regT0, time.Second), regT0.Add(17500*time.Millisecond))
	if e == nil || e.RetryAfter != 3*time.Second || !strings.Contains(e.Next, regT0.Add(20*time.Second).UTC().Format(time.RFC3339)) {
		t.Fatalf("busy after reconcile: %v, want retry_after 3s and the end from the reconciled 20s", e)
	}
	e = r.admit(testJob(r, regT0, time.Second), regT0.Add(time.Hour))
	if e == nil || e.RetryAfter != time.Second {
		t.Fatalf("busy past the reserved end: %v, want the 1s floor", e)
	}
}

func TestTheCancelChannelClosesOnlyForACancelThatWillHappen(t *testing.T) {
	j := newJob("j1-n", regT0, time.Second)
	j.d.beginManifest()
	j.d.settle(true)
	if a := j.requestCancel(); a != cancelAnswerTooLate {
		t.Fatalf("committed: %q", a)
	}
	select {
	case <-j.cancel:
		t.Fatal("a too-late cancel closed the cancel channel")
	default:
	}
}

func TestWindowRetryAfterTakesTheLaterLimit(t *testing.T) {
	// Duration frees later than count.
	var w window
	w.reserve(regT0, time.Minute)
	w.reserve(regT0.Add(10*time.Minute), 28*time.Minute)
	at := regT0.Add(20 * time.Minute)
	if e := w.allow(at, 5*time.Minute, 2, 30*time.Minute); e == nil || e.RetryAfter != 50*time.Minute {
		t.Errorf("duration later: %v, want retry_after 50m", e)
	}
	// Count frees later than duration.
	var c window
	c.reserve(regT0, 20*time.Minute)
	c.reserve(regT0.Add(10*time.Minute), time.Minute)
	if e := c.allow(at, 15*time.Minute, 1, 30*time.Minute); e == nil || e.RetryAfter != 50*time.Minute {
		t.Errorf("count later: %v, want retry_after 50m", e)
	}
	if _, ok := freedBy(nil, time.Second); ok {
		t.Error("freedBy frees something from nothing")
	}
}
