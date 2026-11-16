package perfmcp

import (
	"testing"
	"time"
)

// A job's elapsed stops at its end: a wait long after a capture failed
// or finished shows how long it took, not how long ago it began.
func TestElapsedStopsAtTheEnd(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	j, v := e.run(e.request(300 * time.Millisecond))
	if v.Status != string(jobDone) {
		t.Fatalf("%+v", v)
	}
	time.Sleep(400 * time.Millisecond)
	if later := j.view(); later.Elapsed != v.Elapsed {
		t.Errorf("elapsed went from %s to %s after the end", v.Elapsed, later.Elapsed)
	}
}
