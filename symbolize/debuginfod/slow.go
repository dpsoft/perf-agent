package debuginfod

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// A download that cannot finish within the capture's fetch budget is stopped
// early and remembered on disk, so the next capture does not pay for it again.
// Issue #224.
//
// Without this, such a file cost the whole budget on every capture and was
// never cached: the fetch timeout cut the transfer off part-way, WriteAtomic
// removed the partial file, and the in-memory failure memo died with the
// process. Measured against debuginfod.fedoraproject.org at ~1.2MB/s, Mesa's
// libgallium debuginfo is 233MB -- about three minutes -- so a 5s budget paid
// 5s per capture for nothing. The server cannot resume either: elfutils
// debuginfod ignores Range and answers HEAD with 400, so a partial download
// cannot be continued by the next run.

// slowProbe is how long a transfer runs before its rate is trusted enough to
// project a finish time. Long enough to get past TCP slow start on a
// high-latency link, short enough that an impossible download costs ~1s
// rather than the whole budget.
const slowProbe = time.Second

// slowMarkerTTL is how long a "too slow" verdict stands. The rate it was
// based on is a property of the network at the time, so it must expire; a day
// keeps a workstation's repeated captures cheap without making one bad hour
// permanent.
const slowMarkerTTL = 24 * time.Hour

// slowMarker is the on-disk record of a download that could not fit the
// budget. Size and rate are kept rather than a bare flag so the verdict can be
// re-judged against the budget in force: raising --symbol-fetch-timeout far
// enough retries the file without anyone deleting the marker.
type slowMarker struct {
	Size        int64     `json:"size"`
	BytesPerSec float64   `json:"bytes_per_sec"`
	At          time.Time `json:"at"`
}

// needs is the projected time to download the whole file at the measured rate.
func (m slowMarker) needs() time.Duration {
	if m.BytesPerSec <= 0 {
		return 0
	}
	return time.Duration(float64(m.Size) / m.BytesPerSec * float64(time.Second))
}

// errTooSlow reports a download stopped, or skipped, because it could not
// finish within the budget at the measured rate.
type errTooSlow struct {
	marker  slowMarker
	budget  time.Duration
	left    time.Duration // of the budget, when the transfer started; 0 if skipped
	skipped bool          // true: skipped on a marker from an earlier capture
}

func (e *errTooSlow) Error() string {
	verb := "stopped"
	if e.skipped {
		verb = "skipped"
	}
	needs := e.marker.needs()
	limit := fmt.Sprintf("the %v fetch budget", e.budget)
	// Stopped because this call had already spent most of its budget on other
	// files, not because the file is too large for one: say so, or "needs ~3s,
	// more than the 5s budget" reads as a contradiction.
	if !e.skipped && needs <= e.budget {
		limit = fmt.Sprintf("the %v left of %s", e.left.Round(100*time.Millisecond), limit)
	}
	return fmt.Sprintf("%s: %.0fMB at %.1fMB/s needs ~%v, more than %s",
		verb, float64(e.marker.Size)/1e6, e.marker.BytesPerSec/1e6,
		needs.Round(time.Second), limit)
}

// slowMarkerPath is where the marker for (kind, buildID) lives. Outside
// .build-id on purpose: cache.parsePath takes every file there without a
// .debug suffix for a cached executable, so a marker beside the artifact
// would be indexed, and served, as one.
func slowMarkerPath(dir, kind, buildID string) string {
	return filepath.Join(dir, "slow", kind+"-"+buildID+".json")
}

func readSlowMarker(dir, kind, buildID string) (slowMarker, bool) {
	b, err := os.ReadFile(slowMarkerPath(dir, kind, buildID))
	if err != nil {
		return slowMarker{}, false
	}
	var m slowMarker
	if json.Unmarshal(b, &m) != nil || time.Since(m.At) > slowMarkerTTL {
		return slowMarker{}, false
	}
	return m, true
}

// writeSlowMarker is best-effort: a marker that cannot be written costs one
// more early-stopped attempt next capture, which is the behaviour without it.
func writeSlowMarker(dir, kind, buildID string, m slowMarker) {
	p := slowMarkerPath(dir, kind, buildID)
	if os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, p)
	}
}

func removeSlowMarker(dir, kind, buildID string) {
	_ = os.Remove(slowMarkerPath(dir, kind, buildID))
}

// budgetReader stops a transfer as soon as it is clear it will not finish
// before deadline, instead of letting the deadline cut it off at the end.
//
// It needs the size up front (Content-Length) and a deadline; without either
// it is not installed and the transfer runs as before.
type budgetReader struct {
	r        io.Reader
	size     int64
	got      int64
	start    time.Time
	deadline time.Time
	budget   time.Duration // only reported, in the error
	now      func() time.Time
}

func (b *budgetReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.got += int64(n)
	if err != nil || b.got >= b.size {
		return n, err
	}
	now := b.now()
	elapsed := now.Sub(b.start)
	if elapsed < slowProbe {
		return n, nil
	}
	rate := float64(b.got) / elapsed.Seconds()
	if rate <= 0 {
		return n, nil
	}
	remaining := time.Duration(float64(b.size-b.got) / rate * float64(time.Second))
	if now.Add(remaining).After(b.deadline) {
		// Complete here: WriteAtomic formats this into its own error at once,
		// so a field set after it returns would not reach the message.
		return n, &errTooSlow{
			marker: slowMarker{Size: b.size, BytesPerSec: rate, At: now},
			budget: b.budget,
			left:   b.deadline.Sub(b.start),
		}
	}
	return n, nil
}

// asTooSlow unwraps an errTooSlow, which WriteAtomic wraps as "copy: ...".
func asTooSlow(err error) (*errTooSlow, bool) {
	var ts *errTooSlow
	ok := errors.As(err, &ts)
	return ts, ok
}
