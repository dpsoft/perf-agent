package gpuprobe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/dpsoft/perf-agent/pprof"
)

const testShimPath = "/opt/perfagent/libperfagent-gpu-nvidia.so"

func testBand(t *testing.T) instrumentationBand {
	t.Helper()
	return newInstrumentationBand(shimScope{
		guarded: true,
		paths:   map[string]struct{}{testShimPath: {}},
		base:    "libperfagent-gpu-nvidia.so",
	})
}

func named(name, module string) pp.Frame {
	return pp.Frame{Name: name, Module: module}
}
func raw(module string) pp.Frame {
	return pp.Frame{Name: "0xdead", Module: module, Unresolved: true}
}

func names(frames []pp.Frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.Name
	}
	return out
}

// The measured band, verbatim from a PyTorch capture: one driver dispatch
// frame and six CUPTI frames between the application's cuLaunchKernel and the
// shim's own callback.
func TestTheMeasuredBandCollapsesToOneMarker(t *testing.T) {
	b := testBand(t)
	frames := []pp.Frame{
		named("(anonymous namespace)::on_launch(CUpti_CallbackData const*)", testShimPath),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcuda.so.610.57.04"),
		named("cuLaunchKernel", "/usr/lib/libcuda.so.610.57.04"),
		raw("/usr/lib/libcublasLt.so.13"),
		named("torch::autograd::Engine::execute", "/usr/lib/libtorch_cuda.so"),
	}

	got, removed := b.collapse(frames)

	assert.Equal(t, []string{
		"(anonymous namespace)::on_launch(CUpti_CallbackData const*)", // named: kept
		InstrumentationFrameName,
		"cuLaunchKernel",
		"0xdead", // the cuBLASLt frame: real work, untouched
		"torch::autograd::Engine::execute",
	}, names(got))
	assert.Equal(t, 6, removed, "seven unnamed frames became one; every name survives")
}

// cuLaunchKernel is the last thing the application actually called and the
// boundary a reader needs. A rule that ate named driver frames along with the
// unnamed ones would remove the only landmark in the region.
func TestANamedDriverFrameIsNeverElided(t *testing.T) {
	b := testBand(t)
	frames := []pp.Frame{
		named("on_launch", testShimPath),
		raw("/usr/lib/libcupti.so.13"),
		named("cuLaunchKernel", "/usr/lib/libcuda.so.610.57.04"),
		named("app_main", "/opt/app"),
	}
	got, _ := b.collapse(frames)
	assert.Contains(t, names(got), "cuLaunchKernel")
	assert.Contains(t, names(got), "on_launch", "a named shim frame says more than the marker does")
}

// The care in the rule. libcuda is the CUDA driver and most of its frames are
// the application's own work; only the dispatch frame wedged against the
// callback machinery is ours. Driver frames with no instrumentation anchor
// beside them must survive, or the profile would be deleting its own subject.
func TestDriverFramesAloneAreNeverABand(t *testing.T) {
	b := testBand(t)
	frames := []pp.Frame{
		raw("/usr/lib/libcuda.so.610.57.04"),
		raw("/usr/lib/libcuda.so.610.57.04"),
		named("cuMemcpyAsync", "/usr/lib/libcuda.so.610.57.04"),
		named("app_main", "/opt/app"),
	}
	got, removed := b.collapse(frames)
	assert.Equal(t, 4, len(got), "no anchor, so nothing may be elided: %v", names(got))
	assert.Zero(t, removed)
}

