package gpu

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/cubin"
)

// The fixtures live with the package that produced them (internal/cubin), not
// duplicated here. Two copies of a binary fixture drift, and the point of these
// tests is that the store answers correctly about the SAME bytes that package
// asserts its own claims against.
func fixturePath(name string) string {
	return filepath.Join("..", "internal", "cubin", "testdata", name)
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	require.NoError(t, err, "fixture %s", name)
	require.NotEmpty(t, b)
	return b
}

// symIndexOf returns the .symtab index the store keys its functionIndex table
// on, read from the fixture rather than hard-coded. Whether CUPTI's
// functionIndex IS this index is the design's premise and is measured on
// hardware elsewhere; these tests assert only that the store uses it
// consistently.
func symIndexOf(t *testing.T, b []byte, fn string) uint32 {
	t.Helper()
	c, err := cubin.Parse(b)
	require.NoError(t, err)
	for _, f := range c.Functions() {
		if f.Name == fn {
			require.GreaterOrEqual(t, f.SymIndex, 0)
			require.LessOrEqual(t, int64(f.SymIndex), int64(math.MaxUint32))
			return uint32(f.SymIndex)
		}
	}
	t.Fatalf("fixture has no function %q", fn)
	return 0
}

// damagedLineInfo returns single_lineinfo.cubin with the DW_LNE_set_address
// opcode that .rel.debug_line points at corrupted, which is the third
// line-info state: .debug_line present, table unusable. It is built the same
// way internal/cubin builds it for its own test of that state.
func damagedLineInfo(t *testing.T) []byte {
	t.Helper()
	b := fixture(t, "single_lineinfo.cubin")

	f, err := elf.NewFile(bytes.NewReader(b))
	require.NoError(t, err)
	lineSec := f.Section(".debug_line")
	require.NotNil(t, lineSec)
	rel, err := f.Section(".rel.debug_line").Data()
	require.NoError(t, err)
	require.NotEmpty(t, rel)
	require.NoError(t, f.Close())

	opcodeAt := lineSec.Offset + binary.LittleEndian.Uint64(rel[0:]) - 1
	require.Less(t, opcodeAt, uint64(len(b)))

	damaged := append([]byte(nil), b...)
	require.Equal(t, byte(0x02), damaged[opcodeAt], "expected DW_LNE_set_address here")
	damaged[opcodeAt] = 0x03

	// Confirm the fixture really is in the third state before any store test
	// leans on it, so a toolkit change turns into a failure here rather than a
	// silently weaker test elsewhere.
	c, err := cubin.Parse(damaged)
	require.NoError(t, err)
	require.True(t, c.HasLineInfo())
	require.Error(t, c.LineInfoErr())
	return damaged
}

// counting wraps a store so a test can assert the Resolve* counters against the
// number of calls actually made, which is the identity the design requires.
type counting struct {
	*ModuleStore
	calls uint64
}

func (c *counting) Resolve(crc uint64, fnIdx uint32, pc uint64) Resolution {
	c.calls++
	return c.ModuleStore.Resolve(crc, fnIdx, pc)
}

// requireSumIdentity asserts the two partitions the design fixes: the four
// Resolve* counters account for exactly the calls made, and the four
// classification counters account for exactly the modules stored.
func requireSumIdentity(t *testing.T, c *counting) {
	t.Helper()
	s := c.Stats()
	require.Equal(t, c.calls, s.ResolveTotal(),
		"the four Resolve* counters must sum to every Resolve call: %+v", s)
	require.Equal(t, s.ModulesStored, s.ModulesClassified(),
		"the four classification counters must partition ModulesStored: %+v", s)
	require.Equal(t, s.ModulesEvicted, s.ModulesEvictedCapacity+s.ModulesEvictedBytes,
		"the eviction breakdown must sum to ModulesEvicted: %+v", s)
}

// ---------------------------------------------------------------------------
// The four-valued status
// ---------------------------------------------------------------------------

