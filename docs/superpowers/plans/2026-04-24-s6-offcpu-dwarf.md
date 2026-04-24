# S6: Off-CPU DWARF Variant Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** make `perf-agent --offcpu --unwind dwarf --pid N` end-to-end — the existing off-CPU profiler (FP-based via `bpf_get_stackid`) gains a DWARF-capable sibling that walks user stacks through libpthread / glibc-futex / tokio internals using the S3 hybrid walker.

**Architecture:** a new `bpf/offcpu_dwarf.bpf.c` hooks `sched_switch` tracepoint. On switch-OUT it runs the same `walk_step` hybrid walker used by perf_dwarf (against `bpf_task_pt_regs(prev)` — the user-space registers saved when the task entered kernel), stashes the full sample in an `offcpu_start` HASH keyed by `(pid, tgid)`, and on switch-IN computes elapsed blocking-ns and emits via the `stack_events` ringbuf with `value = delta_ns`. Userspace reuses the S4 ehmaps lifecycle (TableStore, PIDTracker, MmapWatcher) and dwarfagent's sample parser + symbolizer.

**Tech Stack:** Go 1.26, cilium/ebpf v0.21.0, clang+libbpf+CO-RE, existing `unwind/ehmaps` / `unwind/dwarfagent` / `pprof` packages.

---

## Scope

