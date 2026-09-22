package foldedstacks

import (
	"testing"

	"github.com/google/pprof/profile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// foldOne folds a one-sample-type profile whose stacks are given root-first,
// which is how Fold normalizes them regardless of the input order.
func foldOne(t *testing.T, typ string, stacks ...stackSpec) *Result {
	t.Helper()
	p := oneType()
	p.SampleType[0].Type = typ
	for _, sp := range stacks {
		locs := make([]*profile.Location, 0, len(sp.frames))
		for i, f := range sp.frames {
			locs = append(locs, loc(uint64(i+1), f))
		}
		p.Sample = append(p.Sample, sample(sp.value, nil, locs...))
	}
	res, err := Fold(p, Options{SampleIndex: -1})
	require.NoError(t, err)
	return res
}

type stackSpec struct {
	value  int64
	frames []string
}

func TestFusePrefixesEachInputWithItsOwnRoot(t *testing.T) {
	// The whole point: after fusing, every stack from an input hangs under
	// that input's label, so one graph can hold two regimes without the
	// reader having to know which is which.
	cpu := foldOne(t, "cpu", stackSpec{value: 30, frames: []string{"main", "work"}})
	gpu := foldOne(t, "gpu", stackSpec{value: 70, frames: []string{"[gpu:launch]", "kernel"}})

	out, err := Fuse([]FuseInput{
		{Result: cpu, Label: "[cpu]"},
		{Result: gpu, Label: "[gpu]"},
	})
	require.NoError(t, err)

	require.Len(t, out.Stacks, 2)
	assert.Equal(t, []string{"[cpu]", "main", "work"}, out.Stacks[0].Frames)
	assert.Equal(t, []string{"[gpu]", "[gpu:launch]", "kernel"}, out.Stacks[1].Frames)
	assert.Equal(t, int64(100), out.Total)
}

func TestFuseKeepsModulesAndCollapsedAlignedWithFrames(t *testing.T) {
	// The bug this feature exists to avoid: a merge that loses Modules
	// silently disables vendor-run collapsing downstream, because
	// isVendorModule keys on the module and not on the frame name.
	cpu := foldOne(t, "cpu", stackSpec{value: 1, frames: []string{"a", "b"}})

	out, err := Fuse([]FuseInput{{Result: cpu, Label: "[cpu]"}})
	require.NoError(t, err)

	for _, s := range out.Stacks {
		assert.Len(t, s.Modules, len(s.Frames), "Modules must stay parallel to Frames")
		if s.Collapsed != nil {
			assert.Len(t, s.Collapsed, len(s.Frames), "Collapsed must stay parallel to Frames")
		}
	}
}

func TestFuseRefusesToAddDifferentUnits(t *testing.T) {
	// Summing nanoseconds and bytes onto one axis would be a lie the graph
	// makes very easy to tell.
	ns := foldOne(t, "cpu", stackSpec{value: 1, frames: []string{"a"}})
	other := foldOne(t, "alloc", stackSpec{value: 1, frames: []string{"a"}})
	other.Unit = "bytes"

	_, err := Fuse([]FuseInput{{Result: ns, Label: "[a]"}, {Result: other, Label: "[b]"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unit")
}

func TestFuseResultIsRootFirstWhateverTheInputsWere(t *testing.T) {
	// Fold already normalizes Frames to root-first, so the fused result is
	// root-first by construction. Recording it stops the renderer from
	// having to guess -- the failure mode of issue #155.
	cpu := foldOne(t, "cpu", stackSpec{value: 1, frames: []string{"a"}})
	cpu.StackOrder = LeafFirst // as read from a leaf-first file

	out, err := Fuse([]FuseInput{{Result: cpu, Label: "[cpu]"}})
	require.NoError(t, err)
	assert.Equal(t, RootFirst, out.StackOrder)
}

func TestFuseSumsCountersAndDeepensMaxDepthByTheRoot(t *testing.T) {
	cpu := foldOne(t, "cpu", stackSpec{value: 1, frames: []string{"a", "b"}})
	gpu := foldOne(t, "gpu", stackSpec{value: 1, frames: []string{"c"}})

	out, err := Fuse([]FuseInput{{Result: cpu, Label: "[cpu]"}, {Result: gpu, Label: "[gpu]"}})
	require.NoError(t, err)

	assert.Equal(t, cpu.Samples+gpu.Samples, out.Samples)
	assert.Equal(t, cpu.Frames+gpu.Frames+2, out.Frames, "each stack gains one root frame")
	assert.Equal(t, 3, out.MaxDepth, "deepest input was 2, plus its root")
}

func TestFuseRequiresALabelPerInput(t *testing.T) {
	// An unlabelled root is indistinguishable from a real frame, which
	// defeats the entire purpose.
	cpu := foldOne(t, "cpu", stackSpec{value: 1, frames: []string{"a"}})

	_, err := Fuse([]FuseInput{{Result: cpu}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "label")
}

func TestFuseRejectsNoInputsButAcceptsOne(t *testing.T) {
	// One input relabels a single profile, which is meaningful; the
	// "two or more" minimum is the CLI's to enforce, where there is a user
	// to tell.
	_, err := Fuse(nil)
	require.Error(t, err)

	only := foldOne(t, "cpu", stackSpec{value: 1, frames: []string{"a"}})
	out, err := Fuse([]FuseInput{{Result: only, Label: "[cpu]"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"[cpu]", "a"}, out.Stacks[0].Frames)
}
