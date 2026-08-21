# GPU V2 Phase 4b — DWARF Stacks Through the Vendor Libraries Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Recover the *application's* CPU call path for a sampled GPU launch, by walking the stack with the DWARF unwinder instead of frame pointers — so a CUDA flame graph attributes GPU time to the code that caused it rather than to nothing.

**Architecture:** `bpf/unwind_common.h` already carries the whole hybrid walker — `walk_step`, the CFI lookup, the mapping classifier, the per-CPU scratch — and two programs already drive it: `perf_dwarf.bpf.c` (a `perf_event` sampler) and `offcpu_dwarf.bpf.c` (a `tp_btf` tracepoint). This phase adds the **third driver**, inside the existing `SEC("uprobe.multi")` program, and swaps `bpf_get_stackid` for it. The wire format does not change: the batch header's `stack_id` becomes a handle into a map this program owns rather than a kernel stackmap key.

**Tech Stack:** Go 1.26, `github.com/cilium/ebpf` v0.21, C for the BPF program, clang/bpf2go, blazesym via `symbolize.Symbolizer`, testify.

**Spec:** `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` (§4 invariants, §8 output representation, §11 deployment and capabilities, §14 Phase 4)

**Predecessors:** Phase 3 (#35), Phase 4a (#37), the attribution guard (#39), and the CUPTI adapter (#40) — all on `main`.

## Why this phase is required, not optional

Phase 4a shipped frame-pointer stacks on the reasoning that they were a lower-fidelity first cut. Run against a real CUDA workload on an RTX 3090, they were not a lower-fidelity cut — they were an actively misleading one:

```
[gpu:kernel:_Z14perfagent_axpyfPKfPfi]                     3089.52us  51.60%
[gpu:kernel:_Z15perfagent_scalePffi]                       2898.07us  48.40%
(anonymous namespace)::on_callback(void*, CUpti_CallbackDomain, ...)  772.77us  12.91%
```

The only surviving CPU frame was **the profiler's own CUPTI callback**. The chain from the probe to the application is `probe → our callback → libcupti → libcudart cudaLaunchKernel → application`, and a frame-pointer walk stops at the first frame it cannot follow.

`main` currently ships a guard (#39) that refuses such a stack rather than attributing GPU time to the profiler, so the output is honest — and empty. This phase is what makes it non-empty.

## Global Constraints

- Go 1.26.0+. CGO required; export the block in "Build environment" before any `go` command.
- **Linux 6.6+**, and attach stays `link.UprobeMulti`. Never `link.Uprobe` / the `perf_uprobe` PMU — it requires `CAP_SYS_ADMIN`.
- **`CAP_BPF`, `CAP_PERFMON`, `CAP_CHECKPOINT_RESTORE`** — the project's documented set. `CAP_CHECKPOINT_RESTORE` is required for symbolization (`symbolize.NewLocalSymbolizer` errors without it). Do not add `CAP_SYS_ADMIN`; nothing here is simplified by it.
- `ProgramSpec.KernelVersion` set explicitly on every `ebpf.Kprobe`-type program.
- **The USDT wire format is frozen.** `gpu_launch_sampled_v1` is 56 bytes; the batch header is 40 bytes with `stack_id` at offset 32. This phase changes what `stack_id` *means*, not its size or position.
- **No loss is ever silent.** Every walk failure, truncation, map-full and lookup miss increments a counter reachable from `gpuprobe.Stats`.
- **A sampled measurement is never presented as exhaustive**, and no duration is ever scaled. `attributed + unattributed` must still equal the exact measured total.
- Do not commit `CLAUDE.md`. No `Co-Authored-By`. Commit with `git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit`.

## Build environment

```bash
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release
```

Privileged test binaries: build **outside `/tmp`** (nosuid) and link blazesym statically, because a setcap'd binary ignores `LD_LIBRARY_PATH`. The ordinary `-extldflags` recipe does **not** work — the vendored blazesym package hardcodes `#cgo LDFLAGS: -lblazesym_c`, so stage a directory holding only the `.a`:

```bash
mkdir -p /tmp/bzstatic && cp /home/diego/github/blazesym/target/release/libblazesym_c.a /tmp/bzstatic/
CGO_LDFLAGS="-L/tmp/bzstatic -lblazesym_c" go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep /home/diego/gpuprobe.test
```
Verify with `readelf -d /home/diego/gpuprobe.test | grep -c blazesym` → `0`. **Rebuilding strips the capability bit**; do source work first, rebuild once, then re-`setcap`.

## What already exists — do not rewrite

- `bpf/unwind_common.h`: `struct walk_ctx{pc,fp,sp,pid,n_pcs,rec}`, `walk_step` (the `bpf_loop` callback — **it lives in the header, despite a stale comment saying otherwise**), `struct sample_record`/`sample_header`, `walker_scratch` (per-CPU), `stack_events` (ringbuf), `kern_stackmap`, `pids` (`BPF_MAP_TYPE_HASH`, `__u32` → `struct pid_config{type,collect_user,collect_kernel}`), `cfi_miss_events`, `mapping_for_pc`, `classify_rel_pc`, `cfi_lookup`, `emit_cfi_miss`, `MAX_FRAMES`.
- The existing drive pattern, which this phase copies:
  ```c
  rec->hdr.walker_flags = 0;
  bpf_loop(MAX_FRAMES, walk_step, &walker, 0);
  rec->hdr.n_pcs = (__u8)(walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs);
  rec->hdr.mode = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED) ? … : …;
  ```
- `bpf/gpu_usdt.bpf.c`: `SEC("uprobe.multi")`, cookie → record size, per-kind count caps, a 40-byte batch header with `stack_id` at offset 32, `stackmap` (`BPF_MAP_TYPE_STACK_TRACE`), `stacks_missing`, and reservation constants **reserve 3112 / payload at +40 / clamp 3072**.
- `gpuprobe/consumer.go`: `resolveStackLocked` (looks up `stackmap`, deletes the entry, extracts IPs with `internal/bpfstack.ExtractIPs`, symbolizes, converts with `symbolize.ToProfFrames`), the `(pid, correlation)`-keyed side tables, the profiler-only guard (`shimscope.go`), and the counters.
- `unwind/ehcompile` + `unwind/procmap`: build the CFI tables the walker consults, and populate the maps.

## The decisions this plan locks in

**`stack_id` becomes a handle into our own map.** `bpf_get_stackid` populates a kernel `BPF_MAP_TYPE_STACK_TRACE`; a hand-rolled walk cannot. So the driver walks into `walker_scratch`, copies the resulting PCs into a `gpu_stacks` map keyed by an id it generates, and stores that id in the header. The wire format is untouched, `resolveStackLocked` changes which map it reads, and the delete-on-read discipline carries over unchanged.

**Frame pointers stay as the fallback, not as a rival.** `walk_step` is already a *hybrid* walker: it classifies each frame and picks FP or CFI per frame. That is precisely why the vendor libraries are the hard case and the application is not — so there is nothing to choose between; drive the hybrid walker and let it decide.

**The consumer must register PIDs.** `walk_step` consults the per-PID CFI tables — `pid_mappings` / `pid_mapping_lengths` (via `mapping_for_pc`) and the four `cfi_*` maps. It does **not** consult `pids`: that map is a sampling whitelist, read only by `perf_dwarf.bpf.c` and `offcpu_dwarf.bpf.c`, which hook events that fire for every process on the machine and therefore need one. A `uprobe.multi` on the shim inode only fires in processes that mapped the shim, so gpuprobe needs no whitelist and must not populate `pids` — doing so would be dead work that looks like the task was done. Nothing in `gpuprobe` populates the CFI tables today, because `bpf_get_stackid` needed no setup. This is the integration work of the phase, and Task 3 is where it lands.

---

### Task 1: A `gpu_stacks` map, and the walk that fills it

**Files:**
- Modify: `bpf/gpu_usdt.bpf.c`
- Modify: `gpuprobe/consumer.go`
- Test: `gpuprobe/consumer_test.go`

**Interfaces:**
- Produces (BPF): a `gpu_stacks` map (`__u32` id → `__u64[MAX_FRAMES]` plus a length), a per-CPU id counter, and `stack_id` semantics as a handle into it.
- Produces (Go): `resolveStackLocked` reading `gpu_stacks`; `Stats.StackWalkTruncated`, `Stats.StackWalkEmpty`, `Stats.StackMapFull`.

Keep `bpf_get_stackid` available behind a flag for one task if that helps you compare, but the committed default is the walker.

- [ ] **Step 1: Write the failing Go test**

```go
func TestResolveReadsTheWalkerMapAndDeletesTheEntry(t *testing.T) {
	stacks := &fakeStackStore{entries: map[uint32][]uint64{
		7: {0x401000, 0x401100, 0x7f0000001000},
	}}
	c := newTestConsumer(t, withStackStore(stacks))

	frames, ok := c.resolveStackForTest(7, 4242)
	require.True(t, ok)
	require.Len(t, frames, 3)
	assert.Zero(t, c.Stats().StackLookupFailed)
	assert.NotContains(t, stacks.entries, uint32(7),
		"a resolved entry must be deleted so the id can be reused")
}

func TestAnEmptyWalkIsCountedNotAttached(t *testing.T) {
	stacks := &fakeStackStore{entries: map[uint32][]uint64{9: {}}}
	c := newTestConsumer(t, withStackStore(stacks))

	_, ok := c.resolveStackForTest(9, 4242)
	assert.False(t, ok, "a walk that produced no frames is not an attribution")
	assert.Equal(t, uint64(1), c.Stats().StackWalkEmpty)
}

func TestATruncatedWalkIsCountedButStillUsed(t *testing.T) {
	// MAX_FRAMES worth of PCs means the walk hit its bound; the frames we
	// have are real and worth attributing, but the truncation is visible.
	full := make([]uint64, maxWalkFrames)
	for i := range full {
		full[i] = uint64(0x401000 + i*16)
	}
	stacks := &fakeStackStore{entries: map[uint32][]uint64{11: full}}
	c := newTestConsumer(t, withStackStore(stacks))

	frames, ok := c.resolveStackForTest(11, 4242)
	require.True(t, ok)
	assert.Len(t, frames, maxWalkFrames)
	assert.Equal(t, uint64(1), c.Stats().StackWalkTruncated)
}
```

`fakeStackStore` implements the same small interface the real `gpu_stacks` map satisfies — follow the existing `batchReader` seam in `consumer.go` for the pattern.

- [ ] **Step 2: Run and watch it fail**

```bash
go test ./gpuprobe/ -run 'TestResolveReadsTheWalker|TestAnEmptyWalk|TestATruncatedWalk' -v
```

- [ ] **Step 3: Add the map and the walk to `bpf/gpu_usdt.bpf.c`**

Include `unwind_common.h`. Add:

```c
struct gpu_stack {
    __u32 n_pcs;
    __u32 _pad;
    __u64 pcs[MAX_FRAMES];
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u32);
    __type(value, struct gpu_stack);
} gpu_stacks SEC(".maps");
```

`MAX_FRAMES` is **127**, so each value is `8 + 127*8 = 1024` bytes and 4096 entries
reserve **4 MB**. That is the same order as the existing ringbuf, and it is only
reachable if the consumer stops draining — but size it deliberately rather than by
accident, and say in your report what happens when the map fills (it must count, not
silently overwrite a live entry).

Replace the `bpf_get_stackid` call for `KIND_LAUNCH_SAMPLED` with: fetch the per-CPU `walker_scratch` record, zero `walker_flags`, fill a `struct walk_ctx` from `PT_REGS_IP/FP/SP` and the current tgid, run `bpf_loop(MAX_FRAMES, walk_step, &walker, 0)`, then copy `walker.n_pcs` PCs into a `struct gpu_stack` and `bpf_map_update_elem` it under a fresh id. Put that id in `hdr->stack_id`; on any failure store `-1` and count it, exactly as the current code does for a failed `bpf_get_stackid`.

Generate ids so two CPUs cannot collide — a per-CPU counter combined with the CPU index is the obvious construction. Say in your report how you guarantee uniqueness, and what happens on wrap.

**Re-derive the reservation constants and state all three in your report.** The header does not change size, so reserve **3112**, payload at **+40**, clamp **3072** should all hold — confirm from the disassembly rather than assuming, because adding a large map and a `bpf_loop` changes register pressure and instruction count.

- [ ] **Step 4: Point `resolveStackLocked` at the new map**

The read path keeps its shape — look up, **delete**, convert to IPs, symbolize, `ToProfFrames`. Only the source map and the decoding of the value change (a `struct gpu_stack` rather than a raw stackmap value, so `internal/bpfstack.ExtractIPs` may not be the right decoder any more — say which you used and why).

Preserve the delete-on-read discipline: the id space is finite and nothing else reclaims it.

- [ ] **Step 5: Regenerate, run, verify bounds**

```bash
cd gpuprobe && go generate ./... && cd ..
go build ./... && go vet ./gpuprobe/
go test ./gpuprobe/ -v -count=1
go test ./gpuprobe/ -race -count=1
llvm-objdump -d gpuprobe/gpuusdt_x86_bpfel.o | head -120
```

The program is now substantially larger. Report the instruction count and say whether you believe it will still verify — `bpf_loop` is a helper call, so the walk does not inflate the main program's instruction budget, but the surrounding code does.

- [ ] **Step 6: Commit**

---

### Task 2: Truthful mode reporting

**Files:**
- Modify: `bpf/gpu_usdt.bpf.c`, `gpuprobe/consumer.go`
- Test: `gpuprobe/consumer_test.go`

`walk_step` sets `WALKER_FLAG_DWARF_USED` when it actually used CFI for a frame. The existing drivers surface that as `rec->hdr.mode`. A stack walked entirely by frame pointers is exactly the case that produced the misleading CUDA output, so the distinction must survive to the consumer.

- [ ] **Step 1: Carry the walker flags out**

Store the walk's flags alongside the PCs in `struct gpu_stack`, and surface them per resolved stack. Add `Stats.StacksWalkedDWARF` and `Stats.StacksWalkedFPOnly`.

- [ ] **Step 2: Test that an FP-only walk is distinguishable**

```go
// WALKER_FLAG_DWARF_USED is 0x02 in bpf/unwind_common.h; 0x01 is
// FP_TERMINATED and 0x04 is CFI_MISS.
const (
	walkerFlagFPTerminated = 0x01
	walkerFlagDWARFUsed    = 0x02
)

func TestFPOnlyAndDWARFWalksAreCountedSeparately(t *testing.T) {
	// Identical PCs; only the flags the walker reported differ. An FP-only
	// walk through vendor libraries is exactly the case that produced a
	// profiler-only stack on real hardware, so the two must be countable
	// apart — a single "we got a stack" counter cannot express it.
	pcs := []uint64{0x401000, 0x401100}
	stacks := &fakeStackStore{
		entries: map[uint32][]uint64{1: pcs, 2: pcs},
		flags:   map[uint32]uint32{1: walkerFlagDWARFUsed, 2: walkerFlagFPTerminated},
	}
	c := newTestConsumer(t, withStackStore(stacks))

	_, ok := c.resolveStackForTest(1, 4242)
	require.True(t, ok)
	_, ok = c.resolveStackForTest(2, 4242)
	require.True(t, ok)

	assert.Equal(t, uint64(1), c.Stats().StacksWalkedDWARF)
	assert.Equal(t, uint64(1), c.Stats().StacksWalkedFPOnly)
}
```

- [ ] **Step 3: Run, then commit**

---

### Task 3: Register PIDs and CFI tables with the walker

**Files:**
- Modify: `gpuprobe/consumer.go`, `gpuprobe/attach.go` (or wherever `Attach` lives)
- Test: `gpuprobe/consumer_test.go`

**This is the integration work of the phase.** `walk_step` consults the per-PID mapping tables (`pid_mappings`, `pid_mapping_lengths`) and the four `cfi_*` tables built by `unwind/ehcompile` and `unwind/procmap`. It does **not** consult `pids` — see the correction in the preamble; that map is the samplers' whitelist and gpuprobe has no use for it. `bpf_get_stackid` needed none of that, so `gpuprobe` populates none of it today — and a walker with no tables silently degrades to the frame-pointer path, i.e. straight back to the bug this phase exists to fix.

> **This task is deliberately investigation-first.** Unlike Tasks 1 and 2, it does
> not carry the code to write, because the mechanism already exists in
> `profile/` and `unwind/dwarfagent/` and must be *reused*, not reinvented. The
> deliverable of Step 1 is a written answer — which map, populated by which call,
> at which point in the lifecycle — and Steps 2-3 build on that answer. If Step 1
> concludes the existing mechanism cannot be reused from `gpuprobe`, stop and say
> why rather than growing a parallel one.

- [ ] **Step 1: Establish how the existing agents do it**

Read how `profile/` and `unwind/dwarfagent/` attach CFI tables (and, for the samplers only, populate `pids`) — including `AttachAllMappings` and the lazy-attach path visible in the integration logs. **Do not invent a second mechanism.** Report what you found and which of it `gpuprobe` must reuse.

- [ ] **Step 2: Register the profiled PID(s)**

For `Config.PID != 0`, register that PID. For system-wide (`PID == 0`), a PID becomes interesting the first time a sampled launch arrives from it — register lazily on first sight and say what bounds the registration set.

Because the walker needs tables *before* the first sample it will unwind, state plainly in your report what happens to the first launch or two from a newly-seen process, and make it a counted outcome rather than a silent FP-only walk.

- [ ] **Step 3: Test that an unregistered PID is visible**

A stack walked for a PID with no CFI tables must be counted (`StacksWalkedFPOnly` at minimum), not silently attributed as though it were a DWARF walk.

- [ ] **Step 4: Run, then commit**

---

### Task 4: The gate — a real application call path

**Files:**
- Modify: `gpuprobe/gate_test.go`
- Modify: `shim/Makefile` (if the stub needs rebuilding without frame pointers)

The Phase 4a gate proves the pipeline with a stub whose stacks the FP walker can already follow. That does **not** exercise this phase: the stub would pass with frame pointers alone.

- [ ] **Step 1: Make the gate require a DWARF walk**

Keep every Phase 4a assertion, and add: at least one sampled launch resolved through a stack where `WALKER_FLAG_DWARF_USED` was set (`Stats.StacksWalkedDWARF > 0`). Otherwise the gate cannot distinguish this phase from the previous one.

The cleanest way to force it is a producer whose call path the FP walker cannot follow. Build a small helper compiled **`-fomit-frame-pointer`** that calls into the stub's emit path, so the stack crosses an FP-less frame exactly as the CUDA path does. Add it to `shim/` and drive it from the gate.

- [ ] **Step 2: Assert the guard now passes rather than refusing**

`Stats.StacksProfilerOnly` must be zero for this run: with a working DWARF walk, the stack reaches the application, so the #39 guard should no longer refuse it. **That assertion is the real proof of this phase** — the guard was measuring the defect, so the guard going quiet is the fix landing.

- [ ] **Step 3: Run under capabilities**

```bash
CGO_LDFLAGS="-L/tmp/bzstatic -lblazesym_c" go test -c ./gpuprobe/ -o /home/diego/gpuprobe.test
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep /home/diego/gpuprobe.test
cd gpuprobe && /home/diego/gpuprobe.test -test.v -test.timeout=200s
```

- [ ] **Step 4: Commit**

---

### Task 5: Prove it on the GPU

**Files:**
- Modify: `.superpowers/sdd/` report only, unless the run exposes a defect.

The stub gate can be satisfied by construction. The CUDA path is what motivated the phase, and only real hardware settles it.

- [ ] **Step 1: Run the CUPTI adapter end to end**

```bash
sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep /home/diego/gpu-cuda-profile
/home/diego/gpu-cuda-profile \
  -shim  <worktree>/shim/libperfagent-gpu-nvidia.so \
  -workload <worktree>/shim/nvidia/testdata/cuda_workload \
  -iters 2000 -sleep-us 200 -period 8 -out /home/diego/gpu-cuda.pb.gz
go tool pprof -top /home/diego/gpu-cuda.pb.gz | head -25
```

- [ ] **Step 2: Judge the output honestly**

Success is a frame from `cuda_workload`'s **own** call path — not `on_callback`, not only libcupti. Report the pprof verbatim.

If the walk still stops inside the vendor libraries, say so plainly and do not adjust an assertion to make it look otherwise. NVIDIA ships those libraries without CFI in some builds, and `.eh_frame` absence is a real possibility — `readelf -S libcupti.so.13 | grep eh_frame` answers it, and that answer belongs in the report either way. A negative result here is a genuine finding: it would mean the CUDA path needs the vendor's own unwind data, and the honest fallback is the #39 guard continuing to refuse attribution.

- [ ] **Step 3: Record the result**

---

## Phase gate

1. `go test ./gpuprobe/ ./gpu/ ./internal/...`, `-race`, `make -C shim test` and `test-tsan` all pass; `golangci-lint` 0 issues.
2. The gate passes under `cap_bpf,cap_perfmon,cap_checkpoint_restore` with **no `cap_sys_admin`**, including `Stats.StacksWalkedDWARF > 0` and `Stats.StacksProfilerOnly == 0`.
3. Attributed and unattributed GPU time still sum to the exact measured total; no duration is scaled.
4. Every walk failure mode is counted and reachable from `Stats`.
5. The CUDA run's result is recorded verbatim — success **or** a documented negative with the `.eh_frame` evidence.

## Deferred

- Kernel stacks for GPU launches (`kern_stackmap` is already in `unwind_common.h`; nothing needs them yet).
- arm64: the probe's register pinning and `PT_REGS_PARM` reads are x86-64-only, and bpf2go builds arm64. Tracked separately.
- The PID-blindness in `gpu.LaunchCache` / `Timeline.pending` (#36).
- Any change to the sampling period controller; the period stays fixed and configurable.
