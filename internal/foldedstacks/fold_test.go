package foldedstacks

import (
	"strings"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loc builds a location whose Line entries carry the given function names,
// in pprof's order (index 0 innermost).
func loc(addr uint64, names ...string) *profile.Location {
	l := &profile.Location{Address: addr}
	for _, n := range names {
		l.Line = append(l.Line, profile.Line{Function: &profile.Function{Name: n}})
	}
	return l
}

func sample(v int64, labels map[string][]string, locs ...*profile.Location) *profile.Sample {
	return &profile.Sample{Location: locs, Value: []int64{v}, Label: labels}
}

func oneType(samples ...*profile.Sample) *profile.Profile {
	return &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "gpu", Unit: "nanoseconds"}},
		Sample:     samples,
	}
}

func TestFoldRootFirstIsTheDefaultBecauseThatIsWhatPerfAgentWrites(t *testing.T) {
	// perf-agent calls pprof.Reverse before building, so Location[0] is the
	// root. Folding leaf-first would draw a correct-looking graph upside
	// down, which is why this has its own test rather than an assumption.
	p := oneType(sample(7, nil, loc(1, "root"), loc(2, "mid"), loc(3, "leaf")))

	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	require.Len(t, res.Stacks, 1)
	assert.Equal(t, []string{"root", "mid", "leaf"}, res.Stacks[0].Frames)
	assert.Equal(t, int64(7), res.Total)
	assert.Equal(t, RootFirst, res.StackOrder)
	assert.Equal(t, "root-first", res.StackOrder.String())
}

func TestFoldLeafFirstReversesTheStack(t *testing.T) {
	p := oneType(sample(7, nil, loc(1, "leaf"), loc(2, "mid"), loc(3, "root")))

	res, err := Fold(p, Options{SampleIndex: -1, StackOrder: LeafFirst})
	require.NoError(t, err)
	require.Len(t, res.Stacks, 1)
	assert.Equal(t, []string{"root", "mid", "leaf"}, res.Stacks[0].Frames)
	assert.Equal(t, "leaf-first", res.StackOrder.String())
}

func TestFoldExpandsInlinedFramesCallerFirst(t *testing.T) {
	// pprof: "the last entry represents the caller into which the preceding
	// entries were inlined". Emitting Line in storage order would invert
	// every inlined chain.
	p := oneType(sample(3, nil,
		loc(1, "caller"),
		loc(2, "innermost_inlined", "middle_inlined", "outer"),
	))

	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"caller", "outer", "middle_inlined", "innermost_inlined"},
		res.Stacks[0].Frames)
	assert.Equal(t, 2, res.InlinedFrames)
	assert.Equal(t, 4, res.Frames)
}

func TestFoldRendersUnsymbolizedFramesRatherThanDroppingThem(t *testing.T) {
	// A flame graph that drops what it cannot name looks cleanest exactly
	// when symbolization worked worst.
	noLines := &profile.Location{Address: 0x7f2c945ace62}
	p := oneType(sample(5, nil, loc(1, "main"), noLines))

	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, []string{"main", "0x7f2c945ace62"}, res.Stacks[0].Frames)
	assert.Equal(t, 1, res.AddressOnlyFrames)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "have no symbol")
}

func TestFoldCountsAlreadyHexNamedFramesAsUnsymbolized(t *testing.T) {
	// perf-agent's symbolizer formats an unresolved PC as its own name, so
	// there is no "unsymbolized" bit left in the written profile to consult.
	p := oneType(sample(5, nil, loc(1, "main"), loc(2, "0x7f2c945ace62")))

	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, 1, res.AddressOnlyFrames)
}

func TestFoldFallsBackThroughEmptyFunctionNames(t *testing.T) {
	sys := &profile.Location{Address: 9, Line: []profile.Line{
		{Function: &profile.Function{Name: "", SystemName: "_ZN3fooEv"}},
	}}
	file := &profile.Location{Address: 10, Line: []profile.Line{
		{Function: &profile.Function{Name: "  ", Filename: "a.cc"}, Line: 42},
	}}
	nameless := &profile.Location{Address: 11, Line: []profile.Line{{Function: &profile.Function{}}}}
	nothing := &profile.Location{Line: []profile.Line{{}}}

	p := oneType(sample(1, nil, sys, file, nameless, nothing))
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, []string{"_ZN3fooEv", "a.cc:42", "0xb", UnknownFrame}, res.Stacks[0].Frames)
}

func TestFoldCarriesTheMappingFilePerFrame(t *testing.T) {
	kern := &profile.Mapping{File: "[kernel]"}
	l := loc(1, "schedule")
	l.Mapping = kern
	p := oneType(sample(1, nil, loc(2, "main"), l))

	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, []string{"", "[kernel]"}, res.Stacks[0].Modules)
	assert.Len(t, res.Stacks[0].Modules, len(res.Stacks[0].Frames))
}

