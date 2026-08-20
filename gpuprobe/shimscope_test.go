package gpuprobe

import (
	"bufio"
	"debug/elf"
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

// mappedSharedObjects returns the paths of the shared objects mapped into
// this very process, so the deployment-shape tests run against real ELF
// files rather than fixtures that only claim to be one. A CGO-linked test
// binary always maps at least libc and the dynamic loader.
//
// Note what this does NOT do: filter by isELFProgram. An earlier version
// skipped any mapped library the classifier called a program, which stepped
// silently around the one file that proves the classifier wrong - see
// TestAnInterpreterCarryingSharedObjectIsStillALibrary.
func mappedSharedObjects(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("/proc/self/maps")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	var out []string
	seen := map[string]bool{}
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
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	require.NotEmpty(t, out,
		"no shared object mapped into this process; the test binary must be dynamically linked (it is CGO-linked against blazesym)")
	return out
}

// mappedLibrary returns one real shared object to stand in for an injected
// shim.
func mappedLibrary(t *testing.T) string {
	t.Helper()
	return mappedSharedObjects(t)[0]
}

// hasPTInterp reports whether an ELF carries an interpreter segment.
func hasPTInterp(t *testing.T, path string) bool {
	t.Helper()
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}

// The counterexample that broke the first version of isELFProgram, pinned.
//
// Glibc's libc.so.6 is a shared object that carries a PT_INTERP segment - it
// is runnable, it prints its own version banner. "Has PT_INTERP" therefore
// classified it as a program, and a shim .so linked the same way would have
// switched the guard OFF entirely, in exactly the deployment the guard
// exists for. DT_SONAME is what actually separates the two, and it is
// consulted first.
//
// This asserts on the real file mapped into this process rather than
// skipping past it, because a test that skips its own counterexample is not
// testing.
func TestAnInterpreterCarryingSharedObjectIsStillALibrary(t *testing.T) {
	checked := 0
	for _, so := range mappedSharedObjects(t) {
		if !hasPTInterp(t, so) {
			continue
		}
		checked++
		assert.False(t, isELFProgram(so),
			"%s is a shared object that happens to carry PT_INTERP; classifying it as a program would disable the guard", so)
		assert.True(t, newShimScope(so).guarded,
			"a shim spelled like %s must still be guarded as an injected library", so)
	}
	require.Positive(t, checked,
		"no PT_INTERP-carrying shared object is mapped into this process, so the counterexample went unchecked; on glibc libc.so.6 is one")
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

	for _, so := range mappedSharedObjects(t) {
		assert.False(t, isELFProgram(so), "%s is a shared object, not a program", so)
		assert.True(t, newShimScope(so).guarded,
			"a shim that is a shared object is the injected shape (the CUPTI adapter): application code lives in other modules by construction")
	}
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
		// The shim was upgraded under a running process, so the kernel
		// reports its mapping as deleted. Unstripped, that path matches
		// neither the spellings nor the basename and the shim's own frame
		// reads as "outside" - a false accept, during an upgrade, which is
		// the least observable moment there is.
		name:   "the shim was replaced while the process had it mapped",
		frames: []pp.Frame{{Name: "on_callback", Module: shim + " (deleted)"}},
		want:   stackProfilerOnly,
	}, {
		name:   "deleted mapping under another mount namespace's path",
		frames: []pp.Frame{{Name: "on_callback", Module: "/rootfs" + shim + " (deleted)"}},
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
//
// Note what the refusal does NOT do: release the launch it was held for. The
// guard returns before deferred.take, so the held launch waits exactly as it
// would for a capture that failed to resolve - for the next batch, for the
// queue's own bound, or for Flush, which is what releases it here. Bounded
// and lossless, but it is a wait, not a release.
func TestRefusedStackIsNeverAttachedInEitherArrivalOrder(t *testing.T) {
	lib := mappedLibrary(t)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{modules: map[uint64]string{0x1000: lib}}
	c, sm, _ := stackConsumer(t, sink, Config{ShimPath: lib, Symbolizer: sym})
	sm.put(5, 0x1000)

	// Batched twin first: the launch is held, and the capture that follows
	// must not lend it the refused stack. Flush is what lets it go.
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
