# GPU V2 Phase 3 — USDT ABI, Shim Core, eBPF Consumer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the wire between the two halves that already exist — a frozen USDT ABI, the vendor-neutral shim `core/` that emits it, and the eBPF consumer that turns it back into `gpu.Timeline` events — proven end to end by a stub emitter on a machine with no GPU.

**Architecture:** The producer is a C++ static archive (`shim/core/`) linked into a per-vendor `.so`; in this phase the only "vendor" is a stub that fabricates events. It emits fixed-size records through `.note.stapsdt` probes written with inline asm and gated on their semaphores. The consumer is a Go package (`gpuprobe/`) that discovers those probes with `internal/usdt`, attaches them in a single `uprobe_multi` BPF link keyed by cookies, reads batches out of a ringbuf, and feeds `gpu.Timeline`. `gpu.ProjectExecutions` and the `pprof` builder already exist and are not rewritten here.

**Tech Stack:** Go 1.26, `github.com/cilium/ebpf` v0.21 (`link.UprobeMulti`, `ringbuf`), C++17 for the shim (no CUDA, no CUPTI in this phase), clang/bpf2go for the BPF object, testify.

**Spec:** `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` (§4 invariants, §6 the USDT ABI, §6.1 shared runtime, §6.3 record layouts and *What the spike settled*, §7 canonical model and clock domain, §9 overhead control, §10 correlation and joins, §11 deployment and capabilities, §13 testing, §14 Phase 3)

## Global Constraints

- Go 1.26.0+ (go.mod declares `go 1.26.0`; CI pins `1.26`).
- CGO is required to build or test this repo. Export the block in "Build environment" before any `go` command.
- **Linux 6.6+ at runtime.** `uprobe_multi` is the only attach path that consumes USDT without `CAP_SYS_ADMIN` (§11). Never use `link.Uprobe` / the `perf_uprobe` PMU — it needs `CAP_SYS_ADMIN` and silently undoes Phase 1.
- Every `ebpf.Kprobe`-type `ProgramSpec` sets `KernelVersion` explicitly. A setcap'd binary is non-dumpable, so cilium/ebpf's fallback vDSO read through `/proc/self/mem` fails with a permission error that names neither capabilities nor uprobes (§11).
- Records are fixed-size, little-endian, naturally aligned, explicitly sized. No enums, no bitfields, no compiler-layout dependence (§6.3 Conventions).
- All `*_ns` on the wire are CPU-monotonic. The shim converts; the core never does (§7).
- Every launch and execution carries a unique non-zero correlation. Zero is not permitted (§6.1).
- No event is dropped silently. Every loss path increments a counter that reaches the snapshot (§9).
- The shim does no work when no consumer is attached: every emit is gated on its probe's semaphore (§6.1, measured in §9.1).
- Do not commit `CLAUDE.md`.
- No `Co-Authored-By` lines in commit messages.
- Use `git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit`.

## Build environment

```bash
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release
```

Run `go test` directly. Do not run `make test-unit` — it runs `go generate`.

## Prerequisites — resolve before Task 6

**1. clang and llvm are not installed on this box.** `bpf2go` needs `clang` to compile `bpf/*.bpf.c` and `llvm-strip` to strip the object. Tasks 1–5 do not need them; Task 6 onward cannot proceed without them.

```bash
sudo dnf install clang llvm
```

Verify:

```bash
which clang llvm-strip
```

Expected: both resolve. If they do not, stop — do not hand-write BPF instructions with `asm.Instructions` as a workaround. That was acceptable for a throwaway spike; it is not maintainable for the consumer, which reads stacks and reserves ringbuf space.

**2. Kernel 6.6+.** Verify `uname -r` reports ≥ 6.6. The dev box runs 6.19.

**3. Phase 2 is on `main`.** Verify:

```bash
grep -n "func NewTimeline" gpu/timeline.go && grep -n "func ProjectExecutions" gpu/projection.go
```

Expected: both exist.

## What is already built — do not rewrite

- `gpu/types.go` — canonical model (`GPUKernelLaunch`, `GPUKernelExec`, `GPUPCSample`, `GPUModule`, `CorrelationID{Backend,Value string}`, `LaunchContext{PID,TID,TimeNs,CPUStack []pp.Frame,Tags}`), `EventSink`, `Backend`.
- `gpu/timeline.go` — `NewTimeline(TimelineConfig)`, `EmitLaunch/EmitExec/EmitPCSample/EmitModule/EmitEvent`, `Snapshot()`.
- `gpu/sink.go` — `CountingSink` (token bucket + drop accounting).
- `gpu/projection.go` — `ProjectExecutions(Snapshot) []pprof.ProfileSample`, already implementing §8's frames-vs-labels split.
- `internal/usdt` — `ParseFile(path) ([]Probe, error)`; `Probe{Provider, Name, Args, Offset, HasSemaphore, SemaphoreOffset}` with addresses already resolved to file offsets.

## Design decisions this plan locks in

**Registers are pinned by the ABI, not chosen by the compiler.** The spike emitted a probe whose argument descriptor came back as `8@%rax 8@%rdx 8@%rcx` — whatever registers the compiler happened to have free. A consumer that reads `PT_REGS_PARM1..3` would silently read the wrong registers at a different call site or compiler version. The probe macro therefore binds its three arguments to explicit register variables (`rdi`, `rsi`, `rdx`) so the descriptor is constant and the BPF side is a fixed read. Task 2 asserts the descriptor.

**One multi-link, cookies distinguish probes.** All probe sites attach in a single `link.UprobeMulti` call with one `Cookies` entry per address; the BPF program reads `bpf_get_attach_cookie()` to learn which probe fired. This avoids one link and one program per probe kind.

**The stub is a first-class deliverable, not scaffolding.** It is what the phase gate runs, and it stays in the tree as the GPU-free test driver for Phases 4–6.

---

### Task 1: Freeze the record layouts

**Files:**
- Create: `shim/core/usdt_abi.h`
- Create: `internal/gpuabi/records.go`
- Test: `internal/gpuabi/records_test.go`
- Test: `shim/core/usdt_abi_test.c`

**Interfaces:**
- Produces (C): `struct gpu_launch_v1`, `struct gpu_exec_v1`, `struct gpu_module_load_v1`, `struct gpu_pc_sample_batch_v1`, `struct gpu_config_v1`, `struct gpu_dropped_v1`; `GPU_ABI_VERSION`.
- Produces (Go): `gpuabi.Launch`, `gpuabi.Exec`, `gpuabi.ModuleLoad`, `gpuabi.PCSample`, `gpuabi.Dropped`; `gpuabi.DecodeLaunch([]byte) (Launch, error)` and one per record; `gpuabi.SizeLaunch` etc. as untyped constants.

- [ ] **Step 1: Write the failing Go test**

```go
package gpuabi

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeLaunchMatchesTheFrozenLayout(t *testing.T) {
	// correlation, kernel_id, queue_id, context_id, time_ns, tid, _pad
	require.Equal(t, 48, SizeLaunch, "gpu_launch_v1 is 48 bytes; changing it is an ABI break")

	b := make([]byte, SizeLaunch)
	binary.LittleEndian.PutUint64(b[0:], 0x1122334455667788)
	binary.LittleEndian.PutUint64(b[8:], 0xAAAA)
	binary.LittleEndian.PutUint64(b[16:], 7)
	binary.LittleEndian.PutUint64(b[24:], 3)
	binary.LittleEndian.PutUint64(b[32:], 1_000_000_000)
	binary.LittleEndian.PutUint32(b[40:], 4242)

	got, err := DecodeLaunch(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x1122334455667788), got.Correlation)
	assert.Equal(t, uint64(0xAAAA), got.KernelID)
	assert.Equal(t, uint64(7), got.QueueID)
	assert.Equal(t, uint64(3), got.ContextID)
	assert.Equal(t, uint64(1_000_000_000), got.TimeNs)
	assert.Equal(t, uint32(4242), got.TID)
}

func TestDecodeLaunchRejectsShortBuffer(t *testing.T) {
	_, err := DecodeLaunch(make([]byte, SizeLaunch-1))
	require.ErrorIs(t, err, ErrShortRecord)
}

func TestDecodeExecMatchesTheFrozenLayout(t *testing.T) {
	require.Equal(t, 48, SizeExec)

	b := make([]byte, SizeExec)
	binary.LittleEndian.PutUint64(b[0:], 9)
	binary.LittleEndian.PutUint64(b[8:], 0xBBBB)
	binary.LittleEndian.PutUint64(b[16:], 2)
	binary.LittleEndian.PutUint64(b[24:], 1)
	binary.LittleEndian.PutUint64(b[32:], 100)
	binary.LittleEndian.PutUint64(b[40:], 250)

	got, err := DecodeExec(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(9), got.Correlation)
	assert.Equal(t, uint64(1), got.DeviceID)
	assert.Equal(t, uint64(100), got.StartNs)
	assert.Equal(t, uint64(250), got.EndNs)
}

// The spike settled that PC samples key on the cubin CRC, not a module id,
// and carry no correlation in continuous collection (spec §6.3 finding 3).
func TestDecodePCSampleKeysOnCubinCRC(t *testing.T) {
	require.Equal(t, 40, SizePCSample)

	b := make([]byte, SizePCSample)
	binary.LittleEndian.PutUint64(b[0:], 0x45dfed61)
	binary.LittleEndian.PutUint64(b[8:], 0) // correlation unknown in continuous mode
	binary.LittleEndian.PutUint64(b[16:], 0x2f0)
	binary.LittleEndian.PutUint32(b[24:], 11)
	binary.LittleEndian.PutUint32(b[28:], 28)
	binary.LittleEndian.PutUint32(b[32:], 60)

	got, err := DecodePCSample(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x45dfed61), got.CubinCRC)
	assert.Zero(t, got.Correlation, "zero correlation is legal on a PC sample")
	assert.Equal(t, uint64(0x2f0), got.PCOffset)
	assert.Equal(t, uint32(11), got.FunctionIndex)
	assert.Equal(t, uint32(28), got.StallIndex)
	assert.Equal(t, uint32(60), got.Count)
}

func TestDecodeModuleLoadCarriesCubinCRCFirst(t *testing.T) {
	require.Equal(t, 40, SizeModuleLoad)

	b := make([]byte, SizeModuleLoad)
	binary.LittleEndian.PutUint64(b[0:], 0x45dfed61)
	binary.LittleEndian.PutUint64(b[8:], 22)
	got, err := DecodeModuleLoad(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x45dfed61), got.CubinCRC, "cubin_crc leads: PC samples join on it, not module_id")
	assert.Equal(t, uint64(22), got.ModuleID)
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./internal/gpuabi/ -run TestDecode -v
```

Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the C header**

