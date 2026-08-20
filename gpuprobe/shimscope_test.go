package gpuprobe

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

// moduleSymbolizer is fakeSymbolizer with modules: it names each IP
// "fn_<hex>" exactly the same way, and puts each IP's frame in the module
// modules[ip]. A missing entry means a frame the symbolizer could not place
// in any module, which is the "unknown" case the guard must not read as
// "outside the shim".
type moduleSymbolizer struct {
	modules map[uint64]string
}

func (s *moduleSymbolizer) SymbolizeProcess(_ uint32, ips []uint64) ([]symbolize.Frame, error) {
	out := make([]symbolize.Frame, 0, len(ips))
	for _, ip := range ips {
		out = append(out, symbolize.Frame{
			Address: ip,
			Name:    fmt.Sprintf("fn_%x", ip),
			Module:  s.modules[ip],
		})
	}
	return out, nil
}

func (s *moduleSymbolizer) Close() error { return nil }

// mappedLibrary returns the path of a shared object mapped into this very
// process, so the deployment-shape tests run against a real ELF rather than
// a fixture that only claims to be one. A CGO-linked test binary always maps
// at least libc; a fully static one maps nothing, and the test says so
// rather than pretending to have checked.
func mappedLibrary(t *testing.T) string {
	t.Helper()
	f, err := os.Open("/proc/self/maps")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || !strings.HasPrefix(fields[5], "/") {
			continue
		}
		path := fields[5]
		base := filepath.Base(path)
		if !strings.HasSuffix(base, ".so") && !strings.Contains(base, ".so.") {
			continue
		}
		if isELFProgram(path) {
			continue
		}
		return path
	}
	t.Skip("no shared object mapped into this process; needs a dynamically linked test binary")
	return ""
}

// The two deployment shapes, told apart from the shim's ELF alone.
//
// This is the whole basis of the guard: "every frame is inside the shim" is
// a fatal verdict for an injected adapter and a completely normal stack for
// a self-contained producer, so the classifier has to be right about which
// one it is looking at before it judges a single frame.
func TestShimShapeIsClassifiedFromTheELF(t *testing.T) {
	assert.True(t, isELFProgram("/proc/self/exe"),
		"this test binary is a program: ET_EXEC, or a PIE carrying PT_INTERP")
	assert.False(t, newShimScope("/proc/self/exe").guarded,
		"a shim that IS the program is the self-contained shape (shim/stub): profiler and application are the same binary, so there is no boundary to police")

	lib := mappedLibrary(t)
	assert.False(t, isELFProgram(lib), "%s is a shared object, not a program", lib)
	assert.True(t, newShimScope(lib).guarded,
		"a shim that is a shared object is the injected shape (the CUPTI adapter): application code lives in other modules by construction")
}

// An empty ShimPath is not evidence of anything, so it cannot justify
// rejecting a stack. Attach always sets one - it parses that file's USDT
// notes - so this is the unit-test shape.
func TestNoShimPathLeavesTheGuardOff(t *testing.T) {
	s := newShimScope("")
	assert.False(t, s.guarded)
	assert.Equal(t, stackAttributable, s.verdict([]pp.Frame{{Name: "anything"}}),
		"with no idea which module is the profiler's there is no evidence either way; rejecting everything on no evidence is destruction, not honesty")
}

func TestVerdictNeedsPositiveEvidenceOfLeavingTheShim(t *testing.T) {
	const shim = "/opt/perfagent/libperfagent_cupti.so"
	injected := shimScope{
		guarded: true,
		paths:   map[string]struct{}{shim: {}},
		base:    filepath.Base(shim),
	}

	tests := []struct {
		name   string
		frames []pp.Frame
		want   stackVerdict
	}{{
		// The RTX 3090 run, exactly: the frame-pointer walk died in
		// libcupti and the only frame that came back was the adapter's own
		// callback.
		name:   "only the adapter's own callback",
		frames: []pp.Frame{{Name: "(anonymous namespace)::on_callback", Module: shim}},
		want:   stackProfilerOnly,
	}, {
		name: "the walk reached the application",
		frames: []pp.Frame{
			{Name: "main", Module: "/app/train"},
			{Name: "cudaLaunchKernel", Module: "/usr/lib/libcudart.so.12"},
			{Name: "(anonymous namespace)::on_callback", Module: shim},
		},
		want: stackAttributable,
	}, {
		// The target runs in a container, so its maps report the shim under
		// the container's own rootfs. Without the basename fallback that
		// path reads as "outside the shim" and the guard accepts a stack
		// that never left the profiler - the one direction that must not
		// happen.
		name:   "same shim under another mount namespace's path",
		frames: []pp.Frame{{Name: "on_callback", Module: "/rootfs/opt/perfagent/libperfagent_cupti.so"}},
		want:   stackProfilerOnly,
	}, {
		name: "a frame with no module proves nothing",
		frames: []pp.Frame{
			{Name: "0x7f1c00401234"},
			{Name: "on_callback", Module: shim},
		},
		want: stackProfilerOnlyUncertain,
	}, {
		name:   "no module anywhere",
		frames: []pp.Frame{{Name: "0x7f1c00401234"}, {Name: "0x7f1c00405678"}},
		want:   stackProfilerOnlyUncertain,
	}, {
		name: "an unknown module does not weaken real evidence",
		frames: []pp.Frame{
			{Name: "0x7f1c00401234"},
			{Name: "main", Module: "/app/train"},
			{Name: "on_callback", Module: shim},
		},
		want: stackAttributable,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, injected.verdict(tc.frames))
		})
	}
}