// TestModuleStoreAllFourStatusesAreReachable is the core table: every value of
// gpu_src_status is produced by a real fixture through the real store, and each
// increments its own counter. A status that no test can reach is a status the
// profile could carry without anyone ever having seen it.
func TestModuleStoreAllFourStatusesAreReachable(t *testing.T) {
	withInfo := fixture(t, "single_lineinfo.cubin")
	noInfo := fixture(t, "single_nolineinfo.cubin")
	damaged := damagedLineInfo(t)

	const (
		crcWithInfo    = 0x1111
		crcNoInfo      = 0x2222
		crcDamaged     = 0x3333
		crcUnparseable = 0x4444
		crcAbsent      = 0x5555
	)

	st := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{Capacity: 16})}
	require.NoError(t, st.Put(crcWithInfo, withInfo))
	require.NoError(t, st.Put(crcNoInfo, noInfo))
	require.NoError(t, st.Put(crcDamaged, damaged), "a damaged line table is not a parse failure")
	require.Error(t, st.Put(crcUnparseable, []byte("not an ELF at all")))

	idx := symIndexOf(t, withInfo, "addOne")
	noInfoIdx := symIndexOf(t, noInfo, "addOne")

	tests := []struct {
		name     string
		crc      uint64
		fnIdx    uint32
		pc       uint64
		want     SrcStatus
		wantFile string
		wantLine uint32
	}{
		{
			name: "covered pc in a -lineinfo module", crc: crcWithInfo, fnIdx: idx, pc: 0x10,
			want: SrcResolved, wantFile: "single.cu", wantLine: 6,
		},
		{
			name: "module built without -lineinfo", crc: crcNoInfo, fnIdx: noInfoIdx, pc: 0x10,
			want: SrcNoLineInfo,
		},
		{
			name: "crc never offered", crc: crcAbsent, fnIdx: idx, pc: 0x10,
			want: SrcNoModule,
		},
		{
			name: "bytes we hold but cannot parse", crc: crcUnparseable, fnIdx: idx, pc: 0x10,
			want: SrcNoModule,
		},
		{
			name: "line table present but damaged", crc: crcDamaged, fnIdx: idx, pc: 0x10,
			want: SrcNoModule,
		},
		{
			name: "pc past the end of the function", crc: crcWithInfo, fnIdx: idx, pc: 0x180,
			want: SrcUnmapped,
		},
		{
			name: "function index this module does not have", crc: crcWithInfo, fnIdx: idx + 1000, pc: 0x10,
			want: SrcUnmapped,
		},
	}

	seen := make(map[SrcStatus]bool)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := st.Resolve(tc.crc, tc.fnIdx, tc.pc)
			require.Equal(t, tc.want, r.Status(), "status for %s", tc.name)

			fn, file, line, ok := r.Source()
			if tc.want != SrcResolved {
				assert.False(t, ok, "only a resolved status may carry a source location")
				assert.Empty(t, fn)
				assert.Empty(t, file)
				assert.Zero(t, line)
				return
			}
			require.True(t, ok)
			assert.Equal(t, "addOne", fn)
			assert.Equal(t, tc.wantFile, filepath.Base(file))
			assert.Equal(t, tc.wantLine, line)
		})
		seen[tc.want] = true
	}

	for _, s := range SrcStatuses() {
		assert.True(t, seen[s], "status %s is not reachable from this table", s)
	}

	got := st.Stats()
	assert.Equal(t, uint64(1), got.ResolveResolved)
	assert.Equal(t, uint64(1), got.ResolveNoLineInfo)
	assert.Equal(t, uint64(3), got.ResolveNoModule)
	assert.Equal(t, uint64(2), got.ResolveUnmapped)
	requireSumIdentity(t, st)
}

// TestModuleStoreResolveCountersSumToEveryCall drives a mixed, repetitive
// workload across every status and asserts the identity holds for all of it -
// the design states it as a test, not as a property.
func TestModuleStoreResolveCountersSumToEveryCall(t *testing.T) {
	withInfo := fixture(t, "single_lineinfo.cubin")
	noInfo := fixture(t, "single_nolineinfo.cubin")

	st := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{Capacity: 4})}
	require.NoError(t, st.Put(1, withInfo))
	require.NoError(t, st.Put(2, noInfo))
	require.Error(t, st.Put(3, withInfo[:64]))

	idx := symIndexOf(t, withInfo, "addOne")
	for pc := uint64(0); pc < 0x200; pc += 0x10 {
		for crc := uint64(1); crc <= 4; crc++ {
			st.Resolve(crc, idx, pc)
			st.Resolve(crc, idx+7, pc)
		}
	}

	s := st.Stats()
	require.Positive(t, s.ResolveResolved)
	require.Positive(t, s.ResolveNoModule)
	require.Positive(t, s.ResolveNoLineInfo)
	require.Positive(t, s.ResolveUnmapped)
	requireSumIdentity(t, st)
}