`shim/core/usdt_abi.h`:

```c
// The USDT ABI. Frozen in Phase 3; every change bumps the probe's version
// suffix rather than mutating a record. Field order is chosen so every field
// is naturally aligned with no compiler-inserted padding (spec §6.3).
#ifndef PERFAGENT_GPU_USDT_ABI_H
#define PERFAGENT_GPU_USDT_ABI_H

#include <stdint.h>
#include <stddef.h>

#define GPU_ABI_VERSION 1

// Emitted when a kernel is submitted. queue_id is optional: zero means
// unknown. The launch side cannot always derive it (spec §6.3 finding 1).
struct gpu_launch_v1 {
    uint64_t correlation;
    uint64_t kernel_id;
    uint64_t queue_id;
    uint64_t context_id;
    uint64_t time_ns;
    uint32_t tid;
    uint32_t _pad;
};

// Emitted when a kernel's on-device window is known. Authoritative for
// queue and device identity.
struct gpu_exec_v1 {
    uint64_t correlation;
    uint64_t kernel_id;
    uint64_t queue_id;
    uint64_t device_id;
    uint64_t start_ns;
    uint64_t end_ns;
};

// cubin_crc is first because it, not module_id, is what PC samples join on.
struct gpu_module_load_v1 {
    uint64_t cubin_crc;
    uint64_t module_id;
    uint64_t size_bytes;
    uint64_t load_ns;
    uint64_t bytes_ptr;   // adapter-owned copy; never the vendor's buffer
};

// One record per (PC, stall reason) pair. correlation is zero in continuous
// collection, which is the mode we ship (spec §6.3 finding 3).
struct gpu_pc_sample_batch_v1 {
    uint64_t cubin_crc;
    uint64_t correlation;
    uint64_t pc_offset;
    uint32_t function_index;
    uint32_t stall_index;
    uint32_t count;
    uint32_t _pad;
};

struct gpu_config_v1 {
    uint64_t clock_hz;
    uint32_t sampling_factor;
    uint32_t sm_count;
    uint8_t  vendor;
    uint8_t  _pad[7];
};

struct gpu_dropped_v1 {
    uint64_t count;
    uint8_t  klass;
    uint8_t  _pad[7];
};

_Static_assert(sizeof(struct gpu_launch_v1) == 48, "gpu_launch_v1 layout");
_Static_assert(sizeof(struct gpu_exec_v1) == 48, "gpu_exec_v1 layout");
_Static_assert(sizeof(struct gpu_module_load_v1) == 40, "gpu_module_load_v1 layout");
_Static_assert(sizeof(struct gpu_pc_sample_batch_v1) == 40, "gpu_pc_sample_batch_v1 layout");
_Static_assert(sizeof(struct gpu_config_v1) == 24, "gpu_config_v1 layout");
_Static_assert(sizeof(struct gpu_dropped_v1) == 16, "gpu_dropped_v1 layout");
_Static_assert(offsetof(struct gpu_pc_sample_batch_v1, cubin_crc) == 0, "cubin_crc leads");
_Static_assert(offsetof(struct gpu_module_load_v1, cubin_crc) == 0, "cubin_crc leads");

#endif
```

- [ ] **Step 4: Write the Go mirror**

`internal/gpuabi/records.go`:

```go
// Package gpuabi mirrors the frozen USDT record layouts in
// shim/core/usdt_abi.h. The two must agree byte for byte; the sizes are
// asserted on both sides.
package gpuabi

import (
	"encoding/binary"
	"errors"
)

const (
	Version = 1

	SizeLaunch     = 48
	SizeExec       = 48
	SizeModuleLoad = 40
	SizePCSample   = 40
	SizeConfig     = 24
	SizeDropped    = 16
)

// ErrShortRecord means the buffer is smaller than the record it must hold.
var ErrShortRecord = errors.New("gpuabi: buffer shorter than record")

type Launch struct {
	Correlation uint64
	KernelID    uint64
	QueueID     uint64
	ContextID   uint64
	TimeNs      uint64
	TID         uint32
}

type Exec struct {
	Correlation uint64
	KernelID    uint64
	QueueID     uint64
	DeviceID    uint64
	StartNs     uint64
	EndNs       uint64
}

type ModuleLoad struct {
	CubinCRC  uint64
	ModuleID  uint64
	SizeBytes uint64
	LoadNs    uint64
	BytesPtr  uint64
}

type PCSample struct {
	CubinCRC      uint64
	Correlation   uint64
	PCOffset      uint64
	FunctionIndex uint32
	StallIndex    uint32
	Count         uint32
}

type Dropped struct {
	Count uint64
	Class uint8
}

func DecodeLaunch(b []byte) (Launch, error) {
	if len(b) < SizeLaunch {
		return Launch{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return Launch{
		Correlation: le.Uint64(b[0:]),
		KernelID:    le.Uint64(b[8:]),
		QueueID:     le.Uint64(b[16:]),
		ContextID:   le.Uint64(b[24:]),
		TimeNs:      le.Uint64(b[32:]),
		TID:         le.Uint32(b[40:]),
	}, nil
}

func DecodeExec(b []byte) (Exec, error) {
	if len(b) < SizeExec {
		return Exec{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return Exec{
		Correlation: le.Uint64(b[0:]),
		KernelID:    le.Uint64(b[8:]),
		QueueID:     le.Uint64(b[16:]),
		DeviceID:    le.Uint64(b[24:]),
		StartNs:     le.Uint64(b[32:]),
		EndNs:       le.Uint64(b[40:]),
	}, nil
}

func DecodeModuleLoad(b []byte) (ModuleLoad, error) {
	if len(b) < SizeModuleLoad {
		return ModuleLoad{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return ModuleLoad{
		CubinCRC:  le.Uint64(b[0:]),
		ModuleID:  le.Uint64(b[8:]),
		SizeBytes: le.Uint64(b[16:]),
		LoadNs:    le.Uint64(b[24:]),
		BytesPtr:  le.Uint64(b[32:]),
	}, nil
}

func DecodePCSample(b []byte) (PCSample, error) {
	if len(b) < SizePCSample {
		return PCSample{}, ErrShortRecord
	}
	le := binary.LittleEndian
	return PCSample{
		CubinCRC:      le.Uint64(b[0:]),
		Correlation:   le.Uint64(b[8:]),
		PCOffset:      le.Uint64(b[16:]),
		FunctionIndex: le.Uint32(b[24:]),
		StallIndex:    le.Uint32(b[28:]),
		Count:         le.Uint32(b[32:]),
	}, nil
}

func DecodeDropped(b []byte) (Dropped, error) {
	if len(b) < SizeDropped {
		return Dropped{}, ErrShortRecord
	}
	return Dropped{Count: binary.LittleEndian.Uint64(b[0:]), Class: b[8]}, nil
}
```

- [ ] **Step 5: Run the Go tests**

```bash
go test ./internal/gpuabi/ -v
```

Expected: PASS.

- [ ] **Step 6: Prove the C side agrees**

`shim/core/usdt_abi_test.c`:

```c
#include "usdt_abi.h"
int main(void) { return 0; }  // the _Static_asserts are the test
```