func TestFoldAggregatesIdenticalStacksAndSumsValues(t *testing.T) {
	p := oneType(
		sample(3, nil, loc(1, "a"), loc(2, "b")),
		sample(4, nil, loc(1, "a"), loc(2, "b")),
		sample(5, nil, loc(1, "a"), loc(3, "c")),
	)
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	require.Len(t, res.Stacks, 2)
	assert.Equal(t, int64(12), res.Total)
	byPath := map[string]int64{}
	for _, s := range res.Stacks {
		byPath[strings.Join(s.Frames, ";")] = s.Value
	}
	assert.Equal(t, int64(7), byPath["a;b"])
	assert.Equal(t, int64(5), byPath["a;c"])
}

func TestFoldKeepsLabelsOutOfFramesAndSummarisesThemInstead(t *testing.T) {
	// Spec §8: frames are stack identity. Folding a per-sample correlation
	// ID into the path would give every sample its own leaf.
	p := oneType(
		sample(10, map[string][]string{"gpu_correlation": {"cupti:1"}, "gpu_join": {"exact"}}, loc(1, "k")),
		sample(20, map[string][]string{"gpu_correlation": {"cupti:2"}, "gpu_join": {"exact"}}, loc(1, "k")),
	)
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)

	require.Len(t, res.Stacks, 1, "labels must not fragment the folded stack")
	assert.Equal(t, []string{"k"}, res.Stacks[0].Frames)
	assert.Equal(t, int64(30), res.Total)

	require.Len(t, res.Labels, 2)
	assert.Equal(t, "gpu_correlation", res.Labels[0].Key)
	assert.Equal(t, 2, res.Labels[0].Distinct)
	assert.Equal(t, int64(30), res.Labels[0].Total)
	assert.Equal(t, "gpu_join", res.Labels[1].Key)
	assert.Equal(t, 1, res.Labels[1].Distinct)
	assert.Equal(t, "exact", res.Labels[1].Top[0].Value)
}

func TestFoldMarksInferredGPUJoinsAsInexact(t *testing.T) {
	// gpu/projection.go writes gpu_join unconditionally as exact/heuristic/
	// unmatched. A heuristic join means the CPU call path under the launch
	// was guessed; the graph must be able to say so.
	p := oneType(
		sample(10, map[string][]string{"gpu_join": {"exact"}}, loc(1, "a"), loc(2, "k")),
		sample(30, map[string][]string{"gpu_join": {"heuristic"}}, loc(1, "a"), loc(2, "k")),
		sample(5, map[string][]string{"gpu_join": {"exact"}, "gpu_ambiguous": {"true"}}, loc(1, "a"), loc(2, "k")),
	)
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	require.Len(t, res.Stacks, 1)
	assert.Equal(t, int64(45), res.Stacks[0].Value)
	assert.Equal(t, int64(35), res.Stacks[0].Inexact)
	assert.Equal(t, int64(35), res.InexactTotal)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "by inference, not measurement")
}

func TestFoldExactJoinsAreNeverInexact(t *testing.T) {
	p := oneType(sample(10, map[string][]string{"gpu_join": {"exact"}}, loc(1, "k")))
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Zero(t, res.InexactTotal)
	assert.NotContains(t, strings.Join(res.Warnings, "\n"), "by inference")
}

func TestFoldOnAnEmptyProfileReportsRatherThanReturningNothing(t *testing.T) {
	res, err := Fold(oneType(), Options{SampleIndex: -1})
	require.NoError(t, err, "an empty profile is a fact to report, not an error to swallow")
	assert.True(t, res.Degenerate())
	assert.Zero(t, res.Total)
	require.NotEmpty(t, res.Warnings)
	assert.Contains(t, res.Warnings[0], "no samples at all")
}

func TestFoldOnAnAllZeroProfileSaysSo(t *testing.T) {
	p := oneType(sample(0, nil, loc(1, "a")), sample(0, nil, loc(2, "b")))
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.True(t, res.Degenerate())
	assert.Equal(t, 2, res.ZeroValueSamples)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "carries the value 0")
}

func TestFoldRefusesADifferentialProfile(t *testing.T) {
	p := oneType(sample(-5, nil, loc(1, "a")))
	_, err := Fold(p, Options{SampleIndex: -1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "differential")
}

func TestFoldKeepsValuedSamplesThatHaveNoStack(t *testing.T) {
	p := oneType(sample(9, nil))
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, 1, res.EmptyStackSamples)
	assert.Equal(t, []string{UnknownFrame}, res.Stacks[0].Frames)
	assert.Equal(t, int64(9), res.Total)
}

