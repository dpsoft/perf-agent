package perfmcp

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// jobStatus is a capture job's state (§4.0): accepted → preparing →
// running → committing → done | failed | cancelled.
type jobStatus string

const (
	jobAccepted   jobStatus = "accepted"
	jobPreparing  jobStatus = "preparing"
	jobRunning    jobStatus = "running"
	jobCommitting jobStatus = "committing"
	jobDone       jobStatus = "done"
	jobFailed     jobStatus = "failed"
	jobCancelled  jobStatus = "cancelled"
)

// cancelAnswerEnded answers a cancel of a job that has already failed:
// there is nothing left to stop.
const cancelAnswerEnded = "too late: failed"

// terminal reports whether s is one of the three end states.
func (s jobStatus) terminal() bool {
	return s == jobDone || s == jobFailed || s == jobCancelled
}

// cancelAnswer is what a cancel of a job that ended in s is told, or ""
// when s is not terminal.
func (s jobStatus) cancelAnswer() string {
	switch s {
	case jobDone:
		return cancelAnswerTooLate
	case jobCancelled:
		return cancelAnswerCancelled
	case jobFailed:
		return cancelAnswerEnded
	}
	return ""
}

// The values of a done job's derived state: its open and compare results
// are being made, are ready, or could not be made (the capture stays
// committed). recompute is an answer read back from a manifest.
const (
	derivedPending   = "pending"
	derivedReady     = "ready"
	derivedFailed    = "failed"
	derivedRecompute = "recompute"
)

// phaseUndecided is the phase of a committing job whose manifest link had
// an outcome nobody could read: it may have committed. It stays so until a
// later pass settles it from what is on disk.
const phaseUndecided = "undecided"

// captureRequest is what an admitted capture asks for. Exactly one of PID
// and Command is set; Cwd is the command's directory as the agent gave it
// ("" for the default). Settings holds every setting, defaults filled in;
// Explicit names the ones the agent passed (modes, duration, sample_rate,
// unwind, kernel_stacks), so that compare_with inherits the others.
type captureRequest struct {
	PID                        int
	Command                    []string
	Cwd                        string
	Settings                   capture.Settings
	Explicit                   map[string]bool
	Series, Label, CompareWith string
	Keep                       bool
}

// jobView is a job's state as capture_wait shows it; empty values are left
// out.
type jobView struct {
	Job     string `json:"job"`
	ID      string `json:"id,omitzero"`
	Status  string `json:"status"`
	Phase   string `json:"phase,omitzero"`
	Elapsed string `json:"elapsed,omitzero"`
	ETA     string `json:"eta,omitzero"`
	Derived string `json:"derived,omitzero"`
}

// job is one capture, owned by the server (§4.0). The fields before mu
// are set at admission and never change; the ones after it are the job's
// state, which mu guards. mu is held only to read or write that state,
// never across I/O or a channel wait, and never while the registry's mutex
// is taken: the registry may read a job's state, a job never calls the
// registry with mu held.
type job struct {
	id      string
	created time.Time
	// reserve is the duration admission charged to the windows, until the
	// job reconciles it with the effective duration.
	reserve time.Duration
	req     captureRequest
	// began is when admission ran by the wall clock, which elapsed and eta
	// count from (created is on the server's clock, which tests move).
	began time.Time

	// txn is the file transaction of a capture whose commit is undecided,
	// and base its baseline, for the recipe once it is settled. The job sets
	// them before it registers the job as undecided (under the server's
	// capMu), and the pass that settles it reads them after.
	txn  *captureTxn
	base *manifest

	// d decides whether a cancel or the commit wins (§7.4).
	d decisionWord
	// cancel is closed once, by the first requestCancel; the job selects on
	// it to relay the cancel to its Run.
	cancel     chan struct{}
	cancelOnce sync.Once

	mu                        sync.Mutex
	status                    jobStatus
	phase, captureID, derived string
	err                       *capture.Error
	result                    []byte
	// ending is the end state the job is about to show, set while the
	// registry frees its slot, so a cancel and the registry already answer
	// from it.
	ending jobStatus
	// duration is the capture's effective duration, once the job knows it.
	duration time.Duration
	// recipe is how to make a done job's results again from its files, for
	// a wait that finds its handles evicted from the store.
	recipe *recompute
	// trail is every status the job went through (a done one with its
	// derived state), for logs and tests.
	trail []string
	// changed is closed and replaced at each change of state, so a wait
	// selects on it with its own deadline and needs no goroutine.
	changed chan struct{}

	// run is the running capture, once it has started; it is read without
	// a lock by the paths that kill captures.
	run atomic.Pointer[capture.Run]
}