// Unnamed driver frames adjacent to an anchor are the dispatch path and are
// absorbed wherever in the band they sit. This fixture is the shape that
// refuted the first version of the rule, which tried to protect driver frames
// at the band's outer edge: in the real stacks the dispatch frame IS at the
// outer edge, immediately before cuLaunchKernel, so position cannot be what
// distinguishes it. Having a NAME is.
func TestUnnamedDriverFramesAgainstTheBandAreAbsorbed(t *testing.T) {
	b := testBand(t)
	frames := []pp.Frame{
		named("on_launch", testShimPath),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcuda.so.610.57.04"),
		raw("/usr/lib/libcuda.so.610.57.04"),
		named("cuLaunchKernel", "/usr/lib/libcuda.so.610.57.04"),
		named("app_main", "/opt/app"),
	}
	got, removed := b.collapse(frames)
	require.Equal(t, 4, len(got), "%v", names(got))
	assert.Equal(t, "on_launch", got[0].Name)
	assert.Equal(t, InstrumentationFrameName, got[1].Name)
	assert.Equal(t, "cuLaunchKernel", got[2].Name, "the named boundary always survives")
	assert.Equal(t, 2, removed)
}

// A stack that is ENTIRELY instrumentation is shimScope's business, and it
// has already refused those as unattributable. Collapsing one to a lone marker
// would turn a stack the consumer withheld into something that looks like an
// attribution.
func TestAStackThatIsAllInstrumentationIsLeftAlone(t *testing.T) {
	b := testBand(t)
	frames := []pp.Frame{
		raw(testShimPath),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcupti.so.13"),
	}
	got, removed := b.collapse(frames)
	assert.Equal(t, 3, len(got))
	assert.Zero(t, removed)
}

// Without a shim there is no anchor, and matching libcupti on its own would be
// a guess about somebody else's process rather than a fact about ours.
func TestWithNoShimNothingIsElided(t *testing.T) {
	b := newInstrumentationBand(shimScope{})
	frames := []pp.Frame{
		raw("/usr/lib/libcupti.so.13"),
		named("app_main", "/opt/app"),
	}
	got, removed := b.collapse(frames)
	assert.Equal(t, 2, len(got))
	assert.Zero(t, removed)
}

// Two separate bands in one stack each collapse; they must not be merged
// across the application frames between them.
func TestTwoBandsCollapseSeparately(t *testing.T) {
	b := testBand(t)
	frames := []pp.Frame{
		raw(testShimPath),
		raw("/usr/lib/libcupti.so.13"),
		named("app_middle", "/opt/app"),
		raw("/usr/lib/libcupti.so.13"),
		named("app_main", "/opt/app"),
	}
	got, removed := b.collapse(frames)
	assert.Equal(t, []string{
		InstrumentationFrameName, "app_middle", InstrumentationFrameName, "app_main",
	}, names(got))
	assert.Equal(t, 1, removed)
}

// The rule must not depend on which end of the stack the slice starts at.
//
// The bug this pins: a forward-only scan passed over the driver's dispatch
// frame when the frames arrived outermost-first, because an absorbable frame
// may not START a band and its anchor had not been seen yet. Measured on a real
// capture, that left 1.00 libcuda frames per stack in every profile while the
// implementation's own comment said they were absorbed.
func TestTheBandIsTheSameFromEitherEndOfTheStack(t *testing.T) {
	b := testBand(t)
	inner := []pp.Frame{
		raw(testShimPath),
		raw("/usr/lib/libcupti.so.13"),
		raw("/usr/lib/libcuda.so.610.57.04"), // the dispatch frame
		named("cuLaunchKernel", "/usr/lib/libcuda.so.610.57.04"),
		named("app_main", "/opt/app"),
	}
	outer := make([]pp.Frame, len(inner))
	for i := range inner {
		outer[i] = inner[len(inner)-1-i]
	}

	gotInner, removedInner := b.collapse(append([]pp.Frame(nil), inner...))
	gotOuter, removedOuter := b.collapse(append([]pp.Frame(nil), outer...))

	assert.Equal(t, []string{
		InstrumentationFrameName, "cuLaunchKernel", "app_main",
	}, names(gotInner))
	assert.Equal(t, []string{
		"app_main", "cuLaunchKernel", InstrumentationFrameName,
	}, names(gotOuter))
	assert.Equal(t, removedInner, removedOuter,
		"the same stack read from the other end must lose the same frames")
	assert.Equal(t, 2, removedInner)
}
