package capturemeta

import (
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
)

func TestCaptureWindowUsesAbsoluteUTCAndRejectsReversedClock(t *testing.T) {
	start := time.Unix(1700000000, 123)
	end := time.Unix(1700000002, 456)
	c := Window{ID: "capture", StartedAt: start, EndedAt: end}
	require.NoError(t, c.Validate())
	p := &profile.Profile{TimeNanos: start.UnixNano(), DurationNanos: end.UnixNano() - start.UnixNano(), Comments: []string{CaptureIDComment + c.ID}}
	got, err := FromProfile(p)
	require.NoError(t, err)
	require.Equal(t, c, got)
	c.EndedAt = start.Add(-time.Nanosecond)
	require.Error(t, c.Validate())
	p.Comments = []string{CaptureIDComment, CaptureIDComment + "other"}
	_, err = FromProfile(p)
	require.ErrorContains(t, err, "multiple capture identities")
}