**S6 delivers:** end users can run `perf-agent --offcpu --unwind dwarf --pid N` and get an off-CPU pprof that shows blocking-ns by DWARF-unwound stack. `--offcpu --unwind fp` (today's default) keeps working unchanged.

**Explicitly NOT in S6:**
- Sharing the CFI/classification/pid_mappings BPF maps between perf_dwarf and offcpu_dwarf. Each program owns its own instance; when both are active, userspace compiles CFI twice and populates both. Acceptable overhead; S8 cleanup can unify via map pinning.
- System-wide `-a --offcpu --unwind dwarf` — S7.
- Kernel-side stack in off-CPU samples (we match the existing `offcpu.Profiler`'s user-only posture).

## Background for implementers

**Existing off-CPU shape (`bpf/offcpu.bpf.c`):** hooks `tp_btf/sched_switch`, uses `bpf_get_stackid(ctx, &stackmap, BPF_F_USER_STACK)` to capture prev's FP-walked user stack on switch-OUT, aggregates in `offcpu_counts` map keyed by `(pid, kern_stack_id, user_stack_id)`, userspace reads counts + stack IDs and symbolizes post-hoc. Value is blocking-ns.

**Why we can't use `walk_step` directly:** `walk_step` in `unwind_common.h` takes a `walk_ctx` (bpf_loop callback arg) and walks the CURRENT task's user memory via `bpf_probe_read_user(ptr)`. That helper reads CURRENT user's address space. At `tp_btf/sched_switch`, `current` is still `prev` (the scheduler hasn't finished the switch yet), so `bpf_probe_read_user` reads prev's memory — exactly what we want.

**Why we can't just reuse perf_dwarf's sample flow:** perf_dwarf emits the sample synchronously in the same BPF invocation. Off-CPU needs two steps:
1. Switch-OUT: walk prev's stack now, stash `(timestamp, sample_record)` in a map.
2. Switch-IN: compute `now - timestamp`, fetch stashed sample, emit via ringbuf with `value = delta_ns`.

A single-step design (walk on switch-IN with `bpf_task_pt_regs(prev_task_pointer)`) is feasible but adds complexity: prev might have been destroyed by the time we reach switch-IN; also `bpf_probe_read_user` needs current==prev to work. Two-step is simpler.

**Helper availability:** `bpf_task_pt_regs(task)` — kernel 5.15+. We're 6.0+, so safe.

**Map sharing:** perf_dwarf and offcpu_dwarf both include `unwind_common.h`. Each BPF ELF is compiled separately by bpf2go; each gets its OWN instances of all maps declared in the header (cfi_rules, pid_mappings, pids, stack_events, walker_scratch, etc.). When both profilers are active:
- `dwarfagent.Profiler` populates its own cfi_rules/pid_mappings via its TableStore + PIDTracker.
- `dwarfagent.OffCPUProfiler` populates its own via a separate TableStore + PIDTracker.
- CFI gets compiled twice. 2× CPU + 2× memory at agent startup. Acceptable for S6; S8 cleanup.

**Sample record reuse:** the existing `struct sample_record` (32-byte header + 127×u64 PCs = 1032 bytes) fits off-CPU semantics directly. `value` holds blocking-ns instead of 1. Userspace parseSample already handles the header correctly — no Go-side changes needed.

## File Structure

```
bpf/offcpu_dwarf.bpf.c                  CREATE — tp_btf/sched_switch handler; offcpu_start map (pid,tgid → sample_record); walk on switch-OUT, emit on switch-IN.

profile/gen.go                          MODIFY — add bpf2go directives for offcpu_dwarf (amd64 + arm64).
profile/offcpu_dwarf_{x86,arm64}_bpfel.{go,o}  REGENERATE (by go generate).
profile/offcpu_dwarf_export.go          CREATE — LoadOffCPUDwarf + OffCPUDwarf handle with accessors for every map (mirrors dwarf_export.go).

unwind/dwarfagent/offcpu.go             CREATE — OffCPUProfiler (NewOffCPUProfiler / Collect / CollectAndWrite / Close). Attaches via link.AttachTracing, consumes stack_events ringbuf.
unwind/dwarfagent/offcpu_test.go        CREATE — TestOffCPUProfilerEndToEnd: start io_bound workload → NewOffCPUProfiler → sleep → Collect → parse pprof → assert >0 samples with blocking-ns values.

perfagent/agent.go                      MODIFY — add offcpuProfiler interface; dispatch on Config.Unwind in the off-CPU branch inside Start() (mirroring the S5 CPU-profiler dispatch).

test/integration_test.go                MODIFY — add TestPerfAgentOffCPUDwarfUnwind: runs the perf-agent binary with --offcpu --unwind dwarf --pid N against a blocking workload, verifies off-CPU pprof has samples.

docs/dwarf-unwinding-design.md          MODIFY — update S6 row to ✅.
```

---

## Task 1 — BPF `offcpu_dwarf.bpf.c`

**Goal:** the new BPF program. Uses the same hybrid walker from unwind_common.h, hooks `tp_btf/sched_switch`, stashes on switch-OUT, emits on switch-IN.

**Files:**
- Create: `bpf/offcpu_dwarf.bpf.c`

- [ ] **Step 1.1: Write the BPF source**

Create `bpf/offcpu_dwarf.bpf.c`:

```c
//go:build ignore
//
// offcpu_dwarf.bpf.c — DWARF-capable off-CPU sampler (S6).
//
// Loaded only when --offcpu --unwind dwarf is selected. Walks the user
// stack of tasks going off-CPU using the S3 hybrid walker (walk_step in
// unwind_common.h). Emits one ringbuf record per off-CPU interval with
// value = blocking-ns.
//
// Two-step flow:
//   - switch-OUT (prev going off-CPU): walk prev's user stack, stash the
//     full sample_record + timestamp in offcpu_start keyed by (pid,tgid).
//   - switch-IN (prev coming back on): delta = now - start.timestamp;
//     fetch stashed sample, write delta into hdr.value, emit via ringbuf.

#if defined(__TARGET_ARCH_arm64)
#include "vmlinux_arm64.h"
#else
#include "vmlinux.h"
#endif
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include "unwind_common.h"

// offcpu_start keys the stashed sample by (pid, tgid). Value is the
// sample_record captured on switch-OUT. To avoid blowing the 512-byte
// BPF stack, we do NOT wrap it in a struct with a timestamp — instead
// we stash the switch-OUT timestamp in hdr.value (the "sample weight"
// slot, which is u64 and currently unused during the off-CPU interval),
// and overwrite it with the elapsed blocking-ns on switch-IN before
// emission. No consumer sees hdr.value as "timestamp" because emission
// only happens on switch-IN, after the overwrite.
struct offcpu_start_key {
    __u32 pid;
    __u32 tgid;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 2048);
    __type(key, struct offcpu_start_key);
    __type(value, struct sample_record);
} offcpu_start SEC(".maps");

BTF_MATERIALIZE(offcpu_start_key)

static __always_inline void handle_switch_out(struct task_struct *prev) {
    __u32 pid = BPF_CORE_READ(prev, pid);
    __u32 tgid = BPF_CORE_READ(prev, tgid);
    if (pid == 0 || tgid == 0) return;
    if (BPF_CORE_READ(prev, flags) & PF_KTHREAD) return;

    // PID filter (same shape as perf_dwarf).
    if (!bpf_map_lookup_elem(&pids, &tgid)) return;

    // Grab per-CPU scratch to build the sample_record.
    __u32 zero = 0;
    struct sample_record *rec = bpf_map_lookup_elem(&walker_scratch, &zero);
    if (!rec) return;

    // User-space registers of prev.
    struct pt_regs *regs = (struct pt_regs *)bpf_task_pt_regs(prev);
    if (!regs) return;
    __u64 ip = (__u64)PT_REGS_IP(regs);
    __u64 fp = (__u64)PT_REGS_FP(regs);
    __u64 sp = (__u64)PT_REGS_SP(regs);

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

    __u64 now = bpf_ktime_get_ns();
    rec->hdr.pid     = tgid;
    rec->hdr.tid     = pid;
    rec->hdr.time_ns = now;
    rec->hdr.value   = now; // stash timestamp here; overwritten on switch-IN
    rec->hdr.n_pcs   = (__u8)(walker.n_pcs > MAX_FRAMES ? MAX_FRAMES : walker.n_pcs);
    rec->hdr.mode    = (rec->hdr.walker_flags & WALKER_FLAG_DWARF_USED)
        ? MODE_FP_LESS : MODE_FP_SAFE;

    struct offcpu_start_key k = { .pid = pid, .tgid = tgid };
    // Pass the per-CPU scratch pointer to avoid a 1KB stack-local copy
    // (BPF stack is 512 bytes; sample_record is 1032).
    bpf_map_update_elem(&offcpu_start, &k, rec, BPF_ANY);
}

static __always_inline void handle_switch_in(struct task_struct *next) {
    __u32 pid = BPF_CORE_READ(next, pid);
    __u32 tgid = BPF_CORE_READ(next, tgid);
    if (pid == 0) return;

    struct offcpu_start_key k = { .pid = pid, .tgid = tgid };
    struct sample_record *saved = bpf_map_lookup_elem(&offcpu_start, &k);
    if (!saved || saved->hdr.value == 0) return;

    __u64 now = bpf_ktime_get_ns();
    __u64 delta = now - saved->hdr.value;
    saved->hdr.value = delta; // overwrite stashed timestamp with blocking-ns

    bpf_ringbuf_output(&stack_events, saved, sizeof(*saved), 0);
    bpf_map_delete_elem(&offcpu_start, &k);
}

SEC("tp_btf/sched_switch")
int BPF_PROG(offcpu_dwarf_sched_switch, bool preempt,
             struct task_struct *prev, struct task_struct *next) {
    handle_switch_out(prev);
    handle_switch_in(next);
    return 0;
}

char LICENSE[] SEC("license") = "GPL";
```

- [ ] **Step 1.2: Add bpf2go directives**

In `profile/gen.go`, append two lines (after the perf_dwarf arm64 directive):

```go
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile offcpu_dwarf ../bpf/offcpu_dwarf.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=profile offcpu_dwarf ../bpf/offcpu_dwarf.bpf.c -- -I../bpf
```

- [ ] **Step 1.3: Regenerate**

Run: `GOTOOLCHAIN=go1.26.0 make generate`

Expected: the same 4 benign `vmlinux_arm64.h` warnings; new files at `profile/offcpu_dwarf_x86_bpfel.{go,o}` and `profile/offcpu_dwarf_arm64_bpfel.{go,o}`. No compile errors.

If clang errors about `bpf_task_pt_regs` being undeclared: check vmlinux.h — it should be a kfunc or helper. If unavailable as a helper, fall back to `task->stack + TASK_STRUCT_PT_REGS_OFFSET` via BPF_CORE_READ_INTO — but only do this if the simple helper path fails; report and stop so the controller can help.

- [ ] **Step 1.4: Verifier gate — rebuild profile.test and run**

Build capped test binary:

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/profile.test ./profile/
```

DO NOT run it yourself — controller will setcap + run to verify TestPerfDwarfLoads still passes (catches accidental breakage of the shared unwind_common.h).

- [ ] **Step 1.5: Commit**

```
git add bpf/offcpu_dwarf.bpf.c profile/gen.go profile/offcpu_dwarf_x86_bpfel.go profile/offcpu_dwarf_x86_bpfel.o profile/offcpu_dwarf_arm64_bpfel.go profile/offcpu_dwarf_arm64_bpfel.o profile/perf_dwarf_x86_bpfel.o profile/perf_dwarf_arm64_bpfel.o
git commit -m "S6: offcpu_dwarf BPF — sched_switch + hybrid walker + ringbuf emit"
```

(The perf_dwarf .o files may be rebuilt too because `go generate` touches their modtimes even without changes.)

No `--no-verify`. No Co-Authored-By.

---

## Task 2 — profile.LoadOffCPUDwarf wrapper

**Goal:** mirror the `profile.PerfDwarf` / `LoadPerfDwarf` pattern for the offcpu variant. Exposes the loaded BPF objects + every map accessor dwarfagent.OffCPUProfiler needs.

**Files:**
- Create: `profile/offcpu_dwarf_export.go`

- [ ] **Step 2.1: Implement the wrapper**

Create `profile/offcpu_dwarf_export.go`:

```go
package profile

import (
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// OffCPUDwarf is a thin wrapper around the generated offcpu_dwarf BPF
// objects. Construct with LoadOffCPUDwarf; always Close() when done.
type OffCPUDwarf struct {
	objs offcpu_dwarfObjects
}

// LoadOffCPUDwarf loads the BPF program and returns a handle. Caller
// must Close(). The tp_btf program isn't attached yet — see
// unwind/dwarfagent.OffCPUProfiler for the attach wiring.
func LoadOffCPUDwarf() (*OffCPUDwarf, error) {
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true,
		cap.SYS_ADMIN, cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE); err != nil {
		return nil, fmt.Errorf("set capabilities: %w", err)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("remove memlock: %w", err)
	}

	spec, err := loadOffcpu_dwarf()
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf spec: %w", err)
	}
	p := &OffCPUDwarf{}
	if err := spec.LoadAndAssign(&p.objs, nil); err != nil {
		return nil, fmt.Errorf("load and assign: %w", err)
	}
	return p, nil
}