// TestSrcStatusZeroValueIsNotAStatus pins the "no silent default" half of the
// design's requirement. The zero value cannot be one of the four, because a
// caller who forgot to set a status would otherwise ship whichever of the four
// happened to be zero, and it would be unreadable as a mistake.
func TestSrcStatusZeroValueIsNotAStatus(t *testing.T) {
	var zero SrcStatus
	assert.NotContains(t, SrcStatuses(), zero)
	assert.Equal(t, srcStatusInvalid, zero)
	assert.Equal(t, "unset-src-status", zero.String(), "an undecided status names itself as one")
	assert.Equal(t, "invalid-src-status-200", SrcStatus(200).String(), "a fabricated one names itself apart")

	_, err := json.Marshal(zero)
	assert.Error(t, err, "a status nobody decided must not serialize")

	_, err = json.Marshal(SrcStatus(200))
	assert.Error(t, err, "a fabricated status must not serialize")

	var r Resolution
	assert.Equal(t, zero, r.Status())
	_, _, _, ok := r.Source()
	assert.False(t, ok, "the zero Resolution has no source")
}

// TestSrcStatusesIsExhaustiveAndStable asserts SrcStatuses covers exactly the
// four wire values and that each spells itself the way the design fixes it.
// These strings are the gpu_src_status label values; rewording one silently
// changes the profile.
func TestSrcStatusesIsExhaustiveAndStable(t *testing.T) {
	all := SrcStatuses()
	require.Len(t, all, 4)
	assert.Equal(t, []SrcStatus{SrcResolved, SrcNoLineInfo, SrcNoModule, SrcUnmapped}, all)

	want := map[SrcStatus]string{
		SrcResolved:   "resolved",
		SrcNoLineInfo: "no-lineinfo",
		SrcNoModule:   "no-module",
		SrcUnmapped:   "unmapped",
	}
	for _, s := range all {
		assert.Equal(t, want[s], s.String())
		b, err := json.Marshal(s)
		require.NoError(t, err)
		assert.JSONEq(t, `"`+want[s]+`"`, string(b))
	}

	// Mutating the returned slice must not reach the package's own list.
	all[0] = SrcUnmapped
	assert.Equal(t, SrcResolved, SrcStatuses()[0])
}

// ---------------------------------------------------------------------------
// The damaged-table decision, and the counters that keep causes apart
// ---------------------------------------------------------------------------

// TestDamagedLineTableResolvesAsNoModuleNotNoLineInfo is the decision
// internal/cubin recommended and this store implements. A present-but-damaged
// .debug_line reported as no-lineinfo would tell the operator to add a build
// flag they already passed. It resolves as no-module - we hold bytes we cannot
// use - while its own counter keeps the cause visible.
func TestDamagedLineTableResolvesAsNoModuleNotNoLineInfo(t *testing.T) {
	damaged := damagedLineInfo(t)
	idx := symIndexOf(t, damaged, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 4})
	require.NoError(t, st.Put(9, damaged), "the ELF is fine; only its line table is not")

	for pc := uint64(0); pc < 0x180; pc += 0x10 {
		r := st.Resolve(9, idx, pc)
		require.Equal(t, SrcNoModule, r.Status(), "pcOffset %#x", pc)
		require.NotEqual(t, SrcNoLineInfo, r.Status(),
			"a damaged table must never be reported as a missing build flag")
	}

	s := st.Stats()
	assert.Equal(t, uint64(1), s.ModulesDamagedLineInfo)
	assert.Equal(t, uint64(0), s.ModulesUnparseable, "the bytes parsed; only the DWARF did not")
	assert.Equal(t, uint64(0), s.ModulesWithoutLineInfo, "it HAS .debug_line")
	assert.Equal(t, uint64(0), s.ModulesWithLineInfo)
	assert.Equal(t, s.ModulesStored, s.ModulesClassified())
}

// TestUnparseableIsNotWithoutLineInfo is the distinction the design flags: one
// counter means "the bytes are wrong", the other means "the build flags are".
// They answer the reader with the same status but they are not the same fact,
// and a single counter covering both would make a transport bug look like a
// compiler option.
func TestUnparseableIsNotWithoutLineInfo(t *testing.T) {
	noInfo := fixture(t, "single_nolineinfo.cubin")
	withInfo := fixture(t, "single_lineinfo.cubin")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 8})
	require.NoError(t, st.Put(1, noInfo))
	require.Error(t, st.Put(2, []byte("junk")))
	require.Error(t, st.Put(3, withInfo[:len(withInfo)/2]), "a truncated cubin is unparseable")
	require.Error(t, st.Put(4, nil))

	s := st.Stats()
	assert.Equal(t, uint64(1), s.ModulesWithoutLineInfo)
	assert.Equal(t, uint64(3), s.ModulesUnparseable)
	assert.Equal(t, uint64(0), s.ModulesDamagedLineInfo)
	assert.Equal(t, uint64(4), s.ModulesStored, "unparseable modules are stored, not dropped")
	assert.Equal(t, s.ModulesStored, s.ModulesClassified())

	// Both resolve as no-module; the counters are the only thing separating
	// the two causes, which is exactly why they are separate.
	assert.Equal(t, SrcNoModule, st.Resolve(2, 0, 0).Status())
	assert.Equal(t, SrcNoLineInfo, st.Resolve(1, symIndexOf(t, noInfo, "addOne"), 0).Status())
}

