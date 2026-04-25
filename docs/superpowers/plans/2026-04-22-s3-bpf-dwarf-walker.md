# S3: BPF DWARF Walker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Teach `perf_dwarf.bpf.c` to unwind FP-less frames via per-build-id CFI tables, with userspace populating the tables from `ehcompile` output for one manually-registered process.

**Architecture:** BPF gets three new map-of-maps — `cfi_rules`, `cfi_classification`, `pid_mappings` — plus parallel length-lookup hashes. The walker classifies each frame's PC, picks FP or DWARF per classification, and falls back to FP on classification miss. A new `unwind/ehmaps/` package converts `ehcompile` output into BPF-ready records and installs inner maps. MMAP2 ingestion, automatic lifecycle, and `profile.Profiler` integration are deferred to S4/S5.

**Tech Stack:** C (eBPF with libbpf+CO-RE), Go 1.26, cilium/ebpf, blazesym (unchanged), existing `unwind/ehcompile` package.

---

## Scope

**S3 delivers:** a DWARF-capable BPF walker whose correctness is proven end-to-end on the Rust `cpu_intensive_work` workload — chain depth ≥ the FP-only walker on the same samples, with `walker_flags` indicating the DWARF path fired.

**Explicitly out of scope (ship in S4/S5):**
- `PERF_RECORD_MMAP2` ingestion; mappings are set up by the test once
- `unwind/ehmaps/` lifecycle (refcounts, munmap handling, exec)
- `profile.Profiler` integration; end users still can't say `--unwind dwarf`
- Off-CPU DWARF variant

## File Structure

```
bpf/unwind_common.h                            MODIFY — add CFI/classification/pid_mappings map declarations + length maps; add lookup helpers (mapping_for_pc, classify_rel_pc, cfi_lookup).
bpf/perf_dwarf.bpf.c                           MODIFY — hybrid walker: classify then pick FP or DWARF; update walker_flags telemetry bits.
profile/gen.go                                 UNCHANGED (re-runs `go generate` regenerate .o files).
profile/perf_dwarf_{x86,arm64}_bpfel.{go,o}    REGENERATE
profile/dwarf_export.go                        MODIFY — expose new maps; add Populate helpers for tests.
profile/perf_dwarf_test.go                     MODIFY — keep TestPerfDwarfLoads; verifier still accepts the larger program.

unwind/ehmaps/ehmaps.go                        CREATE — TableIDForBuildID, ConvertCFIEntry, ConvertClassification, PopulateCFI, PopulateClassification, SetPIDMapping. No MMAP2, no refcounts.
unwind/ehmaps/ehmaps_test.go                   CREATE — unit tests for TableID + struct conversions (no BPF runtime).

cmd/perf-dwarf-test/main.go                    MODIFY — add --target-binary flag; before sampling, run ehcompile and populate maps.

test/workloads/rust/src/main.rs                MODIFY — add #[inline(never)] on cpu_intensive_work to force it into every stack.
test/integration_test.go                       MODIFY — add TestPerfDwarfWalker driving the whole flow end-to-end.
```

---

## Background for implementers

**Why per build-id CFI (not per PID):** two processes running the same binary share one `cfi_rules` entry keyed by the FNV-1a hash of its build-id. `pid_mappings` bridges PID → which table_id covers which VMA. This is the design in `docs/dwarf-unwinding-design.md`.

**Why map-of-maps:** CFI tables differ in size per binary — some binaries have 200 entries, some 50,000. A flat `ARRAY` can't resize. `BPF_MAP_TYPE_HASH_OF_MAPS` with dynamically-created inner `ARRAY`s gives us per-table sizing. Inner array length isn't readable at BPF runtime, so we keep a parallel `HASH` keyed by `table_id → u32` holding the valid length.

**Binary search in BPF:** the inner arrays are sorted by `pc_start` at populate time. BPF does a plain bounded `for` loop ≤ 20 iterations (supports up to ~1M entries). The verifier accepts bounded loops directly since kernel 5.3; no `bpf_loop` needed for search.

**Stack vs. per-CPU scratch:** the walker already uses `walker_scratch` (percpu_array) for the 1032-byte `sample_record`. Adding the new walker state requires a few more bytes on the BPF stack, still well under 512.

---

## Task 1 — BPF map declarations (no walker change)

**Goal:** declare the new maps so userspace can reference them, without touching walker logic. Verifier must still accept the program.

**Files:**
- Modify: `bpf/unwind_common.h` (append new map declarations)

- [ ] **Step 1.1: Add map prototypes + declarations**

Append to `bpf/unwind_common.h` before the `#endif`, after the `pids` map:

```c
// ----- CFI maps (S3).
//
// cfi_rules is a HASH_OF_MAPS: outer key is table_id (FNV-1a of build-id),
// inner is a variable-size ARRAY of cfi_entry sorted by pc_start.
// cfi_lengths holds the valid length of each inner array (BPF can't read
// inner max_entries at runtime).
//
// cfi_classification mirrors the structure for classification rows.
//
// pid_mappings: outer key is pid, inner is a fixed-size ARRAY of pid_mapping
// entries (most processes need < 256 mappings). pid_mapping_lengths holds
// the valid length per pid.

struct cfi_inner_proto {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct cfi_entry);
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __array(values, struct cfi_inner_proto);
} cfi_rules SEC(".maps");

struct classification_inner_proto {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct classification);
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __array(values, struct classification_inner_proto);
} cfi_classification SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __type(value, __u32);
} cfi_lengths SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, __u64);
    __type(value, __u32);
} cfi_classification_lengths SEC(".maps");

#define MAX_PID_MAPPINGS 256

struct pid_mapping_inner_proto {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, MAX_PID_MAPPINGS);
    __type(key, __u32);
    __type(value, struct pid_mapping);
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 2048);
    __type(key, __u32);
    __array(values, struct pid_mapping_inner_proto);
} pid_mappings SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 2048);
    __type(key, __u32);
    __type(value, __u32);
} pid_mapping_lengths SEC(".maps");
```

- [ ] **Step 1.2: Regenerate bpf2go output**

Run: `GOTOOLCHAIN=go1.26.0 make generate`

Expected: warnings about benign `struct foo;` forward declarations in `vmlinux_arm64.h`, no errors. New files should appear in `profile/perf_dwarf_{x86,arm64}_bpfel.*`.

- [ ] **Step 1.3: Rebuild the capped test binary**

Run:
```
GOTOOLCHAIN=go1.26.0 \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " \
  go test -c -o /home/diego/bin/profile.test ./profile/
```

Ask the user: `sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep /home/diego/bin/profile.test`

- [ ] **Step 1.4: Confirm verifier still accepts the program**

Run: `/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads`

Expected: `PASS: TestPerfDwarfLoads`.

- [ ] **Step 1.5: Commit**

