package perfmcp

import (
	"runtime"
	"sync"
	"testing"
)

// TestACancelRacingAFailedLinkIsNeverLost: a cancel answered "cancelling"
// while the manifest link is in flight is honoured whichever way the link
// goes. When the link fails, settle reports it as cancelled or the word is
// left cancel_requested, so the job rolls back to a cancelled failure; the
// flag never survives the settle that consumed it.
func TestACancelRacingAFailedLinkIsNeverLost(t *testing.T) {
	n := 200000
	if testing.Short() {
		n = 20000
	}
	lost := 0
	for range n {
		var d decisionWord
		if !d.beginManifest() {
			t.Fatal("beginManifest on an open word")
		}
		var ans, set string
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() { <-start; ans = d.requestCancel() })
		wg.Go(func() { <-start; runtime.Gosched(); set = d.settle(false) })
		close(start)
		wg.Wait()
		if ans == cancelAnswerTooLate {
			t.Fatal("a cancel was answered too late although the link failed")
		}
		if ans == cancelAnswerCancelling && set != cancelAnswerCancelled && !d.cancelled() {
			lost++
		}
	}
	if lost > 0 {
		t.Errorf("%d of %d cancels answered cancelling were never honoured", lost, n)
	}
}

// TestACancelRacingASuccessfulLinkIsTooLate: against a link that succeeds,
// a cancel is either reported by settle or answered "too late: committed",
// never "cancelling" with nothing to show for it.
func TestACancelRacingASuccessfulLinkIsTooLate(t *testing.T) {
	for range 20000 {
		var d decisionWord
		d.beginManifest()
		var ans, set string
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Go(func() { <-start; ans = d.requestCancel() })
		wg.Go(func() { <-start; runtime.Gosched(); set = d.settle(true) })
		close(start)
		wg.Wait()
		if ans == cancelAnswerCancelling && set != cancelAnswerTooLate {
			t.Fatalf("cancel answered %q, settle reported %q", ans, set)
		}
	}
}
