package foldedstacks_test

import (
	"bytes"
	"strings"
	"testing"

	gprofile "github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// This is the 16-frame joined stack from the real RTX 3090 capture in
// gpu-cuda-45.pb.gz, transcribed from `go tool pprof -raw` on that file:
// locations 4..18 then 2, with seven consecutive libcuda internals rendering
// as bare hex because NVIDIA ships no symbols for them.
//
// What that raw dump also shows is the loss this change is about. Every
// location reads "0x0 M=1" - address zero, mapping one - and the whole file
// has one mapping, "1: 0x0/0x0/0x0". The frames are missing not merely a
// symbol but the module.
//
// The capture cannot be re-rendered: the mapping data was never written into
// it. So this drives the NEW pipeline over the same stack, starting where the
// old one starts - symbolize.Frames straight out of blazesym, which for a
// miss returns name "", module "", offset 0 and nothing but a Reason
// (capi/src/symbolize.rs zeroes the whole blaze_sym) - and ending where a
// reader looks: the pprof Function names and the folded stacks.
//
// The mapping ranges below are a FIXTURE. The real run's /proc/<pid>/maps was
// never recorded, so the ranges are chosen to contain the observed addresses;
// they are not a claim about where libcuda actually sat on that machine. What
// is real here is the stack, the addresses, and the arithmetic.
var (
	cudaMap = procmap.Mapping{
		Path: "/usr/lib/x86_64-linux-gnu/libcuda.so.550.54.14",
		// Contains all seven unresolved addresses, 0x7f2c944de06b..0x7f2c958b71c6.
		Start: 0x7f2c94400000, Limit: 0x7f2c96000000, Offset: 0x0,
		BuildID: "0bd1a2f4c0de", IsExec: true,
	}
	appMap = procmap.Mapping{
		Path:  "/home/diego/work/cuda_workload",
		Start: 0x400000, Limit: 0x480000, Offset: 0x0,
		BuildID: "1234abcd", IsExec: true,
	}
)

type mapsFixture struct{ maps []procmap.Mapping }

func (f mapsFixture) Lookup(_ uint32, addr uint64) (procmap.Mapping, bool) {
	for _, m := range f.maps {
		if addr >= m.Start && addr < m.Limit {
			return m, true
		}
	}
	return procmap.Mapping{}, false
}

// unresolvedPCs are the seven, in stack order, exactly as the capture has
// them (raw locations 10..16).
var unresolvedPCs = []uint64{
	0x7f2c958b71c6,
	0x7f2c945ace62,
	0x7f2c945acc75,
	0x7f2c945b2dfb,
	0x7f2c945b2c2b,
	0x7f2c945bbf6f,
	0x7f2c944de06b,
}

// blazesymOutput is the CPU half of the stack as the symbolizer receives it,
// root-first: raw locations 4..17. The two synthetic GPU frames above it
// ([gpu:launch], [gpu:kernel:...]) are added by gpu/projection.go, not by the
// symbolizer, and are appended in buildProfile.
func blazesymOutput() []symbolize.Frame {
	named := func(addr uint64, name string) symbolize.Frame {
		return symbolize.Frame{Address: addr, Name: name, Reason: symbolize.FailureNone}
	}
	out := []symbolize.Frame{
		named(0x401060, "_start"),
		named(0x7f2c98029d90, "__libc_start_main_alias_1"),
		named(0x7f2c98029e40, "__libc_start_call_main"),
		named(0x4023a0, "main"),
		named(0x402150, "__device_stub__Z14perfagent_axpyfPKfPfi(float, float const*, float*, int)"),
		named(0x7f2c96a12340, "cudaLaunchKernel"),
	}
	for _, pc := range unresolvedPCs {
		// This is all blazesym gives back for an address it cannot name.
		// symbolize/local.go then writes the hex address into Name so the
		// location renders as something rather than <unknown>.
		out = append(out, symbolize.Frame{
			Address: pc,
			Name:    "0x" + hexs(pc),
			Reason:  symbolize.FailureMissingSymbols,
		})
	}
	return append(out,
		named(0x7f2c92a04510, "(anonymous namespace)::on_callback(void*, CUpti_CallbackDomain, unsigned int, void const*)"),
	)
}

func hexs(v uint64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[i:])
}

