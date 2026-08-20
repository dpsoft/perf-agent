# GPU V2 Phase 4a — Sampled Launch Stacks and Kernel Names Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn Phase 3's GPU-only profiles into CPU+GPU flame graphs, by capturing the launching thread's CPU stack at a sampled subset of launches and resolving kernel names — without a GPU, and without lying about which parts of the profile are measured and which are inferred.

**Architecture:** Phase 3's launch probe is batched, and a batched probe structurally cannot carry per-launch stacks: the stack at flush time belongs to whichever launch triggered it. So a second probe, `gpu_launch_sampled_v1`, fires **unbatched** for a sampled subset of launches, and the eBPF consumer captures the user stack at that probe with `bpf_get_stackid`. Kernel names arrive separately through `gpu_kernel_name_v1` interning, replayed on late attach. The Go consumer resolves stack IDs through the machinery `profile/` already uses, and `gpu.ProjectExecutions` grows an honest split between stack-attributed and unattributed GPU time.

**Tech Stack:** Go 1.26, `github.com/cilium/ebpf` v0.21 (`link.UprobeMulti`, `ringbuf`, `BPF_MAP_TYPE_STACK_TRACE`), C++17 for the shim, clang/bpf2go, blazesym via the existing `symbolize.Symbolizer`, testify.

**Spec:** `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` (§4 invariants, §6 the USDT ABI, §8 output representation, §9 overhead control, §10 correlation and joins, §14 Phase 4, §16 risks — the sampling fallback this plan implements is named there)

**Predecessor:** `docs/superpowers/plans/2026-08-19-gpu-v2-phase3-usdt-transport.md`, complete and merged/in review as PR #35. Everything it built is assumed present and working.

## Global Constraints