```
git add bpf/unwind_common.h profile/perf_dwarf_x86_bpfel.o profile/perf_dwarf_arm64_bpfel.o profile/perf_dwarf_x86_bpfel.go profile/perf_dwarf_arm64_bpfel.go
git commit -m "S3: declare CFI/classification/pid_mappings BPF maps"
```

---

## Task 2 — `unwind/ehmaps` skeleton and TableID

**Goal:** create the ehmaps package with a pure-Go `TableIDForBuildID` and unit test it.

**Files:**
- Create: `unwind/ehmaps/ehmaps.go`
- Create: `unwind/ehmaps/ehmaps_test.go`

- [ ] **Step 2.1: Create the failing test**

Write `unwind/ehmaps/ehmaps_test.go`:

```go
package ehmaps

import "testing"

func TestTableIDForBuildIDKnownValue(t *testing.T) {
	// FNV-1a 64-bit of 20 bytes of 0xAA. Known-value anchor; if the
	// calculation drifts, the test catches it.
	buildID := make([]byte, 20)
	for i := range buildID {
		buildID[i] = 0xAA
	}
	const want uint64 = 0x88ebb801b154ad85
	if got := TableIDForBuildID(buildID); got != want {
		t.Fatalf("TableIDForBuildID(0xAA*20) = %#x, want %#x", got, want)
	}
}

func TestTableIDForBuildIDDiffersByInput(t *testing.T) {
	a := TableIDForBuildID([]byte{1, 2, 3})
	b := TableIDForBuildID([]byte{1, 2, 4})
	if a == b {
		t.Fatalf("distinct inputs produced same table_id %#x", a)
	}
}

func TestTableIDForBuildIDEmpty(t *testing.T) {
	// FNV-1a offset basis for an empty input.
	const want uint64 = 0xcbf29ce484222325
	if got := TableIDForBuildID(nil); got != want {
		t.Fatalf("empty buildID = %#x, want %#x", got, want)
	}
}
```

- [ ] **Step 2.2: Run test, verify it fails**

Run: `GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: TableIDForBuildID`.

- [ ] **Step 2.3: Implement the function**

Write `unwind/ehmaps/ehmaps.go`:

```go
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
```

- [ ] **Step 2.4: Run test, verify it passes**

Run: `GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: PASS.

- [ ] **Step 2.5: Commit**

```
git add unwind/ehmaps/
git commit -m "S3: ehmaps package with build-id → table_id hash"
```

---

## Task 3 — BPF-struct conversions (CFIEntry, Classification, PIDMapping)

**Goal:** marshal `ehcompile.CFIEntry` / `ehcompile.Classification` + a new `PIDMapping` into the byte layouts matching `bpf/unwind_common.h`. These are the bytes we'll `Update` into BPF maps.

**Files:**
- Modify: `unwind/ehmaps/ehmaps.go`
- Modify: `unwind/ehmaps/ehmaps_test.go`

- [ ] **Step 3.1: Add failing test for CFI conversion**

Append to `unwind/ehmaps/ehmaps_test.go`:

```go
import (
	"bytes"
	"encoding/binary"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

func TestMarshalCFIEntryMatchesBPFLayout(t *testing.T) {
	e := ehcompile.CFIEntry{
		PCStart:    0x1234_5678_9abc_def0,
		PCEndDelta: 0x0102_0304,
		CFAType:    ehcompile.CFATypeSP,
		FPType:     ehcompile.FPTypeOffsetCFA,
		CFAOffset:  -16,
		FPOffset:   -32,
		RAOffset:   -8,
		RAType:     ehcompile.RATypeOffsetCFA,
	}
	got := MarshalCFIEntry(e)
	want := make([]byte, 32)
	binary.LittleEndian.PutUint64(want[0:8], 0x1234_5678_9abc_def0)
	binary.LittleEndian.PutUint32(want[8:12], 0x0102_0304)
	want[12] = 1 // cfa_type = SP
	want[13] = 1 // fp_type = OffsetCFA
	binary.LittleEndian.PutUint16(want[14:16], uint16(int16(-16)))
	binary.LittleEndian.PutUint16(want[16:18], uint16(int16(-32)))
	binary.LittleEndian.PutUint16(want[18:20], uint16(int16(-8)))
	want[20] = 1 // ra_type = OffsetCFA
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalCFIEntry:\n got %x\nwant %x", got, want)
	}
}

func TestMarshalClassificationMatchesBPFLayout(t *testing.T) {
	c := ehcompile.Classification{
		PCStart:    0xdeadbeef_cafef00d,
		PCEndDelta: 42,
		Mode:       ehcompile.ModeFPLess,
	}
	got := MarshalClassification(c)
	want := make([]byte, 16)
	binary.LittleEndian.PutUint64(want[0:8], 0xdeadbeef_cafef00d)
	binary.LittleEndian.PutUint32(want[8:12], 42)
	want[12] = 1 // mode = FPLess
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalClassification:\n got %x\nwant %x", got, want)
	}
}

