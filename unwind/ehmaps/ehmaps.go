// Package ehmaps populates the BPF-side CFI and pid-mappings
// maps from unwind/ehcompile output. Handles population plus lifecycle
// management via the refcounting layer in this package.
//
// Build-IDs map to 64-bit table_ids via FNV-1a (non-cryptographic; collision
// resistance is "practically nonexistent" at the scale we care about — a
// single agent tracking at most a few thousand unique binaries).
package ehmaps

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/cilium/ebpf"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// TableIDForBuildID hashes a build-id (raw bytes, typically 20) to the u64
// key used across cfi_rules and pid_mapping.table_id.
// Empty input returns the FNV-1a offset basis, which is fine — the caller
// should validate that a missing build-id doesn't collide with a real one.
func TableIDForBuildID(buildID []byte) uint64 {
	const (
		offset64 uint64 = 0xcbf29ce484222325
		prime64  uint64 = 0x100000001b3
	)
	h := offset64
	for _, b := range buildID {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}

// CFIEntryByteSize matches bpf/unwind_common.h `struct cfi_entry` (32 bytes
// after u64 alignment padding; the active data fills offsets 0..25 and the
// remaining 6 bytes are tail padding the BPF struct expects).
const CFIEntryByteSize = 32

// PIDMappingByteSize matches bpf/unwind_common.h `struct pid_mapping`
// (32 bytes).
const PIDMappingByteSize = 32

// PIDMapping is the Go-side twin of bpf/unwind_common.h `struct pid_mapping`.
// Describes one contiguous load of a binary into a process's address space.
type PIDMapping struct {
	VMAStart uint64
	VMAEnd   uint64
	LoadBias uint64
	TableID  uint64
}

// MarshalCFIEntry writes one ehcompile.CFIEntry in the exact byte order the
// BPF walker expects. Keep in lockstep with bpf/unwind_common.h.
func MarshalCFIEntry(e ehcompile.CFIEntry) []byte {
	out := make([]byte, CFIEntryByteSize)
	binary.LittleEndian.PutUint64(out[0:8], e.PCStart)
	binary.LittleEndian.PutUint32(out[8:12], e.PCEndDelta)
	out[12] = uint8(e.CFAType)
	out[13] = uint8(e.FPType)
	binary.LittleEndian.PutUint16(out[14:16], uint16(e.CFAOffset))
	binary.LittleEndian.PutUint16(out[16:18], uint16(e.FPOffset))
	binary.LittleEndian.PutUint16(out[18:20], uint16(e.RAOffset))
	out[20] = uint8(e.RAType)
	return out
}

// MarshalPIDMapping writes one PIDMapping in BPF layout.
func MarshalPIDMapping(m PIDMapping) []byte {
	out := make([]byte, PIDMappingByteSize)
	binary.LittleEndian.PutUint64(out[0:8], m.VMAStart)
	binary.LittleEndian.PutUint64(out[8:16], m.VMAEnd)
	binary.LittleEndian.PutUint64(out[16:24], m.LoadBias)
	binary.LittleEndian.PutUint64(out[24:32], m.TableID)
	return out
}

// BPF_F_INNER_MAP enables dynamic sizing of HASH_OF_MAPS inner maps.
// Defined in linux/bpf.h; redeclared here since cilium/ebpf doesn't
// re-export it at the package level. Requires kernel 5.10+; our 6.0+
// floor covers this trivially.
const BPF_F_INNER_MAP = 0x1000

// MaxPIDMappings mirrors bpf/unwind_common.h's MAX_PID_MAPPINGS. Keep in
// lockstep. The BPF inner map must be created with this max_entries value
// for the walker's bpf_loop bound to hold.
const MaxPIDMappings = 256

// binarySearchMaxIters mirrors bpf/unwind_common.h's BINARY_SEARCH_MAX_ITERS.
// Keep in lockstep; TestSearchBoundMirrorsTheBPFHeader reads the header.
const binarySearchMaxIters = 24

// MaxSearchableRows is the largest table the BPF walker's binary search can
// reach: a search over n sorted rows needs ceil(log2(n)) halvings, and the
// walker is bounded to binarySearchMaxIters of them.
//
// A TABLE BIGGER THAN THIS IS WORSE THAN NO TABLE, which is why it is refused
// rather than installed with a warning. The search narrows to a range of two
// or three and then ends, so the lookup fails for almost every PC -- and both
// failure paths in the walker LIE about it. cfi_lookup returns NULL, which
// walk_step reads as a CFI miss and stops the walk. classify_rel_pc returns
// MODE_FP_SAFE, which sends a frame-pointer-less frame down the
// frame-pointer path. Having no table at all produces the second of those and
// not the first, so an unsearchable table costs the walk more than omitting it
// does.
//
// This existed silently before: at 20 iterations libtorch_cpu.so's 2,359,137
// rows needed 22, and every GPU launch stack in a PyTorch capture abandoned
// inside libtorch's dispatcher -- 4,299 of them, none reaching root -- with
// nothing anywhere saying why.
const MaxSearchableRows = 1 << binarySearchMaxIters

// ErrTableTooLarge is returned when a binary's compiled tables are larger than
// the walker's binary search can reach. Named rather than silent: the caller
// logs it and the binary is unwound by frame pointer, which is a degradation
// an operator can see rather than one they cannot.
var ErrTableTooLarge = errors.New("ehmaps: table larger than the walker's binary search can reach")

// bitsNeeded is ceil(log2(n)): how many halvings a binary search over n rows
// needs in the worst case.
func bitsNeeded(n int) int {
	b := 0
	for v := n - 1; v > 0; v >>= 1 {
		b++
	}
	return b
}

// PopulateCFIArgs bundles what the caller already has in memory — an already-
// compiled set of rules plus the outer and length maps from the loaded BPF
// program.
type PopulateCFIArgs struct {
	TableID   uint64
	Entries   []ehcompile.CFIEntry
	OuterMap  *ebpf.Map // cfi_rules (HASH_OF_MAPS)
	LengthMap *ebpf.Map // cfi_lengths (HASH)
}

// PopulateCFI creates a right-sized inner ARRAY, fills it with Entries, and
// installs it into OuterMap keyed by TableID. Also writes the valid length
// into LengthMap. On success the inner map stays owned by the kernel (the
// outer map holds a reference); our userspace handle is closed immediately.
func PopulateCFI(args PopulateCFIArgs) error {
	if len(args.Entries) == 0 {
		return fmt.Errorf("ehmaps: PopulateCFI: no entries")
	}
	if len(args.Entries) > MaxSearchableRows {
		return fmt.Errorf("%w: %d CFI rows for table %#x needs %d search iterations, the walker has %d",
			ErrTableTooLarge, len(args.Entries), args.TableID,
			bitsNeeded(len(args.Entries)), binarySearchMaxIters)
	}
	spec := &ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  CFIEntryByteSize,
		MaxEntries: uint32(len(args.Entries)),
		Flags:      BPF_F_INNER_MAP,
	}
	inner, err := ebpf.NewMap(spec)
	if err != nil {
		return fmt.Errorf("ehmaps: create inner cfi map: %w", err)
	}
	for i, e := range args.Entries {
		key := uint32(i)
		if err := inner.Update(key, MarshalCFIEntry(e), ebpf.UpdateAny); err != nil {
			_ = inner.Close()
			return fmt.Errorf("ehmaps: write cfi[%d]: %w", i, err)
		}
	}
	if err := args.OuterMap.Update(args.TableID, uint32(inner.FD()), ebpf.UpdateAny); err != nil {
		_ = inner.Close()
		return fmt.Errorf("ehmaps: install inner cfi map: %w", err)
	}
	_ = inner.Close()

	length := uint32(len(args.Entries))
	if err := args.LengthMap.Update(args.TableID, length, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("ehmaps: write cfi length: %w", err)
	}
	return nil
}