// Program returns the tp_btf/sched_switch program for link.AttachTracing.
func (p *OffCPUDwarf) Program() *ebpf.Program { return p.objs.OffcpuDwarfSchedSwitch }

// RingbufMap returns the stack_events ringbuf for ringbuf.NewReader.
func (p *OffCPUDwarf) RingbufMap() *ebpf.Map { return p.objs.StackEvents }

// AddPID registers a target PID for sampling. Semantics match
// profile.PerfDwarf.AddPID.
func (p *OffCPUDwarf) AddPID(pid uint32) error {
	cfg := offcpu_dwarfPidConfig{
		Type:          0,
		CollectUser:   1,
		CollectKernel: 0,
	}
	return p.objs.Pids.Update(pid, &cfg, ebpf.UpdateAny)
}

// Close releases all BPF objects.
func (p *OffCPUDwarf) Close() error { return p.objs.Close() }

// Outer map accessors — dwarfagent.OffCPUProfiler uses these to wire
// its own TableStore / PIDTracker / MmapWatcher lifecycle (same shape
// as profile.PerfDwarf).

func (p *OffCPUDwarf) CFIRulesMap() *ebpf.Map                  { return p.objs.CfiRules }
func (p *OffCPUDwarf) CFILengthsMap() *ebpf.Map                { return p.objs.CfiLengths }
func (p *OffCPUDwarf) CFIClassificationMap() *ebpf.Map         { return p.objs.CfiClassification }
func (p *OffCPUDwarf) CFIClassificationLengthsMap() *ebpf.Map  { return p.objs.CfiClassificationLengths }
func (p *OffCPUDwarf) PIDMappingsMap() *ebpf.Map               { return p.objs.PidMappings }
func (p *OffCPUDwarf) PIDMappingLengthsMap() *ebpf.Map         { return p.objs.PidMappingLengths }
```

The bpf2go-generated type names (`offcpu_dwarfObjects`, `offcpu_dwarfPidConfig`, `OffcpuDwarfSchedSwitch`, `Pids`, etc.) follow the same mangling bpf2go uses for perf_dwarf. If any field name differs (e.g. bpf2go generates `OffcpudwarfSchedSwitch` instead of `OffcpuDwarfSchedSwitch`), adjust to match the actual generated file. Check with:

```
grep -n "SchedSwitch\|type offcpu_dwarf\|Cfi\|Pid" profile/offcpu_dwarf_x86_bpfel.go
```

- [ ] **Step 2.2: Verify build**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./profile/
```

