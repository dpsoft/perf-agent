package pyunwind

import (
	"debug/elf"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fedora's stock interpreter, in fixture form: the eval loop's bulk lives in a
// local .cold fragment that stripping removed from the shipped object, so it
// survives only inside .gnu_debugdata. Without reading MiniDebugInfo, .dynsym
// shows 994 bytes against the 8192-byte floor and the interpreter is refused
// (#170).
//
// The first half of this test is the part that matters: it establishes that
// the fixture CANNOT be satisfied from .dynsym or .symtab. A fixture whose
// .cold fragment stayed global would pass without the MiniDebugInfo path at
// all, which is why gen-minidebug.sh makes it local and why the precondition
// is asserted here rather than trusted.
func TestEvalRangesReadsMiniDebugInfo(t *testing.T) {
	path := filepath.Join("testdata", "minidebug.so")

	f, err := elf.Open(path)
	require.NoError(t, err, "the fixture must be a readable ELF; regenerate with testdata/gen-minidebug.sh")
	defer func() { _ = f.Close() }()

	require.NotNil(t, f.Section(".gnu_debugdata"),
		"the fixture carries no .gnu_debugdata, so it cannot be exercising the path this test is about")
	_, symErr := f.Symbols()
	require.ErrorIs(t, symErr, elf.ErrNoSymbols,
		"the fixture still has a .symtab, so these symbols are reachable without MiniDebugInfo")

	// What the pre-#170 code saw: the hot fragment alone, under the floor.
	var dynTotal uint64
	dyn, err := f.DynamicSymbols()
	require.NoError(t, err)
	for _, s := range dyn {
		if elf.ST_TYPE(s.Info) == elf.STT_FUNC && isEvalFragment(s.Name) {
			dynTotal += s.Size
		}
	}
	require.Less(t, dynTotal, uint64(minEvalLoopBytes),
		"the fixture's .dynsym alone already clears the floor (%d bytes), so this test would pass without reading MiniDebugInfo",
		dynTotal)

	// And what it sees now.
	ranges, err := EvalRangesForFile(path)
	require.NoError(t, err, "the eval loop is in .gnu_debugdata and must be found there")
	require.Len(t, ranges, 2, "both the hot fragment and the .cold one must be installed: %+v", ranges)

	var total uint64
	for _, r := range ranges {
		assert.Greater(t, r.Hi, r.Lo, "a range must be non-empty")
		total += r.Hi - r.Lo
	}
	assert.Equal(t, uint64(994+80785), total,
		"the two fragments must total what the fixture declares")
	// Fragments are sorted largest-first, and the .cold one is the bulk.
	assert.Equal(t, uint64(80785), ranges[0].Hi-ranges[0].Lo,
		"the largest fragment must lead, so a truncated install keeps the dispatch loop")
}

// A binary with no .gnu_debugdata must behave exactly as before: the section
// is absent on most things pyunwind is pointed at, and treating that as an
// error would refuse interpreters that are walked fine today.
func TestMiniDebugInfoAbsentIsNotAnError(t *testing.T) {
	f, err := elf.Open("/proc/self/exe")
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	if f.Section(".gnu_debugdata") != nil {
		t.Skip("this test binary happens to carry MiniDebugInfo; nothing to assert")
	}
	assert.Empty(t, miniDebugFuncSymbols(f),
		"a binary without the section must yield no symbols and no panic")
}
