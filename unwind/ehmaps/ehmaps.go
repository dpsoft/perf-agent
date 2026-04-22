// Package ehmaps populates the BPF-side CFI / classification / pid-mappings
// maps from unwind/ehcompile output. S3 scope: pure population — no MMAP2
// ingestion, no refcounting, no munmap cleanup. S4 adds the lifecycle layer
// on top of this package's primitives.
//
// Build-IDs map to 64-bit table_ids via FNV-1a (non-cryptographic; collision
// resistance is "practically nonexistent" at the scale we care about — a
// single agent tracking at most a few thousand unique binaries).
package ehmaps

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