// newJob is an accepted job with the given id, admission time and
// reservation.
func newJob(id string, created time.Time, reserve time.Duration) *job {
	return &job{
		id:      id,
		created: created,
		reserve: reserve,
		cancel:  make(chan struct{}),
		status:  jobAccepted,
		changed: make(chan struct{}),
		trail:   []string{string(jobAccepted)},
	}
}

// decideFailed decides an undecided job whose link was not made: a cancel
// recorded meanwhile wins, and the job is ending cancelled; otherwise it is
// ending failed, and a later cancel is told so. j.mu is held.
func (j *job) decideFailed() (cancelled bool) {
	cancelled = j.d.settle(false) == cancelAnswerCancelled
	j.ending = jobFailed
	if cancelled {
		j.ending = jobCancelled
	}
	return cancelled
}

// set changes the job's state under its mutex, through f, and wakes every
// wait.
func (j *job) set(f func()) {
	j.mu.Lock()
	defer j.mu.Unlock()
	f()
	mark := string(j.status)
	if j.status == jobDone && j.derived != "" {
		mark += "/" + j.derived
	}
	if len(j.trail) == 0 || j.trail[len(j.trail)-1] != mark {
		j.trail = append(j.trail, mark)
	}
	close(j.changed)
	j.changed = make(chan struct{})
}

// setState moves the job to status and phase.
func (j *job) setState(status jobStatus, phase string) {
	j.set(func() { j.status, j.phase = status, phase })
}

// etaSlack is what a capture takes beyond its duration, as eta counts it.
const etaSlack = 5 * time.Second

// view is the job's state now.
func (j *job) view() jobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.viewLocked(time.Now())
}

func (j *job) viewLocked(now time.Time) jobView {
	v := jobView{Job: j.id, ID: j.captureID, Status: string(j.status), Phase: j.phase}
	if j.status == jobRunning {
		if r := j.run.Load(); r != nil {
			v.Phase = r.Phase()
		}
	}
	if j.status == jobDone {
		v.Derived = j.derived
	}
	from := j.began
	if from.IsZero() {
		from = j.created
	}
	elapsed := now.Sub(from)
	v.Elapsed = elapsed.Round(100 * time.Millisecond).String()
	if !j.status.terminal() && v.Phase != phaseUndecided {
		d := j.duration
		if d == 0 {
			d = j.reserve
		}
		v.ETA = max(d+etaSlack-elapsed, 0).Round(100 * time.Millisecond).String()
	}
	return v
}

// watch is the job's state now, whether a wait has nothing more to wait
// for, and the channel closed at its next change.
func (j *job) watch() (jobView, bool, <-chan struct{}) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.viewLocked(time.Now()), j.settled(), j.changed
}

// expected is how long the job is expected to take in all: its effective
// duration, else its reservation, and the slack after it, as eta counts.
func (j *job) expected() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return cmp.Or(j.duration, j.reserve) + etaSlack
}

// settled reports whether a wait has nothing more to wait for: the job
// failed, was cancelled, or is done with its derivations made.
func (j *job) settled() bool {
	switch j.status {
	case jobFailed, jobCancelled:
		return true
	case jobDone:
		return j.derived != derivedPending
	}
	return false
}

// wait returns the job's state once it has settled, or when d has passed
// or ctx is done, whichever is first. It starts no goroutine: one timer and
// a select on the job's change channel.
func (j *job) wait(ctx context.Context, d time.Duration) jobView {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		j.mu.Lock()
		if j.settled() {
			v := j.viewLocked(time.Now())
			j.mu.Unlock()
			return v
		}
		ch := j.changed
		j.mu.Unlock()
		select {
		case <-ch:
		case <-timer.C:
			return j.view()
		case <-ctx.Done():
			return j.view()
		}
	}
}

// requestCancel asks the job to stop and says what will happen. It runs
// on the control path (the reader goroutine): one CAS on the decision word
// and one channel close, no I/O, never a wait. A job that has already
// ended is answered from its final state and its word is left as it is,
// since a failed manifest link leaves the word open. A job whose commit is
// undecided records the cancel, which the settle reports, and answers that
// the decision is pending.
func (j *job) requestCancel() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if a := cmp.Or(j.status.cancelAnswer(), j.ending.cancelAnswer()); a != "" {
		return a
	}
	a := j.d.requestCancel()
	if j.phase == phaseUndecided {
		return cancelAnswerPending
	}
	if a == cancelAnswerCancelling {
		j.cancelOnce.Do(func() { close(j.cancel) })
	}
	return a
}

// historical is a finished capture read back from its manifest, with the
// baseline it was compared with: base when that manifest was read,
// baseGone saying why not when it was not.
type historical struct {
	m        manifest
	base     *manifest
	baseGone string
}