// PopulatePIDMappingsArgs installs a list of mappings for one PID. The
// inner map is always sized at MaxPIDMappings so the BPF walker's bpf_loop
// bound is a compile-time constant.
type PopulatePIDMappingsArgs struct {
	PID       uint32
	Mappings  []PIDMapping
	OuterMap  *ebpf.Map // pid_mappings
	LengthMap *ebpf.Map // pid_mapping_lengths
}

func PopulatePIDMappings(args PopulatePIDMappingsArgs) error {
	if len(args.Mappings) == 0 {
		return fmt.Errorf("ehmaps: PopulatePIDMappings: no mappings")
	}
	if len(args.Mappings) > MaxPIDMappings {
		return fmt.Errorf("ehmaps: PopulatePIDMappings: %d > MaxPIDMappings=%d",
			len(args.Mappings), MaxPIDMappings)
	}
	spec := &ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  PIDMappingByteSize,
		MaxEntries: MaxPIDMappings,
		Flags:      BPF_F_INNER_MAP,
	}
	inner, err := ebpf.NewMap(spec)
	if err != nil {
		return fmt.Errorf("ehmaps: create inner pid_mappings map: %w", err)
	}
	for i, m := range args.Mappings {
		key := uint32(i)
		if err := inner.Update(key, MarshalPIDMapping(m), ebpf.UpdateAny); err != nil {
			_ = inner.Close()
			return fmt.Errorf("ehmaps: write pid_mapping[%d]: %w", i, err)
		}
	}
	if err := args.OuterMap.Update(args.PID, uint32(inner.FD()), ebpf.UpdateAny); err != nil {
		_ = inner.Close()
		return fmt.Errorf("ehmaps: install inner pid_mappings map: %w", err)
	}
	_ = inner.Close()

	length := uint32(len(args.Mappings))
	if err := args.LengthMap.Update(args.PID, length, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("ehmaps: write pid mapping length: %w", err)
	}
	return nil
}