func TestChooseSampleIndexPrefersDefaultSampleType(t *testing.T) {
	p := &profile.Profile{
		SampleType: []*profile.ValueType{
			{Type: "alloc_objects", Unit: "count"},
			{Type: "alloc_space", Unit: "bytes"},
		},
		DefaultSampleType: "alloc_objects",
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc(1, "a")}, Value: []int64{3, 4096}},
		},
	}
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, 0, res.SampleIndex)
	assert.Equal(t, "alloc_objects", res.SampleTypeName)
	assert.Equal(t, int64(3), res.Total)
}

func TestChooseSampleIndexFallsBackToTheLastType(t *testing.T) {
	// google/pprof's own driver treats the last sample type as the default,
	// which for this repo's memory builder picks bytes over object counts.
	p := &profile.Profile{
		SampleType: []*profile.ValueType{
			{Type: "alloc_objects", Unit: "count"},
			{Type: "alloc_space", Unit: "bytes"},
		},
		Sample: []*profile.Sample{
			{Location: []*profile.Location{loc(1, "a")}, Value: []int64{3, 4096}},
		},
	}
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Equal(t, 1, res.SampleIndex)
	assert.Equal(t, "alloc_space", res.SampleTypeName)
	assert.Equal(t, int64(4096), res.Total)
	assert.Contains(t, strings.Join(res.Warnings, "\n"), "value axes")
}

func TestFoldRejectsAnOutOfRangeSampleIndex(t *testing.T) {
	_, err := Fold(oneType(sample(1, nil, loc(1, "a"))), Options{SampleIndex: 4})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

func TestFoldIsDeterministic(t *testing.T) {
	build := func() *profile.Profile {
		return oneType(
			sample(1, nil, loc(1, "z"), loc(2, "y")),
			sample(2, nil, loc(3, "a"), loc(4, "b")),
			sample(3, nil, loc(5, "m")),
		)
	}
	var first string
	for range 5 {
		res, err := Fold(build(), Options{SampleIndex: -1})
		require.NoError(t, err)
		var b strings.Builder
		_, err = res.WriteFolded(&b)
		require.NoError(t, err)
		if first == "" {
			first = b.String()
			continue
		}
		assert.Equal(t, first, b.String(), "two folds of the same profile must be byte-identical")
	}
	assert.Equal(t, "a;b 2\nm 3\nz;y 1\n", first)
}

func TestWriteFoldedSubstitutesSeparatorsInsideFrameNames(t *testing.T) {
	p := oneType(sample(1, nil, loc(1, "weird;name"), loc(2, "two\nlines")))
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)

	var b strings.Builder
	n, err := res.WriteFolded(&b)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the caller must be told the text form was lossy")
	assert.Equal(t, "weird；name;two lines 1\n", b.String())
	assert.Equal(t, []string{"weird;name", "two\nlines"}, res.Stacks[0].Frames,
		"the structured stacks the renderer consumes must keep the original name")
}

func TestFoldRejectsNilAndTypelessProfiles(t *testing.T) {
	_, err := Fold(nil, Options{})
	require.Error(t, err)

	_, err = Fold(&profile.Profile{}, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no sample types")
}

// The shape of a sampled-launch GPU profile is actively misleading without
// this. gpu/projection.go samples launch STACKS one-in-N but records every
// execution, so the attributed call path is narrow by roughly N while the
// rest of the measured GPU time sits under [gpu:launch unsampled] with no
// caller. A reader who takes the attributed width as "the share of GPU time
// this path is responsible for" is wrong by that factor.
func TestFoldExplainsWhatLaunchSamplingDoesToTheShape(t *testing.T) {
	p := oneType(
		sample(5590723, map[string][]string{"gpu_join": {"exact"}},
			loc(1, UnsampledLaunchFrame), loc(2, "[gpu:kernel:k]")),
		sample(397131, map[string][]string{"gpu_join": {"exact"}, SamplePeriodLabel: {"8"}},
			loc(3, "main"), loc(4, "[gpu:launch]"), loc(2, "[gpu:kernel:k]")),
	)
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)

	assert.Equal(t, int64(5590723), res.UnsampledLaunchTotal)
	joined := strings.Join(res.Warnings, "\n")
	assert.Contains(t, joined, "93.4% of the total sits under [gpu:launch unsampled]")
	assert.Contains(t, joined, "one launch in 8 contributed a CPU stack")
	assert.Contains(t, joined, "not a sample of GPU time")
	assert.Contains(t, joined, "every value is a measured duration",
		"the page must not let a reader think the widths were scaled up by the period")
}

func TestFoldSaysNothingAboutSamplingWhenThereIsNone(t *testing.T) {
	p := oneType(sample(10, nil, loc(1, "main"), loc(2, "work")))
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	assert.Zero(t, res.UnsampledLaunchTotal)
	joined := strings.Join(res.Warnings, "\n")
	assert.NotContains(t, joined, "launch")
	assert.NotContains(t, joined, "sample of GPU time")
}
