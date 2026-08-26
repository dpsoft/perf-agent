package pprof

import (
	"path/filepath"
	"testing"

	"github.com/google/pprof/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// libcudaFrame is an unresolved frame carrying the mapping the symbolizer
// looked up while the target was alive: 0x7f2c958b71c6 inside libcuda.so.1,
// mapped at 0x7f2c94700000 with file offset 0x1000.
func libcudaFrame() Frame {
	return Frame{
		Name:       "0x7f2c958b71c6",
		Address:    0x7f2c958b71c6,
		Module:     "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
		BuildID:    "cafe",
		MapStart:   0x7f2c94700000,
		MapLimit:   0x7f2c96000000,
		MapOff:     0x1000,
		Unresolved: true,
	}
}

func onlyBuilder(t *testing.T, bs *ProfileBuilders) *ProfileBuilder {
	t.Helper()
	require.Len(t, bs.Builders, 1)
	for _, b := range bs.Builders {
		return b
	}
	return nil
}

func leafLocation(t *testing.T, b *ProfileBuilder) *profile.Location {
	t.Helper()
	require.Len(t, b.Profile.Sample, 1)
	require.NotEmpty(t, b.Profile.Sample[0].Location)
	return b.Profile.Sample[0].Location[0]
}

// The headline behaviour: a frame with no symbol but a known mapping renders
// as "<module base>+0x<file offset>", not as an ASLR'd absolute address.
func TestUnresolvedFrameRendersAsModulePlusOffset(t *testing.T) {
	bs := NewProfileBuilders(BuildersOptions{SampleRate: 1})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeGpu, Value: 100,
		Stack: []Frame{libcudaFrame()},
	})

	b := onlyBuilder(t, bs)
	loc := leafLocation(t, b)

	require.Len(t, loc.Line, 1)
	assert.Equal(t, "libcuda.so.1+0x11b81c6", loc.Line[0].Function.Name)

	// The name and the Location agree: both are the same module-relative
	// offset, so either can be fed to addr2line against libcuda.so.1.
	assert.Equal(t, uint64(0x11b81c6), loc.Address)

	// And the profile now has a real mapping rather than the 0x0/0x0/0x0
	// default. This is what `go tool pprof -raw` reports.
	require.NotNil(t, loc.Mapping)
	assert.Equal(t, "/usr/lib/x86_64-linux-gnu/libcuda.so.1", loc.Mapping.File)
	assert.Equal(t, uint64(0x7f2c94700000), loc.Mapping.Start)
	assert.Equal(t, uint64(0x7f2c96000000), loc.Mapping.Limit)
	assert.Equal(t, uint64(0x1000), loc.Mapping.Offset)
	assert.Equal(t, "cafe", loc.Mapping.BuildID)

	// The mapping must not claim to have symbols. It is here precisely
	// because it does not.
	assert.False(t, loc.Mapping.HasFunctions)
}

// The other half of the contract: no mapping, no module. The frame stays a
// bare address, on the default mapping, and is not quietly folded in with the
// frames that do know where they are.
func TestUnresolvedFrameWithNoMappingStaysBare(t *testing.T) {
	bs := NewProfileBuilders(BuildersOptions{SampleRate: 1})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeGpu, Value: 100,
		Stack: []Frame{{Name: "0x7f2c945ace62", Address: 0x7f2c945ace62, Unresolved: true}},
	})

	b := onlyBuilder(t, bs)
	loc := leafLocation(t, b)
	assert.Equal(t, "0x7f2c945ace62", loc.Line[0].Function.Name)
	assert.Equal(t, uint64(1), loc.Mapping.ID, "should be the default mapping")
	assert.Empty(t, loc.Mapping.File)
}