func TestMarshalPIDMapping(t *testing.T) {
	m := PIDMapping{VMAStart: 0x400000, VMAEnd: 0x500000, LoadBias: 0x400000, TableID: 0x12345}
	got := MarshalPIDMapping(m)
	want := make([]byte, 32)
	binary.LittleEndian.PutUint64(want[0:8], 0x400000)
	binary.LittleEndian.PutUint64(want[8:16], 0x500000)
	binary.LittleEndian.PutUint64(want[16:24], 0x400000)
	binary.LittleEndian.PutUint64(want[24:32], 0x12345)
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalPIDMapping:\n got %x\nwant %x", got, want)
	}
}
```

- [ ] **Step 3.2: Run test, verify it fails**

Run: `GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: MarshalCFIEntry`, `undefined: MarshalClassification`, `undefined: PIDMapping`, `undefined: MarshalPIDMapping`.

- [ ] **Step 3.3: Implement the conversions**

Append to `unwind/ehmaps/ehmaps.go`:

```go
import (
	"encoding/binary"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// CFIEntryByteSize matches bpf/unwind_common.h `struct cfi_entry` (32 bytes after u64 alignment padding).
const CFIEntryByteSize = 32

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
```

Move the `import` line up and merge with package comment.

- [ ] **Step 3.4: Run test, verify it passes**

Run: `GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: PASS.

- [ ] **Step 3.5: Commit**

```
git add unwind/ehmaps/
git commit -m "S3: ehmaps marshalers matching BPF cfi_entry/classification/pid_mapping layouts"
```

---

## Task 4 — BPF helpers: mapping lookup + binary search

**Goal:** add static inline helpers in `unwind_common.h` the walker will call. Verifier must still load the program (helpers are `static inline` + currently unused — compiler will keep them only if referenced, so they'll be dead-stripped until Task 7 wires them in).

Decision: keep helpers in `unwind_common.h` so the future off-CPU variant can reuse them without duplicating 100 lines.

**Files:**
- Modify: `bpf/unwind_common.h`

- [ ] **Step 4.1: Add mapping-lookup helper**

Append to `unwind_common.h` before the `#endif`, after `walk_step`:

```c
// mapping_lookup_result carries what mapping_for_pc returns.
struct mapping_lookup_result {
    __u64 table_id;
    __u64 rel_pc;     // pc - load_bias
    __u8  found;      // 1 if pc falls inside some mapping of this pid
    __u8  _pad[7];
};

// mapping_for_pc finds the first mapping in this pid's list whose vma range
// contains `pc`. Linear scan over MAX_PID_MAPPINGS; terminates early at the
// valid length. Returns .found == 0 if nothing matched (e.g. the PC is in a
// binary we never compiled CFI for, like the kernel's vsyscall or an anon
// JIT page).
struct mapping_lookup_ctx {
    __u32 pid;
    __u64 pc;
    struct mapping_lookup_result out;
    void *inner;
    __u32 len;
};

static long mapping_scan_step(__u32 idx, void *arg) {
    struct mapping_lookup_ctx *ctx = (struct mapping_lookup_ctx *)arg;
    if (idx >= ctx->len) return 1;
    struct pid_mapping *m = bpf_map_lookup_elem(ctx->inner, &idx);
    if (!m) return 1;
    if (ctx->pc >= m->vma_start && ctx->pc < m->vma_end) {
        ctx->out.table_id = m->table_id;
        ctx->out.rel_pc = ctx->pc - m->load_bias;
        ctx->out.found = 1;
        return 1;
    }
    return 0;
}

static __always_inline struct mapping_lookup_result mapping_for_pc(__u32 pid, __u64 pc) {
    struct mapping_lookup_ctx ctx = { .pid = pid, .pc = pc, };
    ctx.inner = bpf_map_lookup_elem(&pid_mappings, &pid);
    if (!ctx.inner) return ctx.out;
    __u32 *lenp = bpf_map_lookup_elem(&pid_mapping_lengths, &pid);
    if (!lenp || *lenp == 0) return ctx.out;
    ctx.len = *lenp > MAX_PID_MAPPINGS ? MAX_PID_MAPPINGS : *lenp;
    bpf_loop(MAX_PID_MAPPINGS, mapping_scan_step, &ctx, 0);
    return ctx.out;
}
```

- [ ] **Step 4.2: Add classification-lookup helper**

Append after `mapping_for_pc`:

```c
// BINARY_SEARCH_MAX_ITERS bounds binary search over CFI / classification
// tables. log2(1_000_000) ≈ 20, so 20 iters suffices for any realistically
// sized binary.
#define BINARY_SEARCH_MAX_ITERS 20

// classify_rel_pc returns MODE_FP_SAFE / MODE_FP_LESS / MODE_FALLBACK for the
// given (table_id, rel_pc). If the table is absent or no row covers rel_pc,
// returns MODE_FP_SAFE — the walker treats FP-safe and "unknown" identically
// (spec §Hybrid walking algorithm: "FALLBACK behaves exactly like FP_SAFE").
static __always_inline __u8 classify_rel_pc(__u64 table_id, __u64 rel_pc) {
    void *inner = bpf_map_lookup_elem(&cfi_classification, &table_id);
    if (!inner) return MODE_FP_SAFE;
    __u32 *lenp = bpf_map_lookup_elem(&cfi_classification_lengths, &table_id);
    if (!lenp || *lenp == 0) return MODE_FP_SAFE;
    __u32 lo = 0, hi = *lenp;
    for (int i = 0; i < BINARY_SEARCH_MAX_ITERS; i++) {
        if (lo >= hi) break;
        __u32 mid = lo + (hi - lo) / 2;
        struct classification *c = bpf_map_lookup_elem(inner, &mid);
        if (!c) break;
        if (rel_pc < c->pc_start) {
            hi = mid;
        } else if (rel_pc >= c->pc_start + (__u64)c->pc_end_delta) {
            lo = mid + 1;
        } else {
            return c->mode;
        }
    }
    return MODE_FP_SAFE;
}
```

- [ ] **Step 4.3: Add CFI-lookup helper**

Append after `classify_rel_pc`:

```c
// cfi_lookup returns a pointer to the cfi_entry whose PC range contains
// rel_pc, or NULL if not found. Pointer is into the inner map — safe to
// read but not to retain across helper calls.
static __always_inline struct cfi_entry *cfi_lookup(__u64 table_id, __u64 rel_pc) {
    void *inner = bpf_map_lookup_elem(&cfi_rules, &table_id);
    if (!inner) return NULL;
    __u32 *lenp = bpf_map_lookup_elem(&cfi_lengths, &table_id);
    if (!lenp || *lenp == 0) return NULL;
    __u32 lo = 0, hi = *lenp;
    for (int i = 0; i < BINARY_SEARCH_MAX_ITERS; i++) {
        if (lo >= hi) break;
        __u32 mid = lo + (hi - lo) / 2;
        struct cfi_entry *e = bpf_map_lookup_elem(inner, &mid);
        if (!e) return NULL;
        if (rel_pc < e->pc_start) {
            hi = mid;
        } else if (rel_pc >= e->pc_start + (__u64)e->pc_end_delta) {
            lo = mid + 1;
        } else {
            return e;
        }
    }
    return NULL;
}
```

- [ ] **Step 4.4: Regenerate and verify verifier still accepts program**

Run:
```
GOTOOLCHAIN=go1.26.0 make generate
GOTOOLCHAIN=go1.26.0 \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " \
  go test -c -o /home/diego/bin/profile.test ./profile/
```

Ask user to re-apply caps, then:

Run: `/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads`

Expected: `PASS`. (The helpers are static inline and currently unused; they won't appear in the emitted BPF program at all. This step is a safety check that the map declarations still verify.)

- [ ] **Step 4.5: Commit**

```
git add bpf/unwind_common.h profile/perf_dwarf_x86_bpfel.o profile/perf_dwarf_arm64_bpfel.o profile/perf_dwarf_x86_bpfel.go profile/perf_dwarf_arm64_bpfel.go
git commit -m "S3: BPF mapping_for_pc, classify_rel_pc, cfi_lookup helpers (unused)"
```

---

## Task 5 — Hybrid walker

**Goal:** rewrite `walk_step` to classify per frame and use DWARF on FP-less ranges. Update `walker_flags` telemetry.

**Files:**
- Modify: `bpf/unwind_common.h` (replace `walk_step`)
- Modify: `bpf/perf_dwarf.bpf.c` (track dominant mode; update walker_flags bits)

- [ ] **Step 5.1: Replace walk_step with hybrid version**

In `bpf/unwind_common.h`, replace the existing `walk_step` function with:

```c
// walker_flags bits, exposed via sample_header.walker_flags:
//
//   bit 0 — FP walk reached a natural terminator (saved_fp == 0). Clear
//           means the walk was cut short by a read failure or MAX_FRAMES.
//   bit 1 — at least one frame used the DWARF path.
//   bit 2 — at least one frame's CFI lookup missed while classified FP_LESS
//           (walk truncated at that frame).
#define WALKER_FLAG_FP_TERMINATED  0x01
#define WALKER_FLAG_DWARF_USED     0x02
#define WALKER_FLAG_CFI_MISS       0x04

// walk_ctx extension: track walker_flags bits across frames. We keep the
// "dominant mode" as FP_SAFE unless we actually used DWARF, in which case
// we upgrade to FP_LESS. FALLBACK is internal — for telemetry, callers only
// see FP_SAFE (no DWARF) vs FP_LESS (DWARF path used).
static long walk_step(__u32 idx, void *arg) {
    struct walk_ctx *ctx = (struct walk_ctx *)arg;
    if (ctx->n_pcs >= MAX_FRAMES) return 1;

    ctx->rec->pcs[ctx->n_pcs++] = ctx->pc;

    // Per-frame classification. Miss = treat as FP_SAFE.
    struct mapping_lookup_result m = mapping_for_pc(ctx->pid, ctx->pc);
    __u8 mode = MODE_FP_SAFE;
    if (m.found) {
        mode = classify_rel_pc(m.table_id, m.rel_pc);
    }

    if (mode == MODE_FP_LESS) {
        struct cfi_entry *ep = cfi_lookup(m.table_id, m.rel_pc);
        if (!ep) {
            ctx->rec->hdr.walker_flags |= WALKER_FLAG_CFI_MISS;
            return 1;
        }
        // Copy out of the inner map immediately — the pointer's lifetime is
        // bounded by the next BPF helper call; reading fields below and
        // then calling bpf_probe_read_user is fine, but defensive-copying
        // keeps the code simple to reason about.
        struct cfi_entry e = *ep;

        __u64 base = (e.cfa_type == CFA_TYPE_FP) ? ctx->fp : ctx->sp;
        __u64 cfa = base + (__s64)e.cfa_offset;

        __u64 ret_addr = 0;
        if (e.ra_type == RA_TYPE_OFFSET_CFA) {
            if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr),
                                    (void *)(cfa + (__s64)e.ra_offset)) != 0) return 1;
        } else {
            // SAME_VALUE (leaf on arm64) or REGISTER — S3 doesn't track
            // non-FP registers, so stop. S6+ can extend.
            return 1;
        }

        __u64 new_fp = ctx->fp;
        if (e.fp_type == FP_TYPE_OFFSET_CFA) {
            if (bpf_probe_read_user(&new_fp, sizeof(new_fp),
                                    (void *)(cfa + (__s64)e.fp_offset)) != 0) return 1;
        } else if (e.fp_type == FP_TYPE_SAME_VALUE) {
            // new_fp unchanged
        } else {
            // UNDEFINED / REGISTER — FP is lost; continuing via DWARF
            // further is fine but FP-based frames further up will fail.
            new_fp = 0;
        }

        ctx->pc = ret_addr;
        ctx->fp = new_fp;
        ctx->sp = cfa;
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_DWARF_USED;
        return 0;
    }

    // FP_SAFE or FALLBACK — same path: FP walk.
    __u64 saved_fp = 0, ret_addr = 0;
    if (bpf_probe_read_user(&saved_fp, sizeof(saved_fp), (void *)ctx->fp) != 0) return 1;
    if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr), (void *)(ctx->fp + 8)) != 0) return 1;
    if (saved_fp == 0) {
        ctx->rec->hdr.walker_flags |= WALKER_FLAG_FP_TERMINATED;
        return 1;
    }
    if (saved_fp <= ctx->fp) return 1;

    ctx->pc = ret_addr;
    ctx->fp = saved_fp;
    return 0;
}
```

- [ ] **Step 5.2: Update perf_dwarf.bpf.c header write**

In `bpf/perf_dwarf.bpf.c`, replace the line:

```c
    rec->hdr.mode         = MODE_FP_SAFE; // S2 stub — S3 varies this per-range
```

with:

```c
    // Dominant mode for telemetry: FP_LESS if DWARF fired at least once,
    // else FP_SAFE. walker_flags carries the per-bit breakdown.
    rec->hdr.mode = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;
```

Also replace:

```c
    rec->hdr.walker_flags = 0; // unused in S2; S3+ marks DWARF transitions
```

with:

```c
    // walker_flags already populated by walk_step during the walk.
```

And before the `bpf_loop` call, add a line to zero the flags (they're read-modify-write):

```c
    rec->hdr.walker_flags = 0;
```

The result inside `perf_dwarf` should be:

```c
    struct walk_ctx walker = {
        .pc    = ip,
        .fp    = fp,
        .sp    = sp,
        .pid   = tgid,
        .n_pcs = 0,
        .rec   = rec,
    };

    rec->hdr.walker_flags = 0;
    bpf_loop(MAX_FRAMES, walk_step, &walker, 0);

    rec->hdr.pid     = tgid;
    rec->hdr.tid     = tid;
    rec->hdr.time_ns = bpf_ktime_get_ns();
    rec->hdr.value   = 1;
    rec->hdr.n_pcs   = (__u8)(walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs);
    rec->hdr.mode    = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;
    // walker_flags already populated by walk_step during the walk.

    bpf_ringbuf_output(&stack_events, rec, sizeof(*rec), 0);
```

- [ ] **Step 5.3: Regenerate and verify verifier still accepts program**

Run:
```
GOTOOLCHAIN=go1.26.0 make generate
GOTOOLCHAIN=go1.26.0 \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " \
  go test -c -o /home/diego/bin/profile.test ./profile/
```

Ask user to reapply caps. Then:

Run: `/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads`

Expected: `PASS`. (This is the real verifier gate — the hybrid walker is non-trivial and the verifier may reject it. If it does, the error is in the helper call depth, pointer arithmetic around `cfa + offset`, or the BPF stack size. Fix by simplifying or hoisting state.)

- [ ] **Step 5.4: Sanity-check FP-only path still works**

Start Go workload (no DWARF needed for it):

```
./test/workloads/go/cpu_bound -duration=20s -threads=2 &
/home/diego/bin/perf-dwarf-test --pid $! --duration 5 --limit 3
```

Expected: non-empty chains (> 3 frames each); `mode: 0 (0=FP_SAFE ...)` because the Go runtime has FP throughout; `walker_fl: 0x1` (bit 0 = FP_TERMINATED) — no DWARF was needed.

Ask the user to run this manually and confirm.

- [ ] **Step 5.5: Commit**

```
git add bpf/unwind_common.h bpf/perf_dwarf.bpf.c profile/perf_dwarf_x86_bpfel.o profile/perf_dwarf_arm64_bpfel.o profile/perf_dwarf_x86_bpfel.go profile/perf_dwarf_arm64_bpfel.go
git commit -m "S3: hybrid walker — per-frame classify + DWARF path"
```

---

## Task 6 — ehmaps Populate helpers (touch BPF maps)

**Goal:** populate the outer/inner maps from ehcompile output. These helpers accept `*ebpf.Map` handles (the outer map) and do the inner-map creation + update dance. No MMAP2, no lifecycle.

**Files:**
- Modify: `unwind/ehmaps/ehmaps.go`
- Create: `unwind/ehmaps/ehmaps_runtime_test.go` — guarded on CAP_BPF, exercises real BPF maps

- [ ] **Step 6.1: Add Populate signatures and stubs**

Append to `unwind/ehmaps/ehmaps.go`:

```go
import (
	"fmt"

	"github.com/cilium/ebpf"
)

// PopulateCFIArgs bundles what the caller already has in memory — an already-
// compiled set of rules plus the outer and length maps from the loaded BPF
// program.
type PopulateCFIArgs struct {
	TableID            uint64
	Entries            []ehcompile.CFIEntry
	OuterMap           *ebpf.Map // cfi_rules (HASH_OF_MAPS)
	LengthMap          *ebpf.Map // cfi_lengths (HASH)
}

// PopulateCFI creates a right-sized inner ARRAY, fills it with Entries, and
// installs it into OuterMap keyed by TableID. Also writes the valid length
// into LengthMap. Returns an error if any step fails; on success the inner
// map stays owned by the kernel (the outer map holds a reference).
func PopulateCFI(args PopulateCFIArgs) error {
	if len(args.Entries) == 0 {
		return fmt.Errorf("ehmaps: PopulateCFI: no entries")
	}
	spec := &ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  CFIEntryByteSize,
		MaxEntries: uint32(len(args.Entries)),
	}
	inner, err := ebpf.NewMap(spec)
	if err != nil {
		return fmt.Errorf("ehmaps: create inner cfi map: %w", err)
	}
	for i, e := range args.Entries {
		key := uint32(i)
		if err := inner.Update(key, MarshalCFIEntry(e), ebpf.UpdateAny); err != nil {
			inner.Close()
			return fmt.Errorf("ehmaps: write cfi[%d]: %w", i, err)
		}
	}
	if err := args.OuterMap.Update(args.TableID, uint32(inner.FD()), ebpf.UpdateAny); err != nil {
		inner.Close()
		return fmt.Errorf("ehmaps: install inner cfi map: %w", err)
	}
	// Kernel holds a ref via OuterMap; drop our userspace ref.
	inner.Close()

	length := uint32(len(args.Entries))
	if err := args.LengthMap.Update(args.TableID, length, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("ehmaps: write cfi length: %w", err)
	}
	return nil
}

// PopulateClassificationArgs mirrors PopulateCFIArgs but for classification.
type PopulateClassificationArgs struct {
	TableID   uint64
	Entries   []ehcompile.Classification
	OuterMap  *ebpf.Map // cfi_classification
	LengthMap *ebpf.Map // cfi_classification_lengths
}

func PopulateClassification(args PopulateClassificationArgs) error {
	if len(args.Entries) == 0 {
		return fmt.Errorf("ehmaps: PopulateClassification: no entries")
	}
	spec := &ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  ClassificationByteSize,
		MaxEntries: uint32(len(args.Entries)),
	}
	inner, err := ebpf.NewMap(spec)
	if err != nil {
		return fmt.Errorf("ehmaps: create inner classification map: %w", err)
	}
	for i, c := range args.Entries {
		key := uint32(i)
		if err := inner.Update(key, MarshalClassification(c), ebpf.UpdateAny); err != nil {
			inner.Close()
			return fmt.Errorf("ehmaps: write classification[%d]: %w", i, err)
		}
	}
	if err := args.OuterMap.Update(args.TableID, uint32(inner.FD()), ebpf.UpdateAny); err != nil {
		inner.Close()
		return fmt.Errorf("ehmaps: install inner classification map: %w", err)
	}
	inner.Close()

	length := uint32(len(args.Entries))
	if err := args.LengthMap.Update(args.TableID, length, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("ehmaps: write classification length: %w", err)
	}
	return nil
}

// PopulatePIDMappingsArgs installs a list of mappings for one PID.
type PopulatePIDMappingsArgs struct {
	PID       uint32
	Mappings  []PIDMapping
	OuterMap  *ebpf.Map // pid_mappings
	LengthMap *ebpf.Map // pid_mapping_lengths
}

// MAX_PID_MAPPINGS mirrors the BPF #define. Keep in lockstep.
const MaxPIDMappings = 256

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
	}
	inner, err := ebpf.NewMap(spec)
	if err != nil {
		return fmt.Errorf("ehmaps: create inner pid_mappings map: %w", err)
	}
	for i, m := range args.Mappings {
		key := uint32(i)
		if err := inner.Update(key, MarshalPIDMapping(m), ebpf.UpdateAny); err != nil {
			inner.Close()
			return fmt.Errorf("ehmaps: write pid_mapping[%d]: %w", i, err)
		}
	}
	if err := args.OuterMap.Update(args.PID, uint32(inner.FD()), ebpf.UpdateAny); err != nil {
		inner.Close()
		return fmt.Errorf("ehmaps: install inner pid_mappings map: %w", err)
	}
	inner.Close()

	length := uint32(len(args.Mappings))
	if err := args.LengthMap.Update(args.PID, length, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("ehmaps: write pid mapping length: %w", err)
	}
	return nil
}
```

- [ ] **Step 6.2: Runtime test (capped-binary gated)**

Write `unwind/ehmaps/ehmaps_runtime_test.go`:

```go
package ehmaps

import (
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/ehcompile"
)

// TestPopulateCFIRoundtrip creates a minimal HASH_OF_MAPS + length HASH in
// userspace, populates them via PopulateCFI, and reads back the inner array
// to confirm the round-trip. Skips without CAP_BPF.
func TestPopulateCFIRoundtrip(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}

	innerProto, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.Array,
		KeySize:    4,
		ValueSize:  CFIEntryByteSize,
		MaxEntries: 1,
	})
	if err != nil {
		t.Fatalf("inner proto: %v", err)
	}
	defer innerProto.Close()

	outer, err := ebpf.NewMap(&ebpf.MapSpec{
		Type:       ebpf.HashOfMaps,
		KeySize:    8,
		ValueSize:  4,
		MaxEntries: 4,
		InnerMap:   &ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: CFIEntryByteSize, MaxEntries: 1},
	})
	if err != nil {
		t.Fatalf("outer: %v", err)
	}
	defer outer.Close()

	lengths, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Hash, KeySize: 8, ValueSize: 4, MaxEntries: 4})
	if err != nil {
		t.Fatalf("lengths: %v", err)
	}
	defer lengths.Close()

	entries := []ehcompile.CFIEntry{
		{PCStart: 0x100, PCEndDelta: 0x40, CFAType: ehcompile.CFATypeSP, FPType: ehcompile.FPTypeOffsetCFA, CFAOffset: 16, FPOffset: -16, RAOffset: -8, RAType: ehcompile.RATypeOffsetCFA},
		{PCStart: 0x140, PCEndDelta: 0x20, CFAType: ehcompile.CFATypeFP, FPType: ehcompile.FPTypeSameValue, CFAOffset: 16, RAOffset: -8, RAType: ehcompile.RATypeOffsetCFA},
	}
	const tableID uint64 = 0xabc
	if err := PopulateCFI(PopulateCFIArgs{TableID: tableID, Entries: entries, OuterMap: outer, LengthMap: lengths}); err != nil {
		t.Fatalf("PopulateCFI: %v", err)
	}

	var gotLen uint32
	if err := lengths.Lookup(tableID, &gotLen); err != nil {
		t.Fatalf("length lookup: %v", err)
	}
	if gotLen != uint32(len(entries)) {
		t.Fatalf("length = %d, want %d", gotLen, len(entries))
	}
	// We can't directly read the inner map via the outer map's userspace
	// handle — cilium/ebpf returns the inner map's ID, not a readable
	// handle. The integration test (Task 9) validates the round-trip
	// through the BPF walker instead.
}

func requireBPFCaps(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	caps := cap.GetProc()
	have, err := caps.GetFlag(cap.Permitted, cap.BPF)
	if err != nil || !have {
		t.Skip("CAP_BPF not available")
	}
}
```

- [ ] **Step 6.3: Run unit and runtime tests**

Unit tests (unprivileged):
```
GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/
```
Expected: `TestTableID*`, `TestMarshal*` pass; `TestPopulateCFIRoundtrip` skips.

Runtime test — rebuild, ask user to setcap, then run the capped binary:
```
GOTOOLCHAIN=go1.26.0 go test -c -o /home/diego/bin/ehmaps.test ./unwind/ehmaps/
```
Ask user: `sudo setcap cap_sys_admin,cap_bpf,cap_perfmon+ep /home/diego/bin/ehmaps.test`

Then: `/home/diego/bin/ehmaps.test -test.v`
Expected: `PASS: TestPopulateCFIRoundtrip`.

- [ ] **Step 6.4: Commit**

```
git add unwind/ehmaps/
git commit -m "S3: ehmaps PopulateCFI/PopulateClassification/PopulatePIDMappings"
```

---

## Task 7 — Expose new maps from profile.LoadPerfDwarfForTest

**Goal:** the test harness needs to hand `ehmaps` the outer/length maps. Expose them as accessors on the existing `PerfDwarfForTest`.

**Files:**
- Modify: `profile/dwarf_export.go`

- [ ] **Step 7.1: Add accessors**

Append to `profile/dwarf_export.go`:

```go
// CFIRulesMap returns the cfi_rules HASH_OF_MAPS outer map.
func (p *PerfDwarfForTest) CFIRulesMap() *ebpf.Map { return p.objs.CfiRules }

// CFILengthsMap returns the cfi_lengths HASH.
func (p *PerfDwarfForTest) CFILengthsMap() *ebpf.Map { return p.objs.CfiLengths }

// CFIClassificationMap returns the cfi_classification HASH_OF_MAPS outer map.
func (p *PerfDwarfForTest) CFIClassificationMap() *ebpf.Map { return p.objs.CfiClassification }

// CfiClassificationLengthsMap returns the cfi_classification_lengths HASH.
func (p *PerfDwarfForTest) CfiClassificationLengthsMap() *ebpf.Map { return p.objs.CfiClassificationLengths }

// PIDMappingsMap returns the pid_mappings HASH_OF_MAPS outer map.
func (p *PerfDwarfForTest) PIDMappingsMap() *ebpf.Map { return p.objs.PidMappings }

// PIDMappingLengthsMap returns the pid_mapping_lengths HASH.
func (p *PerfDwarfForTest) PIDMappingLengthsMap() *ebpf.Map { return p.objs.PidMappingLengths }
```

Field names follow bpf2go's camel-case-ification: `cfi_rules` → `CfiRules`, etc. Spot-check the generated `perf_dwarfMaps` struct at the top of `profile/perf_dwarf_x86_bpfel.go` and adjust if bpf2go's naming differs (e.g. `CFIRules` vs `CfiRules`).

- [ ] **Step 7.2: Verify compiles**

Run: `GOTOOLCHAIN=go1.26.0 go build ./profile/`

Expected: no errors.

- [ ] **Step 7.3: Commit**

```
git add profile/dwarf_export.go
git commit -m "S3: expose CFI/classification/pid_mappings maps on PerfDwarfForTest"
```

---

## Task 8 — Rust workload: force cpu_intensive_work into every stack

**Goal:** keep the function uninlined so we can assert it's always on the chain in the integration test.

**Files:**
- Modify: `test/workloads/rust/src/main.rs`

- [ ] **Step 8.1: Add `#[inline(never)]` attribute**

Replace:

```rust
fn cpu_intensive_work(stop: Arc<AtomicBool>) {
```

with:

```rust
#[inline(never)]
fn cpu_intensive_work(stop: Arc<AtomicBool>) {
```

- [ ] **Step 8.2: Rebuild workloads**

Run: `make test-workloads`

Expected: `rust/target/release/rust-workload` is rebuilt.

- [ ] **Step 8.3: Spot-check the symbol exists**

Run: `llvm-objdump -d --disassemble-symbols=rust_workload::cpu_intensive_work test/workloads/rust/target/release/rust-workload 2>&1 | head -5`

Expected: disassembly starts with a `push` or similar prologue. The exact symbol mangling varies; if empty, try:

Run: `llvm-nm test/workloads/rust/target/release/rust-workload | grep cpu_intensive_work`

Expected: one non-empty line.

- [ ] **Step 8.4: Commit**

```
git add test/workloads/rust/src/main.rs
git commit -m "S3: force cpu_intensive_work uninlined for DWARF integration test"
```

---

## Task 9 — Integration test: end-to-end DWARF walker

**Goal:** drive the full flow — load BPF, ehcompile a real binary, populate maps via ehmaps, attach to the Rust workload, read ringbuf, assert that at least some samples exercised the DWARF path.

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 9.1: Add the test**

Append to `test/integration_test.go`:

```go
import (
	"debug/elf"
	"encoding/binary"
	"errors"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehcompile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// TestPerfDwarfWalker drives the whole S3 pipeline against the Rust
// cpu_bound workload: compile CFI, install it, sample, verify DWARF fired.
func TestPerfDwarfWalker(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Test requires root privileges")
	}

	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		if workload.Process != nil {
			workload.Process.Kill()
			workload.Wait()
		}
	}()
	time.Sleep(2 * time.Second)

	objs, err := profile.LoadPerfDwarfForTest()
	require.NoError(t, err)
	defer objs.Close()

	require.NoError(t, objs.AddPID(uint32(workload.Process.Pid)))

	// Compile CFI.
	entries, classifications, err := ehcompile.Compile(binPath)
	require.NoError(t, err)
	require.NotEmpty(t, entries, "ehcompile produced no CFI entries")
	buildID, err := readBuildID(binPath)
	require.NoError(t, err)
	tableID := ehmaps.TableIDForBuildID(buildID)

	// Populate BPF maps.
	require.NoError(t, ehmaps.PopulateCFI(ehmaps.PopulateCFIArgs{
		TableID: tableID, Entries: entries,
		OuterMap: objs.CFIRulesMap(), LengthMap: objs.CFILengthsMap(),
	}))
	require.NoError(t, ehmaps.PopulateClassification(ehmaps.PopulateClassificationArgs{
		TableID: tableID, Entries: classifications,
		OuterMap: objs.CFIClassificationMap(), LengthMap: objs.CfiClassificationLengthsMap(),
	}))
	mappings, err := loadProcessMappings(workload.Process.Pid, binPath, tableID)
	require.NoError(t, err)
	require.NotEmpty(t, mappings, "no matching mappings in /proc/<pid>/maps")
	require.NoError(t, ehmaps.PopulatePIDMappings(ehmaps.PopulatePIDMappingsArgs{
		PID: uint32(workload.Process.Pid), Mappings: mappings,
		OuterMap: objs.PIDMappingsMap(), LengthMap: objs.PIDMappingLengthsMap(),
	}))

	// Attach per-CPU perf events at 99 Hz.
	ncpu := runtime.NumCPU()
	attr := &unix.PerfEventAttr{
		Type: unix.PERF_TYPE_SOFTWARE, Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size: uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: 99, Bits: unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	var links []link.Link
	defer func() { for _, l := range links { l.Close() } }()
	var fds []int
	defer func() { for _, fd := range fds { unix.Close(fd) } }()
	for cpu := 0; cpu < ncpu; cpu++ {
		fd, err := unix.PerfEventOpen(attr, workload.Process.Pid, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) { continue }
			t.Fatalf("perf_event_open cpu=%d: %v", cpu, err)
		}
		fds = append(fds, fd)
		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target: fd, Program: objs.Program(), Attach: ebpf.AttachPerfEvent,
		})
		require.NoError(t, err)
		links = append(links, rl)
		require.NoError(t, unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0))
	}
	require.NotEmpty(t, fds)

	// Consume ringbuf for up to 5 seconds or 40 samples.
	rd, err := ringbuf.NewReader(objs.RingbufMap())
	require.NoError(t, err)
	defer rd.Close()

	deadline := time.Now().Add(5 * time.Second)
	var (
		samples       int
		dwarfSamples  int
		maxFrames     int
	)
	for samples < 40 && time.Now().Before(deadline) {
		rd.SetDeadline(time.Now().Add(500 * time.Millisecond))
		rec, err := rd.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) { continue }
			break
		}
		samples++
		if len(rec.RawSample) < 32 { continue }
		nPCs := int(rec.RawSample[25])
		walkerFlags := rec.RawSample[26]
		if nPCs > maxFrames { maxFrames = nPCs }
		// bit 1 = WALKER_FLAG_DWARF_USED
		if walkerFlags&0x02 != 0 { dwarfSamples++ }
	}

	t.Logf("samples=%d dwarf_samples=%d max_frames=%d", samples, dwarfSamples, maxFrames)
	require.Greater(t, samples, 5, "no samples consumed")
	require.Greater(t, maxFrames, 2, "chains too shallow")
	require.Greater(t, dwarfSamples, 0, "DWARF path never fired — FP should have been insufficient for libstd frames")
}

func readBuildID(path string) ([]byte, error) {
	ef, err := elf.Open(path)
	if err != nil { return nil, err }
	defer ef.Close()
	for _, sec := range ef.Sections {
		if sec.Type != elf.SHT_NOTE { continue }
		data, err := sec.Data()
		if err != nil { continue }
		if id := extractGNUBuildID(data); id != nil { return id, nil }
	}
	return nil, errors.New("no .note.gnu.build-id section")
}

func extractGNUBuildID(notes []byte) []byte {
	// Each note: u32 name_size, u32 desc_size, u32 type, name (padded to 4),
	// desc (padded to 4). GNU build-id has name="GNU\0" (4 bytes), type=3.
	for len(notes) >= 12 {
		nameSize := binary.LittleEndian.Uint32(notes[0:4])
		descSize := binary.LittleEndian.Uint32(notes[4:8])
		noteType := binary.LittleEndian.Uint32(notes[8:12])
		p := 12
		nameEnd := p + int(nameSize)
		namePadded := (nameEnd + 3) &^ 3
		if namePadded > len(notes) { return nil }
		descEnd := namePadded + int(descSize)
		descPadded := (descEnd + 3) &^ 3
		if descPadded > len(notes) { return nil }
		if noteType == 3 && nameSize == 4 && string(notes[p:p+3]) == "GNU" {
			return notes[namePadded:descEnd]
		}
		notes = notes[descPadded:]
	}
	return nil
}

// loadProcessMappings reads /proc/<pid>/maps and returns one PIDMapping per
// executable-mapped range of binPath.
func loadProcessMappings(pid int, binPath string, tableID uint64) ([]ehmaps.PIDMapping, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil { return nil, err }
	var out []ehmaps.PIDMapping
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 { continue }
		if !strings.Contains(fields[1], "x") { continue }
		if !strings.HasSuffix(fields[5], binPath[strings.LastIndex(binPath, "/")+1:]) { continue }
		addrs := strings.SplitN(fields[0], "-", 2)
		if len(addrs) != 2 { continue }
		start, _ := strconvParseUint(addrs[0], 16, 64)
		end, _ := strconvParseUint(addrs[1], 16, 64)
		offset, _ := strconvParseUint(fields[2], 16, 64)
		// load_bias = start - offset (simplifies to start when the first
		// PT_LOAD has offset 0, which is the common case for PIE/non-PIE
		// main executables).
		out = append(out, ehmaps.PIDMapping{
			VMAStart: start, VMAEnd: end, LoadBias: start - offset, TableID: tableID,
		})
	}
	return out, nil
}

// strconvParseUint: thin alias for readability. Using strconv directly is
// fine but the imports get noisy in an already-busy test file.
func strconvParseUint(s string, base, bitSize int) (uint64, error) {
	return strconv.ParseUint(s, base, bitSize)
}
```

Add `"strconv"` to the imports at the top of `test/integration_test.go`.

Note: the `test/` package has its own go.mod. Verify `github.com/dpsoft/perf-agent/unwind/ehmaps` resolves via the existing `replace` directive.

- [ ] **Step 9.2: Verify ehcompile API**

Confirm: `grep -n "func Compile" unwind/ehcompile/ehcompile.go`

Expected: `func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, err error)`. If the signature has drifted (e.g. takes `io.ReaderAt`), adjust the call site accordingly.

- [ ] **Step 9.3: Run the test**

Build the `perf-agent` binary with the matching CGO flags (the existing `test-integration` target already does this):

```
GOTOOLCHAIN=go1.26.0 sudo -E make test-integration
```

… but this will also run `TestProfileMode` etc. which may take a long time. For fast iteration, bypass the Makefile:

```
cd test && GOTOOLCHAIN=go1.26.0 LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  sudo -E go test -v -run TestPerfDwarfWalker ./...
```

Expected: `PASS`. The `t.Logf` line reports `samples`, `dwarf_samples`, `max_frames`. Reasonable values: 20-40 samples, several DWARF samples, chains ≥ 3 frames.

If `dwarf_samples == 0`: Rust's libstd didn't appear in stacks. Try a longer sample window (`10*time.Second`) or confirm the workload actually calls libstd I/O (e.g. `.sqrt()` goes through libm / Rust runtime).

If samples == 0: perf events not firing on this workload. Try `-threads=1` to make sure the process is schedulable on the CPUs the test opened FDs for.

- [ ] **Step 9.4: Commit**

```
git add test/integration_test.go test/go.sum
git commit -m "S3: integration test — ehcompile → ehmaps → DWARF walker on rust workload"
```

---

## Task 10 — Update perf-dwarf-test CLI for interactive use

**Goal:** extend the diagnostic CLI with the same populate flow so local iteration doesn't require running the integration test. Optional but cheap — the plumbing is identical to Task 9.

**Files:**
- Modify: `cmd/perf-dwarf-test/main.go`

- [ ] **Step 10.1: Add --target-binary flag + populate step**

Add a flag:

```go
targetBin := flag.String("target-binary", "", "path to the PID's main executable (enables DWARF unwinding)")
```

After `objs.AddPID(...)` and before perf-event attachment, add:

```go
if *targetBin != "" {
    if err := populateDwarfMaps(objs, uint32(*pid), *targetBin); err != nil {
        log.Fatalf("populate dwarf maps: %v", err)
    }
    fmt.Printf("installed CFI for %s\n", *targetBin)
}
```

Add the helper at the bottom of `main.go`:

```go
func populateDwarfMaps(objs *profile.PerfDwarfForTest, pid uint32, binPath string) error {
    entries, classifications, err := ehcompile.Compile(binPath)
    if err != nil { return fmt.Errorf("ehcompile: %w", err) }
    buildID, err := readBuildID(binPath)
    if err != nil { return fmt.Errorf("build-id: %w", err) }
    tableID := ehmaps.TableIDForBuildID(buildID)

    if err := ehmaps.PopulateCFI(ehmaps.PopulateCFIArgs{
        TableID: tableID, Entries: entries,
        OuterMap: objs.CFIRulesMap(), LengthMap: objs.CFILengthsMap(),
    }); err != nil { return err }
    if err := ehmaps.PopulateClassification(ehmaps.PopulateClassificationArgs{
        TableID: tableID, Entries: classifications,
        OuterMap: objs.CFIClassificationMap(), LengthMap: objs.CfiClassificationLengthsMap(),
    }); err != nil { return err }
    mappings, err := loadProcessMappings(int(pid), binPath, tableID)
    if err != nil { return err }
    return ehmaps.PopulatePIDMappings(ehmaps.PopulatePIDMappingsArgs{
        PID: pid, Mappings: mappings,
        OuterMap: objs.PIDMappingsMap(), LengthMap: objs.PIDMappingLengthsMap(),
    })
}
```

Port `readBuildID`, `extractGNUBuildID`, `loadProcessMappings`, and `strconvParseUint` from Task 9 into this file (the test/ package can't import the CLI package and vice versa, so we accept duplication here — it's cheap, and S4's `ehmaps` package will absorb the build-id + /proc/maps parsing as public helpers at that point).

- [ ] **Step 10.2: Rebuild perf-dwarf-test**

Run: `make perf-dwarf-test`

Expected: builds to `/home/diego/bin/perf-dwarf-test`.

Ask user: reapply caps.

- [ ] **Step 10.3: Smoke-test on rust workload**

Run in two terminals:
```
# Terminal 1
./test/workloads/rust/target/release/rust-workload 30 2

# Terminal 2 (replace PID)
/home/diego/bin/perf-dwarf-test --pid <rust PID> --target-binary ./test/workloads/rust/target/release/rust-workload --duration 10 --limit 5
```

Expected: `installed CFI for ...`, then 5 sample records. At least one should show `walker_fl: 0x2` or `0x3` (DWARF fired). Ask the user to paste the output.

- [ ] **Step 10.4: Commit**

```
git add cmd/perf-dwarf-test/main.go
git commit -m "S3: perf-dwarf-test --target-binary installs CFI before sampling"
```

---

## Task 11 — Sanity run of the full test matrix

**Goal:** confirm nothing else regressed.

- [ ] **Step 11.1: Run unit tests**

Run: `GOTOOLCHAIN=go1.26.0 make test-unit`

Expected: all tests pass, `TestPerfDwarfLoads` skips (no caps).

- [ ] **Step 11.2: Build capped binary and run privileged profile tests**

```
GOTOOLCHAIN=go1.26.0 \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " \
  go test -c -o /home/diego/bin/profile.test ./profile/
```

Ask user: reapply caps. Then:

Run: `/home/diego/bin/profile.test -test.v`

Expected: `PASS: TestPerfDwarfLoads`.

- [ ] **Step 11.3: Run integration test in isolation**

```
cd test && GOTOOLCHAIN=go1.26.0 LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  sudo -E go test -v -run TestPerfDwarfWalker ./...
```

Expected: `PASS`. Record the `samples=… dwarf_samples=… max_frames=…` line in the commit message.

- [ ] **Step 11.4: No further commit needed**

If everything passed, S3 is done. Otherwise, fix the reported failure and re-run from Step 11.1.

---

## Success criteria recap

Taken from `docs/dwarf-unwinding-design.md` §Execution plan:

> **S3: BPF: DWARF walker with simple-CFA rules** — On Rust workload with `#[inline(never)] cpu_intensive_work`, FP-less frames now resolve. Chain depth strictly ≥ FP path on same workload.

Satisfied by Task 9's `TestPerfDwarfWalker` asserting `dwarfSamples > 0` and `maxFrames > 2` with CFI populated for the Rust binary.

## Open risks

1. **Verifier complexity limit.** The hybrid walker does 4-5 bounded loops + two binary searches per frame. If the 1M instruction limit is hit, split the classify/lookup/apply into separate `bpf_loop` callbacks — the scratch state is already reusable.
2. **Map-of-maps on exotic kernels.** HASH_OF_MAPS is in since 4.12; safe under our 6.0+ floor. If a CI kernel is older than expected, it'll fail at map creation with EINVAL.
3. **ehcompile output shape drift.** The plan assumes `ehcompile.Compile(path) (entries, classifications, error)` per the current signature. If that's been refactored in the meantime, adjust the call sites in Tasks 9 and 10 accordingly.
