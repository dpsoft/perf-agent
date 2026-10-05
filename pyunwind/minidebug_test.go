package pyunwind

import (
	"debug/elf"
	"os"
	"path/filepath"
	"reflect"
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
	assert.Empty(t, miniDebugFuncSymbols(f, "/proc/self/exe"),
		"a binary without the section must yield no symbols and no panic")
}

// The decompression must happen ONCE per file identity. Asserted by slice
// identity rather than by timing: the cached call returns the very slice the
// first call stored, so the data pointers match. A timing assertion would be
// flaky and would not actually prove the decompressor was skipped.
//
// Why this matters: .gnu_debugdata on Fedora's libpython3.14 is a 604KB image
// out of a 118KB xz payload, and decompressing it costs 21.3ms against 289us
// for the same file with the section removed -- 74x, all pure-Go xz.
// EvalRangesForFile is called twice per enrolment (AttachProcess validates,
// module.Enroll installs), so without the cache every interpreter paid ~42ms
// and N processes sharing one libpython paid it 2N times.
func TestMiniDebugSymbolsAreDecompressedOncePerFile(t *testing.T) {
	path := filepath.Join("testdata", "minidebug.so")
	f, err := elf.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	first := miniDebugFuncSymbols(f, path)
	require.NotEmpty(t, first, "the fixture carries MiniDebugInfo symbols; see gen-minidebug.sh")
	second := miniDebugFuncSymbols(f, path)
	require.Equal(t, len(first), len(second))

	p1 := reflect.ValueOf(first).Pointer()
	p2 := reflect.ValueOf(second).Pointer()
	assert.Equal(t, p1, p2,
		"the second call re-decompressed instead of reading the cache (#190's 21ms per call)")
}

// And the cache must not serve one file's symbols for another. Keyed on
// (dev, ino, size, mtime), so two distinct files never collide however similar
// their paths -- a package upgrade that rebinds a path mid-run must not hand
// back the previous build's symbol addresses, which would put the eval-loop
// range over the wrong text.
func TestMiniDebugCacheDoesNotServeOneFileForAnother(t *testing.T) {
	src := filepath.Join("testdata", "minidebug.so")
	body, err := os.ReadFile(src)
	require.NoError(t, err)

	// A byte-identical copy at a different inode: same size, same content,
	// different identity. Its symbols must be computed, not borrowed.
	copyPath := filepath.Join(t.TempDir(), "minidebug-copy.so")
	require.NoError(t, os.WriteFile(copyPath, body, 0o644))

	fa, err := elf.Open(src)
	require.NoError(t, err)
	defer func() { _ = fa.Close() }()
	fb, err := elf.Open(copyPath)
	require.NoError(t, err)
	defer func() { _ = fb.Close() }()

	a := miniDebugFuncSymbols(fa, src)
	b := miniDebugFuncSymbols(fb, copyPath)
	require.NotEmpty(t, a)
	require.Equal(t, len(a), len(b), "the copy must yield the same symbols")

	assert.NotEqual(t, reflect.ValueOf(a).Pointer(), reflect.ValueOf(b).Pointer(),
		"a different inode was served the first file's cached slice; the key is not distinguishing them")

	ka, okA := miniDebugKeyFor(src)
	kb, okB := miniDebugKeyFor(copyPath)
	require.True(t, okA)
	require.True(t, okB)
	assert.NotEqual(t, ka, kb, "two distinct files produced the same cache key")
}

// A path that cannot be stat'd must decompress and NOT cache: a miss costs
// time, a wrong hit costs correctness.
func TestMiniDebugKeyRefusesWhatItCannotStat(t *testing.T) {
	_, ok := miniDebugKeyFor(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.False(t, ok, "an unstattable path produced a cache key, which could alias another file")
}
