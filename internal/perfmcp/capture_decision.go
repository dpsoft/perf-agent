package perfmcp

import "sync/atomic"

// The values of a job's decision word.
const (
	decisionOpen int32 = iota
	decisionCancelRequested
	decisionManifestInFlight
	decisionCommitted
)

// The answers a cancel gets.
const (
	cancelAnswerCancelling = "cancelling"
	cancelAnswerTooLate    = "too late: committed"
	cancelAnswerCancelled  = "cancelled"
)

// decisionWord decides, once per job, whether a cancel or the commit wins
// (§7.4, the terminal decision rule). The control path only ever calls
// requestCancel, which does no I/O and never blocks; the job calls
// beginManifest before it links the manifest and settle once the link has
// resolved. "too late: committed" is said only once the link succeeded,
// and a cancel never loses to a manifest that did not commit.
type decisionWord struct {
	v atomic.Int32
	// cancelInFlight records a cancel that arrived while the manifest link
	// was in flight; settle reports it.
	cancelInFlight atomic.Bool
}

// requestCancel asks the job to stop and says what will happen.
func (d *decisionWord) requestCancel() string {
	for {
		switch v := d.v.Load(); v {
		case decisionOpen:
			if d.v.CompareAndSwap(decisionOpen, decisionCancelRequested) {
				return cancelAnswerCancelling
			}
		case decisionCancelRequested:
			return cancelAnswerCancelling
		case decisionManifestInFlight:
			d.cancelInFlight.Store(true)
			// The job stores committed before it reads cancelInFlight, and
			// this reads the word after storing it, so one of the two sees
			// the other: either settle reports the cancel as too late, or
			// this answer already says so.
			if d.v.Load() == decisionCommitted {
				return cancelAnswerTooLate
			}
			return cancelAnswerCancelling
		default:
			return cancelAnswerTooLate
		}
	}
}

// beginManifest claims the commit: it is false when a cancel came first,
// and the job must then roll back to a cancelled failure instead of
// linking the manifest.
func (d *decisionWord) beginManifest() bool {
	return d.v.CompareAndSwap(decisionOpen, decisionManifestInFlight)
}

// settle records the manifest link's outcome and returns what to report
// about a cancel that arrived during it: "too late: committed" after a
// link that succeeded, "cancelled" after one that failed, "" when no
// cancel came.
func (d *decisionWord) settle(linked bool) (cancel string) {
	if linked {
		d.v.Store(decisionCommitted)
		if d.cancelInFlight.Load() {
			return cancelAnswerTooLate
		}
		return ""
	}
	if d.cancelInFlight.Load() {
		d.v.Store(decisionCancelRequested)
		return cancelAnswerCancelled
	}
	d.v.Store(decisionOpen)
	return ""
}

// cancelled reports whether a cancel is waiting to be honoured.
func (d *decisionWord) cancelled() bool {
	return d.v.Load() == decisionCancelRequested
}