// buildProfile runs the frames through the three steps the GPU pipeline does:
// attach modules (inside the symbolizer, while the target is still alive) ->
// ToProfFrames -> the pprof builder, then appends the two synthetic frames
// gpu/projection.go nests above the CPU stack. Passing idx==nil reproduces
// today's behaviour.
//
// attachModules is unexported, so the loop below stands in for it; what it
// does is the contract symbolize/module_test.go pins directly.
func buildProfile(t *testing.T, idx symbolize.ModuleIndex) *gprofile.Profile {
	t.Helper()
	frames := blazesymOutput()
	if idx != nil {
		for i := range frames {
			if frames[i].Reason == symbolize.FailureNone {
				continue
			}
			m, ok := idx.Lookup(4242, frames[i].Address)
			if !ok {
				continue
			}
			frames[i].Module = m.Path
			frames[i].BuildID = m.BuildID
			frames[i].MapStart, frames[i].MapLimit, frames[i].MapOff = m.Start, m.Limit, m.Offset
		}
	}

	stack := symbolize.ToProfFrames(frames)
	stack = append(stack,
		pp.FrameFromName("[gpu:launch]"),
		pp.FrameFromName("[gpu:kernel:_Z14perfagent_axpyfPKfPfi]"),
	)

	bs := pp.NewProfileBuilders(pp.BuildersOptions{SampleRate: 1})
	bs.AddSample(&pp.ProfileSample{
		Pid: 4242, SampleType: pp.SampleTypeGpu, Value: 1536, Stack: stack,
	})

	var buf bytes.Buffer
	for _, b := range bs.Builders {
		_, err := b.Write(&buf)
		require.NoError(t, err)
		break
	}
	p, err := gprofile.Parse(&buf)
	require.NoError(t, err)
	return p
}

func names(p *gprofile.Profile) []string {
	var out []string
	for _, loc := range p.Sample[0].Location {
		for _, ln := range loc.Line {
			out = append(out, ln.Function.Name)
		}
	}
	return out
}

// TestGPUStack_Before reproduces gpu-cuda-45.pb.gz: no module index, so seven
// frames are bare hex and the whole profile has the one default mapping.
func TestGPUStack_Before(t *testing.T) {
	p := buildProfile(t, nil)

	require.Len(t, p.Mapping, 1, "the profile has exactly one mapping")
	assert.Equal(t, uint64(0), p.Mapping[0].Start)
	assert.Equal(t, uint64(0), p.Mapping[0].Limit)
	assert.Empty(t, p.Mapping[0].File, "0x0/0x0/0x0")

	got := names(p)
	require.Len(t, got, 16)
	assert.Equal(t, []string{
		"0x7f2c958b71c6", "0x7f2c945ace62", "0x7f2c945acc75", "0x7f2c945b2dfb",
		"0x7f2c945b2c2b", "0x7f2c945bbf6f", "0x7f2c944de06b",
	}, got[6:13], "the seven frames as the real capture renders them")
}

