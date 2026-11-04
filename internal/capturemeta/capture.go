package capturemeta

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/pprof/profile"
)

const CaptureIDComment = "perf-agent.capture_id="

// Window records the observed wall-clock lifecycle of a capture, not
// the sum of sample values or the requested duration.
type Window struct {
	ID        string
	StartedAt time.Time
	EndedAt   time.Time
}

func (c Window) Validate() error {
	if c.ID == "" || len(c.ID) > 256 || strings.ContainsAny(c.ID, "\r\n") || c.StartedAt.UnixNano() <= 0 || c.EndedAt.UnixNano() <= c.StartedAt.UnixNano() {
		return fmt.Errorf("invalid profile capture identity or window")
	}
	return nil
}

func FromProfile(p *profile.Profile) (Window, error) {
	var id string
	found := false
	for _, comment := range p.Comments {
		if value, ok := strings.CutPrefix(comment, CaptureIDComment); ok {
			if found {
				return Window{}, fmt.Errorf("profile has multiple capture identities")
			}
			id = value
			found = true
		}
	}
	if p.TimeNanos <= 0 || p.DurationNanos <= 0 || p.TimeNanos > int64(^uint64(0)>>1)-p.DurationNanos {
		return Window{}, fmt.Errorf("profile needs a valid capture time and duration to join GPU telemetry")
	}
	c := Window{ID: id, StartedAt: time.Unix(0, p.TimeNanos), EndedAt: time.Unix(0, p.TimeNanos+p.DurationNanos)}
	return c, c.Validate()
}