Expected: success.

- [ ] **Step 2.3: Commit**

```
git add profile/offcpu_dwarf_export.go
git commit -m "S6: profile.LoadOffCPUDwarf wrapper + map accessors"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 3 — `dwarfagent.OffCPUProfiler`

**Goal:** the Go-side profiler with the same shape as `offcpu.Profiler`. Attaches via `link.AttachTracing`, consumes the stack_events ringbuf (reusing `parseSample` from Task 3 of S5), aggregates by `(pid, stack-hash)` summing blocking-ns.

**Files:**
- Create: `unwind/dwarfagent/offcpu.go`

- [ ] **Step 3.1: Implement OffCPUProfiler**

Create `unwind/dwarfagent/offcpu.go`:

```go
package dwarfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// OffCPUProfiler is the DWARF-capable off-CPU profiler. Same public
// shape as offcpu.Profiler — Collect / CollectAndWrite / Close.
type OffCPUProfiler struct {
	pid  int
	tags []string

	objs       *profile.OffCPUDwarf
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    *ehmaps.MmapWatcher
	link       link.Link
	ringReader *ringbuf.Reader

	symbolizer *blazesym.Symbolizer

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64 // value = accumulated blocking-ns
	stacks  map[sampleKey][]uint64
}

// NewOffCPUProfiler loads the offcpu_dwarf BPF program, primes the
// ehmaps lifecycle (CFI/pid_mappings), attaches the tp_btf program,
// and starts the ringbuf reader. Returns (nil, err) with all resources
// released on failure.
func NewOffCPUProfiler(pid int, tags []string) (*OffCPUProfiler, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 (system-wide is S7 scope)")
	}
	objs, err := profile.LoadOffCPUDwarf()
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf: %w", err)
	}
	if err := objs.AddPID(uint32(pid)); err != nil {
		objs.Close()
		return nil, fmt.Errorf("add pid to filter: %w", err)
	}

	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())

	nAttached, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("attach initial mappings: %w", err)
	}
	log.Printf("dwarfagent (offcpu): attached %d binaries from /proc/%d/maps", nAttached, pid)

	watcher, err := ehmaps.NewMmapWatcher(uint32(pid))
	if err != nil {
		objs.Close()
		return nil, fmt.Errorf("mmap watcher: %w", err)
	}

	tpLink, err := link.AttachTracing(link.TracingOptions{
		Program: objs.Program(),
	})
	if err != nil {
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("attach tp_btf: %w", err)
	}

	rd, err := ringbuf.NewReader(objs.RingbufMap())
	if err != nil {
		tpLink.Close()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("ringbuf reader: %w", err)
	}

	symbolizer, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		rd.Close()
		tpLink.Close()
		watcher.Close()
		objs.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	p := &OffCPUProfiler{
		pid:        pid,
		tags:       tags,
		objs:       objs,
		store:      store,
		tracker:    tracker,
		watcher:    watcher,
		link:       tpLink,
		ringReader: rd,
		symbolizer: symbolizer,
		stop:       make(chan struct{}),
		samples:    map[sampleKey]uint64{},
	}

	p.trackerWG.Add(1)
	go func() {
		defer p.trackerWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-p.stop
			cancel()
		}()
		p.tracker.Run(ctx, p.watcher)
	}()

	p.readerWG.Add(1)
	go p.consume()

	return p, nil
}

