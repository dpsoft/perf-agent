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
	// cancelAnswerPending answers a cancel of a capture whose commit nobody
	// could read yet: it may have committed, and the cancel is reported
	// once that is settled.
	cancelAnswerPending = "cancelling (decision pending)"
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
			// settle stores the outcome before it reads cancelInFlight, and
			// this reads the word after storing the flag, so one of the two
			// sees the other. Still in flight: settle will see the flag.
			// Committed: the answer is too late. Back to open or
			// cancel_requested: the link failed and settle may have read
			// the flag before it was set, so go round and ask again, which
			// moves an open word to cancel_requested.
			switch d.v.Load() {
			case decisionManifestInFlight:
				return cancelAnswerCancelling
			case decisionCommitted:
				return cancelAnswerTooLate
			}
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
		if d.cancelInFlight.Swap(false) {
			return cancelAnswerTooLate
		}
		return ""
	}
	// The word leaves manifest_in_flight before the flag is read, so a
	// cancel that sets the flag later sees the word move and asks again.
	d.v.Store(decisionOpen)
	if d.cancelInFlight.Swap(false) {
		d.v.Store(decisionCancelRequested)
		return cancelAnswerCancelled
	}
	return ""
}

// commitAfter records a commit found after a failed manifest link was
// settled (the link was made after all): the word becomes committed, and a
// cancel requested meanwhile is reported as too late.
func (d *decisionWord) commitAfter() (cancel string) {
	prev := d.v.Swap(decisionCommitted)
	if d.cancelInFlight.Swap(false) || prev == decisionCancelRequested {
		return cancelAnswerTooLate
	}
	return ""
}

// cancelPending reports whether a cancel arrived while the manifest link
// was in flight and has not been settled yet. It changes nothing.
func (d *decisionWord) cancelPending() bool { return d.cancelInFlight.Load() }

// cancelled reports whether a cancel is waiting to be honoured.
func (d *decisionWord) cancelled() bool {
	return d.v.Load() == decisionCancelRequested
}