```bash
cc -std=c11 -Wall -Werror -o /tmp/abi_test shim/core/usdt_abi_test.c && /tmp/abi_test && echo "C layout OK"
```

Expected: compiles and prints `C layout OK`. A layout drift fails the compile with the assert's message.

- [ ] **Step 7: Commit**

```bash
git add shim/core/usdt_abi.h shim/core/usdt_abi_test.c internal/gpuabi/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(gpuabi): freeze the USDT record layouts in C and Go"
```

---

### Task 2: USDT probe macros with pinned registers

**Files:**
- Create: `shim/core/usdt_probe.h`
- Create: `shim/core/probe_selftest.cc`
- Test: `internal/usdt/producer_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces (C): `PERFAGENT_USDT_SEMAPHORE(name)`, `PERFAGENT_USDT_PROBE3(name, ptr, count, seq)`, `PERFAGENT_USDT_ENABLED(name)`.

The provider is always `perfagent`. Every probe takes exactly three arguments — a pointer to an array of fixed-size records, how many, and a per-probe sequence number (§6.3 Conventions).

- [ ] **Step 1: Write the failing test**

`internal/usdt/producer_test.go`:

```go
package usdt_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/internal/usdt"
)

// The shim's probe macros must produce notes this parser can read, with a
// register descriptor that does not vary with the compiler's mood. The spike
// observed "8@%rax 8@%rdx 8@%rcx" from an unpinned macro, which a consumer
// reading fixed registers would silently misread.
func TestShimProbeMacrosProduceAParsableNoteWithPinnedRegisters(t *testing.T) {
	if _, err := exec.LookPath("g++"); err != nil {
		t.Skip("g++ not available")
	}
	dir := t.TempDir()
	so := filepath.Join(dir, "libprobeselftest.so")

	cmd := exec.Command("g++", "-O2", "-shared", "-fPIC",
		"-o", so, "shim/core/probe_selftest.cc")
	cmd.Dir = repoRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "compile failed: %s", out)

	probes, err := usdt.ParseFile(so)
	require.NoError(t, err)
	require.Len(t, probes, 1)

	p := probes[0]
	assert.Equal(t, "perfagent", p.Provider)
	assert.Equal(t, "gpu_launch_v1", p.Name)
	assert.True(t, p.HasSemaphore, "the shim must be able to skip work when nobody listens")
	assert.Equal(t, "8@%rdi 8@%rsi 8@%rdx", p.Args,
		"the ABI pins its argument registers; an unpinned macro lets the compiler choose")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Dir(filepath.Dir(wd)) // internal/usdt -> repo root
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./internal/usdt/ -run TestShimProbeMacros -v
```

Expected: FAIL — `shim/core/probe_selftest.cc` does not exist.

- [ ] **Step 3: Write the probe header**

`shim/core/usdt_probe.h`:

```c
// USDT probes without a systemtap build dependency: the .note.stapsdt notes
// are emitted directly. Decided on evidence in spec §15 — a hand-written note
// round-trips through readelf and through internal/usdt.
//
// The three arguments are bound to explicit registers so the note's argument
// descriptor is a constant of the ABI. Letting the compiler choose produced
// "8@%rax 8@%rdx 8@%rcx" in the spike; the consumer reads fixed registers.
#ifndef PERFAGENT_USDT_PROBE_H
#define PERFAGENT_USDT_PROBE_H

#define PERFAGENT_USDT_BASE                                                 \
    ".pushsection .stapsdt.base,\"aG\",\"progbits\",.stapsdt.base,comdat\n" \
    ".weak _.stapsdt.base\n"                                                \
    ".hidden _.stapsdt.base\n"                                              \
    "_.stapsdt.base: .space 1\n"                                            \
    ".size _.stapsdt.base,1\n"                                              \
    ".popsection\n"

// The semaphore the kernel maintains through link.UprobeOptions RefCtrOffset.
// Hidden visibility: core/ must not leak symbols into the application it is
// injected into (spec §6.1).
#define PERFAGENT_USDT_SEMAPHORE(name)                                      \
    __asm__ (                                                               \
        ".pushsection .probes,\"aw\",\"progbits\"\n"                        \
        ".balign 2\n"                                                       \
        ".globl perfagent_" #name "_semaphore\n"                            \
        ".hidden perfagent_" #name "_semaphore\n"                           \
        ".type perfagent_" #name "_semaphore,@object\n"                     \
        ".size perfagent_" #name "_semaphore,2\n"                           \
        "perfagent_" #name "_semaphore: .zero 2\n"                          \
        ".popsection\n");                                                   \
    extern "C" unsigned short perfagent_##name##_semaphore                  \
        __attribute__((visibility("hidden")))

// True when a consumer is attached. Every emit path checks this first.
#define PERFAGENT_USDT_ENABLED(name) (perfagent_##name##_semaphore != 0)

#define PERFAGENT_USDT_PROBE3(name, ptr, count, seq)                        \
  do {                                                                      \
    register unsigned long _a0 __asm__("rdi") = (unsigned long)(ptr);       \
    register unsigned long _a1 __asm__("rsi") = (unsigned long)(count);     \
    register unsigned long _a2 __asm__("rdx") = (unsigned long)(seq);       \
    __asm__ __volatile__ (                                                  \
      "990: nop\n"                                                          \
      PERFAGENT_USDT_BASE                                                   \
      ".pushsection .note.stapsdt,\"\",\"note\"\n"                          \
      ".balign 4\n"                                                         \
      ".4byte 992f-991f, 994f-993f, 3\n"                                    \
      "991: .asciz \"stapsdt\"\n"                                           \
      "992: .balign 4\n"                                                    \
      "993: .8byte 990b\n"                                                  \
      ".8byte _.stapsdt.base\n"                                             \
      ".8byte perfagent_" #name "_semaphore\n"                              \
      ".asciz \"perfagent\"\n"                                              \
      ".asciz \"" #name "\"\n"                                              \
      ".asciz \"8@%%rdi 8@%%rsi 8@%%rdx\"\n"                                \
      "994: .balign 4\n"                                                    \
      ".popsection\n"                                                       \
      :: "r"(_a0), "r"(_a1), "r"(_a2));                                     \
  } while (0)

#endif
```

- [ ] **Step 4: Write the self-test translation unit**

`shim/core/probe_selftest.cc`:

```cpp
// Exists so the probe macros can be compiled and inspected without the rest
// of core/. internal/usdt's producer test builds this file.
#include "usdt_probe.h"

PERFAGENT_USDT_SEMAPHORE(gpu_launch_v1);

extern "C" void perfagent_selftest_emit(const void *ptr, unsigned long count,
                                        unsigned long seq) {
    if (!PERFAGENT_USDT_ENABLED(gpu_launch_v1)) return;
    PERFAGENT_USDT_PROBE3(gpu_launch_v1, ptr, count, seq);
}
```

- [ ] **Step 5: Run the test**

```bash
go test ./internal/usdt/ -run TestShimProbeMacros -v
```

Expected: PASS, with `Args` exactly `8@%rdi 8@%rsi 8@%rdx`. If the descriptor differs, the register binding did not take — do not "fix" the assertion.

- [ ] **Step 6: Commit**

```bash
git add shim/core/usdt_probe.h shim/core/probe_selftest.cc internal/usdt/producer_test.go
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): USDT probe macros with ABI-pinned argument registers"
```

---

### Task 3: Batching and per-probe sequence numbers

**Files:**
- Create: `shim/core/batch.h`, `shim/core/batch.cc`
- Test: `shim/core/batch_test.cc`
- Create: `shim/Makefile`

**Interfaces:**
- Consumes: `usdt_abi.h`, `usdt_probe.h`.
- Produces: `perfagent::Batch<T, N>` with `bool add(const T&)`, `void flush()`, `uint64_t seq() const`, `uint64_t dropped() const`; construction takes an `emit_fn` of type `void(*)(const void *ptr, unsigned long count, unsigned long seq)`.

A batch that fills emits immediately; a batch that cannot emit (no consumer) counts a drop rather than growing. §4's batching requirement and §9's drop accounting meet here.

- [ ] **Step 1: Write the failing test**

`shim/core/batch_test.cc`:

```cpp
#include "batch.h"
#include <cassert>
#include <cstdio>
#include <vector>

using perfagent::Batch;

struct Rec { uint64_t v; };

static std::vector<std::pair<unsigned long, unsigned long>> g_emits; // count, seq
static bool g_enabled = true;

static void fake_emit(const void *, unsigned long count, unsigned long seq) {
    g_emits.push_back({count, seq});
}
static bool fake_enabled() { return g_enabled; }