// consume is the ringbuf reader goroutine. Adds Sample.Value (blocking-ns)
// into the aggregated total for each (pid, stack) key.
func (p *OffCPUProfiler) consume() {
	defer p.readerWG.Done()
	for {
		select {
		case <-p.stop:
			return
		default:
		}
		p.ringReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		rec, err := p.ringReader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			log.Printf("dwarfagent (offcpu): ringbuf read: %v", err)
			return
		}
		s, err := parseSample(rec.RawSample)
		if err != nil {
			log.Printf("dwarfagent (offcpu): parseSample: %v", err)
			continue
		}
		if len(s.PCs) == 0 || s.Value == 0 {
			continue
		}
		key := sampleKey{pid: s.PID, hash: hashPCs(s.PCs)}
		p.mu.Lock()
		p.samples[key] += s.Value
		if p.stacks == nil {
			p.stacks = map[sampleKey][]uint64{}
		}
		if _, have := p.stacks[key]; !have {
			p.stacks[key] = append([]uint64(nil), s.PCs...)
		}
		p.mu.Unlock()
	}
}

// Collect symbolizes accumulated off-CPU samples and writes a gzipped
// pprof to w. Sample type is off-CPU; Value is blocking-ns.
func (p *OffCPUProfiler) Collect(w io.Writer) error {
	p.mu.Lock()
	samples := make(map[sampleKey]uint64, len(p.samples))
	stacks := make(map[sampleKey][]uint64, len(p.stacks))
	for k, v := range p.samples {
		samples[k] = v
	}
	for k, v := range p.stacks {
		stacks[k] = v
	}
	p.mu.Unlock()

	if len(samples) == 0 {
		log.Println("dwarfagent (offcpu): no samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		PerPIDProfile: false,
		Comments:      p.tags,
	})

	for key, totalNs := range samples {
		frames := symbolizePID(p.symbolizer, key.pid, stacks[key])
		sample := pprof.ProfileSample{
			Pid:         key.pid,
			SampleType:  pprof.SampleTypeOffCpu,
			Aggregation: pprof.SampleAggregated,
			Stack:       frames,
			Value:       totalNs,
		}
		builders.AddSample(&sample)
	}

	for _, b := range builders.Builders {
		if _, err := b.Write(w); err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break
	}
	return nil
}