// ---------------------------------------------------------------------------
// Source resolution against the fixtures
// ---------------------------------------------------------------------------

// TestModuleStoreResolvesExactSourceLines pins the store's answers against the
// line table internal/cubin asserts from the same bytes. Note lines 10, 9, 10
// at 0xa0/0xb0/0xc0: the table is not monotonic in line number, so an answer
// that merely looked plausible would not pass.
func TestModuleStoreResolvesExactSourceLines(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 4})
	require.NoError(t, st.Put(1, b))

	want := []struct {
		pc   uint64
		line uint32
	}{
		{0x00, 5}, {0x10, 6}, {0x40, 7}, {0x60, 8},
		{0xa0, 10}, {0xb0, 9}, {0xc0, 10}, {0xd0, 12},
		{0x30, 6}, {0x50, 7}, {0x9f, 8}, {0x17f, 12},
	}
	for _, w := range want {
		r := st.Resolve(1, idx, w.pc)
		require.Equal(t, SrcResolved, r.Status(), "pcOffset %#x", w.pc)
		fn, file, line, ok := r.Source()
		require.True(t, ok)
		assert.Equal(t, "addOne", fn)
		assert.Equal(t, "single.cu", filepath.Base(file))
		assert.Equal(t, w.line, line, "pcOffset %#x", w.pc)
	}

	// The end_sequence sits at the function's size; nothing beyond resolves,
	// and no line is ever synthesized for it.
	for _, pc := range []uint64{0x180, 0x190, 1 << 40} {
		r := st.Resolve(1, idx, pc)
		assert.Equal(t, SrcUnmapped, r.Status(), "pcOffset %#x", pc)
	}
}

// TestModuleStoreBindsFunctionIndexToTheRightKernel uses the two-kernel fixture,
// whose kernels occupy the identical PC range and whose source-line ranges are
// disjoint. A store that mixed the two indices up would still answer
// "resolved", with confidently wrong lines - the failure mode the whole design
// exists to prevent.
func TestModuleStoreBindsFunctionIndexToTheRightKernel(t *testing.T) {
	b := fixture(t, "two_kernels_lineinfo.cubin")
	scaleIdx := symIndexOf(t, b, "scale")
	offsetIdx := symIndexOf(t, b, "offset")
	require.NotEqual(t, scaleIdx, offsetIdx)

	st := NewModuleStore(ModuleStoreConfig{Capacity: 4})
	require.NoError(t, st.Put(1, b))

	var scaleHits, offsetHits int
	for pc := uint64(0); pc < 0x180; pc += 0x10 {
		if r := st.Resolve(1, scaleIdx, pc); r.Status() == SrcResolved {
			fn, _, line, ok := r.Source()
			require.True(t, ok)
			assert.Equal(t, "scale", fn)
			assert.Less(t, line, uint32(13), "scale's lines are all below offset's")
			scaleHits++
		}
		if r := st.Resolve(1, offsetIdx, pc); r.Status() == SrcResolved {
			fn, _, line, ok := r.Source()
			require.True(t, ok)
			assert.Equal(t, "offset", fn)
			assert.Greater(t, line, uint32(11), "offset's lines are all above scale's")
			offsetHits++
		}
	}
	assert.Positive(t, scaleHits)
	assert.Positive(t, offsetHits)
}

// ---------------------------------------------------------------------------
// Bounds
// ---------------------------------------------------------------------------

// TestModuleStoreEvictionIsExactUnderPressure asserts ModulesEvicted is exact,
// not approximate, and that its breakdown reconciles. A bound whose counter is
// only roughly right is the kind of green-when-worst reading this project keeps
// finding.
func TestModuleStoreEvictionIsExactUnderPressure(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")

	const capacity, puts = 3, 50
	st := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{Capacity: capacity})}
	for i := range uint64(puts) {
		require.NoError(t, st.Put(i, b))
		require.LessOrEqual(t, st.Len(), capacity, "the bound must hold at every step")
	}

	s := st.Stats()
	assert.Equal(t, capacity, s.Live)
	assert.Equal(t, uint64(puts), s.ModulesStored)
	assert.Equal(t, uint64(puts-capacity), s.ModulesEvicted)
	assert.Equal(t, uint64(puts-capacity), s.ModulesEvictedCapacity)
	assert.Equal(t, uint64(0), s.ModulesEvictedBytes)
	assert.Equal(t, int64(capacity*len(b)), s.LiveBytes)
	requireSumIdentity(t, st)
}

