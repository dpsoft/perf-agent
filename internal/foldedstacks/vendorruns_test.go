package foldedstacks

import (
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lt = "/usr/lib/python3/site-packages/nvidia/cu13/lib/libcublasLt.so.13"
const bl = "/usr/lib/python3/site-packages/nvidia/cu13/lib/libcublas.so.13"

// The measured shape: a fixed run of address-only frames from one library,
// which after the profiler's own band is elided is what still stands between
// the application and the kernel.
func TestARunOfAddressOnlyVendorFramesBecomesTheModule(t *testing.T) {
	frames := []string{
		"torch::autograd::Engine::execute",
		"libcublasLt.so.13+0xf8e200",
		"libcublasLt.so.13+0xe2b573",
		"libcublasLt.so.13+0x1df0eb0",
		"cuLaunchKernel",
	}
	mods := []string{"/usr/lib/libtorch_cuda.so", lt, lt, lt, "/usr/lib/libcuda.so.610"}

	gotF, gotM, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, []string{
		"torch::autograd::Engine::execute",
		"libcublasLt.so.13",
		"cuLaunchKernel",
	}, gotF)
	assert.Equal(t, lt, gotM[1], "the collapsed frame keeps the module it came from")
	assert.Equal(t, 2, removed)
}

// The symbol server serves OBFUSCATED names for internal functions, so the
// frames are symbolized and a rule testing for a missing name would never fire
// in the configuration this project recommends. That is the whole reason the
// rule cannot key on "unsymbolized" as #122 originally proposed.
func TestObfuscatedSymbolServerNamesAreCollapsedToo(t *testing.T) {
	frames := []string{
		"main",
		"libcublasLt_82ed00161d0a6b6290add9bc600fcefbbf94f832",
		"libcublasLt_c44dd15e5b30f7ac1684653f64405e357708e83b",
		"cuLaunchKernel",
	}
	mods := []string{"/app/main", lt, lt, "/usr/lib/libcuda.so.610"}

	gotF, _, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, []string{"main", "libcublasLt.so.13", "cuLaunchKernel"}, gotF)
	assert.Equal(t, 1, removed)
}

// The thing this must never do. Real names are what a flame graph exists to
// show, and a run of them from one library is a call path, not noise.
func TestNamedFramesFromOneLibraryAreNeverMerged(t *testing.T) {
	torch := "/usr/lib/libtorch_cuda.so"
	frames := []string{
		"at::native::add_kernel",
		"at::TensorIteratorBase::build",
		"at::native::gpu_kernel_impl",
		"c10::cuda::CUDAStream::synchronize",
	}
	mods := []string{torch, torch, torch, torch}

	gotF, _, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, frames, gotF, "four things a reader can look up must stay four")
	assert.Zero(t, removed)
}

// Two adjacent vendor libraries are two different places; merging them would
// invent a call path through neither.
func TestAdjacentDifferentModulesDoNotMerge(t *testing.T) {
	frames := []string{
		"libcublas.so.13+0x2cc690",
		"libcublas.so.13+0x1c6b85",
		"libcublasLt.so.13+0xf8e200",
		"libcublasLt.so.13+0xe2b573",
	}
	mods := []string{bl, bl, lt, lt}

	gotF, _, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, []string{"libcublas.so.13", "libcublasLt.so.13"}, gotF)
	assert.Equal(t, 2, removed)
}

// A lone uninformative frame is left exactly as it is: renaming it would
// discard the address without collapsing anything, which is strictly less
// information for no gain.
func TestALoneUninformativeFrameKeepsItsAddress(t *testing.T) {
	frames := []string{"main", "libcublasLt.so.13+0xf8e200", "cuLaunchKernel"}
	mods := []string{"/app/main", lt, "/usr/lib/libcuda.so.610"}

	gotF, _, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, frames, gotF)
	assert.Zero(t, removed)
}

// The "+0x" test is anchored on the module's own basename, so a genuine symbol
// that merely contains the substring cannot be mistaken for an address.
func TestASymbolContainingPlus0xIsNotAnAddress(t *testing.T) {
	mods := []string{lt, lt}
	frames := []string{"operator+0x_helper", "another+0x_helper"}

	gotF, _, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, frames, gotF, "these are names, however unusual")
	assert.Zero(t, removed)
}