int main() {
    // A batch emits when it fills, and the sequence number advances per emit.
    {
        g_emits.clear(); g_enabled = true;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        for (int i = 0; i < 9; i++) assert(b.add(Rec{(uint64_t)i}));
        assert(g_emits.size() == 2);
        assert(g_emits[0].first == 4 && g_emits[0].second == 0);
        assert(g_emits[1].first == 4 && g_emits[1].second == 1);
        b.flush();
        assert(g_emits.size() == 3);
        assert(g_emits[2].first == 1 && g_emits[2].second == 2);
        assert(b.dropped() == 0);
    }
    // Flushing an empty batch emits nothing and does not burn a sequence number.
    {
        g_emits.clear(); g_enabled = true;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        b.flush(); b.flush();
        assert(g_emits.empty());
        assert(b.seq() == 0);
    }
    // With no consumer attached, adds are counted as drops, never buffered.
    {
        g_emits.clear(); g_enabled = false;
        Batch<Rec, 4> b(fake_emit, fake_enabled);
        for (int i = 0; i < 10; i++) assert(!b.add(Rec{(uint64_t)i}));
        b.flush();
        assert(g_emits.empty());
        assert(b.dropped() == 10);
    }
    printf("batch_test OK\n");
    return 0;
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
c++ -std=c++17 -I shim/core -o /tmp/batch_test shim/core/batch_test.cc shim/core/batch.cc
```

Expected: FAIL — `batch.h` does not exist.

- [ ] **Step 3: Implement**

`shim/core/batch.h`:

```cpp
// Batching with per-probe sequence numbers and drop accounting.
// A batch never grows: when no consumer is attached, adds are counted and
// discarded, so an unprofiled application pays a branch and nothing else.
#ifndef PERFAGENT_BATCH_H
#define PERFAGENT_BATCH_H

#include <cstdint>
#include <cstddef>

namespace perfagent {

using EmitFn = void (*)(const void *ptr, unsigned long count, unsigned long seq);
using EnabledFn = bool (*)();

template <typename T, size_t N>
class Batch {
public:
    Batch(EmitFn emit, EnabledFn enabled) : emit_(emit), enabled_(enabled) {}

    // Returns true if the record was accepted into the batch.
    bool add(const T &rec) {
        if (!enabled_()) { dropped_++; return false; }
        buf_[n_++] = rec;
        if (n_ == N) flush();
        return true;
    }

    void flush() {
        if (n_ == 0) return;
        if (!enabled_()) { dropped_ += n_; n_ = 0; return; }
        emit_(buf_, (unsigned long)n_, (unsigned long)seq_);
        seq_++;
        n_ = 0;
    }

    uint64_t seq() const { return seq_; }
    uint64_t dropped() const { return dropped_; }
    size_t pending() const { return n_; }

private:
    EmitFn emit_;
    EnabledFn enabled_;
    T buf_[N];
    size_t n_ = 0;
    uint64_t seq_ = 0;
    uint64_t dropped_ = 0;
};

}  // namespace perfagent

#endif
```

`shim/core/batch.cc`:

```cpp
// The template lives in the header; this translation unit exists so the
// archive has an object for it and so the Makefile rule is uniform.
#include "batch.h"
```

- [ ] **Step 4: Write the Makefile**

`shim/Makefile`:

```make
CXX ?= c++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra -fPIC -fvisibility=hidden -I core

CORE_SRC := core/batch.cc core/clock.cc core/drain.cc
CORE_OBJ := $(CORE_SRC:.cc=.o)

.PHONY: all test clean
all: libperfagent-gpu-core.a

libperfagent-gpu-core.a: $(CORE_OBJ)
	ar rcs $@ $^

%.o: %.cc
	$(CXX) $(CXXFLAGS) -c $< -o $@

test: core/batch_test.cc core/clock_test.cc $(CORE_SRC)
	$(CXX) -std=c++17 -I core -o /tmp/batch_test core/batch_test.cc core/batch.cc && /tmp/batch_test
	$(CXX) -std=c++17 -I core -o /tmp/clock_test core/clock_test.cc core/clock.cc && /tmp/clock_test

clean:
	rm -f $(CORE_OBJ) libperfagent-gpu-core.a
```

Note: `clock.cc` and `drain.cc` arrive in Tasks 4 and 5. Until then, build only the batch target:

```bash
c++ -std=c++17 -I shim/core -o /tmp/batch_test shim/core/batch_test.cc shim/core/batch.cc && /tmp/batch_test
```

- [ ] **Step 5: Run the test**

Expected: `batch_test OK`.

- [ ] **Step 6: Commit**

```bash
git add shim/core/batch.h shim/core/batch.cc shim/core/batch_test.cc shim/Makefile
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): batching with per-probe sequence numbers and drop accounting"
```

---

### Task 4: Clock pairing with step detection

**Files:**
- Create: `shim/core/clock.h`, `shim/core/clock.cc`
- Test: `shim/core/clock_test.cc`

**Interfaces:**
- Produces: `perfagent::ClockFit` with `void resample(uint64_t vendor_ns, uint64_t mono_ns)`, `uint64_t to_monotonic(uint64_t vendor_ns) const`, `uint64_t steps() const`, `bool valid() const`.

The spike settled that `cuptiGetTimestamp` is `CLOCK_REALTIME` (§7), so this is an offset, not a drift fit. The failure mode to defend against is a **step**, not slew: a jump moves every subsequent GPU timestamp at once and can place an execution before its own launch.

- [ ] **Step 1: Write the failing test**

`shim/core/clock_test.cc`:

```cpp
#include "clock.h"
#include <cassert>
#include <cstdio>

using perfagent::ClockFit;