// A frame whose carried range does not actually contain its address is a bug
// upstream. Building a mapping from it would put the location at a nonsense
// offset under a confidently-named file, so it is refused.
func TestFrameCarryingAnInconsistentMappingIsRefused(t *testing.T) {
	f := libcudaFrame()
	f.Address = 0x400000 // below MapStart

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 1})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeGpu, Value: 100, Stack: []Frame{f},
	})

	b := onlyBuilder(t, bs)
	loc := leafLocation(t, b)
	assert.Equal(t, "0x7f2c958b71c6", loc.Line[0].Function.Name, "name must not be rewritten")
	assert.Equal(t, uint64(1), loc.Mapping.ID)
	assert.Len(t, b.Profile.Mapping, 1, "no mapping may be interned from an inconsistent frame")
}

// A resolved frame renders exactly as it did before, mapping or no mapping.
func TestResolvedFrameIsNeverRenamed(t *testing.T) {
	f := libcudaFrame()
	f.Name = "cuLaunchKernel"
	f.Unresolved = false

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 1})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeGpu, Value: 100, Stack: []Frame{f},
	})

	b := onlyBuilder(t, bs)
	loc := leafLocation(t, b)
	assert.Equal(t, "cuLaunchKernel", loc.Line[0].Function.Name)
	assert.True(t, loc.Mapping.HasFunctions)
}

// Kernel frames route through the "[kernel]" sentinel, which is not a file.
// They must keep whatever name they arrived with - "[kernel]+0x..." would be
// a fabricated module.
func TestKernelSentinelIsNeverUsedAsAModuleName(t *testing.T) {
	bs := NewProfileBuilders(BuildersOptions{SampleRate: 1})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeCpu, Value: 1,
		Stack: []Frame{{
			Name: "0xffffffffc0201234", Address: 0xffffffffc0201234,
			IsKernel: true, Unresolved: true,
		}},
	})

	b := onlyBuilder(t, bs)
	loc := leafLocation(t, b)
	assert.Equal(t, "0xffffffffc0201234", loc.Line[0].Function.Name)
	assert.Equal(t, "[kernel]", loc.Mapping.File)
}

// The builder's own Resolver wins when it has an answer, so nothing about the
// CPU profilers' existing mapping attribution changes; the rename then runs on
// top of the mapping the Resolver produced.
func TestResolverStillWinsAndRenameAppliesOnTop(t *testing.T) {
	resolver := procmap.NewResolver(procmap.WithProcRoot(
		filepath.Join("..", "unwind", "procmap", "testdata", "proc")))
	defer resolver.Close()

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 99, Resolver: resolver})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeCpu, Value: 1,
		Stack: []Frame{{Name: "0x401000", Address: 0x00401000, Unresolved: true}},
	})

	b := onlyBuilder(t, bs)
	loc := leafLocation(t, b)
	require.NotNil(t, loc.Mapping)
	assert.Equal(t, "/usr/bin/target", loc.Mapping.File)
	assert.Equal(t, "target+0x"+hexs(loc.Address), loc.Line[0].Function.Name)
	assert.False(t, loc.Mapping.HasFunctions)
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

// Two unresolved frames at different offsets in the same library must stay
// two frames. Renaming must not collapse a call path into one box.
func TestTwoOffsetsInOneModuleStayDistinct(t *testing.T) {
	f1 := libcudaFrame()
	f2 := libcudaFrame()
	f2.Address = 0x7f2c958b7200
	f2.Name = "0x7f2c958b7200"

	bs := NewProfileBuilders(BuildersOptions{SampleRate: 1})
	bs.AddSample(&ProfileSample{
		Pid: 4242, SampleType: SampleTypeGpu, Value: 100, Stack: []Frame{f1, f2},
	})

	b := onlyBuilder(t, bs)
	require.Len(t, b.Profile.Sample[0].Location, 2)
	n0 := b.Profile.Sample[0].Location[0].Line[0].Function.Name
	n1 := b.Profile.Sample[0].Location[1].Line[0].Function.Name
	assert.NotEqual(t, n0, n1)
	assert.Equal(t, "libcuda.so.1+0x11b81c6", n0)
	assert.Equal(t, "libcuda.so.1+0x11b8200", n1)
	assert.Len(t, b.Profile.Mapping, 2, "one default + one libcuda")
}