// TestModuleStoreLRUKeepsWhatIsBeingResolved is why this is an LRU and not an
// insertion-ordered FIFO. A module under active PC sampling must survive a
// burst of unrelated module loads; under a FIFO it would not, and its samples
// would silently become no-module while the store still had room for it.
func TestModuleStoreLRUKeepsWhatIsBeingResolved(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 2})
	require.NoError(t, st.Put(1, b))
	require.NoError(t, st.Put(2, b))

	// Touch 1 by resolving against it, making 2 the least recently used.
	require.Equal(t, SrcResolved, st.Resolve(1, idx, 0x10).Status())

	require.NoError(t, st.Put(3, b))
	assert.Equal(t, SrcResolved, st.Resolve(1, idx, 0x10).Status(),
		"the module being resolved against must survive")
	assert.Equal(t, SrcNoModule, st.Resolve(2, idx, 0x10).Status(),
		"the untouched module is the one that goes")
	assert.Equal(t, SrcResolved, st.Resolve(3, idx, 0x10).Status())

	// A re-offer counts as use too: a producer re-announcing a module is
	// evidence it is live.
	require.NoError(t, st.Put(1, b))
	require.NoError(t, st.Put(4, b))
	assert.Equal(t, SrcResolved, st.Resolve(1, idx, 0x10).Status())
	assert.Equal(t, SrcNoModule, st.Resolve(3, idx, 0x10).Status())
}

// TestResolveAfterEvictionIsNoModuleNotStale is the explicit test the design
// calls for. Nothing in the store memoizes a resolution, so an evicted module's
// answers revert to no-module immediately and completely: a stale source
// location is indistinguishable from a measured one once it reaches a label.
func TestResolveAfterEvictionIsNoModuleNotStale(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{Capacity: 1})}
	require.NoError(t, st.Put(1, b))

	before := st.Resolve(1, idx, 0x10)
	require.Equal(t, SrcResolved, before.Status())
	_, _, line, ok := before.Source()
	require.True(t, ok)
	require.Equal(t, uint32(6), line)

	require.NoError(t, st.Put(2, b))
	require.Equal(t, uint64(1), st.Stats().ModulesEvicted)

	// Every offset that resolved before must now answer no-module, at the
	// exact same coordinates. Repeatedly: a cache that answered correctly once
	// and staled later would pass a single check.
	for range 3 {
		for pc := uint64(0); pc < 0x180; pc += 0x10 {
			after := st.Resolve(1, idx, pc)
			require.Equal(t, SrcNoModule, after.Status(), "pcOffset %#x", pc)
			fn, file, ln, ok := after.Source()
			require.False(t, ok)
			require.Empty(t, fn)
			require.Empty(t, file)
			require.Zero(t, ln)
		}
	}

	// The Resolution taken before the eviction is a value, so it still reads
	// as it did. That is not staleness in the store - nothing the store
	// answers now depends on it - and the point of this assertion is that the
	// two are different things.
	assert.Equal(t, SrcResolved, before.Status())
	requireSumIdentity(t, st)
}

// TestModuleStoreByteBoundEvicts asserts the second bound is real. A count
// bound alone is not a memory bound, and a store whose only limit is "512
// modules" can hold hundreds of megabytes without any counter noticing.
func TestModuleStoreByteBoundEvicts(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")

	st := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{
		Capacity: 1000,
		MaxBytes: int64(2 * len(b)),
	})}
	for i := range uint64(10) {
		require.NoError(t, st.Put(i, b))
		require.LessOrEqual(t, st.Stats().LiveBytes, int64(2*len(b)))
	}

	s := st.Stats()
	assert.Equal(t, 2, s.Live)
	assert.Equal(t, uint64(8), s.ModulesEvicted)
	assert.Equal(t, uint64(8), s.ModulesEvictedBytes)
	assert.Equal(t, uint64(0), s.ModulesEvictedCapacity)
	requireSumIdentity(t, st)
}