int main() {
    // An unsampled fit converts nothing.
    { ClockFit f; assert(!f.valid()); }

    // The conversion is a plain offset: vendor - (vendor0 - mono0).
    {
        ClockFit f;
        f.resample(1'000'000'000ULL, 500'000'000ULL);   // offset = 500ms
        assert(f.valid());
        assert(f.to_monotonic(1'000'000'000ULL) == 500'000'000ULL);
        assert(f.to_monotonic(1'200'000'000ULL) == 700'000'000ULL);
        assert(f.steps() == 0);
    }

    // Slew is absorbed silently: small offset movement is normal on an
    // NTP-disciplined box (measured under 15us over 20s).
    {
        ClockFit f;
        f.resample(1'000'000'000ULL, 500'000'000ULL);
        f.resample(2'000'000'010ULL, 1'500'000'000ULL); // offset moved 10ns
        assert(f.steps() == 0);
        assert(f.to_monotonic(2'000'000'010ULL) == 1'500'000'000ULL);
    }

    // A step is detected and re-anchored, not smoothed away.
    {
        ClockFit f;
        f.resample(1'000'000'000ULL, 500'000'000ULL);
        f.resample(5'000'000'000ULL, 1'500'000'000ULL); // offset jumped 3s
        assert(f.steps() == 1);
        // Re-anchored on the new pair rather than averaging the two.
        assert(f.to_monotonic(5'000'000'000ULL) == 1'500'000'000ULL);
    }
    printf("clock_test OK\n");
    return 0;
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
c++ -std=c++17 -I shim/core -o /tmp/clock_test shim/core/clock_test.cc shim/core/clock.cc
```

Expected: FAIL — no such file.

- [ ] **Step 3: Implement**

`shim/core/clock.h`:

```cpp
// Vendor clock to CPU-monotonic conversion.
//
// The vendor clock is not a drifting device clock: cuptiGetTimestamp is
// CLOCK_REALTIME (spec §7, measured 2000/2000 bracketed). So the conversion
// is an offset between two host clocks, and the hazard is a REALTIME step -
// by NTP, an administrator, or a container host - which shifts every GPU
// timestamp at once and can place an execution before its own launch.
// Slew is absorbed; a step re-anchors and is counted.
#ifndef PERFAGENT_CLOCK_H
#define PERFAGENT_CLOCK_H

#include <cstdint>

namespace perfagent {

// Offset movement beyond this between consecutive samples is a step, not
// slew. 1ms is far above observed NTP slew (<15us over 20s) and far below
// any step worth worrying about.
constexpr int64_t kStepThresholdNs = 1'000'000;

class ClockFit {
public:
    void resample(uint64_t vendor_ns, uint64_t mono_ns) {
        const int64_t offset = (int64_t)vendor_ns - (int64_t)mono_ns;
        if (valid_) {
            const int64_t delta = offset - offset_;
            const int64_t mag = delta < 0 ? -delta : delta;
            if (mag > kStepThresholdNs) steps_++;
        }
        offset_ = offset;
        valid_ = true;
    }

    uint64_t to_monotonic(uint64_t vendor_ns) const {
        if (!valid_) return 0;
        return (uint64_t)((int64_t)vendor_ns - offset_);
    }

    bool valid() const { return valid_; }
    uint64_t steps() const { return steps_; }
    int64_t offset_ns() const { return offset_; }

private:
    int64_t offset_ = 0;
    bool valid_ = false;
    uint64_t steps_ = 0;
};

}  // namespace perfagent

#endif
```

`shim/core/clock.cc`:

```cpp
#include "clock.h"
```

- [ ] **Step 4: Run the test**

```bash
c++ -std=c++17 -I shim/core -o /tmp/clock_test shim/core/clock_test.cc shim/core/clock.cc && /tmp/clock_test
```

Expected: `clock_test OK`.

- [ ] **Step 5: Commit**

```bash
git add shim/core/clock.h shim/core/clock.cc shim/core/clock_test.cc
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): realtime-to-monotonic clock pairing with step detection"
```

---

### Task 5: Drain timer and late-attach replay

**Files:**
- Create: `shim/core/drain.h`, `shim/core/drain.cc`
- Test: `shim/core/drain_test.cc`

**Interfaces:**
- Produces: `perfagent::Drainer` with `void start(unsigned period_ms)`, `void stop()`, `void on_tick(TickFn)`; `perfagent::ReplayLog` with `void record_module(const gpu_module_load_v1&)`, `void record_config(const gpu_config_v1&)`, `void replay_if_newly_attached(bool enabled_now)`, `uint64_t replays() const`.

Two §6.1 requirements land here. The drain timer exists because both vendors deliver events in buffers handed over only when full — measured at up to 15 s of latency on an idle GPU, and CUPTI's own periodic-flush knob cannot drain a partial buffer (§10). Replay exists because module loads, stall maps and config happen before a consumer attaches.

- [ ] **Step 1: Write the failing test**

`shim/core/drain_test.cc`:

```cpp
#include "drain.h"
#include <cassert>
#include <cstdio>
#include <atomic>
#include <thread>
#include <chrono>

using perfagent::Drainer;
using perfagent::ReplayLog;

int main() {
    // The timer ticks on its own thread until stopped.
    {
        std::atomic<int> ticks{0};
        Drainer d;
        d.on_tick([&] { ticks++; });
        d.start(20);
        std::this_thread::sleep_for(std::chrono::milliseconds(130));
        d.stop();
        const int seen = ticks.load();
        assert(seen >= 3);            // ~6 expected; loose for slow CI
        std::this_thread::sleep_for(std::chrono::milliseconds(60));
        assert(ticks.load() == seen); // stopped means stopped
    }

    // Replay fires on the transition from unattached to attached, once.
    {
        ReplayLog log;
        struct gpu_module_load_v1 m {}; m.cubin_crc = 0xabc; 
        log.record_module(m);
        int emitted = 0;
        log.on_replay_module([&](const gpu_module_load_v1 &r) {
            assert(r.cubin_crc == 0xabc);
            emitted++;
        });

        log.replay_if_newly_attached(false);
        assert(log.replays() == 0 && emitted == 0);

        log.replay_if_newly_attached(true);
        assert(log.replays() == 1 && emitted == 1);

        log.replay_if_newly_attached(true);   // still attached: no re-replay
        assert(log.replays() == 1 && emitted == 1);

        log.replay_if_newly_attached(false);  // detach
        log.replay_if_newly_attached(true);   // re-attach replays again
        assert(log.replays() == 2 && emitted == 2);
    }
    printf("drain_test OK\n");
    return 0;
}
```

- [ ] **Step 2: Run it and watch it fail**

```bash
c++ -std=c++17 -pthread -I shim/core -o /tmp/drain_test shim/core/drain_test.cc shim/core/drain.cc
```

Expected: FAIL — no such file.

- [ ] **Step 3: Implement**

`shim/core/drain.h`:

```cpp
// The drain timer and the late-attach replay log.
//
// Drain: both vendors hand over event buffers only when full, so an idle GPU
// delivers nothing for as long as it takes to fill one - measured at 7.6s p50
// and 15s max at 25 launches/s, with nothing delivered until process exit
// (spec §10). CUPTI's own cuptiActivityFlushPeriod cannot help: it returns
// only full buffers. core/ therefore owns the timer.
//
// Replay: module loads, stall maps and device config happen before a consumer
// attaches. Their records are retained and replayed on the unattached ->
// attached transition (spec §6.1).
#ifndef PERFAGENT_DRAIN_H
#define PERFAGENT_DRAIN_H

#include "usdt_abi.h"

#include <atomic>
#include <chrono>
#include <cstdint>
#include <functional>
#include <mutex>
#include <thread>
#include <vector>

namespace perfagent {

class Drainer {
public:
    using TickFn = std::function<void()>;

    ~Drainer() { stop(); }

    void on_tick(TickFn fn) { tick_ = std::move(fn); }

    void start(unsigned period_ms) {
        stop_.store(false);
        thread_ = std::thread([this, period_ms] {
            while (!stop_.load()) {
                std::this_thread::sleep_for(std::chrono::milliseconds(period_ms));
                if (stop_.load()) break;
                if (tick_) tick_();
            }
        });
    }

    void stop() {
        if (!thread_.joinable()) return;
        stop_.store(true);
        thread_.join();
    }

private:
    TickFn tick_;
    std::thread thread_;
    std::atomic<bool> stop_{false};
};

class ReplayLog {
public:
    using ModuleFn = std::function<void(const gpu_module_load_v1 &)>;
    using ConfigFn = std::function<void(const gpu_config_v1 &)>;

    void on_replay_module(ModuleFn fn) { module_fn_ = std::move(fn); }
    void on_replay_config(ConfigFn fn) { config_fn_ = std::move(fn); }

    void record_module(const gpu_module_load_v1 &m) {
        std::lock_guard<std::mutex> g(mu_);
        modules_.push_back(m);
    }

    void record_config(const gpu_config_v1 &c) {
        std::lock_guard<std::mutex> g(mu_);
        config_ = c;
        have_config_ = true;
    }

    // Call on every drain tick with the probe's current enabled state.
    void replay_if_newly_attached(bool enabled_now) {
        std::lock_guard<std::mutex> g(mu_);
        if (enabled_now && !was_attached_) {
            if (config_fn_ && have_config_) config_fn_(config_);
            if (module_fn_) for (const auto &m : modules_) module_fn_(m);
            replays_++;
        }
        was_attached_ = enabled_now;
    }

    uint64_t replays() const { return replays_; }

private:
    std::mutex mu_;
    std::vector<gpu_module_load_v1> modules_;
    gpu_config_v1 config_{};
    bool have_config_ = false;
    bool was_attached_ = false;
    uint64_t replays_ = 0;
    ModuleFn module_fn_;
    ConfigFn config_fn_;
};

}  // namespace perfagent

#endif
```

`shim/core/drain.cc`:

```cpp
#include "drain.h"
```

- [ ] **Step 4: Run the test**

```bash
c++ -std=c++17 -pthread -I shim/core -o /tmp/drain_test shim/core/drain_test.cc shim/core/drain.cc && /tmp/drain_test
```

Expected: `drain_test OK`.

- [ ] **Step 5: Commit**

```bash
git add shim/core/drain.h shim/core/drain.cc shim/core/drain_test.cc
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): drain timer and late-attach replay log"
```

---

### Task 6: The stub emitter

**Files:**
- Create: `shim/stub/stub.cc`
- Modify: `shim/Makefile`

**Interfaces:**
- Consumes: everything in `shim/core/`.
- Produces: `libperfagent-gpu-stub.so` exporting `perfagent_stub_run(unsigned launches, unsigned period_us)`, and a `perfagent-gpu-stub` executable wrapping it.

The stub fabricates a deterministic workload — correlations 1..N, two kernel ids, one queue, exec windows that follow their launches — so the consumer can be tested without a GPU. It is the phase gate's driver and stays in the tree for Phases 4–6.

- [ ] **Step 1: Write the stub**

`shim/stub/stub.cc`:

```cpp
// A GPU-free producer. Emits the same records a vendor adapter would, so the
// consumer and the whole pipeline can be exercised on a machine with no GPU
// (spec §14, Phase 3 gate).
#include "batch.h"
#include "clock.h"
#include "drain.h"
#include "usdt_abi.h"
#include "usdt_probe.h"

#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <sys/syscall.h>
#include <thread>
#include <unistd.h>

static inline uint32_t current_tid() { return (uint32_t)syscall(SYS_gettid); }

PERFAGENT_USDT_SEMAPHORE(gpu_launch_v1);
PERFAGENT_USDT_SEMAPHORE(gpu_exec_v1);

static void emit_launch(const void *p, unsigned long n, unsigned long s) {
    PERFAGENT_USDT_PROBE3(gpu_launch_v1, p, n, s);
}
static bool launch_enabled() { return PERFAGENT_USDT_ENABLED(gpu_launch_v1); }
static void emit_exec(const void *p, unsigned long n, unsigned long s) {
    PERFAGENT_USDT_PROBE3(gpu_exec_v1, p, n, s);
}
static bool exec_enabled() { return PERFAGENT_USDT_ENABLED(gpu_exec_v1); }

static uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

extern "C" void perfagent_stub_run(unsigned launches, unsigned period_us) {
    perfagent::Batch<gpu_launch_v1, 32> lb(emit_launch, launch_enabled);
    perfagent::Batch<gpu_exec_v1, 32> eb(emit_exec, exec_enabled);

    perfagent::Drainer drainer;
    drainer.on_tick([&] { lb.flush(); eb.flush(); });
    drainer.start(100);

    for (unsigned i = 1; i <= launches; i++) {
        const uint64_t now = mono_ns();
        gpu_launch_v1 l{};
        l.correlation = i;                 // never zero: spec §6.1
        l.kernel_id = (i % 2) ? 0x1111 : 0x2222;
        l.queue_id = 1;
        l.context_id = 1;
        l.time_ns = now;
        l.tid = current_tid();
        lb.add(l);

        gpu_exec_v1 e{};
        e.correlation = i;
        e.kernel_id = l.kernel_id;
        e.queue_id = 1;
        e.device_id = 0;
        e.start_ns = now + 10000;          // 10us after the launch
        e.end_ns = now + 10000 + 50000;    // 50us on device
        eb.add(e);

        if (period_us) std::this_thread::sleep_for(std::chrono::microseconds(period_us));
    }

    lb.flush();
    eb.flush();
    drainer.stop();
    fprintf(stderr, "stub: launches=%u launch_dropped=%llu exec_dropped=%llu\n",
            launches, (unsigned long long)lb.dropped(), (unsigned long long)eb.dropped());
}

int main(int argc, char **argv) {
    const unsigned n = argc > 1 ? (unsigned)atoi(argv[1]) : 1000;
    const unsigned us = argc > 2 ? (unsigned)atoi(argv[2]) : 100;
    perfagent_stub_run(n, us);
    return 0;
}
```


- [ ] **Step 2: Add the stub targets to `shim/Makefile`**

```make
perfagent-gpu-stub: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -o $@ stub/stub.cc $(CORE_SRC)
```

- [ ] **Step 3: Build it and confirm the probes are discoverable**

```bash
make -C shim perfagent-gpu-stub
readelf -n shim/perfagent-gpu-stub | grep -A4 'Provider: perfagent'
```

Expected: two notes, `gpu_launch_v1` and `gpu_exec_v1`, each with a semaphore and `8@%rdi 8@%rsi 8@%rdx`.

- [ ] **Step 4: Confirm it does nothing when unattached**

```bash
./shim/perfagent-gpu-stub 1000 0
```

Expected: `launch_dropped=1000 exec_dropped=1000` — with no consumer, every record is counted and discarded, matching the measured semaphore-gate behaviour in §9.1.

- [ ] **Step 5: Commit**

```bash
git add shim/stub/stub.cc shim/Makefile
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): GPU-free stub emitter for the Phase 3 gate"
```

---

### Task 7: The eBPF consumer

**Files:**
- Create: `bpf/gpu_usdt.bpf.c`
- Create: `gpuprobe/gen.go`
- Create: `gpuprobe/consumer.go`
- Test: `gpuprobe/consumer_test.go`

**Interfaces:**
- Consumes: `internal/gpuabi` decoders, `internal/usdt.ParseFile`.
- Produces: `gpuprobe.Attach(cfg Config) (*Consumer, error)`; `Config{ShimPath string, PID int, Sink gpu.EventSink, Backend gpu.GPUBackendID}`; `(*Consumer).Run(ctx) error`, `(*Consumer).Close() error`, `(*Consumer).Stats() Stats`.

Each probe site attaches in one `link.UprobeMulti` with a cookie identifying the record kind. The BPF program reads the three pinned registers, copies `count` records of the kind's size out of user memory, and pushes them to a ringbuf with a small header.

- [ ] **Step 1: Write the BPF program**

`bpf/gpu_usdt.bpf.c`:

```c
// Consumer for the perfagent GPU USDT ABI. One program serves every probe;
// bpf_get_attach_cookie() says which record kind fired.
#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

#define KIND_LAUNCH 1
#define KIND_EXEC   2
#define KIND_MODULE 3
#define KIND_PC     4

#define MAX_RECORDS_PER_BATCH 64
#define MAX_RECORD_BYTES      48

struct batch_hdr {
    __u32 kind;
    __u32 count;
    __u64 seq;
    __u32 pid;
    __u32 tid;
    __u64 bytes;
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 22);   // 4MB
} events SEC(".maps");

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 8);
    __type(key, __u32);
    __type(value, __u64);
} dropped SEC(".maps");

static __always_inline __u32 record_size(__u32 kind) {
    switch (kind) {
    case KIND_LAUNCH: return 48;
    case KIND_EXEC:   return 48;
    case KIND_MODULE: return 40;
    case KIND_PC:     return 40;
    }
    return 0;
}

static __always_inline void count_drop(__u32 kind) {
    __u64 *d = bpf_map_lookup_elem(&dropped, &kind);
    if (d) __sync_fetch_and_add(d, 1);
}

SEC("uprobe")
int gpu_usdt_batch(struct pt_regs *ctx) {
    // The ABI pins its arguments: ptr=rdi, count=rsi, seq=rdx.
    __u64 ptr   = PT_REGS_PARM1(ctx);
    __u64 count = PT_REGS_PARM2(ctx);
    __u64 seq   = PT_REGS_PARM3(ctx);

    __u32 kind = (__u32)bpf_get_attach_cookie(ctx);
    __u32 rsz = record_size(kind);
    if (rsz == 0 || count == 0) return 0;
    if (count > MAX_RECORDS_PER_BATCH) {
        count_drop(kind);
        count = MAX_RECORDS_PER_BATCH;
    }

    __u64 bytes = count * rsz;
    struct batch_hdr *hdr = bpf_ringbuf_reserve(&events, sizeof(*hdr) + (MAX_RECORDS_PER_BATCH * MAX_RECORD_BYTES), 0);
    if (!hdr) { count_drop(kind); return 0; }

    __u64 id = bpf_get_current_pid_tgid();
    hdr->kind = kind;
    hdr->count = (__u32)count;
    hdr->seq = seq;
    hdr->pid = (__u32)(id >> 32);
    hdr->tid = (__u32)id;
    hdr->bytes = bytes;

    void *dst = (void *)(hdr + 1);
    if (bytes > MAX_RECORDS_PER_BATCH * MAX_RECORD_BYTES) bytes = MAX_RECORDS_PER_BATCH * MAX_RECORD_BYTES;
    if (bpf_probe_read_user(dst, (__u32)bytes, (void *)ptr) != 0) {
        bpf_ringbuf_discard(hdr, 0);
        count_drop(kind);
        return 0;
    }

    bpf_ringbuf_submit(hdr, 0);
    return 0;
}
```

- [ ] **Step 2: Add the generate directive**

`gpuprobe/gen.go`:

```go
package gpuprobe

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=gpuprobe gpuusdt ../bpf/gpu_usdt.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=gpuprobe gpuusdt ../bpf/gpu_usdt.bpf.c -- -I../bpf
```

- [ ] **Step 3: Generate the bytecode**

```bash
cd gpuprobe && go generate ./... && cd ..
ls gpuprobe/gpuusdt_bpfel*.o
```

Expected: objects for both arches. This is the step that needs `clang` and `llvm-strip` from the prerequisites.

- [ ] **Step 4: Write the failing consumer test**

`gpuprobe/consumer_test.go`:

```go
package gpuprobe

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cookie values are part of the contract between the Go attach code and the
// BPF program's record_size switch. A mismatch decodes garbage silently.
func TestProbeKindCookiesMatchTheBPFProgram(t *testing.T) {
	assert.Equal(t, uint64(1), cookieFor("gpu_launch_v1"))
	assert.Equal(t, uint64(2), cookieFor("gpu_exec_v1"))
	assert.Equal(t, uint64(3), cookieFor("gpu_module_load_v1"))
	assert.Equal(t, uint64(4), cookieFor("gpu_pc_sample_batch_v1"))
	assert.Equal(t, uint64(0), cookieFor("gpu_unknown_v9"), "unknown probes are not attached")
}

// Module and PC-sample records are on the wire in Phase 3 but are not turned
// into canonical events until Phases 4 and 6. They must be counted, not
// silently discarded — §6.1 admits no silent loss anywhere.
func TestUndecodedKindsAreCountedNotDropped(t *testing.T) {
	c := &Consumer{seqByKind: map[uint32]uint64{}}
	buf := make([]byte, 32+40)
	putU32(buf[0:], kindModule)
	putU32(buf[4:], 1)
	putU64(buf[24:], 40)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	assert.Equal(t, uint64(1), c.Stats().Undecoded)
	assert.Zero(t, c.Stats().Records)
}

func TestDecodeBatchSplitsRecordsByKind(t *testing.T) {
	// header: kind=1 count=2 seq=7 pid=11 tid=12 bytes=96
	buf := make([]byte, 32+96)
	putU32(buf[0:], 1)
	putU32(buf[4:], 2)
	putU64(buf[8:], 7)
	putU32(buf[16:], 11)
	putU32(buf[20:], 12)
	putU64(buf[24:], 96)
	putU64(buf[32:], 100) // first launch correlation
	putU64(buf[32+48:], 101)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), b.Kind)
	assert.Equal(t, uint64(7), b.Seq)
	require.Len(t, b.Launches, 2)
	assert.Equal(t, uint64(100), b.Launches[0].Correlation)
	assert.Equal(t, uint64(101), b.Launches[1].Correlation)
}

func TestDecodeBatchRejectsTruncatedPayload(t *testing.T) {
	buf := make([]byte, 32+10)
	putU32(buf[0:], 1)
	putU32(buf[4:], 2)
	putU64(buf[24:], 96) // claims 96 bytes it does not have
	_, err := decodeBatch(buf)
	require.Error(t, err)
}

// Gaps in a probe's sequence numbers are losses the consumer did not observe
// and must not hide (spec §6.1).
func TestSequenceGapsAreCounted(t *testing.T) {
	c := &Consumer{seqByKind: map[uint32]uint64{}}
	c.noteSeq(1, 0)
	c.noteSeq(1, 1)
	assert.Zero(t, c.stats.SequenceGaps)
	c.noteSeq(1, 5) // 2,3,4 never arrived
	assert.Equal(t, uint64(3), c.stats.SequenceGaps)
}
```

Add small `putU32`/`putU64` helpers in the test file using `encoding/binary.LittleEndian`.

- [ ] **Step 5: Run it and watch it fail**

```bash
go test ./gpuprobe/ -v
```

Expected: FAIL — `cookieFor`, `decodeBatch`, `Consumer` undefined.

- [ ] **Step 6: Implement the consumer**

`gpuprobe/consumer.go` — the essential shape, with the two constraints this
plan exists to protect:

```go
// Package gpuprobe consumes the perfagent GPU USDT ABI from a shim library
// and feeds the normalized events into a gpu.EventSink.
package gpuprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/dpsoft/perf-agent/internal/usdt"
)

const (
	kindLaunch = 1
	kindExec   = 2
	kindModule = 3
	kindPC     = 4

	batchHdrSize = 32
)

func cookieFor(probeName string) uint64 {
	switch probeName {
	case "gpu_launch_v1":
		return kindLaunch
	case "gpu_exec_v1":
		return kindExec
	case "gpu_module_load_v1":
		return kindModule
	case "gpu_pc_sample_batch_v1":
		return kindPC
	}
	return 0
}

// kernelVersionCode supplies what BPF_PROG_TYPE_KPROBE requires. cilium/ebpf
// would otherwise read the vDSO through /proc/self/mem, which a setcap'd
// binary cannot do because file capabilities make the process non-dumpable.
func kernelVersionCode() uint32 {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return 0
	}
	var a, b, c uint32
	fmt.Sscanf(string(u.Release[:]), "%d.%d.%d", &a, &b, &c)
	if c > 255 {
		c = 255
	}
	return a<<16 | b<<8 | c
}