// The injected shape end to end: a capture that resolves to frames wholly
// inside the adapter must not reach the launch. The launch itself is
// untouched - it ships, stackless, and its GPU time projects as
// unattributed.
func TestInjectedShimOnlyStackIsWithheldAndCounted(t *testing.T) {
	lib := mappedLibrary(t)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{modules: map[uint64]string{0x1000: lib, 0x2000: lib}}
	c, sm, _ := stackConsumer(t, sink, Config{ShimPath: lib, Symbolizer: sym})
	sm.put(5, 0x2000, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 5, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1, "the launch is never dropped for this: only its stack is withheld")
	assert.Empty(t, sink.launches[0].Launch.CPUStack,
		"a stack that never left the profiler must not be presented as an attribution")
	assert.Zero(t, sink.launches[0].Launch.SamplePeriod,
		"no stack, no period: gpu_sample_period rides only on the attributed population")

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksProfilerOnly, "no loss is silent")
	assert.Zero(t, st.StacksProfilerOnlyUncertain, "every frame's module was known, so the rejection is proven")
	assert.Zero(t, st.StacksAttached)
	assert.Equal(t, uint64(1), st.StacksResolved, "the capture did resolve; it was refused afterwards")
	assert.Zero(t, st.PendingStacks, "a refused stack is not parked waiting for a twin that cannot use it")
}

// The same injected shim, but the walk got out into the application. That is
// a real attribution and must be attached untouched.
func TestInjectedShimStackThatReachesTheApplicationIsKept(t *testing.T) {
	lib := mappedLibrary(t)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{modules: map[uint64]string{0x1000: "/app/train", 0x2000: lib}}
	c, sm, _ := stackConsumer(t, sink, Config{ShimPath: lib, Symbolizer: sym})
	sm.put(5, 0x2000, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 5, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1)
	assert.Equal(t, []string{"fn_1000", "fn_2000"}, frameNames(sink.launches[0].Launch.CPUStack),
		"one frame outside the shim is proof the walk reached the application")
	assert.Equal(t, uint32(8), sink.launches[0].Launch.SamplePeriod)
	assert.Zero(t, c.Stats().StacksProfilerOnly)
	assert.Equal(t, uint64(1), c.Stats().StacksAttached)
}

// The self-contained shape end to end, which is what the phase gate runs:
// shim/stub IS the program, its whole stack is "inside the shim", and that
// is a perfectly good attribution. A guard that rejected this would be
// testing the wrong thing.
func TestSelfContainedShimStackIsAnAttribution(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{modules: map[uint64]string{0x1000: self, 0x2000: self}}
	c, sm, _ := stackConsumer(t, sink, Config{ShimPath: self, Symbolizer: sym})
	sm.put(5, 0x2000, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 5, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1)
	assert.Equal(t, []string{"fn_1000", "fn_2000"}, frameNames(sink.launches[0].Launch.CPUStack),
		"main -> perfagent_stub_run is entirely inside the shim and is still the application's own call path")
	assert.Zero(t, c.Stats().StacksProfilerOnly,
		"zero by construction for a self-contained producer: there is no boundary to fail to cross")
	assert.Equal(t, uint64(1), c.Stats().StacksAttached)
}

// A rejection with no proof behind it is still a rejection - the honest
// direction - but it is counted apart from the proven ones, because the two
// have different causes and different fixes.
func TestUnprovenRejectionIsCountedSeparately(t *testing.T) {
	lib := mappedLibrary(t)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{} // no module for any IP
	c, sm, _ := stackConsumer(t, sink, Config{ShimPath: lib, Symbolizer: sym})
	sm.put(5, 0x2000, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 5, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1)
	assert.Empty(t, sink.launches[0].Launch.CPUStack)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksProfilerOnly, "the uncertain count is a subset, not a separate bucket")
	assert.Equal(t, uint64(1), st.StacksProfilerOnlyUncertain)
}

// The guard runs before the stack is parked, so a refused capture never
// occupies the bounded side table and never gets attached by the other
// arrival order either.
func TestRefusedStackIsNeverAttachedInEitherArrivalOrder(t *testing.T) {
	lib := mappedLibrary(t)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{modules: map[uint64]string{0x1000: lib}}
	c, sm, _ := stackConsumer(t, sink, Config{ShimPath: lib, Symbolizer: sym})
	sm.put(5, 0x1000)

	// Batched twin first: the launch is held, and the capture that follows
	// must release it without lending it the refused stack.
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, sampledBatchWith(4242, 7, 5, 8))
	c.Flush()

	require.Len(t, sink.launches, 1)
	assert.Empty(t, sink.launches[0].Launch.CPUStack)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksProfilerOnly)
	assert.Zero(t, st.PendingStacks)
	assert.Zero(t, st.StacksAttached)
}
