// Package ehmaps populates the BPF-side CFI / classification / pid-mappings
// maps from unwind/ehcompile output. S3 scope: pure population — no MMAP2
// ingestion, no refcounting, no munmap cleanup. S4 adds the lifecycle layer
// on top of this package's primitives.
//
// Build-IDs map to 64-bit table_ids via FNV-1a (non-cryptographic; collision
// resistance is "practically nonexistent" at the scale we care about — a
// single agent tracking at most a few thousand unique binaries).
package ehmaps

import (
	"encoding/binary"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// TableIDForBuildID hashes a build-id (raw bytes, typically 20) to the u64
// key used across cfi_rules, cfi_classification, and pid_mapping.table_id.
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

// CFIEntryByteSize matches bpf/unwind_common.h `struct cfi_entry` (24 bytes).
const CFIEntryByteSize = 24

// ClassificationByteSize matches bpf/unwind_common.h `struct classification`
// (16 bytes).
const ClassificationByteSize = 16

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

// MarshalClassification writes one ehcompile.Classification in BPF layout.
func MarshalClassification(c ehcompile.Classification) []byte {
	out := make([]byte, ClassificationByteSize)
	binary.LittleEndian.PutUint64(out[0:8], c.PCStart)
	binary.LittleEndian.PutUint32(out[8:12], c.PCEndDelta)
	out[12] = uint8(c.Mode)
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