type Config struct {
	ShimPath string
	PID      int
	Backend  gpu.GPUBackendID
	Sink     gpu.EventSink
}

type Stats struct {
	Batches      uint64
	Records      uint64
	SequenceGaps uint64
	SinkRejected uint64
	// Undecoded counts records of a kind this phase carries on the wire but
	// does not yet normalize (module loads, PC samples). Counted so the loss
	// is visible rather than silent.
	Undecoded uint64
}

type Consumer struct {
	cfg       Config
	objs      gpuusdtObjects
	links     []link.Link
	reader    *ringbuf.Reader
	seqByKind map[uint32]uint64
	stats     Stats
}

// Attach discovers the shim's probes and attaches them all in one
// uprobe_multi link.
//
// It must be uprobe_multi, not link.Uprobe: the perf_uprobe PMU path requires
// CAP_SYS_ADMIN, measured, while the BPF-link path works with CAP_BPF +
// CAP_PERFMON alone (spec §11). Using the obvious API silently undoes the
// capability reduction Phase 1 delivered. Requires Linux 6.6+.
func Attach(cfg Config) (*Consumer, error) {
	probes, err := usdt.ParseFile(cfg.ShimPath)
	if err != nil {
		return nil, fmt.Errorf("parse usdt notes: %w", err)
	}

	var addrs, refCtrs, cookies []uint64
	for _, p := range probes {
		c := cookieFor(p.Name)
		if c == 0 || p.Provider != "perfagent" {
			continue
		}
		if !p.HasSemaphore {
			return nil, fmt.Errorf("probe %s has no semaphore; the shim cannot tell when to emit", p.Name)
		}
		addrs = append(addrs, p.Offset)
		refCtrs = append(refCtrs, p.SemaphoreOffset)
		cookies = append(cookies, c)
	}
	if len(addrs) == 0 {
		return nil, errors.New("no perfagent probes found in shim")
	}

	spec, err := loadGpuusdt()
	if err != nil {
		return nil, err
	}
	for _, p := range spec.Programs {
		p.KernelVersion = kernelVersionCode()
	}

	c := &Consumer{cfg: cfg, seqByKind: map[uint32]uint64{}}
	if err := spec.LoadAndAssign(&c.objs, nil); err != nil {
		return nil, err
	}

	ex, err := link.OpenExecutable(cfg.ShimPath)
	if err != nil {
		return nil, err
	}
	l, err := ex.UprobeMulti(nil, c.objs.GpuUsdtBatch, &link.UprobeMultiOptions{
		Addresses:     addrs,
		RefCtrOffsets: refCtrs,
		Cookies:       cookies,
		PID:           uint32(cfg.PID),
	})
	if err != nil {
		return nil, fmt.Errorf("uprobe_multi attach (needs Linux 6.6+): %w", err)
	}
	c.links = append(c.links, l)

	c.reader, err = ringbuf.NewReader(c.objs.Events)
	if err != nil {
		return nil, err
	}
	return c, nil
}

