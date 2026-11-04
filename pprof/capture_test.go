package pprof

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capturemeta"
	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/require"
)

func TestCaptureMetadataForEverySampleTypeAndWriter(t *testing.T) {
	for _, kind := range []SampleType{SampleTypeCpu, SampleTypeOffCpu, SampleTypeGpu} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			builders := NewProfileBuilders(BuildersOptions{SampleRate: 99, Comments: []string{"keep me", CaptureIDComment + "old"}})
			sample := &ProfileSample{SampleType: kind, Value: 10, Stack: []Frame{{Name: "work"}}}
			builder := builders.BuilderForSample(sample)
			builder.CreateSample(sample)
			c := CaptureWindow{ID: "capture-test", StartedAt: time.Unix(1700000000, 1234), EndedAt: time.Unix(1700000003, 5678)}
			builders.SetCapture(c)
			var output bytes.Buffer
			_, err := builder.Write(&output)
			require.NoError(t, err)
			parsed, err := profile.Parse(&output)
			require.NoError(t, err)
			got, err := capturemeta.FromProfile(parsed)
			require.NoError(t, err)
			require.Equal(t, c, got)
			require.Contains(t, parsed.Comments, "keep me")
			require.NotContains(t, parsed.Comments, CaptureIDComment+"old")
			require.Equal(t, int64(0), builder.Profile.DurationNanos, "serialization must not mutate the live builder header")
		})
	}
}

func TestInvalidCaptureWindowStillWritesReadableUnlinkedProfile(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99, Comments: []string{CaptureIDComment + "stale"}})
	sample := &ProfileSample{SampleType: SampleTypeCpu, Value: 10, Stack: []Frame{{Name: "work"}}}
	builder := builders.BuilderForSample(sample)
	builder.CreateSample(sample)
	start := time.Now()
	builders.SetCapture(CaptureWindow{ID: "capture", StartedAt: start, EndedAt: start.Add(-time.Second)})
	var output bytes.Buffer
	_, err := builder.Write(&output)
	require.NoError(t, err)
	p, err := profile.Parse(&output)
	require.NoError(t, err)
	require.Len(t, p.Sample, 1)
	require.Zero(t, p.DurationNanos)
	for _, comment := range p.Comments {
		require.NotContains(t, comment, CaptureIDComment)
	}
	_, err = capturemeta.FromProfile(p)
	require.Error(t, err)
}