// TestModuleStoreOversizedModuleLeavesTheStoreEmpty pins the absolute reading
// of MaxBytes. A single module larger than the whole budget is not kept "just
// this once": it is evicted like anything else, the store ends empty, and its
// Resolves say no-module - which is true, because it is not held.
func TestModuleStoreOversizedModuleLeavesTheStoreEmpty(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 10, MaxBytes: int64(len(b) / 2)})
	require.NoError(t, st.Put(1, b))

	s := st.Stats()
	assert.Equal(t, 0, s.Live)
	assert.Equal(t, int64(0), s.LiveBytes)
	assert.Equal(t, uint64(1), s.ModulesStored)
	assert.Equal(t, uint64(1), s.ModulesEvicted)
	assert.Equal(t, uint64(1), s.ModulesEvictedBytes)
	assert.Equal(t, SrcNoModule, st.Resolve(1, idx, 0x10).Status())
}

// TestModuleStoreDefaultsAreApplied keeps a zero config from meaning
// "unbounded", which is how a bound becomes a bound only for callers who
// remembered to ask for one.
func TestModuleStoreDefaultsAreApplied(t *testing.T) {
	st := NewModuleStore(ModuleStoreConfig{})
	assert.Equal(t, defaultModuleStoreCapacity, st.cfg.Capacity)
	assert.Equal(t, int64(defaultModuleStoreMaxBytes), st.cfg.MaxBytes)

	neg := NewModuleStore(ModuleStoreConfig{Capacity: -1, MaxBytes: -1})
	assert.Equal(t, defaultModuleStoreCapacity, neg.cfg.Capacity)
	assert.Equal(t, int64(defaultModuleStoreMaxBytes), neg.cfg.MaxBytes)
}

// ---------------------------------------------------------------------------
// Put's contract
// ---------------------------------------------------------------------------

// TestPutOfAKnownCRCIsANoOp asserts a re-offer costs nothing and changes
// nothing. cubin_crc is content addressed, so a repeated CRC is the same bytes
// by definition.
func TestPutOfAKnownCRCIsANoOp(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 4})
	require.NoError(t, st.Put(1, b))
	for range 5 {
		require.NoError(t, st.Put(1, b))
	}

	s := st.Stats()
	assert.Equal(t, uint64(1), s.ModulesStored)
	assert.Equal(t, uint64(1), s.ModulesWithLineInfo)
	assert.Equal(t, int64(len(b)), s.LiveBytes, "a re-offer must not double-count bytes")
	assert.Equal(t, uint64(0), s.ModulesEvicted)

	// A re-offer of an unparseable module returns the same diagnostic error
	// without re-parsing.
	require.Error(t, st.Put(2, []byte("junk")))
	require.Error(t, st.Put(2, []byte("junk")))
	assert.Equal(t, uint64(1), st.Stats().ModulesUnparseable)
}

// TestPutDoesNotRetainCallerBytes is not defensiveness about mutation: the
// transport hands the store an mmap of a sealed memfd and drops the mapping
// once the offer is handled. internal/cubin.Parse is written not to retain its
// input for that reason, and a store that retained it would put the hazard
// straight back.
func TestPutDoesNotRetainCallerBytes(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	caller := append([]byte(nil), b...)
	st := NewModuleStore(ModuleStoreConfig{Capacity: 4})
	require.NoError(t, st.Put(1, caller))

	// Scribble over the caller's buffer, standing in for an unmap.
	for i := range caller {
		caller[i] = 0xAA
	}

	r := st.Resolve(1, idx, 0x10)
	require.Equal(t, SrcResolved, r.Status())
	_, file, line, ok := r.Source()
	require.True(t, ok)
	assert.Equal(t, "single.cu", filepath.Base(file))
	assert.Equal(t, uint32(6), line)
	assert.Equal(t, int64(len(b)), st.Stats().LiveBytes)
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

// TestModuleStoreConcurrentPutAndResolve exercises the store under -race. Both
// Put and Resolve mutate (Resolve refreshes LRU recency), so both take the
// write lock, and the identities must survive concurrent use.
func TestModuleStoreConcurrentPutAndResolve(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	noInfo := fixture(t, "single_nolineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 8})

	const workers, iters = 8, 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	var calls uint64

	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := uint64(0)
			for i := range uint64(iters) {
				crc := (uint64(w)*iters + i) % 20
				switch i % 3 {
				case 0:
					_ = st.Put(crc, b)
				case 1:
					_ = st.Put(crc, noInfo)
				default:
					st.Resolve(crc, idx, (i%0x18)*0x10)
					local++
				}
			}
			mu.Lock()
			calls += local
			mu.Unlock()
		}()
	}
	wg.Wait()

	s := st.Stats()
	assert.LessOrEqual(t, s.Live, 8)
	assert.Equal(t, calls, s.ResolveTotal())
	assert.Equal(t, s.ModulesStored, s.ModulesClassified())
	assert.Equal(t, s.ModulesEvicted, s.ModulesEvictedCapacity+s.ModulesEvictedBytes)
	assert.Equal(t, s.ModulesStored-uint64(s.Live), s.ModulesEvicted,
		"every stored module is either live or evicted")
}

