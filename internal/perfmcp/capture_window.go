package perfmcp

import (
	"fmt"
	"slices"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// captureWindowSpan is the length of the rolling windows that bound the
// captures started and the duration requested (§6.1).
const captureWindowSpan = time.Hour

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