type batch struct {
	Kind     uint32
	Seq      uint64
	PID, TID uint32
	RawCount uint32
	Launches []gpuabi.Launch
	Execs    []gpuabi.Exec
}

func decodeBatch(b []byte) (batch, error) {
	if len(b) < batchHdrSize {
		return batch{}, gpuabi.ErrShortRecord
	}
	le := binary.LittleEndian
	out := batch{
		Kind: le.Uint32(b[0:]),
		Seq:  le.Uint64(b[8:]),
		PID:  le.Uint32(b[16:]),
		TID:  le.Uint32(b[20:]),
	}
	count := int(le.Uint32(b[4:]))
	out.RawCount = uint32(count)
	nbytes := int(le.Uint64(b[24:]))
	payload := b[batchHdrSize:]
	if nbytes > len(payload) {
		return batch{}, fmt.Errorf("batch claims %d payload bytes, has %d", nbytes, len(payload))
	}
	payload = payload[:nbytes]

	switch out.Kind {
	case kindLaunch:
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeLaunch(payload[i*gpuabi.SizeLaunch:])
			if err != nil {
				return batch{}, err
			}
			out.Launches = append(out.Launches, rec)
		}
	case kindExec:
		for i := 0; i < count; i++ {
			rec, err := gpuabi.DecodeExec(payload[i*gpuabi.SizeExec:])
			if err != nil {
				return batch{}, err
			}
			out.Execs = append(out.Execs, rec)
		}
	}
	return out, nil
}

// noteSeq counts records lost between batches. A gap is loss the consumer did
// not observe and must never be silent (spec §6.1).
func (c *Consumer) noteSeq(kind uint32, seq uint64) {
	prev, seen := c.seqByKind[kind]
	if seen && seq > prev+1 {
		c.stats.SequenceGaps += seq - prev - 1
	}
	c.seqByKind[kind] = seq
}

func correlationOf(backend gpu.GPUBackendID, v uint64) gpu.CorrelationID {
	return gpu.CorrelationID{Backend: backend, Value: strconv.FormatUint(v, 10)}
}

func (c *Consumer) Run(ctx context.Context) error {
	for {
		rec, err := c.reader.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		b, err := decodeBatch(rec.RawSample)
		if err != nil {
			continue
		}
		c.applyBatch(b)
	}
}

func (c *Consumer) applyBatch(b batch) {
	c.stats.Batches++
	c.noteSeq(b.Kind, b.Seq)

	switch b.Kind {
	case kindLaunch:
		for _, l := range b.Launches {
			c.stats.Records++
			ev := gpu.GPUKernelLaunch{
				Correlation: correlationOf(c.cfg.Backend, l.Correlation),
				TimeNs:      l.TimeNs,
				Launch:      gpu.LaunchContext{PID: b.PID, TID: l.TID, TimeNs: l.TimeNs},
			}
			if err := c.cfg.Sink.EmitLaunch(ev); err != nil {
				c.stats.SinkRejected++
			}
		}
	case kindExec:
		for _, e := range b.Execs {
			c.stats.Records++
			ev := gpu.GPUKernelExec{
				Correlation: correlationOf(c.cfg.Backend, e.Correlation),
				StartNs:     e.StartNs,
				EndNs:       e.EndNs,
			}
			if err := c.cfg.Sink.EmitExec(ev); err != nil {
				c.stats.SinkRejected++
			}
		}
	default:
		// Carried on the wire, not yet normalized. Counted, never silent.
		c.stats.Undecoded += uint64(b.RawCount)
	}
}