// ---------------------------------------------------------------------------
// FunctionName - the accessor the continuous-mode join needs
// ---------------------------------------------------------------------------

// TestFunctionNameWorksWithoutLineInfo is the reason this accessor exists at
// all. Resolve carries a function name only under SrcResolved, which is
// correct for source labels and wrong for attribution: the continuous-mode
// chain is cubin_crc -> module -> function -> KERNEL, and a kernel built
// without -lineinfo still has a name, still runs, and still owns its PC
// samples. If attribution went through Resolve, a missing build flag would
// silently cost the whole join rather than just the source lines.
func TestFunctionNameWorksWithoutLineInfo(t *testing.T) {
	withInfo := fixture(t, "single_lineinfo.cubin")
	noInfo := fixture(t, "single_nolineinfo.cubin")

	st := NewModuleStore(ModuleStoreConfig{})
	require.NoError(t, st.Put(1, withInfo))
	require.NoError(t, st.Put(2, noInfo))

	name, ok := st.FunctionName(1, symIndexOf(t, withInfo, "addOne"))
	require.True(t, ok)
	assert.Equal(t, "addOne", name)

	name, ok = st.FunctionName(2, symIndexOf(t, noInfo, "addOne"))
	require.True(t, ok, "a module with no line table still has a symbol table")
	assert.Equal(t, "addOne", name)

	// And the contrast that makes the point: the same module answers
	// no-lineinfo for source, with no name at all.
	res := st.Resolve(2, symIndexOf(t, noInfo, "addOne"), 0)
	assert.Equal(t, SrcNoLineInfo, res.Status())
	_, _, _, hasSource := res.Source()
	assert.False(t, hasSource, "no-lineinfo carries no location, and that is unchanged")
}

// TestFunctionNameBindsTheIndexToTheRightKernel uses the two-kernel fixture:
// an accessor that returned a neighbouring function would still look healthy
// on a single-kernel module. Attribution built on a swapped index puts a
// kernel's stalls on a different kernel, confidently.
func TestFunctionNameBindsTheIndexToTheRightKernel(t *testing.T) {
	b := fixture(t, "two_kernels_lineinfo.cubin")
	st := NewModuleStore(ModuleStoreConfig{})
	require.NoError(t, st.Put(7, b))

	for _, kernel := range []string{"scale", "offset"} {
		name, ok := st.FunctionName(7, symIndexOf(t, b, kernel))
		require.Truef(t, ok, "kernel %s", kernel)
		assert.Equal(t, kernel, name)
	}
}

// TestFunctionNameRefusesRatherThanGuesses covers every way the answer is
// unavailable. Each returns ok=false and an empty name; none returns a
// neighbouring function, and none is a partial answer a caller could mistake
// for a real one.
func TestFunctionNameRefusesRatherThanGuesses(t *testing.T) {
	withInfo := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, withInfo, "addOne")

	st := NewModuleStore(ModuleStoreConfig{})
	require.NoError(t, st.Put(1, withInfo))
	require.Error(t, st.Put(2, []byte("not an ELF at all")))

	cases := []struct {
		name    string
		crc     uint64
		fnIndex uint32
	}{
		{"a CRC nothing was ever offered for", 99, idx},
		{"a module whose bytes did not parse", 2, idx},
		{"an index that is not in the symbol table", 1, idx + 9999},
		{"index zero, which is the ELF null symbol", 1, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, ok := st.FunctionName(tc.crc, tc.fnIndex)
			assert.False(t, ok)
			assert.Empty(t, name, "a refusal must carry no name at all, not a plausible one")
		})
	}
}

// TestFunctionNameSurvivesADamagedLineTable pins the one case where
// FunctionName and Resolve deliberately disagree. A cubin whose .debug_line
// cannot be read resolves as no-module - we hold bytes we cannot use for
// source - but its symbol table is intact and the kernel's identity does not
// come from DWARF. Attribution must not be lost to a broken line table.
func TestFunctionNameSurvivesADamagedLineTable(t *testing.T) {
	damaged := damagedLineInfo(t)
	idx := symIndexOf(t, damaged, "addOne")

	st := NewModuleStore(ModuleStoreConfig{})
	require.NoError(t, st.Put(1, damaged))

	assert.Equal(t, SrcNoModule, st.Resolve(1, idx, 0).Status(),
		"source resolution is correctly lost")
	name, ok := st.FunctionName(1, idx)
	require.True(t, ok, "attribution is not")
	assert.Equal(t, "addOne", name)
}

