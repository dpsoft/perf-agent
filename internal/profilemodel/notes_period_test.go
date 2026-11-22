package profilemodel

import (
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// periodView is a GPU view whose launched samples carry the given
// gpu_sample_period values, one sample per value.
func periodView(t *testing.T, periods ...string) *View {
	t.Helper()
	var samples []pmfixture.Sample
	for i, p := range periods {
		samples = append(samples, pmfixture.Sample{
			Frames: []pmfixture.Frame{pmfixture.Fn("main" + strings.Repeat("_", i)), pmfixture.Fn("[gpu:launch]"), pmfixture.Fn("[gpu:kernel:k]")},
			Value:  10,
			Labels: map[string][]string{foldedstacks.SamplePeriodLabel: {p}},
		})
	}
	prof, err := FromBytes(pmfixture.Build("gpu", "nanoseconds", nil, samples...), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := prof.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// At period 1 alone every launch carried its stack, so the width under
// [gpu:launch] is that path's share of GPU time: a note saying it is "a
// sample of launches" would be false. Fold says nothing then, and the
// view's notes must agree with it.
func TestViewNotesSayNothingWhenEveryLaunchCarriedAStack(t *testing.T) {
	notes, err := periodView(t, "1", "1").Notes()
	if err != nil {
		t.Fatal(err)
	}
	if notes != nil {
		t.Fatalf("notes = %q", notes)
	}

	notes, err = periodView(t, "1", "8").Notes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "not a sample of GPU time") {
		t.Fatalf("a mixed-period view must keep the sampling note: %q", notes)
	}
}