func (c *Consumer) Stats() Stats { return c.stats }

func (c *Consumer) Close() error {
	if c.reader != nil {
		c.reader.Close()
	}
	for _, l := range c.links {
		l.Close()
	}
	return c.objs.Close()
}
```

`gpu.GPUQueueRef` and `KernelName` are left unset here: the kernel-name interning table (`gpu_kernel_name_v1`) is Task 8's, and queue identity is optional on launches by §6.3 finding 1.

- [ ] **Step 7: Run the unit tests**

```bash
go test ./gpuprobe/ -v
```

Expected: PASS. These tests do not attach anything, so they need no privileges.

- [ ] **Step 8: Commit**

```bash
git add bpf/gpu_usdt.bpf.c gpuprobe/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(gpuprobe): eBPF consumer for the GPU USDT ABI over uprobe_multi"
```

---

### Task 8: The phase gate — stub to pprof with no GPU

**Files:**
- Create: `gpuprobe/gate_test.go`
- Create: `cmd/gpu-stub-profile/main.go`

**Interfaces:**
- Consumes: `gpuprobe.Attach`, `gpu.NewTimeline`, `gpu.ProjectExecutions`, the `pprof` builder, `shim/perfagent-gpu-stub`.

- [ ] **Step 1: Write the gate test**

`gpuprobe/gate_test.go`:

```go
package gpuprobe_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
)

// hasBPFAndPerfmon mirrors perfagent/agent.go's hasCapSysPtrace: check
// Permitted as well as Effective, because a setcap'd binary has not promoted
// Permitted yet, and never gate on Getuid alone.
func hasBPFAndPerfmon() bool {
	if os.Geteuid() == 0 {
		return true
	}
	set := cap.GetProc()
	if set == nil {
		return false
	}
	for _, want := range []cap.Value{cap.BPF, cap.PERFMON} {
		ok := false
		for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
			if have, err := set.GetFlag(flag, want); err == nil && have {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// The Phase 3 gate: the stub drives the full pipeline to pprof samples on a
// machine with no GPU.
func TestStubDrivesThePipelineToPprofWithoutAGPU(t *testing.T) {
	if !hasBPFAndPerfmon() {
		t.Skip("needs CAP_BPF and CAP_PERFMON; setcap the test binary")
	}
	stub := filepath.Join("..", "shim", "perfagent-gpu-stub")
	requireBuilt(t, stub)

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: stub,
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     timeline,
	})
	require.NoError(t, err)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The stub only emits once the semaphore says someone is listening, so it
	// must start after Attach.
	out, err := exec.Command(stub, "500", "200").CombinedOutput()
	require.NoError(t, err, string(out))

	time.Sleep(500 * time.Millisecond) // let the drain timer flush the tail
	cancel()
	<-done

	stats := c.Stats()
	assert.Zero(t, stats.SequenceGaps, "no batch may be lost silently")
	assert.GreaterOrEqual(t, stats.Records, uint64(900), "500 launches + 500 execs")

	snap := timeline.Snapshot()
	samples := gpu.ProjectExecutions(snap)
	require.NotEmpty(t, samples, "the gate is pprof samples, not counters")

	// Every execution joined its launch exactly, because the stub emits a
	// correlation on both sides.
	assert.Zero(t, snap.JoinStats.HeuristicExecutionJoinCount,
		"the stub supplies correlations; no join should need the heuristic")
}

func requireBuilt(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make unavailable")
	}
	cmd := exec.Command("make", "-C", filepath.Dir(filepath.Dir(path)), "perfagent-gpu-stub")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build stub: %s", out)
}
```


- [ ] **Step 2: Run it and watch it fail**

```bash
go test ./gpuprobe/ -run TestStubDrives -v
```

Expected: FAIL or SKIP. If it skips, setcap the test binary:

```bash
go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test
sudo setcap cap_bpf,cap_perfmon+ep /home/diego/gpuprobe.test
/home/diego/gpuprobe.test -test.run TestStubDrives -test.v
```

Build the test binary outside `/tmp` — `/tmp` is `nosuid`, so file capabilities do not survive exec there.

- [ ] **Step 3: Make it pass**

Two failures are expected here and both have a right fix:

- *Records arrive after `cancel()`.* The stub's last partial batch leaves on a
  drain tick, so the test must outwait one drain period. Raise the sleep to
  exceed the stub's 100 ms drain period — do not shorten the drain period to
  suit the test.
- *`SequenceGaps` is non-zero.* This means the ringbuf dropped a batch, not
  that the assertion is too strict. Raise the ringbuf's `max_entries` in
  `bpf/gpu_usdt.bpf.c` and regenerate, or lower the stub's rate with a larger
  `period_us`. Never loosen the assertion: a silent gap is the exact failure
  §6.1 exists to prevent.

- [ ] **Step 4: Write the demonstration command**

`cmd/gpu-stub-profile/main.go`:

```go
// Attaches to the stub, runs it, and writes gpu-stub.pb.gz through the
// existing pprof builder — the Phase 3 gate as an artifact a human can open.
package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/pprof"
)

func main() {
	const stub = "./shim/perfagent-gpu-stub"

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: stub,
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     timeline,
	})
	if err != nil {
		log.Fatalf("attach: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := c.Run(ctx); err != nil {
			log.Printf("consumer: %v", err)
		}
	}()

	if out, err := exec.Command(stub, "2000", "200").CombinedOutput(); err != nil {
		log.Fatalf("stub: %v: %s", err, out)
	}
	time.Sleep(500 * time.Millisecond) // outwait one drain period
	cancel()

	snap := timeline.Snapshot()
	samples := gpu.ProjectExecutions(snap)
	if len(samples) == 0 {
		log.Fatal("no samples projected; the pipeline produced nothing")
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{SampleRate: 1})
	for i := range samples {
		builders.AddSample(&samples[i])
	}

	f, err := os.Create("gpu-stub.pb.gz")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	for _, b := range builders.Builders {
		if _, err := b.Write(f); err != nil {
			log.Fatalf("write profile: %v", err)
		}
		break
	}
	log.Printf("wrote gpu-stub.pb.gz: %d samples, stats=%+v", len(samples), c.Stats())
}
```

- [ ] **Step 5: Verify the artifact**

```bash
go build -o /home/diego/gpu-stub-profile ./cmd/gpu-stub-profile
sudo setcap cap_bpf,cap_perfmon+ep /home/diego/gpu-stub-profile
/home/diego/gpu-stub-profile && go tool pprof -top gpu-stub.pb.gz | head -20
```

Expected: kernel frames (`[gpu:kernel:...]`) under `[gpu:launch]`.

- [ ] **Step 6: Commit**

```bash
git add gpuprobe/gate_test.go cmd/gpu-stub-profile/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "test(gpuprobe): Phase 3 gate — stub drives the pipeline to pprof with no GPU"
```

---

## Phase gate

Phase 3 is done when all of the following hold:

1. `go test ./internal/gpuabi/ ./internal/usdt/ ./gpuprobe/` passes, and `make -C shim test` passes.
2. `TestStubDrivesThePipelineToPprofWithoutAGPU` passes on a box with no GPU, with zero sequence gaps.
3. `gpu-stub-profile` writes a pprof whose samples carry `[gpu:launch]` and `[gpu:kernel:*]` frames.
4. The stub, unattached, reports every record dropped and emits nothing.
5. `getcap` on the test binary shows `cap_bpf,cap_perfmon` only — **no `cap_sys_admin`**. If the attach needs it, the consumer is using the wrong mechanism.
6. **The ABI review has been done on paper** against the rocprofiler bridge's taxonomy on branch `gpu-profiling-spec` (§6.1, §14). Walk `examples/rocprofiler_sdk_preload_bridge.cpp` and confirm each of `gpu_launch_v1`, `gpu_exec_v1`, `gpu_module_load_v1` and `gpu_kernel_name_v1` can be populated from what rocprofiler delivers, or record why not. Do not skip this because NVIDIA ships first — it is the whole reason `core/` is worth having.

## Deferred to Phase 4, deliberately

- CPU stack capture at the launch probe. The frames-and-labels split in `gpu/projection.go` already expects `LaunchContext.CPUStack`; wiring `bpf_get_stack` into `gpu_usdt.bpf.c` and symbolizing through blazesym is Phase 4's work, and it is what turns these into CPU+GPU flame graphs rather than GPU-only ones.
- `gpu_kernel_name_v1` interning and the kernel-name table.
- `gpu_stall_reason_map_v1` and PC sampling (Phase 6).
- The token bucket on the callback path. §9.1 measured the callback path at −0.5%, so it is not the pressure point; the activity path is, and that lives in the CUPTI adapter.