// TestFunctionNameAfterEvictionIsRefusedNotStale is the store's central
// no-memo guarantee applied to this accessor: an answer must never outlive the
// bytes it came from.
func TestFunctionNameAfterEvictionIsRefusedNotStale(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 1})
	require.NoError(t, st.Put(1, b))
	name, ok := st.FunctionName(1, idx)
	require.True(t, ok)
	require.Equal(t, "addOne", name)

	require.NoError(t, st.Put(2, b)) // evicts CRC 1

	name, ok = st.FunctionName(1, idx)
	assert.False(t, ok, "an evicted module must answer nothing, not its last known name")
	assert.Empty(t, name)
}

// TestFunctionNameRefreshesRecency: a module whose functions are being joined
// is a module in use. Without the touch, a burst of unrelated module loads
// would silently stop attribution for a live kernel while the store still had
// room for it.
func TestFunctionNameRefreshesRecency(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := NewModuleStore(ModuleStoreConfig{Capacity: 2})
	require.NoError(t, st.Put(1, b))
	require.NoError(t, st.Put(2, b))

	// Touch 1 through FunctionName only, then push a third module in.
	_, ok := st.FunctionName(1, idx)
	require.True(t, ok)
	require.NoError(t, st.Put(3, b))

	_, ok = st.FunctionName(1, idx)
	assert.True(t, ok, "the module FunctionName just used must not be the one evicted")
	_, ok = st.FunctionName(2, idx)
	assert.False(t, ok, "the untouched module is the one that goes")
}

// TestFunctionNameDoesNotDisturbTheResolveIdentity pins the deliberate
// omission: FunctionName increments none of the four Resolve* counters,
// because those four partition calls to Resolve exactly and that identity is
// the store's main self-check. Folding a second entry point into it would
// break the identity while looking like better instrumentation.
func TestFunctionNameDoesNotDisturbTheResolveIdentity(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	c := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{})}
	require.NoError(t, c.Put(1, b))

	for range 5 {
		_, _ = c.FunctionName(1, idx)
		_, _ = c.FunctionName(99, idx)
	}
	require.Zero(t, c.Stats().ResolveTotal(), "no Resolve call has been made yet")

	for range 3 {
		c.Resolve(1, idx, 0x10)
	}
	requireSumIdentity(t, c)
}

// TestHasIsLiveMembershipAndCountsAsUse covers the accessor the cubin
// transport asks before it maps a payload (gpuprobe.Config.Modules).
//
// Two properties, and the store is the only place either can be guaranteed:
//
//   - it answers about what is held NOW. An evicted module answers false, so
//     the transport admits the next offer for it and the module comes back.
//     Anything that remembered CRCs across an eviction would turn one eviction
//     into permanent unresolvability, with every counter on both sides reading
//     healthy;
//   - it counts as use. A producer re-announcing a module is evidence the
//     module is live, exactly as Put's already-held path treats it, so a
//     module offered often but sampled rarely does not age out under a burst
//     of unrelated loads.
//
// And it must not touch the Resolve* counters: those four partition calls to
// Resolve exactly, and that identity is this store's main self-check.
func TestHasIsLiveMembershipAndCountsAsUse(t *testing.T) {
	b := fixture(t, "single_lineinfo.cubin")
	idx := symIndexOf(t, b, "addOne")

	st := &counting{ModuleStore: NewModuleStore(ModuleStoreConfig{Capacity: 2})}
	require.NoError(t, st.Put(1, b))
	require.NoError(t, st.Put(2, b))
	assert.False(t, st.Has(3), "a CRC that was never offered is not held")

	// Touch 1 through Has alone, making 2 the least recently used.
	require.True(t, st.Has(1))
	require.NoError(t, st.Put(3, b))
	assert.True(t, st.Has(1), "Has did not count as use, so the module it asked about aged out")
	assert.False(t, st.Has(2), "the untouched module is the one that goes")

	// Live membership, not a memory: the evicted CRC answers false, and
	// offering it again brings it back.
	require.Equal(t, SrcNoModule, st.Resolve(2, idx, 0x10).Status())
	require.NoError(t, st.Put(2, b))
	assert.True(t, st.Has(2))
	assert.Equal(t, SrcResolved, st.Resolve(2, idx, 0x10).Status(),
		"a re-offered module did not become resolvable again")

	// The identity still holds: Has is not a fifth Resolve.
	assert.Equal(t, st.calls, st.Stats().ResolveTotal(),
		"Has incremented a Resolve counter; the four no longer partition Resolve calls")
}
