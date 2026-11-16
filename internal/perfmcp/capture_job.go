package perfmcp

import (
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
	}
}

// requestCancel asks the job to stop and says what will happen. It runs
// on the control path (the reader goroutine): one CAS on the decision word
// and one channel close, no I/O, never a wait. A job that has already
// ended is answered from its final state and its word is left as it is,
// since a failed manifest link leaves the word open.
func (j *job) requestCancel() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if a := j.status.cancelAnswer(); a != "" {
		return a
	}
	a := j.d.requestCancel()
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