- Go 1.26.0+. CGO is required; export the block in "Build environment" before any `go` command.
- **Linux 6.6+ at runtime.** Attach is `link.UprobeMulti` only. Never `link.Uprobe` / the `perf_uprobe` PMU — it requires `CAP_SYS_ADMIN` and undoes Phase 1. The gate asserts `getcap` shows no `cap_sys_admin`.
- Every `ebpf.Kprobe`-type `ProgramSpec` sets `KernelVersion` explicitly, or a setcap'd (non-dumpable) binary fails reading the vDSO through `/proc/self/mem`.
- Records stay fixed-size, little-endian, naturally aligned. Existing layouts are **frozen**: Launch 48, Exec 48, ModuleLoad 40, PCSample 40, Config 24, Dropped 16. New records are new probes, not mutations of old ones.
- Probe arguments stay pinned to `rdi/rsi/rdx`; the descriptor is `8@%rdi 8@%rsi 8@%rdx`.
- All `*_ns` on the wire are CPU-monotonic.
- **No loss is ever silent.** Sampling is not loss, but the *sampled-out* population must be counted and visible, never silently folded into the sampled one.
- **A sampled measurement is never presented as an exhaustive one** (§4's honesty rule, the same one that governs heuristic joins).
- The shim does no work when no consumer is attached; every emit is gated on its probe's semaphore.
- Do not commit `CLAUDE.md`. No `Co-Authored-By` lines.
- Commit with `git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit`.

## Build environment

```bash
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release
```

Run `go test` and `go generate` directly. Do not run `make test-unit`.

Privileged tests need `cap_bpf,cap_perfmon`. Build such binaries **outside `/tmp`** (it is `nosuid`, so file capabilities do not survive exec) and link blazesym statically (a setcap'd binary runs in secure-execution mode and ignores `LD_LIBRARY_PATH`):

```bash
go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test \
  -ldflags '-linkmode external -extldflags "-Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"'
sudo setcap cap_bpf,cap_perfmon+ep /home/diego/gpuprobe.test
```
**Rebuilding strips the capability bit.** Do all source work first, rebuild once, then re-`setcap`.

## Why sampling, and what it costs

A batched launch probe cannot carry per-launch stacks. Firing the launch probe unbatched so every launch can capture one is too expensive: an attached uprobe trap is roughly 1–2 µs, and Phase 3 measured a 393k launches/s ceiling — 0.4–0.8 s of overhead per second, and still 1–10% at a realistic 10–50k/s. §16 pre-committed to the remedy: *"sampling launches while preserving correlation anchors for sampled kernels only."*

The consequence, which the output representation must carry honestly:

- **GPU execution totals stay exact.** Sampling applies only to stack capture. Every execution is still recorded and joined, so "how much GPU time did this kernel consume" remains measured.
- **Stack attribution is statistical.** "Which call path caused this kernel" is observed for the sampled subset and unknown for the rest.

Blending those two silently — scaling sampled stacks up and presenting the result as though every launch were observed — is the same dishonesty §4 forbids for heuristic joins. Task 5 keeps them separable.

## What already exists — do not rewrite

- `shim/core/`: `usdt_abi.h` (frozen layouts, `GPU_STATIC_ASSERT`), `usdt_probe.h` (`PERFAGENT_USDT_SEMAPHORE` / `PERFAGENT_USDT_PROBE3` / `PERFAGENT_USDT_ENABLED` / `PERFAGENT_USDT_EMITTER`, `.stapsdt.base` guarded), `batch.h` (mutex-guarded `Batch<T,N>`, lock held across emit), `clock.h` (`ClockFit`), `drain.h` (`Drainer`, `ReplayLog`).
- `shim/stub/stub.cc`: the GPU-free producer, and `shim/Makefile` with `test` / `test-tsan` targets.
- `internal/gpuabi`: `Decode{Launch,Exec,ModuleLoad,PCSample,Dropped}`, `Size*`, `ErrShortRecord`.
- `internal/usdt`: `ParseFile` → probes with file-resolved `Offset` / `SemaphoreOffset`.
- `gpuprobe`: `Attach/Run/Stats/Close`, cookie-keyed decode, per-`(kind,pid)` sequence-gap accounting, `Stats{Batches,Records,SequenceGaps,SinkRejected,Undecoded,Malformed,ZeroCorrelation,KernelDropped}`.
- `bpf/gpu_usdt.bpf.c`: `SEC("uprobe.multi")`, cookie → record size, ringbuf batch with a 32-byte header.
- `gpu/`: `Timeline`, `EventSink`, `ProjectExecutions`, `JoinStats`.
- `bpf/unwind_common.h`: shared unwind types, maps and inlines (`mapping_for_pc`, `classify_rel_pc`, `cfi_lookup`). Its own comment records that the *walker* lives per-program in `perf_dwarf.bpf.c` / `offcpu_dwarf.bpf.c` — a GPU driver would be the third, and that is Phase 4b, not this plan.
- The stack→frames path this plan reuses verbatim:
  `bpf_get_stackid` → `Map.LookupBytes(uint32(id))` → `internal/bpfstack.ExtractIPs(bytes) []uint64` → `symbolize.Symbolizer.SymbolizeProcess(pid, ips) []symbolize.Frame` → `symbolize.ToProfFrames(frames) []pprof.Frame`.

## Scope boundary

**In:** sampled launch stacks via frame-pointer `bpf_get_stackid`, kernel-name interning, symbolization, honest projection, a GPU-free gate.

**Out, deliberately:** the DWARF unwind driver (`unwind_common.h`'s third consumer) — frame-pointer stacks first, with the record shaped so DWARF slots in without an ABI change; the CUPTI adapter and anything needing a GPU; PC sampling; arm64.

---

### Task 1: The sampled-launch and kernel-name records

**Files:**
- Modify: `shim/core/usdt_abi.h`
- Modify: `internal/gpuabi/records.go`
- Test: `internal/gpuabi/records_test.go`
- Test: `shim/core/usdt_abi_test.c`

**Interfaces:**
- Produces (C): `struct gpu_launch_sampled_v1`, `struct gpu_kernel_name_v1`; `GPU_KERNEL_NAME_MAX`.
- Produces (Go): `gpuabi.LaunchSampled`, `gpuabi.KernelName`; `DecodeLaunchSampled`, `DecodeKernelName`; `SizeLaunchSampled`, `SizeKernelName`.

`gpu_launch_sampled_v1` is a superset of `gpu_launch_v1` plus the sampling denominator. It is a **separate probe**, not a wider launch record: the frozen layout stays frozen, and a consumer that ignores the new probe still works.

`sample_period` is the N in "one launch in N was sampled" at the moment of capture. Carrying it per record rather than only in `gpu_config_v1` means a profile remains interpretable when the controller adapts the rate mid-run.

Kernel names are variable-length, which the fixed-size rule forbids, so `gpu_kernel_name_v1` uses a fixed `GPU_KERNEL_NAME_MAX` char array with an explicit length. CUDA kernel names are mangled C++ and can be long; 256 bytes covers the overwhelming majority and truncation is recorded rather than silent.

- [ ] **Step 1: Write the failing Go test**

```go
func TestDecodeLaunchSampledCarriesTheSamplingDenominator(t *testing.T) {
	require.Equal(t, 56, SizeLaunchSampled, "gpu_launch_sampled_v1 is 56 bytes")

	b := make([]byte, SizeLaunchSampled)
	le := binary.LittleEndian
	le.PutUint64(b[0:], 7)          // correlation
	le.PutUint64(b[8:], 0xAAAA)     // kernel_id
	le.PutUint64(b[16:], 3)         // queue_id
	le.PutUint64(b[24:], 1)         // context_id
	le.PutUint64(b[32:], 500)       // time_ns
	le.PutUint32(b[40:], 4242)      // tid
	le.PutUint32(b[44:], 64)        // sample_period
	le.PutUint64(b[48:], 99)        // launch_seq

	got, err := DecodeLaunchSampled(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(7), got.Correlation)
	assert.Equal(t, uint32(4242), got.TID)
	assert.Equal(t, uint32(64), got.SamplePeriod, "the N in one-in-N, per record")
	assert.Equal(t, uint64(99), got.LaunchSeq)
}

func TestDecodeLaunchSampledRejectsZeroSamplePeriod(t *testing.T) {
	b := make([]byte, SizeLaunchSampled)
	binary.LittleEndian.PutUint64(b[0:], 7)
	// sample_period left zero
	_, err := DecodeLaunchSampled(b)
	require.ErrorIs(t, err, ErrInvalidSamplePeriod,
		"a zero denominator would make the scale factor a division by zero")
}

func TestDecodeKernelNameTruncatesAtTheDeclaredLength(t *testing.T) {
	require.Equal(t, 272, SizeKernelName)

	b := make([]byte, SizeKernelName)
	binary.LittleEndian.PutUint64(b[0:], 0xAAAA) // kernel_id
	binary.LittleEndian.PutUint16(b[8:], 5)      // name_len
	copy(b[16:], "_Z4kAddPfi")                   // 10 bytes present, 5 declared

	got, err := DecodeKernelName(b)
	require.NoError(t, err)
	assert.Equal(t, uint64(0xAAAA), got.KernelID)
	assert.Equal(t, "_Z4kA", got.Name, "name_len is authoritative, not the NUL")
	assert.False(t, got.Truncated)
}

func TestDecodeKernelNameFlagsTruncation(t *testing.T) {
	b := make([]byte, SizeKernelName)
	binary.LittleEndian.PutUint16(b[8:], uint16(GPUKernelNameMax))
	b[10] = 1 // truncated flag
	for i := range GPUKernelNameMax {
		b[16+i] = 'x'
	}
	got, err := DecodeKernelName(b)
	require.NoError(t, err)
	assert.True(t, got.Truncated, "a truncated name must be visible, not silently short")
	assert.Len(t, got.Name, GPUKernelNameMax)
}

func TestDecodeKernelNameRejectsLengthPastTheBuffer(t *testing.T) {
	b := make([]byte, SizeKernelName)
	binary.LittleEndian.PutUint16(b[8:], uint16(GPUKernelNameMax+1))
	_, err := DecodeKernelName(b)
	require.Error(t, err, "a length past the fixed array must not read out of bounds")
}
```

- [ ] **Step 2: Run and watch it fail**

```bash
go test ./internal/gpuabi/ -run 'TestDecodeLaunchSampled|TestDecodeKernelName' -v
```

Expected: FAIL — undefined identifiers.

- [ ] **Step 3: Add the C records**

Append to `shim/core/usdt_abi.h`, before the assertions:

```c
// Longest kernel name carried inline. CUDA names are mangled C++ and can
// exceed this; truncation is flagged per record rather than hidden.
#define GPU_KERNEL_NAME_MAX 256

// A launch selected for CPU-stack capture. Fires UNBATCHED, one record per
// probe, so the consumer's bpf_get_stackid captures the stack of the thread
// that made THIS launch. gpu_launch_v1 stays batched and unchanged.
struct gpu_launch_sampled_v1 {
    uint64_t correlation;
    uint64_t kernel_id;
    uint64_t queue_id;
    uint64_t context_id;
    uint64_t time_ns;
    uint32_t tid;
    uint32_t sample_period;   // the N in one-in-N, at capture time; never 0
    uint64_t launch_seq;      // ordinal among ALL launches, sampled or not
};

// kernel_id -> name, emitted once on first sight and replayed on late
// attach. Fixed-size by the ABI's rules; name_len is authoritative.
struct gpu_kernel_name_v1 {
    uint64_t kernel_id;
    uint16_t name_len;
    uint8_t  truncated;
    uint8_t  _pad[5];
    char     name[GPU_KERNEL_NAME_MAX];
};

GPU_STATIC_ASSERT(sizeof(struct gpu_launch_sampled_v1) == 56, "gpu_launch_sampled_v1 layout");
GPU_STATIC_ASSERT(sizeof(struct gpu_kernel_name_v1) == 272, "gpu_kernel_name_v1 layout");
GPU_STATIC_ASSERT(offsetof(struct gpu_launch_sampled_v1, sample_period) == 44, "sample_period position");
```

`launch_seq` is the ordinal among **all** launches. With `sample_period`, it lets the consumer verify the sampler actually delivered one-in-N rather than trusting it.

- [ ] **Step 4: Add the Go mirror**

In `internal/gpuabi/records.go`:

```go
const (
	SizeLaunchSampled = 56
	SizeKernelName    = 272

	// GPUKernelNameMax mirrors GPU_KERNEL_NAME_MAX in shim/core/usdt_abi.h.
	GPUKernelNameMax = 256
)

// ErrInvalidSamplePeriod means a sampled launch declared a zero denominator.
// Scaling by it would divide by zero, so it is rejected at the boundary.
var ErrInvalidSamplePeriod = errors.New("gpuabi: sampled launch has zero sample_period")

type LaunchSampled struct {
	Correlation  uint64
	KernelID     uint64
	QueueID      uint64
	ContextID    uint64
	TimeNs       uint64
	TID          uint32
	SamplePeriod uint32
	LaunchSeq    uint64
}

type KernelName struct {
	KernelID  uint64
	Name      string
	Truncated bool
}

func DecodeLaunchSampled(b []byte) (LaunchSampled, error) {
	if len(b) < SizeLaunchSampled {
		return LaunchSampled{}, ErrShortRecord
	}
	le := binary.LittleEndian
	out := LaunchSampled{
		Correlation:  le.Uint64(b[0:]),
		KernelID:     le.Uint64(b[8:]),
		QueueID:      le.Uint64(b[16:]),
		ContextID:    le.Uint64(b[24:]),
		TimeNs:       le.Uint64(b[32:]),
		TID:          le.Uint32(b[40:]),
		SamplePeriod: le.Uint32(b[44:]),
		LaunchSeq:    le.Uint64(b[48:]),
	}
	if out.SamplePeriod == 0 {
		return LaunchSampled{}, ErrInvalidSamplePeriod
	}
	return out, nil
}

func DecodeKernelName(b []byte) (KernelName, error) {
	if len(b) < SizeKernelName {
		return KernelName{}, ErrShortRecord
	}
	le := binary.LittleEndian
	n := int(le.Uint16(b[8:]))
	if n > GPUKernelNameMax {
		return KernelName{}, fmt.Errorf("gpuabi: kernel name length %d exceeds %d", n, GPUKernelNameMax)
	}
	return KernelName{
		KernelID:  le.Uint64(b[0:]),
		Name:      string(b[16 : 16+n]),
		Truncated: b[10] != 0,
	}, nil
}
```

- [ ] **Step 5: Run both sides**

```bash
go test ./internal/gpuabi/ -v -count=1
cc -std=c11 -Wall -Werror -I shim/core -o /tmp/abi_c shim/core/usdt_abi_test.c && /tmp/abi_c
g++ -std=c++17 -I shim/core -fsyntax-only shim/core/probe_selftest.cc && echo "C++ ok"
```

Expected: all pass. Confirm the assertions still bite by changing one asserted size and seeing both `cc` and `g++` fail, then restore.

- [ ] **Step 6: Commit**

```bash
git add shim/core/usdt_abi.h internal/gpuabi/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(gpuabi): sampled-launch and kernel-name records"
```

---

### Task 2: The shim's sampler and kernel-name table

**Files:**
- Create: `shim/core/sampler.h`, `shim/core/sampler.cc`
- Create: `shim/core/kernelnames.h`
- Test: `shim/core/sampler_test.cc`
- Modify: `shim/Makefile`

**Interfaces:**
- Consumes: `usdt_abi.h`.
- Produces: `perfagent::Sampler` with `bool should_sample()`, `uint32_t period() const`, `void set_period(uint32_t)`, `uint64_t observed() const`, `uint64_t sampled() const`; `perfagent::KernelNameTable` with `bool intern(uint64_t id, const char *name, gpu_kernel_name_v1 *out)`, `void replay(std::function<void(const gpu_kernel_name_v1&)>)`, `size_t size() const`.

The sampler is deterministic one-in-N, not randomized. Randomized sampling would be defensible statistically, but determinism makes the gate reproducible and lets the consumer verify the sampler from `launch_seq` alone. Randomization can come later if bias against periodic launch patterns is ever measured — record that in the header rather than leaving it to be rediscovered.

- [ ] **Step 1: Write the failing test**

`shim/core/sampler_test.cc`:

```cpp
#include "sampler.h"
#include "kernelnames.h"
#include <cassert>
#include <cstdio>
#include <cstring>
#include <vector>

using perfagent::Sampler;
using perfagent::KernelNameTable;

int main() {
    // One in N, deterministically, including the very first launch.
    {
        Sampler s(4);
        int hits = 0;
        for (int i = 0; i < 40; i++) if (s.should_sample()) hits++;
        assert(hits == 10);
        assert(s.observed() == 40);
        assert(s.sampled() == 10);
    }
    // The first launch is always sampled, so a short-lived process is not
    // silently stack-less.
    {
        Sampler s(1000);
        assert(s.should_sample());
    }
    // Period 1 samples everything; period 0 is coerced to 1 rather than
    // dividing by zero or disabling capture silently.
    {
        Sampler s(1);
        for (int i = 0; i < 5; i++) assert(s.should_sample());
        Sampler z(0);
        assert(z.period() == 1);
    }
    // Changing the period mid-run takes effect and does not lose the counts.
    {
        Sampler s(2);
        for (int i = 0; i < 10; i++) s.should_sample();
        s.set_period(5);
        assert(s.period() == 5);
        assert(s.observed() == 10);
    }
    // Interning: first sight yields a record, repeats do not.
    {
        KernelNameTable t;
        gpu_kernel_name_v1 rec {};
        assert(t.intern(0xAAAA, "_Z4kAddPfi", &rec));
        assert(rec.kernel_id == 0xAAAA);
        assert(rec.name_len == 10);
        assert(!rec.truncated);
        assert(memcmp(rec.name, "_Z4kAddPfi", 10) == 0);
        assert(!t.intern(0xAAAA, "_Z4kAddPfi", &rec));
        assert(t.size() == 1);
    }
    // Over-long names are truncated AND flagged.
    {
        KernelNameTable t;
        std::string huge(GPU_KERNEL_NAME_MAX + 50, 'x');
        gpu_kernel_name_v1 rec {};
        assert(t.intern(1, huge.c_str(), &rec));
        assert(rec.name_len == GPU_KERNEL_NAME_MAX);
        assert(rec.truncated == 1);
    }
    // Replay hands back every interned name, for a late-attaching consumer.
    {
        KernelNameTable t;
        gpu_kernel_name_v1 rec {};
        t.intern(1, "a", &rec);
        t.intern(2, "b", &rec);
        std::vector<uint64_t> seen;
        t.replay([&](const gpu_kernel_name_v1 &r) { seen.push_back(r.kernel_id); });
        assert(seen.size() == 2);
    }
    printf("sampler_test OK\n");
    return 0;
}
```

- [ ] **Step 2: Run and watch it fail**

```bash
c++ -std=c++17 -I shim/core -o /tmp/sampler_test shim/core/sampler_test.cc shim/core/sampler.cc
```

Expected: FAIL — no such headers.

- [ ] **Step 3: Implement the sampler**

`shim/core/sampler.h`:

```cpp
// Deterministic one-in-N launch sampling for CPU-stack capture.
//
// Why sampling at all: a batched launch probe cannot carry per-launch
// stacks, and firing it unbatched costs an uprobe trap (~1-2us) per launch,
// which is 0.4-0.8 s/s at the measured 393k launches/s ceiling. Spec §16
// names sampling as the remedy.
//
// Why deterministic rather than randomized: reproducibility. The consumer
// can verify the sampler from launch_seq alone, and the phase gate gets an
// exact expected count. If bias against periodic launch patterns is ever
// measured, randomize here — the ABI carries sample_period per record, so
// the consumer needs no change.
#ifndef PERFAGENT_SAMPLER_H
#define PERFAGENT_SAMPLER_H

#include <atomic>
#include <cstdint>

namespace perfagent {

class Sampler {
public:
    explicit Sampler(uint32_t period) : period_(period ? period : 1) {}

    // Call once per launch. True means capture a stack for this one.
    bool should_sample() {
        const uint64_t n = observed_.fetch_add(1, std::memory_order_relaxed);
        if (n % period_.load(std::memory_order_relaxed) != 0) return false;
        sampled_.fetch_add(1, std::memory_order_relaxed);
        return true;
    }

    uint32_t period() const { return period_.load(std::memory_order_relaxed); }
    void set_period(uint32_t p) { period_.store(p ? p : 1, std::memory_order_relaxed); }
    uint64_t observed() const { return observed_.load(std::memory_order_relaxed); }
    uint64_t sampled() const { return sampled_.load(std::memory_order_relaxed); }

private:
    std::atomic<uint32_t> period_;
    std::atomic<uint64_t> observed_{0};
    std::atomic<uint64_t> sampled_{0};
};

}  // namespace perfagent

#endif
```

The counters are atomic because launches arrive on whatever threads the application uses, and `Batch` established that this shim's state is shared.

`shim/core/sampler.cc`:

```cpp
#include "sampler.h"
```

- [ ] **Step 4: Implement the name table**

`shim/core/kernelnames.h`:

```cpp
// kernel_id -> name interning, with replay for late-attaching consumers.
// Names are variable-length and repeat constantly, so they must not ride on
// every execution record (spec §6.3).
#ifndef PERFAGENT_KERNELNAMES_H
#define PERFAGENT_KERNELNAMES_H

#include "usdt_abi.h"

#include <cstring>
#include <functional>
#include <mutex>
#include <unordered_map>
#include <vector>

namespace perfagent {

class KernelNameTable {
public:
    // Fills *out and returns true the first time this id is seen.
    bool intern(uint64_t id, const char *name, gpu_kernel_name_v1 *out) {
        std::lock_guard<std::mutex> g(mu_);
        if (seen_.count(id)) return false;

        gpu_kernel_name_v1 rec {};
        rec.kernel_id = id;
        size_t n = name ? strlen(name) : 0;
        if (n > GPU_KERNEL_NAME_MAX) { n = GPU_KERNEL_NAME_MAX; rec.truncated = 1; }
        rec.name_len = (uint16_t)n;
        if (n) memcpy(rec.name, name, n);

        seen_.insert(id);
        records_.push_back(rec);
        *out = rec;
        return true;
    }

    void replay(const std::function<void(const gpu_kernel_name_v1 &)> &fn) {
        std::vector<gpu_kernel_name_v1> copy;
        { std::lock_guard<std::mutex> g(mu_); copy = records_; }
        for (const auto &r : copy) fn(r);   // callback outside the lock
    }

    size_t size() const { std::lock_guard<std::mutex> g(mu_); return records_.size(); }

private:
    mutable std::mutex mu_;
    std::unordered_map<uint64_t, char> seen_;
    std::vector<gpu_kernel_name_v1> records_;
};

}  // namespace perfagent

#endif
```

`replay` copies under the lock and invokes the callback outside it. This is deliberate: `ReplayLog` in `drain.h` calls its callbacks while holding its mutex, and a callback that interns a name would deadlock. That hazard was flagged in Phase 3 and deferred; do not reproduce it here.

- [ ] **Step 5: Wire into the Makefile**

Add `core/sampler.cc` to `CORE_SRC`, and add to the `test` target:

```make
	$(CXX) -std=c++17 -I core -o /tmp/sampler_test core/sampler_test.cc core/sampler.cc && /tmp/sampler_test
```

- [ ] **Step 6: Run**

```bash
c++ -std=c++17 -I shim/core -o /tmp/sampler_test shim/core/sampler_test.cc shim/core/sampler.cc && /tmp/sampler_test
make -C shim clean && make -C shim && make -C shim test && make -C shim test-tsan
```

Expected: `sampler_test OK` and all suites green.

- [ ] **Step 7: Commit**

```bash
git add shim/core/sampler.h shim/core/sampler.cc shim/core/sampler_test.cc shim/core/kernelnames.h shim/Makefile
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): deterministic launch sampler and kernel-name interning"
```

---

### Task 3: The stub emits sampled launches and names

**Files:**
- Modify: `shim/stub/stub.cc`

**Interfaces:**
- Consumes: `Sampler`, `KernelNameTable`, the new probes.
- Produces: `perfagent-gpu-stub` emitting `gpu_launch_sampled_v1` unbatched and `gpu_kernel_name_v1` on first sight, alongside the existing batched probes.

- [ ] **Step 1: Extend the stub**

Declare two more semaphores and emitters alongside the existing ones:

```cpp
PERFAGENT_USDT_SEMAPHORE(gpu_launch_sampled_v1);
PERFAGENT_USDT_SEMAPHORE(gpu_kernel_name_v1);
```

The sampled probe fires **one record per probe**, not a batch — the point is that the stack captured belongs to this launch:

```cpp
static void emit_sampled(const void *p, unsigned long n, unsigned long s) {
    PERFAGENT_USDT_PROBE3(gpu_launch_sampled_v1, p, n, s);
}
```

In `perfagent_stub_run`, take a sampling period from a third argument (default 8), intern the two kernel names on first sight, and for each launch:

```cpp
    if (sampler.should_sample() && PERFAGENT_USDT_ENABLED(gpu_launch_sampled_v1)) {
        gpu_launch_sampled_v1 sl {};
        sl.correlation = i;
        sl.kernel_id = l.kernel_id;
        sl.queue_id = 1;
        sl.context_id = 1;
        sl.time_ns = now;
        sl.tid = current_tid();
        sl.sample_period = sampler.period();
        sl.launch_seq = i - 1;              // ordinal among all launches
        emit_sampled(&sl, 1, sampled_seq++);
    }
```

Emit each interned name once, gated on its own semaphore, and register the table with the `Drainer` tick so a late-attaching consumer gets a replay.

Report the sampler's counts on exit alongside the existing drop counts, so the gate can assert them:

```cpp
    fprintf(stderr, "stub: launches=%u observed=%llu sampled=%llu period=%u "
                    "launch_dropped=%llu exec_dropped=%llu\n", ...);
```

- [ ] **Step 2: Build and inspect the probes**

```bash
make -C shim perfagent-gpu-stub
readelf -n shim/perfagent-gpu-stub | grep -E 'Name: gpu|Arguments:'
```

Expected: **four** probes — `gpu_launch_v1`, `gpu_exec_v1`, `gpu_launch_sampled_v1`, `gpu_kernel_name_v1` — every one with `Arguments: 8@%rdi 8@%rsi 8@%rdx`. A missing probe means the `.stapsdt.base` guard or an emitter binding is wrong; fix that rather than proceeding.

- [ ] **Step 3: Confirm the unattached path still does nothing**

```bash
./shim/perfagent-gpu-stub 1000 0 8
```

Expected: every record dropped, `sampled=` counted but nothing emitted — the sampler may run (it is a counter increment) but no probe fires. Confirm from the output that no emission happened.

- [ ] **Step 4: Commit**

```bash
git add shim/stub/stub.cc
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(shim): stub emits sampled launches and interned kernel names"
```

---

### Task 4: The BPF program captures the stack

**Files:**
- Modify: `bpf/gpu_usdt.bpf.c`
- Modify: `gpuprobe/consumer.go`
- Test: `gpuprobe/consumer_test.go`

**Interfaces:**
- Produces: cookies `kindLaunchSampled = 5`, `kindKernelName = 6`; a `BPF_MAP_TYPE_STACK_TRACE` map named `stackmap`; a `stack_id` field in the batch header.

- [ ] **Step 1: Add the stack map and capture**

In `bpf/gpu_usdt.bpf.c`:

```c
#define KIND_LAUNCH_SAMPLED 5
#define KIND_KERNEL_NAME    6

#define PERF_MAX_STACK_DEPTH 127
#define STACK_MAP_SIZE       16384

struct {
    __uint(type, BPF_MAP_TYPE_STACK_TRACE);
    __uint(max_entries, STACK_MAP_SIZE);
    __uint(key_size, sizeof(__u32));
    __uint(value_size, PERF_MAX_STACK_DEPTH * sizeof(__u64));
} stackmap SEC(".maps");
```

Extend `record_size` with `case KIND_LAUNCH_SAMPLED: return 56;` and `case KIND_KERNEL_NAME: return 272;`.

**This moves a verifier-critical constant.** `MAX_RECORD_BYTES` is 48 today because
48 was the largest record; the largest is now 272. Both new probes always fire with
`count == 1`, so real traffic stays far under the reservation — but the clamp must
still be sound for any `count` the program will accept, and `64 * 272 = 17408`
exceeds the current `64 * 48 = 3072` payload budget. Decide explicitly and say which
you chose in your report: either raise `MAX_RECORD_BYTES` to 272 (reservation grows
to `40 + 64*272 = 17448` bytes per batch, which at a 4 MB ring is ~240 batches in
flight — likely too few), or clamp per-kind so a batch's payload budget is
`MAX_RECORDS_PER_BATCH * record_size(kind)` capped at the existing 3072. The second
keeps the reservation where it is and is almost certainly right, since the large
records are the ones that never batch. Whichever you pick, re-derive the reserve,
payload-offset and clamp constants and state all three.

Extend `struct batch_hdr` with `__s32 stack_id; __u32 _pad;` — **append, do not reorder**; the Go decoder's existing offsets must not move. Update `batchHdrSize` on both sides together, and re-derive the reservation arithmetic: the header grows by 8, so the reserve constant grows by 8. State the new numbers in your report.

Capture only for the sampled kind, since it is the only probe that fires per-launch:

```c
    __s32 stack_id = -1;
    if (kind == KIND_LAUNCH_SAMPLED) {
        stack_id = bpf_get_stackid(ctx, &stackmap, BPF_F_USER_STACK);
        if (stack_id < 0) count_drop(KIND_LAUNCH_SAMPLED);  // no silent loss
    }
    hdr->stack_id = stack_id;
```

A negative `stack_id` is a real outcome — a stack too deep, a map full, or a frame-pointer-less binary. It is counted, and the record still flows with `stack_id = -1` so the launch is not lost merely because its stack was.

- [ ] **Step 2: Regenerate and re-verify bounds**

```bash
cd gpuprobe && go generate ./... && cd ..
llvm-objdump -d gpuprobe/gpuusdt_x86_bpfel.o | head -60
```

Confirm the reserve size, the payload offset and the clamp are all consistent with the new 40-byte header. Report the three numbers.

- [ ] **Step 3: Write the failing Go test**

```go
func TestDecodeBatchCarriesTheStackID(t *testing.T) {
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunchSampled)
	putU32(buf[0:], kindLaunchSampled)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeLaunchSampled))
	putU32(buf[32:], uint32(int32(4242)))          // stack_id
	putU64(buf[batchHdrSize:], 7)                  // correlation
	putU32(buf[batchHdrSize+44:], 8)               // sample_period

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	assert.Equal(t, int32(4242), b.StackID)
	require.Len(t, b.SampledLaunches, 1)
	assert.Equal(t, uint32(8), b.SampledLaunches[0].SamplePeriod)
}

func TestMissingStackIsCountedNotDropped(t *testing.T) {
	c := &Consumer{seqByStream: map[seqKey]uint64{}, cfg: Config{Sink: &countingSink{}}}
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunchSampled)
	putU32(buf[0:], kindLaunchSampled)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeLaunchSampled))
	putU32(buf[32:], uint32(int32(-1)))            // capture failed
	putU64(buf[batchHdrSize:], 7)
	putU32(buf[batchHdrSize+44:], 8)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	assert.Equal(t, uint64(1), c.Stats().StacksMissing,
		"a launch whose stack capture failed is still a launch")
	assert.Equal(t, uint64(1), c.Stats().Records)
}
```

- [ ] **Step 4: Implement decode and cookies**

Extend `cookieFor` with the two new probe names, `batch` with `StackID int32`, `SampledLaunches []gpuabi.LaunchSampled` and `KernelNames []gpuabi.KernelName`, and `decodeBatch` to read the header's `stack_id` and stride the new record sizes. Add `StacksMissing` to `Stats`.

- [ ] **Step 5: Run**

```bash
go test ./gpuprobe/ -v -count=1 && go test ./gpuprobe/ -race -count=1
```

- [ ] **Step 6: Commit**

```bash
git add bpf/gpu_usdt.bpf.c gpuprobe/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(gpuprobe): capture the launching thread's stack at sampled launches"
```

---

### Task 5: Symbolize, and keep the two populations separable

**Files:**
- Modify: `gpuprobe/consumer.go`
- Modify: `gpu/types.go`, `gpu/projection.go`
- Test: `gpuprobe/consumer_test.go`, `gpu/projection_test.go`

**Interfaces:**
- Consumes: `symbolize.Symbolizer`, `internal/bpfstack.ExtractIPs`, `symbolize.ToProfFrames`.
- Produces: `Config.Symbolizer symbolize.Symbolizer`; `gpu.LaunchContext.SamplePeriod uint32`; a `[gpu:launch unsampled]` projection node.

This is the task the honesty rule lands in. Resolve a stack ID exactly as `profile/` does:

```go
raw, err := c.objs.Stackmap.LookupBytes(uint32(stackID))
ips := bpfstack.ExtractIPs(raw)
frames, err := c.cfg.Symbolizer.SymbolizeProcess(pid, ips)
cpuStack := symbolize.ToProfFrames(frames)
```

and put it on `GPUKernelLaunch.Launch.CPUStack`, with `SamplePeriod` alongside so the scale factor travels with the stack rather than being reconstructed later.

**The projection rule.** `ProjectExecutions` currently emits one sample per execution. It now emits into two populations:

- An execution whose launch carried a sampled stack: frames are the real CPU stack, then `[gpu:launch]`, then `[gpu:kernel:<name>]`. Its value is the execution's own duration — **not** scaled. Scaling here would inflate a measured duration into an estimate.
- An execution with no sampled stack: frames are `[gpu:launch unsampled]` then `[gpu:kernel:<name>]`. Its value is its own duration.

Both populations keep exact durations; what differs is whether a CPU call path is claimed. The reader sees the true GPU total, split into the part attributable to a call path and the part that is not, with the ratio implied by the sampling period. A per-sample label carries `sample_period` so a consumer that wants a scaled estimate can compute one deliberately.

- [ ] **Step 1: Write the failing projection test**

```go
func TestUnsampledExecutionsGetTheirOwnNodeNotAFabricatedStack(t *testing.T) {
	snap := Snapshot{Executions: []ExecutionView{
		{Exec: GPUKernelExec{StartNs: 0, EndNs: 100, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{KernelName: "kAdd", Launch: LaunchContext{
				CPUStack: []pp.Frame{{Name: "main"}, {Name: "run"}}, SamplePeriod: 8}}},
		{Exec: GPUKernelExec{StartNs: 100, EndNs: 300, KernelName: "kAdd"},
			Launch: &GPUKernelLaunch{KernelName: "kAdd"}}, // joined, no stack
	}}

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 2)

	var attributed, unattributed uint64
	for _, s := range samples {
		names := frameNames(s.Stack)
		if slices.Contains(names, "[gpu:launch unsampled]") {
			unattributed += s.Value
			assert.NotContains(t, names, "main", "an unsampled launch must not borrow a stack")
		} else {
			attributed += s.Value
			assert.Contains(t, names, "main")
			assert.Equal(t, "8", s.Labels["gpu_sample_period"])
		}
	}
	assert.Equal(t, uint64(100), attributed, "durations are never scaled")
	assert.Equal(t, uint64(200), unattributed)
	assert.Equal(t, uint64(300), attributed+unattributed, "the GPU total stays exact")
}
```

- [ ] **Step 2: Run, watch it fail, implement, run again**

```bash
go test ./gpu/ -run TestUnsampled -v -count=1
```

- [ ] **Step 3: Commit**

```bash
git add gpu/ gpuprobe/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "feat(gpu): symbolized sampled stacks, with unattributed GPU time kept separate"
```

---

### Task 6: The gate — a CPU+GPU flame graph with no GPU

**Files:**
- Modify: `gpuprobe/gate_test.go`
- Modify: `cmd/gpu-stub-profile/main.go`

- [ ] **Step 1: Extend the gate**

Keep every Phase 3 assertion. Add, against a stub run with `sample_period = 8` and 500 launches:

- `Stats.SampledLaunches == 63` (`ceil(500/8)`, deterministic because the sampler is), asserted exactly.
- The stub's stderr `observed=`/`sampled=` agree with what the consumer counted — the producer and consumer must not disagree about how many stacks were taken.
- Every sampled launch's `LaunchContext.CPUStack` is non-empty and contains a frame naming the stub's own launch function. The stub is built with frame pointers, so a failure here means the capture or symbolization is broken, not the workload.
- `Stats.StacksMissing == 0`.
- Every execution carries a non-empty `KernelName`, resolved through interning.
- The two projected populations sum to the exact total GPU duration.

- [ ] **Step 2: Build privileged and run**

```bash
go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test \
  -ldflags '-linkmode external -extldflags "-Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"'
sudo setcap cap_bpf,cap_perfmon+ep /home/diego/gpuprobe.test
cd gpuprobe && /home/diego/gpuprobe.test -test.run TestStubDrives -test.v -test.timeout=120s
```

- [ ] **Step 3: Look at the flame graph**

```bash
go build -o /home/diego/gpu-stub-profile ./cmd/gpu-stub-profile
sudo setcap cap_bpf,cap_perfmon+ep /home/diego/gpu-stub-profile
/home/diego/gpu-stub-profile && go tool pprof -top gpu-stub.pb.gz | head -25
```

Expected: real CPU frames from the stub's own call path, under them `[gpu:launch]`, under that `[gpu:kernel:_Z4kAddPfi]` — and a separate `[gpu:launch unsampled]` subtree holding the rest of the GPU time. **That is the first CPU+GPU flame graph this project has produced**, on a machine with no GPU.

- [ ] **Step 4: Commit**

```bash
git add gpuprobe/gate_test.go cmd/gpu-stub-profile/
git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit -m "test(gpuprobe): gate asserts sampled CPU stacks and kernel names"
```

---

## Phase gate

1. `go test ./internal/gpuabi/ ./internal/usdt/ ./gpuprobe/ ./gpu/` passes; `make -C shim test` and `make -C shim test-tsan` pass.
2. The gate test passes with the sampled-stack and kernel-name assertions, on a machine with no GPU.
3. `gpu-stub-profile` writes a pprof containing real CPU frames above `[gpu:launch]` above `[gpu:kernel:<name>]`, plus a distinct `[gpu:launch unsampled]` subtree.
4. Attributed and unattributed GPU time sum to the exact measured total; no duration is scaled.
5. `getcap` on the test binary shows `cap_bpf,cap_perfmon` and **no `cap_sys_admin`**.
6. `golangci-lint run --timeout=5m` (v2.11.4, CI's pin) reports 0 issues.
7. The unattached stub still emits nothing and counts every record dropped.

## Deferred to Phase 4b and beyond

- **The DWARF unwind driver.** Frame-pointer stacks via `bpf_get_stackid` cover binaries built with frame pointers; the third `unwind_common.h` driver covers the rest. The wire format does not change — `stack_id` becomes a walker handle instead — so this is additive.
- The CUPTI adapter itself, and everything needing a GPU.
- An adaptive sampling controller. The period is fixed here; §9's controller belongs with real launch-rate data.
- PC sampling, producer-side drop surfacing, arm64.