func TestAFrameWithNoModuleIsNeverCollapsed(t *testing.T) {
	frames := []string{"libcublasLt.so.13+0xf8e200", "libcublasLt.so.13+0xe2b573"}
	mods := []string{"", ""}

	gotF, _, _, removed := collapseVendorRuns(frames, mods)

	assert.Equal(t, frames, gotF, "with no module there is nothing to name the merge after")
	assert.Zero(t, removed)
}

// The constraint from #122's research, and the one that decides whether any of
// this achieves anything: the run LENGTH must not reach the merge key. Two
// stacks differing only in how many uninformative frames they carry have to
// become the same node.
func TestRunsOfDifferentLengthCollapseToTheSameFrame(t *testing.T) {
	four := []string{"main", "libcublasLt.so.13+0x1", "libcublasLt.so.13+0x2",
		"libcublasLt.so.13+0x3", "libcublasLt.so.13+0x4", "cuLaunchKernel"}
	six := []string{"main", "libcublasLt.so.13+0x1", "libcublasLt.so.13+0x2",
		"libcublasLt.so.13+0x3", "libcublasLt.so.13+0x4",
		"libcublasLt.so.13+0x5", "libcublasLt.so.13+0x6", "cuLaunchKernel"}
	mods4 := []string{"/app/main", lt, lt, lt, lt, "/usr/lib/libcuda.so.610"}
	mods6 := []string{"/app/main", lt, lt, lt, lt, lt, lt, "/usr/lib/libcuda.so.610"}

	got4, _, _, _ := collapseVendorRuns(four, mods4)
	got6, _, _, _ := collapseVendorRuns(six, mods6)

	require.Equal(t, got4, got6,
		"a run of 4 and a run of 6 must fold to the identical stack, or they stay two "+
			"nodes and the collapse has achieved nothing")
	assert.Equal(t, []string{"main", "libcublasLt.so.13", "cuLaunchKernel"}, got4)
}

// The count must reach the page, not only the tooltip. A reader who never
// hovers anything still has to be able to see that the picture and the profile
// do not have the same number of frames.
func TestFoldSaysWhenItMergedVendorRuns(t *testing.T) {
	p := profileWithVendorRun(t)

	on, err := Fold(p, Options{SampleIndex: -1, StackOrder: RootFirst, CollapseVendorRuns: true})
	require.NoError(t, err)
	require.Positive(t, on.VendorFramesCollapsed, "the fixture must actually collapse something")
	require.Contains(t, joined(on.Warnings), "frame slots were merged")
	assert.Contains(t, joined(on.Warnings), "profile itself is unchanged",
		"the note must say the data is intact, or it reads as a loss")

	off, err := Fold(p, Options{SampleIndex: -1, StackOrder: RootFirst})
	require.NoError(t, err)
	assert.Zero(t, off.VendorFramesCollapsed)
	assert.NotContains(t, joined(off.Warnings), "frame slots were merged",
		"nothing was merged, so nothing may be claimed")
}

func joined(w []string) string { return strings.Join(w, "\n") }

// A profile whose stack carries a run of three address-only frames from one
// vendor library, which is the measured shape this collapse exists for.
func profileWithVendorRun(t *testing.T) *profile.Profile {
	t.Helper()
	m := &profile.Mapping{ID: 1, File: lt}
	at := func(name string, mapping *profile.Mapping) *profile.Location {
		l := &profile.Location{Address: 1, Mapping: mapping}
		l.Line = append(l.Line, profile.Line{Function: &profile.Function{Name: name}})
		return l
	}
	app := &profile.Mapping{ID: 2, File: "/usr/bin/app"}
	return &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "gpu", Unit: "nanoseconds"}},
		Mapping:    []*profile.Mapping{m, app},
		Sample: []*profile.Sample{{
			Value: []int64{5},
			Location: []*profile.Location{
				at("main", app),
				at("libcublasLt.so.13+0xf8e200", m),
				at("libcublasLt.so.13+0xe2b573", m),
				at("libcublasLt.so.13+0x1df0eb0", m),
				at("cuLaunchKernel", app),
			},
		}},
	}
}