// CollectAndWrite is a convenience wrapper for file output.
func (p *OffCPUProfiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close stops goroutines and releases all resources. Idempotent.
func (p *OffCPUProfiler) Close() error {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.readerWG.Wait()
	p.ringReader.Close()
	p.watcher.Close()
	p.trackerWG.Wait()
	if p.link != nil {
		_ = p.link.Close()
	}
	if p.symbolizer != nil {
		p.symbolizer.Close()
	}
	return p.objs.Close()
}
```

- [ ] **Step 3.2: Verify build**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./unwind/dwarfagent/
```

Expected: success.

- [ ] **Step 3.3: Commit**

```
git add unwind/dwarfagent/offcpu.go
git commit -m "S6: dwarfagent.OffCPUProfiler — tp_btf attach + ringbuf consumer"
```

---

## Task 4 — End-to-end test for OffCPUProfiler

**Goal:** CAP-gated test: start an I/O-bound workload → NewOffCPUProfiler → sleep → Collect → parse pprof → assert samples with non-zero blocking-ns Value.

**Files:**
- Create: `unwind/dwarfagent/offcpu_test.go`

- [ ] **Step 4.1: Write the test**

Create `unwind/dwarfagent/offcpu_test.go`:

```go
package dwarfagent_test

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/unwind/dwarfagent"
)

// TestOffCPUProfilerEndToEnd runs the dwarfagent off-CPU profiler
// against the io_bound workload (blocks on I/O frequently) and
// asserts that the resulting pprof has samples with non-zero
// blocking-ns values.
func TestOffCPUProfilerEndToEnd(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	binPath := "../../test/workloads/go/io_bound"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("go io_bound workload not built: %v", err)
	}

	workload := exec.Command(binPath, "-duration=10s", "-threads=2")
	if err := workload.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(1 * time.Second)

	p, err := dwarfagent.NewOffCPUProfiler(workload.Process.Pid, nil)
	if err != nil {
		t.Fatalf("NewOffCPUProfiler: %v", err)
	}

	time.Sleep(3 * time.Second)

	var buf bytes.Buffer
	if err := p.Collect(&buf); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Logf("Close (non-fatal): %v", err)
	}

	if buf.Len() == 0 {
		t.Fatal("Collect produced 0 bytes")
	}
	prof, err := profile.Parse(&buf)
	if err != nil {
		t.Fatalf("parse pprof: %v", err)
	}
	if len(prof.Sample) == 0 {
		t.Fatal("pprof has no samples")
	}
	var totalNs int64
	for _, s := range prof.Sample {
		for _, v := range s.Value {
			totalNs += v
		}
	}
	if totalNs == 0 {
		t.Fatal("pprof samples all have zero value — no blocking time accumulated")
	}
	t.Logf("off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}
```

- [ ] **Step 4.2: Verify compile**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/dwarfagent.test ./unwind/dwarfagent/
```

Expected: success; harmless static-glibc warnings OK.

- [ ] **Step 4.3: DO NOT run capped binary yourself**

Controller will setcap + run.

- [ ] **Step 4.4: Commit**

```
git add unwind/dwarfagent/offcpu_test.go
git commit -m "S6: dwarfagent off-CPU end-to-end test against io_bound workload"
```

---

## Task 5 — perfagent off-CPU dispatch

**Goal:** mirror the S5 CPU-profiler dispatch (Task 7 of S5) but for the off-CPU path. When `--offcpu --unwind dwarf`, construct `dwarfagent.OffCPUProfiler` instead of `offcpu.Profiler`.

**Files:**
- Modify: `perfagent/agent.go`

- [ ] **Step 5.1: Check offcpu.Profiler.Close() signature**

Run:
```
grep -n "^func (.*) Close" offcpu/profiler.go unwind/dwarfagent/offcpu.go
```

Expected: `offcpu.Profiler.Close()` returns nothing; `dwarfagent.OffCPUProfiler.Close()` returns error. Matches the CPU case — we need an adapter.

- [ ] **Step 5.2: Add offcpuProfiler interface + adapter**

In `perfagent/agent.go`, at package scope (near the existing `cpuProfiler` interface + `dwarfProfilerAdapter`), add:

```go
// offcpuProfiler is the narrow shape both offcpu.Profiler and
// dwarfagent.OffCPUProfiler satisfy, letting Agent dispatch on --unwind
// for the off-CPU path.
type offcpuProfiler interface {
	Collect(w io.Writer) error
	CollectAndWrite(path string) error
	Close()
}

// dwarfOffCPUProfilerAdapter wraps dwarfagent.OffCPUProfiler so its
// Close() matches the void Close() the FP offcpu profiler exposes.
type dwarfOffCPUProfilerAdapter struct{ *dwarfagent.OffCPUProfiler }

func (a dwarfOffCPUProfilerAdapter) Close() {
	if err := a.OffCPUProfiler.Close(); err != nil {
		log.Printf("dwarfagent.OffCPUProfiler.Close: %v", err)
	}
}
```

- [ ] **Step 5.3: Change the offcpuProfiler field type**

In `perfagent/agent.go`'s Agent struct, change:

```go
offcpuProfiler *offcpu.Profiler
```

to:

```go
offcpuProfiler offcpuProfiler
```

- [ ] **Step 5.4: Dispatch on Unwind in Start()**

Find the existing off-CPU construction block:

```go
if a.config.EnableOffCPUProfile {
    profiler, err := offcpu.NewProfiler(
        a.config.PID,
        a.config.SystemWide,
        a.config.Tags,
    )
    if err != nil {
        a.cleanup()
        return fmt.Errorf("create off-CPU profiler: %w", err)
    }
    a.offcpuProfiler = profiler
    if a.config.SystemWide {
        log.Println("Off-CPU profiler enabled (system-wide)")
    } else {
        log.Printf("Off-CPU profiler enabled (PID: %d)", a.config.PID)
    }
}
```

Replace with:

```go
if a.config.EnableOffCPUProfile {
    switch a.config.Unwind {
    case "dwarf":
        if a.config.SystemWide {
            return fmt.Errorf("--unwind dwarf does not support system-wide mode yet (S7)")
        }
        p, err := dwarfagent.NewOffCPUProfiler(a.config.PID, a.config.Tags)
        if err != nil {
            a.cleanup()
            return fmt.Errorf("create DWARF off-CPU profiler: %w", err)
        }
        a.offcpuProfiler = dwarfOffCPUProfilerAdapter{p}
        log.Printf("Off-CPU profiler enabled (PID: %d, DWARF)", a.config.PID)
    default:
        profiler, err := offcpu.NewProfiler(
            a.config.PID,
            a.config.SystemWide,
            a.config.Tags,
        )
        if err != nil {
            a.cleanup()
            return fmt.Errorf("create off-CPU profiler: %w", err)
        }
        a.offcpuProfiler = profiler
        if a.config.SystemWide {
            log.Println("Off-CPU profiler enabled (system-wide)")
        } else {
            log.Printf("Off-CPU profiler enabled (PID: %d)", a.config.PID)
        }
    }
}
```

- [ ] **Step 5.5: Verify build + tests**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./...
GOTOOLCHAIN=go1.26.0 make test-unit
```

Expected: both succeed.

- [ ] **Step 5.6: Commit**

```
git add perfagent/agent.go
git commit -m "S6: perfagent off-CPU dispatch on Config.Unwind"
```

---

## Task 6 — Integration test

**Goal:** run the full `perf-agent` binary with `--offcpu --unwind dwarf --pid N` against an I/O-blocking workload.

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 6.1: Append the test**

Append to `test/integration_test.go`:

```go
// TestPerfAgentOffCPUDwarfUnwind runs the full perf-agent binary with
// --offcpu --unwind dwarf against the io_bound workload and verifies
// the resulting off-CPU pprof has samples with non-zero blocking-ns.
func TestPerfAgentOffCPUDwarfUnwind(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/go/io_bound"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("go io_bound workload not built: %v", err)
	}

	workload := exec.Command(binPath, "-duration=20s", "-threads=2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "offcpu-dwarf.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--offcpu",
		"--offcpu-output", outputFile,
		"--unwind", "dwarf",
		"--pid", fmt.Sprintf("%d", workload.Process.Pid),
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)

	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "off-CPU profile should have samples")

	var totalNs int64
	for _, s := range prof.Sample {
		for _, v := range s.Value {
			totalNs += v
		}
	}
	require.Greater(t, totalNs, int64(0), "off-CPU profile should have non-zero blocking-ns values")
	t.Logf("off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}
```

- [ ] **Step 6.2: Rebuild integration.test**

```
cd test && GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/integration.test .
```

DO NOT run the capped binary yourself — controller handles it.

- [ ] **Step 6.3: Commit**

```
cd /home/diego/github/perf-agent
git add test/integration_test.go
git commit -m "S6: TestPerfAgentOffCPUDwarfUnwind — end-to-end --offcpu --unwind dwarf"
```

---

## Task 7 — Sanity matrix + doc update

**Goal:** final check + mark S6 ✅.

- [ ] **Step 7.1: Full test-unit**

`GOTOOLCHAIN=go1.26.0 make test-unit`

Expected: all PASS, CAP-gated tests SKIP.

- [ ] **Step 7.2: Capped test matrix**

```
/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads
/home/diego/bin/ehmaps.test -test.v
cd unwind/dwarfagent && /home/diego/bin/dwarfagent.test -test.v
cd test && /home/diego/bin/integration.test -test.v -test.run "TestPerfDwarf|TestPerfAgentDwarf|TestPerfAgentOffCPUDwarf"
```

Expected: all PASS.

- [ ] **Step 7.3: Update design doc**

In `docs/dwarf-unwinding-design.md`, update the S6 row:

```
| S6 ✅  | `offcpu_dwarf.bpf.c` — off-CPU variant                     | 2-3d    | Entry path differs: tracepoint on `sched_switch` rather than perf_event; regs via `bpf_task_pt_regs(task)`, stack via explicit `bpf_probe_read_user()` from `task->thread.sp`. Walker and classification logic reused from common.h unchanged. End state: `--unwind dwarf --offcpu` produces deep stacks through libpthread/glibc-futex. **Shipped**: see `docs/superpowers/plans/2026-04-24-s6-offcpu-dwarf.md`. |
```

- [ ] **Step 7.4: Commit**

```
git add docs/dwarf-unwinding-design.md docs/superpowers/plans/2026-04-24-s6-offcpu-dwarf.md
git commit -m "S6: design doc status update + preserve implementation plan"
```

---

## Success criteria recap

From `docs/dwarf-unwinding-design.md` §Execution plan:

> **S6: `offcpu_dwarf.bpf.c` — off-CPU variant** — End state: `--unwind dwarf --offcpu` produces deep stacks through libpthread/glibc-futex.

Satisfied by:
- `TestOffCPUProfilerEndToEnd` — direct-library test against io_bound.
- `TestPerfAgentOffCPUDwarfUnwind` — full-binary test with `--offcpu --unwind dwarf`, asserts non-zero blocking-ns.

## Open risks

1. **`bpf_task_pt_regs(prev)` in tracepoint context.** Available since 5.15; our floor is 6.0. If the BPF verifier rejects the walker under this entry path (e.g. arg type mismatch between tp_btf and pt_regs helpers), the walker may need adaptation via `BPF_CORE_READ_INTO` from `prev->thread.sp` / `prev->thread.ip`. Flagged in Task 1 Step 1.3.
2. **Duplicated CFI compile when both --profile and --offcpu --unwind dwarf are enabled.** Each profiler owns its own cfi_rules; TableStore refcounts are per-profiler. 2× CPU at startup, 2× memory. Acceptable for S6; S8 cleanup via pinned maps.
3. **Walker verifier complexity.** The walker is already at the verifier limit under perf_dwarf; under tp_btf the verifier runs a different state-machine. If the offcpu_dwarf program is rejected with "program too complex", splitting the walker across multiple bpf_loop callbacks (one per frame) or limiting MAX_FRAMES for off-CPU would be the fallback.
4. **Stack churn under high context-switch rates.** Each switch-OUT stashes a ~1040-byte sample. At 10k switches/s across all CPUs, that's ~10 MB/s of map churn. BPF map update is cheap; GC is automatic on delete. Acceptable, but for extreme workloads a FULL `offcpu_start` map would silently drop new entries (BPF_ANY on full HASH). Documented.