// TestGPUStack_After is what the new pipeline produces for the same stack.
func TestGPUStack_After(t *testing.T) {
	idx := mapsFixture{maps: []procmap.Mapping{appMap, cudaMap}}
	p := buildProfile(t, idx)

	got := names(p)
	require.Len(t, got, 16)

	want := []string{
		"_start",
		"__libc_start_main_alias_1",
		"__libc_start_call_main",
		"main",
		"__device_stub__Z14perfagent_axpyfPKfPfi(float, float const*, float*, int)",
		"cudaLaunchKernel",
		// The seven. Each offset is its PC minus cudaMap.Start.
		"libcuda.so.550.54.14+0x14b71c6",
		"libcuda.so.550.54.14+0x1ace62",
		"libcuda.so.550.54.14+0x1acc75",
		"libcuda.so.550.54.14+0x1b2dfb",
		"libcuda.so.550.54.14+0x1b2c2b",
		"libcuda.so.550.54.14+0x1bbf6f",
		"libcuda.so.550.54.14+0xde06b",
		"(anonymous namespace)::on_callback(void*, CUpti_CallbackDomain, unsigned int, void const*)",
		"[gpu:launch]",
		"[gpu:kernel:_Z14perfagent_axpyfPKfPfi]",
	}
	assert.Equal(t, want, got)

	// Every frame that already had a name renders byte-for-byte as before -
	// including the two synthetic GPU frames, which carry no address and must
	// never acquire a module.
	before := names(buildProfile(t, nil))
	for _, i := range []int{0, 1, 2, 3, 4, 5, 13, 14, 15} {
		assert.Equal(t, before[i], got[i], "frame %d changed", i)
	}

	// The profile now describes a real file, and the Location addresses are
	// the same module-relative offsets the names carry - so `go tool pprof
	// -raw` shows "0x14b71c6 M=2" where it used to show "0x0 M=1", and either
	// number can be fed to addr2line -e libcuda.so.550.54.14.
	var cuda *gprofile.Mapping
	for _, m := range p.Mapping {
		if m.File == cudaMap.Path {
			cuda = m
		}
	}
	require.NotNil(t, cuda, "libcuda mapping interned")
	assert.Equal(t, cudaMap.Start, cuda.Start)
	assert.Equal(t, cudaMap.Limit, cuda.Limit)
	assert.Equal(t, cudaMap.BuildID, cuda.BuildID)
	assert.False(t, cuda.HasFunctions, "the mapping must not claim symbols it does not have")

	for _, loc := range p.Sample[0].Location {
		if loc.Mapping != cuda {
			continue
		}
		assert.Equal(t, "libcuda.so.550.54.14+0x"+hexs(loc.Address), loc.Line[0].Function.Name,
			"the name and Location.Address must be the same number")
	}
}

// A mapping index that covers only the application leaves the libcuda frames
// exactly as they are today. Recovering the module is not all-or-nothing, and
// the frames that miss must stay bare rather than borrow a neighbour's file.
func TestGPUStack_PartialIndexLeavesTheRestBare(t *testing.T) {
	p := buildProfile(t, mapsFixture{maps: []procmap.Mapping{appMap}})
	got := names(p)
	for i := 6; i < 13; i++ {
		assert.True(t, strings.HasPrefix(got[i], "0x"), "frame %d = %q", i, got[i])
	}
	require.Len(t, p.Mapping, 1)
}

// The honesty checks downstream must not improve just because the names did.
// Seven of sixteen frame slots still have no symbol; the warning banner must
// say so with and without modules.
func TestGPUStack_SymbolizationGapStillReported(t *testing.T) {
	idx := mapsFixture{maps: []procmap.Mapping{appMap, cudaMap}}

	for _, tc := range []struct {
		name string
		idx  symbolize.ModuleIndex
	}{{"before", nil}, {"after", idx}} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := foldedstacks.Fold(buildProfile(t, tc.idx), foldedstacks.Options{})
			require.NoError(t, err)
			assert.Equal(t, 16, res.Frames)
			assert.Equal(t, 7, res.AddressOnlyFrames,
				"the symbolization gap must read the same whether or not modules were recovered")

			var warned bool
			for _, w := range res.Warnings {
				if strings.Contains(w, "have no symbol") {
					warned = true
				}
			}
			assert.True(t, warned, "warnings: %v", res.Warnings)
		})
	}
}

// And the flame graph must still colour them as unsymbolized rather than
// promoting them to ordinary vendor frames now that the module is known.
func TestGPUStack_FlameGraphDomains(t *testing.T) {
	idx := mapsFixture{maps: []procmap.Mapping{appMap, cudaMap}}
	res, err := foldedstacks.Fold(buildProfile(t, idx), foldedstacks.Options{})
	require.NoError(t, err)
	require.Len(t, res.Stacks, 1)

	st := res.Stacks[0]
	require.Len(t, st.Frames, 16)
	for i := 6; i < 13; i++ {
		assert.Equal(t, flamegraph.DomainUnsymbolized,
			flamegraph.Classify(st.Frames[i], st.Modules[i]),
			"frame %d (%s) should read as unsymbolized", i, st.Frames[i])
	}
	assert.Equal(t, flamegraph.DomainBoundary, flamegraph.Classify(st.Frames[14], st.Modules[14]))
	assert.Equal(t, flamegraph.DomainGPUKernel, flamegraph.Classify(st.Frames[15], st.Modules[15]))
}
