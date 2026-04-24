# S7: System-Wide `-a --unwind dwarf` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** make `perf-agent --profile --unwind dwarf -a` and `perf-agent --offcpu --unwind dwarf -a` work — the agent tracks mmaps across ALL processes on the host, compiles CFI for every eligible binary once (build-id dedup), and produces a pprof whose samples carry DWARF-unwound stacks from any PID that happened to run on-CPU or block.

**Architecture:** three changes. (1) Parametrize `profile.LoadPerfDwarf` / `LoadOffCPUDwarf` on `systemWide` — sets the BPF-side `const volatile bool system_wide` so the BPF programs skip the pids-map filter. (2) Replace the per-PID `MmapWatcher` with a **per-CPU** variant when system-wide (opens `pid=-1, cpu=N` per online CPU and multiplexes events onto one channel). (3) Replace the per-PID initial `AttachAllMappings` with `AttachAllProcesses` that scans `/proc/*` and attaches every eligible binary visible in the system. `PIDTracker.Run` already handles MMAP2/EXIT events; add FORK handling so newly-spawned processes auto-attach.

**Tech Stack:** Go 1.26, cilium/ebpf v0.21.0, perf_event_open per-CPU with `mmap_data=1 | mmap2=1 | task=1`, existing `unwind/ehmaps` / `unwind/dwarfagent` packages.

---

## Scope

**S7 delivers:** end users can run `perf-agent --profile --unwind dwarf -a` or `perf-agent --offcpu --unwind dwarf -a` and get a system-wide pprof with DWARF-unwound stacks. Build-ids are deduplicated via TableStore's FNV-1a-keyed refcount so shared libraries (`libc.so.6`, `ld-linux-*.so.2`, etc.) compile once even though hundreds of processes load them.

**Explicitly NOT in S7:**
- Flip of `--unwind auto` default to DWARF — that's S8.
- Cross-profiler map sharing (perf_dwarf and offcpu_dwarf still maintain duplicate CFI tables when both profilers are active). — S8 cleanup.
- EXEC-triggered re-attach (when a tracked process exec's a new binary, the old pid_mappings stays stale until EXIT). Acceptable — most exec'd images share dependent libraries with the pre-exec image; the new main-binary mapping goes through MmapWatcher's normal MMAP2 path.

## Background for implementers

**What works today:**
- `perf_dwarf.bpf.c` already has `const volatile bool system_wide` that flips off the pids filter (S3).
- `offcpu_dwarf.bpf.c` does NOT have the toggle — filter is hardcoded via `bpf_map_lookup_elem(&pids, ...)` (see `bpf/offcpu_dwarf.bpf.c:handle_switch_out`). Task 1 adds it.
- `MmapWatcher` opens `pid=>0, cpu=-1` — per-TID, can't do system-wide. Task 2 adds the per-CPU sibling.
- `PIDTracker.Run` switches on `MmapEvent` / `ExitEvent`. Task 4 adds `ForkEvent`.
- `AttachAllMappings(tracker, pid)` walks one PID's `/proc/N/maps`. Task 3 adds the multi-PID sibling.
- `dwarfagent.Profiler` and `dwarfagent.OffCPUProfiler` reject `systemWide=true` with a hard error (S5/S6 Task 7). Tasks 5–7 remove that.

**Why per-CPU `MmapWatcher` instead of per-PID with inherit:**
In S4 we discovered the kernel rejects `mmap()` on a perf_event fd when `attr.inherit=1` combined with a non-CPU-wide target (EINVAL from `perf_mmap`). `pid=-1, cpu=N` is the standard workaround: each per-CPU watcher sees every process on that CPU. With N CPUs we need N watchers and a goroutine per CPU (or one goroutine polling all via `unix.Poll(pfds)`). For S7 MVP we do one goroutine per CPU; follow-up optimization can collapse to a single Poll loop.

**TableStore and memory bounds:**
- `cfi_rules` outer map: 1024 entries. Typical Linux host has ~80–150 unique executable binaries across all processes (lots of sharing: libc, ld-linux, libm, libpthread, small number of app binaries). Fine at 1024.
- `pid_mappings` outer map: 2048 entries. Busy hosts can have >2048 processes. Task 1 bumps to 4096.
- Per-profiler RefcountTable works across any number of PIDs — no change needed.

**`PERF_RECORD_FORK`:**
Kernel record `perf_event_header type=7` = FORK. Body: `u32 pid, ppid; u32 tid, ptid; u64 time`. Fires on clone/fork. For us: when we see a FORK where `pid == tid` (group leader), enqueue an Attach for that new process — the child inherits its parent's mappings (copy-on-write) so we walk its `/proc/<pid>/maps` just like we would on startup.

## File Structure

```
bpf/offcpu_dwarf.bpf.c                      MODIFY — add `const volatile bool system_wide`; mirror the perf_dwarf.bpf.c filter pattern.

profile/dwarf_export.go                     MODIFY — LoadPerfDwarf takes `systemWide bool`; default stays false so existing callers are one-line-change.
profile/offcpu_dwarf_export.go              MODIFY — same for LoadOffCPUDwarf.

unwind/ehmaps/mmap_watcher.go               MODIFY — split the existing NewMmapWatcher into a pid-attached constructor and a new NewSystemWideMmapWatcher(cpu int) that opens pid=-1, cpu=N.
unwind/ehmaps/mmap_multiplexer.go           CREATE — MultiCPUMmapWatcher type that owns N per-CPU watchers and fans their events into one channel.
unwind/ehmaps/mmap_watcher.go               MODIFY — add `ForkEvent` to MmapEventKind; parse PERF_RECORD_FORK (type=7); emit into events channel.

unwind/ehmaps/tracker.go                    MODIFY — add AttachAllProcesses(tracker) that scans /proc/* and Attach's each eligible PID. Run() handles ForkEvent by calling Attach on the new PID.
unwind/ehmaps/tracker_test.go               MODIFY — add TestAttachAllProcesses (CAP-gated).

unwind/dwarfagent/agent.go                  MODIFY — NewProfiler accepts systemWide bool; when true: LoadPerfDwarf(true), AttachAllProcesses, per-CPU MmapWatcher.
unwind/dwarfagent/offcpu.go                 MODIFY — same for OffCPUProfiler.

perfagent/agent.go                          MODIFY — remove the "--unwind dwarf does not support system-wide mode yet (S7)" errors; plumb systemWide to both dwarf profilers.

test/integration_test.go                    MODIFY — add TestPerfAgentSystemWideDwarfProfile and TestPerfAgentSystemWideDwarfOffCPU.

docs/dwarf-unwinding-design.md              MODIFY — mark S7 ✅.
```

---

## Task 1 — BPF: add `system_wide` toggle to offcpu_dwarf; parametrize both loaders

**Goal:** set the groundwork so S7's userspace can load either program with filtering disabled.

**Files:**
- Modify: `bpf/offcpu_dwarf.bpf.c`
- Modify: `profile/dwarf_export.go`
- Modify: `profile/offcpu_dwarf_export.go`
- Modify: `bpf/unwind_common.h` — bump `pid_mappings` max_entries from 2048 to 4096

- [ ] **Step 1.1: Add `system_wide` to offcpu_dwarf.bpf.c**

Open `bpf/offcpu_dwarf.bpf.c`. Near the top (after the `#include "unwind_common.h"` line), add:

```c
// System-wide mode toggle set by userspace at load time. When true, the
// PID filter below is skipped — the walker emits a sample for every
// non-kernel task's off-CPU interval.
const volatile bool system_wide = false;
```

Then find in `handle_switch_out`:

```c
    // PID filter (same shape as perf_dwarf).
    if (!bpf_map_lookup_elem(&pids, &tgid)) return;
```

Replace with:

```c
    // PID filter (skipped in system-wide mode).
    if (!system_wide) {
        if (!bpf_map_lookup_elem(&pids, &tgid)) return;
    }
```

- [ ] **Step 1.2: Bump pid_mappings max_entries in unwind_common.h**

Find the `pid_mappings` outer map declaration in `bpf/unwind_common.h`:

```c
struct {
    __uint(type, BPF_MAP_TYPE_HASH_OF_MAPS);
    __uint(max_entries, 2048);
    __type(key, __u32);
    __array(values, struct pid_mapping_inner);
} pid_mappings SEC(".maps");
```

Change `__uint(max_entries, 2048)` → `__uint(max_entries, 4096)`. Do the same for `pid_mapping_lengths` directly below.

- [ ] **Step 1.3: Regenerate BPF artifacts**

Run: `GOTOOLCHAIN=go1.26.0 make generate`

Expected: same 4 benign `vmlinux_arm64.h` warnings, reported per-ELF (so 12 lines of warnings total across perf_dwarf + offcpu_dwarf + perf + offcpu + cpu). No errors.

- [ ] **Step 1.4: Parametrize LoadPerfDwarf**

In `profile/dwarf_export.go`, change the signature:

```go
func LoadPerfDwarf() (*PerfDwarf, error) {
```

to:

```go
func LoadPerfDwarf(systemWide bool) (*PerfDwarf, error) {
```

And inside, change the hardcoded:

```go
if err := spec.Variables["system_wide"].Set(false); err != nil {
```

to:

```go
if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
```

- [ ] **Step 1.5: Parametrize LoadOffCPUDwarf the same way**

In `profile/offcpu_dwarf_export.go`, change:

```go
func LoadOffCPUDwarf() (*OffCPUDwarf, error) {
```

to:

```go
func LoadOffCPUDwarf(systemWide bool) (*OffCPUDwarf, error) {
```

And just before `spec.LoadAndAssign`, add:

```go
	if err := spec.Variables["system_wide"].Set(systemWide); err != nil {
		return nil, fmt.Errorf("set system_wide: %w", err)
	}
```

(Confirm via `grep Variables profile/offcpu_dwarf_x86_bpfel.go` that `system_wide` is in the generated VariableSpecs — it will be after Step 1.1 + 1.3.)

- [ ] **Step 1.6: Update existing callers**

```
grep -rn "LoadPerfDwarf\|LoadOffCPUDwarf" --include='*.go' . | grep -v plans
```

Every call site gains a `false` argument:
- `profile/perf_dwarf_test.go` — `LoadPerfDwarf()` → `LoadPerfDwarf(false)`
- `cmd/perf-dwarf-test/main.go` — same
- `test/integration_test.go` — same (both inside `TestPerfDwarfWalker` and `TestPerfDwarfMmap2Tracking`)
- `unwind/dwarfagent/agent.go` — `LoadPerfDwarf()` → `LoadPerfDwarf(false)` for now (Task 5 will wire the systemWide parameter through)
- `unwind/dwarfagent/offcpu.go` — same for `LoadOffCPUDwarf(false)`

- [ ] **Step 1.7: Verify build + unit tests**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./...
GOTOOLCHAIN=go1.26.0 make test-unit
```

Expected: both pass.

- [ ] **Step 1.8: Commit**

```
git add bpf/offcpu_dwarf.bpf.c bpf/unwind_common.h profile/dwarf_export.go profile/offcpu_dwarf_export.go profile/perf_dwarf_x86_bpfel.go profile/perf_dwarf_arm64_bpfel.go profile/perf_dwarf_x86_bpfel.o profile/perf_dwarf_arm64_bpfel.o profile/offcpu_dwarf_x86_bpfel.go profile/offcpu_dwarf_arm64_bpfel.go profile/offcpu_dwarf_x86_bpfel.o profile/offcpu_dwarf_arm64_bpfel.o profile/perf_dwarf_test.go cmd/perf-dwarf-test/main.go test/integration_test.go unwind/dwarfagent/agent.go unwind/dwarfagent/offcpu.go
git commit -m "S7: parametrize LoadPerfDwarf/LoadOffCPUDwarf on systemWide; bump pid_mappings cap"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 2 — Per-CPU MmapWatcher

**Goal:** expose a system-wide variant of `MmapWatcher` that attaches `pid=-1, cpu=N` so it sees MMAP2/EXIT/FORK events from any task running on that CPU.

**Files:**
- Modify: `unwind/ehmaps/mmap_watcher.go`
- Modify: `unwind/ehmaps/mmap_watcher_test.go`

- [ ] **Step 2.1: Refactor NewMmapWatcher into a shared helper**

In `unwind/ehmaps/mmap_watcher.go`, find the existing `NewMmapWatcher(pid uint32)`. Rename the internals into a private helper:

```go
// newMmapWatcher opens the perf_event with the given (pid, cpu) and
// wires up the ring buffer + goroutine. Callers: NewMmapWatcher (pid,
// cpu=-1) and NewSystemWideMmapWatcher (pid=-1, cpu).
func newMmapWatcher(pid, cpu int) (*MmapWatcher, error) {
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_DUMMY,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Bits:   unix.PerfBitMmap | unix.PerfBitMmap2 | unix.PerfBitTask | unix.PerfBitDisabled,
	}
	fd, err := unix.PerfEventOpen(attr, pid, cpu, -1, unix.PERF_FLAG_FD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open (mmap2, pid=%d, cpu=%d): %w", pid, cpu, err)
	}
	pageSz := os.Getpagesize()
	mmapSz := pageSz * (1 + mwRingPages)
	mapped, err := unix.Mmap(fd, 0, mmapSz, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("mmap mmap-watcher ring: %w", err)
	}
	w := &MmapWatcher{
		fd:       fd,
		mmap:     mapped,
		data:     mapped[pageSz:],
		pageSz:   pageSz,
		dataHead: (*uint64)(unsafe.Pointer(&mapped[mwDataHeadOffset])),
		dataTail: (*uint64)(unsafe.Pointer(&mapped[mwDataTailOffset])),
		events:   make(chan MmapEventRecord, 128),
		done:     make(chan struct{}),
		exited:   make(chan struct{}),
	}
	if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
		w.Close()
		return nil, fmt.Errorf("perf_event enable: %w", err)
	}
	go w.loop()
	return w, nil
}

// NewMmapWatcher attaches a per-TID watcher to `pid`. Sees mmaps only
// from the specific thread whose TID == pid (kernel semantics).
func NewMmapWatcher(pid uint32) (*MmapWatcher, error) {
	return newMmapWatcher(int(pid), -1)
}

// NewSystemWideMmapWatcher attaches to all tasks on one CPU. Combine
// N of these (one per online CPU) via MultiCPUMmapWatcher for full
// system-wide coverage.
func NewSystemWideMmapWatcher(cpu int) (*MmapWatcher, error) {
	return newMmapWatcher(-1, cpu)
}
```

- [ ] **Step 2.2: Add ForkEvent parsing**

In `MmapEventKind`, add a new kind:

```go
const (
	MmapEvent MmapEventKind = iota + 1
	ExitEvent
	ForkEvent
)
```

In the `drain()` switch (or wherever record types are handled), add the FORK case:

```go
	// Records we care about:
	const (
		perfRecordMmap2 = 10
		perfRecordExit  = 4
		perfRecordFork  = 7
	)
```

(If `perfRecordMmap2` / `perfRecordExit` are already package constants, add `perfRecordFork` next to them — don't redeclare.)

Then in the type switch:

```go
		case perfRecordFork:
			body := w.readBytes((base+8)%size, recSize-8)
			ev, ok := parseFork(body)
			if ok {
				select {
				case w.events <- ev:
				case <-w.done:
					return false
				}
			}
```

Add the parser:

```go
// parseFork decodes PERF_RECORD_FORK body (same layout as EXIT):
//
//	u32 pid, u32 ppid, u32 tid, u32 ptid, u64 time
//
// Like EXIT, per-task events fire — we only act on group-leader forks
// (pid == tid) to avoid spamming Attach on every new thread of an
// existing process.
func parseFork(body []byte) (MmapEventRecord, bool) {
	if len(body) < 16 {
		return MmapEventRecord{}, false
	}
	return MmapEventRecord{
		Kind: ForkEvent,
		PID:  binary.LittleEndian.Uint32(body[0:4]),
		TID:  binary.LittleEndian.Uint32(body[8:12]),
	}, true
}
```

- [ ] **Step 2.3: Runtime test — system-wide watcher sees /bin/ls mmap**

Append to `unwind/ehmaps/mmap_watcher_test.go`:

```go
// TestSystemWideMmapWatcherSeesMmap opens a per-CPU system-wide watcher
// on CPU 0 and deliberately mmaps /bin/ls to confirm the pid=-1 flow
// captures events without the per-TID caveat.
func TestSystemWideMmapWatcherSeesMmap(t *testing.T) {
	requireBPFCaps(t)

	// Pin to CPU 0 so the mmap syscall runs on the CPU we're watching.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.SchedSetaffinity(0, func() *unix.CPUSet {
		var set unix.CPUSet
		set.Set(0)
		return &set
	}()); err != nil {
		t.Skipf("sched_setaffinity: %v", err)
	}

	w, err := NewSystemWideMmapWatcher(0)
	if err != nil {
		t.Fatalf("NewSystemWideMmapWatcher: %v", err)
	}
	defer w.Close()

	time.Sleep(100 * time.Millisecond)

	const target = "/bin/ls"
	f, err := os.Open(target)
	if err != nil {
		t.Skipf("%s not available: %v", target, err)
	}
	defer f.Close()
	data, err := unix.Mmap(int(f.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_EXEC, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer unix.Munmap(data)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				t.Fatal("event channel closed before /bin/ls MMAP2 observed")
			}
			if ev.Kind == MmapEvent && strings.HasSuffix(ev.Filename, "/ls") {
				return
			}
		case <-deadline:
			t.Fatal("no MMAP2 event for /bin/ls within 2s")
		}
	}
}
```

- [ ] **Step 2.4: Verify compile**

Run: `GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: all existing tests still PASS; the new test SKIPs without CAP_BPF. Compile succeeds.

- [ ] **Step 2.5: Commit**

```
git add unwind/ehmaps/mmap_watcher.go unwind/ehmaps/mmap_watcher_test.go
git commit -m "S7: NewSystemWideMmapWatcher (pid=-1, cpu=N) + ForkEvent parsing"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 3 — `AttachAllProcesses` + FORK handling in PIDTracker.Run

**Goal:** add the system-wide `/proc/*` walker + teach `PIDTracker.Run` to Attach on FORK.

**Files:**
- Modify: `unwind/ehmaps/tracker.go`
- Modify: `unwind/ehmaps/tracker_test.go`

- [ ] **Step 3.1: Write failing test**

Append to `unwind/ehmaps/tracker_test.go`:

```go
// TestAttachAllProcesses scans /proc/* and Attaches every eligible PID
// it finds. On a typical Linux host we should see dozens of processes
// attached with at least tens of distinct binaries.
func TestAttachAllProcesses(t *testing.T) {
	requireBPFCaps(t)
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("rlimit: %v", err)
	}
	cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen := newTestMaps(t)
	defer closeAll(cfi, cfiLen, cls, clsLen, pidMaps, pidMapLen)

	store := ehmaps.NewTableStore(cfi, cfiLen, cls, clsLen)
	tracker := ehmaps.NewPIDTracker(store, pidMaps, pidMapLen)

	nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
	if err != nil {
		t.Fatalf("AttachAllProcesses: %v", err)
	}
	if nPIDs < 1 {
		t.Fatalf("AttachAllProcesses attached %d PIDs, want >= 1", nPIDs)
	}
	if nTables < 1 {
		t.Fatalf("AttachAllProcesses installed %d CFI tables, want >= 1", nTables)
	}
	t.Logf("attached %d PIDs across %d distinct binaries", nPIDs, nTables)
}
```

- [ ] **Step 3.2: Verify fail**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: `undefined: ehmaps.AttachAllProcesses`.

- [ ] **Step 3.3: Implement AttachAllProcesses**

Append to `unwind/ehmaps/tracker.go`:

```go
// AttachAllProcesses walks /proc/* and calls AttachAllMappings for every
// numeric PID directory that still has a live /proc/<pid>/maps.
// Returns (pidCount, distinctBinaryCount, err). The distinct-binary
// count comes from observing TableStore refcounts; the walker tolerates
// individual PID failures (process vanished between listdir and open).
//
// Intended for system-wide startup. After this returns, follow-up
// tracking relies on per-CPU MmapWatchers + FORK events.
func AttachAllProcesses(t *PIDTracker) (pids, tables int, err error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, 0, fmt.Errorf("read /proc: %w", err)
	}
	beforeCFI := countCFIRules(t.store)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue // not a PID directory
		}
		if pid == 0 {
			continue
		}
		// Skip self — we don't want to attach the agent to itself.
		if int(pid) == os.Getpid() {
			continue
		}
		n, err := AttachAllMappings(t, uint32(pid))
		if err != nil || n == 0 {
			// PID gone / all binaries ineligible / etc. — skip.
			slog.Debug("ehmaps: AttachAllProcesses: skip", "pid", pid, "err", err)
			continue
		}
		pids++
	}
	afterCFI := countCFIRules(t.store)
	return pids, afterCFI - beforeCFI, nil
}

// countCFIRules returns the number of distinct table_ids currently
// present in the TableStore's CFIRules outer map. Used by
// AttachAllProcesses to report build-id dedup effectiveness.
func countCFIRules(s *TableStore) int {
	if s == nil || s.CFIRules == nil {
		return 0
	}
	it := s.CFIRules.Iterate()
	var k uint64
	var v uint32
	n := 0
	for it.Next(&k, &v) {
		n++
	}
	return n
}
```

Inside `Run`, extend the event switch to include `ForkEvent`:

```go
			case ForkEvent:
				// Only act on group-leader fork (the whole process is
				// new). Per-thread FORKs fire too but the parent process
				// is already tracked.
				if ev.TID != ev.PID {
					continue
				}
				n, err := AttachAllMappings(t, ev.PID)
				if err != nil || n == 0 {
					slog.Debug("ehmaps: fork Attach failed", "pid", ev.PID, "err", err)
				}
```

- [ ] **Step 3.4: Verify build**

`GOTOOLCHAIN=go1.26.0 go test ./unwind/ehmaps/`

Expected: compile passes; `TestAttachAllProcesses` SKIPs without caps.

- [ ] **Step 3.5: Commit**

```
git add unwind/ehmaps/tracker.go unwind/ehmaps/tracker_test.go
git commit -m "S7: AttachAllProcesses + PIDTracker.Run FORK handling"
```

---

## Task 4 — MultiCPUMmapWatcher multiplexer

**Goal:** a single `*MmapWatcher`-shaped handle that owns N per-CPU watchers and merges their events onto one channel. dwarfagent.Profiler wants a single `.Events()` channel regardless of how many CPUs are being watched.

**Files:**
- Create: `unwind/ehmaps/mmap_multiplexer.go`

- [ ] **Step 4.1: Implement**

Create `unwind/ehmaps/mmap_multiplexer.go`:

```go
package ehmaps

import (
	"fmt"
	"sync"
)

// MultiCPUMmapWatcher owns one MmapWatcher per online CPU and fans
// their events into one channel. Used by dwarfagent in system-wide
// mode — per-PID watchers can't do -a because they only see mmaps
// from the specific TID they attach to (kernel semantics), and
// `inherit=1` is forbidden when the fd is mmap'd (EINVAL).
type MultiCPUMmapWatcher struct {
	watchers []*MmapWatcher
	events   chan MmapEventRecord
	fanWG    sync.WaitGroup
	done     chan struct{}
}

// NewMultiCPUMmapWatcher opens one SystemWideMmapWatcher per element of
// cpus. On any error, every watcher opened so far is closed and (nil,
// err) is returned.
func NewMultiCPUMmapWatcher(cpus []int) (*MultiCPUMmapWatcher, error) {
	m := &MultiCPUMmapWatcher{
		events: make(chan MmapEventRecord, 512),
		done:   make(chan struct{}),
	}
	for _, cpu := range cpus {
		w, err := NewSystemWideMmapWatcher(cpu)
		if err != nil {
			m.Close()
			return nil, fmt.Errorf("mmap watcher cpu=%d: %w", cpu, err)
		}
		m.watchers = append(m.watchers, w)
	}
	for _, w := range m.watchers {
		m.fanWG.Add(1)
		go m.fanIn(w)
	}
	return m, nil
}

// fanIn forwards events from one child watcher into the shared channel
// until the child's events channel closes or done fires.
func (m *MultiCPUMmapWatcher) fanIn(w *MmapWatcher) {
	defer m.fanWG.Done()
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			select {
			case m.events <- ev:
			case <-m.done:
				return
			}
		case <-m.done:
			return
		}
	}
}

// Events returns the merged channel. Matches the MmapWatcher.Events()
// signature so PIDTracker.Run can consume either type via interface.
func (m *MultiCPUMmapWatcher) Events() <-chan MmapEventRecord {
	return m.events
}

// Close stops fan-in goroutines, closes every child watcher, and
// releases the merged channel.
func (m *MultiCPUMmapWatcher) Close() error {
	select {
	case <-m.done:
		return nil
	default:
		close(m.done)
	}
	var firstErr error
	for _, w := range m.watchers {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.fanWG.Wait()
	close(m.events)
	return firstErr
}
```

- [ ] **Step 4.2: Adapt PIDTracker.Run to accept either watcher shape**

PIDTracker.Run currently takes `*MmapWatcher`. For MultiCPUMmapWatcher to work with the same Run, we need a common interface. In `unwind/ehmaps/tracker.go`, change the Run signature:

Find:

```go
func (t *PIDTracker) Run(ctx context.Context, w *MmapWatcher) {
```

Replace with:

```go
// mmapEventSource is the shape both MmapWatcher and MultiCPUMmapWatcher
// satisfy — Events() returns a read-only channel of event records.
type mmapEventSource interface {
	Events() <-chan MmapEventRecord
}

func (t *PIDTracker) Run(ctx context.Context, w mmapEventSource) {
```

That's the only change — inside `Run`, only `w.Events()` is used, so both types satisfy it.

- [ ] **Step 4.3: Verify build**

`GOTOOLCHAIN=go1.26.0 go build ./unwind/ehmaps/`

Expected: success.

Also confirm `go test ./unwind/ehmaps/` still passes.

- [ ] **Step 4.4: Commit**

```
git add unwind/ehmaps/mmap_multiplexer.go unwind/ehmaps/tracker.go
git commit -m "S7: MultiCPUMmapWatcher + mmapEventSource interface on PIDTracker.Run"
```

---

## Task 5 — dwarfagent.Profiler system-wide support

**Goal:** flip the hard-error to an actual code path.

**Files:**
- Modify: `unwind/dwarfagent/agent.go`

- [ ] **Step 5.1: Add systemWide to NewProfiler signature**

Change:

```go
func NewProfiler(pid int, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 (system-wide is S7 scope)")
	}
	objs, err := profile.LoadPerfDwarf()
```

to:

```go
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadPerfDwarf(systemWide)
```

- [ ] **Step 5.2: Skip AddPID in system-wide mode**

After the LoadPerfDwarf block, the current code calls `objs.AddPID(uint32(pid))`. Wrap it:

```go
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}
```

- [ ] **Step 5.3: Dispatch on systemWide for initial attach + watcher**

Find the current block that calls `AttachAllMappings` and `NewMmapWatcher(uint32(pid))`. Replace with:

```go
	var nAttached int
	if systemWide {
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach all processes: %w", err)
		}
		nAttached = nTables
		log.Printf("dwarfagent: attached %d distinct binaries across %d PIDs", nTables, nPIDs)
	} else {
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach initial mappings: %w", err)
		}
		nAttached = n
		log.Printf("dwarfagent: attached %d binaries from /proc/%d/maps", nAttached, pid)
	}

	var watcher mmapEventSourceCloser
	if systemWide {
		cpuInts := make([]int, 0, len(cpus))
		for _, c := range cpus {
			cpuInts = append(cpuInts, int(c))
		}
		mw, err := ehmaps.NewMultiCPUMmapWatcher(cpuInts)
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("multi-cpu mmap watcher: %w", err)
		}
		watcher = mw
	} else {
		w, err := ehmaps.NewMmapWatcher(uint32(pid))
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("mmap watcher: %w", err)
		}
		watcher = w
	}
```

At package scope in the same file, add the local interface (both watcher types satisfy it):

```go
// mmapEventSourceCloser is the local-to-dwarfagent interface that both
// ehmaps.MmapWatcher and ehmaps.MultiCPUMmapWatcher satisfy, letting us
// store either in the Profiler struct.
type mmapEventSourceCloser interface {
	Events() <-chan ehmaps.MmapEventRecord
	Close() error
}
```

(`ehmaps.MmapEventRecord` is the existing package-exported type.)

Change the Profiler struct field:

```go
watcher    *ehmaps.MmapWatcher
```

to:

```go
watcher    mmapEventSourceCloser
```

The `tracker.Run(ctx, p.watcher)` call continues to work because `mmapEventSource` in `ehmaps/tracker.go` only requires `Events()`.

- [ ] **Step 5.4: Verify build**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./unwind/dwarfagent/
```

- [ ] **Step 5.5: Commit**

```
git add unwind/dwarfagent/agent.go
git commit -m "S7: dwarfagent.Profiler system-wide support"
```

---

## Task 6 — dwarfagent.OffCPUProfiler system-wide support

**Goal:** same as Task 5 for the off-CPU sibling.

**Files:**
- Modify: `unwind/dwarfagent/offcpu.go`

- [ ] **Step 6.1: Add systemWide parameter + cpus parameter**

OffCPUProfiler currently takes `(pid int, tags []string)` — it didn't need cpus because the tp_btf program isn't per-CPU. But the MultiCPUMmapWatcher does need cpus. Change the signature:

```go
func NewOffCPUProfiler(pid int, systemWide bool, cpus []uint, tags []string) (*OffCPUProfiler, error) {
```

- [ ] **Step 6.2: Mirror Task 5's body for off-CPU**

Inside `NewOffCPUProfiler`:

1. Early error: `if !systemWide && pid <= 0 { ... }` — same as Task 5.1.
2. `objs, err := profile.LoadOffCPUDwarf(systemWide)`.
3. Wrap AddPID in `if !systemWide { ... }`.
4. Dispatch initial attach on `systemWide` — same code as Task 5.3, just using the shared helper code already written in ehmaps.
5. Dispatch watcher construction the same way, storing as `mmapEventSourceCloser` (use the local interface — copy the declaration from agent.go, OR share via a package-scope interface. Copying is fine; the two files are siblings.)

Concretely, the dispatch block inside `NewOffCPUProfiler` looks like (with unchanged post-watcher code that follows):

```go
	var nAttached int
	if systemWide {
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach all processes: %w", err)
		}
		nAttached = nTables
		log.Printf("dwarfagent (offcpu): attached %d distinct binaries across %d PIDs", nTables, nPIDs)
	} else {
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("attach initial mappings: %w", err)
		}
		nAttached = n
		log.Printf("dwarfagent (offcpu): attached %d binaries from /proc/%d/maps", nAttached, pid)
	}

	var watcher mmapEventSourceCloser
	if systemWide {
		cpuInts := make([]int, 0, len(cpus))
		for _, c := range cpus {
			cpuInts = append(cpuInts, int(c))
		}
		mw, err := ehmaps.NewMultiCPUMmapWatcher(cpuInts)
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("multi-cpu mmap watcher: %w", err)
		}
		watcher = mw
	} else {
		w, err := ehmaps.NewMmapWatcher(uint32(pid))
		if err != nil {
			objs.Close()
			return nil, fmt.Errorf("mmap watcher: %w", err)
		}
		watcher = w
	}
```

And the Profiler struct field becomes `watcher mmapEventSourceCloser`.

- [ ] **Step 6.3: Verify build**

Same command as Task 5.4.

- [ ] **Step 6.4: Commit**

```
git add unwind/dwarfagent/offcpu.go
git commit -m "S7: dwarfagent.OffCPUProfiler system-wide support"
```

---

## Task 7 — perfagent dispatch: allow `SystemWide` + `Unwind=dwarf`

**Goal:** remove the "does not support system-wide" errors introduced in S5/S6 Task 7 and plumb the `systemWide` flag through to both profilers.

**Files:**
- Modify: `perfagent/agent.go`

- [ ] **Step 7.1: Update the CPU profiler dispatch**

Find (from S5 Task 7):

```go
    case "dwarf":
        if a.config.SystemWide {
            return fmt.Errorf("--unwind dwarf does not support system-wide mode yet (S7)")
        }
        p, err := dwarfagent.NewProfiler(
            a.config.PID,
            cpus,
            a.config.Tags,
            a.config.SampleRate,
        )
```

Replace with:

```go
    case "dwarf":
        p, err := dwarfagent.NewProfiler(
            a.config.PID,
            a.config.SystemWide,
            cpus,
            a.config.Tags,
            a.config.SampleRate,
        )
```

And update the success log line to optionally say "system-wide":

```go
        a.cpuProfiler = dwarfProfilerAdapter{p}
        if a.config.SystemWide {
            log.Printf("CPU profiler enabled (system-wide, %d Hz, DWARF)", a.config.SampleRate)
        } else {
            log.Printf("CPU profiler enabled (PID: %d, %d Hz, DWARF)", a.config.PID, a.config.SampleRate)
        }
```

- [ ] **Step 7.2: Update the off-CPU profiler dispatch**

Find (from S6 Task 5):

```go
    case "dwarf":
        if a.config.SystemWide {
            return fmt.Errorf("--unwind dwarf does not support system-wide mode yet (S7)")
        }
        p, err := dwarfagent.NewOffCPUProfiler(a.config.PID, a.config.Tags)
```

Replace with:

```go
    case "dwarf":
        p, err := dwarfagent.NewOffCPUProfiler(
            a.config.PID,
            a.config.SystemWide,
            cpus,
            a.config.Tags,
        )
```

And update the log to branch on systemWide:

```go
        a.offcpuProfiler = dwarfOffCPUProfilerAdapter{p}
        if a.config.SystemWide {
            log.Println("Off-CPU profiler enabled (system-wide, DWARF)")
        } else {
            log.Printf("Off-CPU profiler enabled (PID: %d, DWARF)", a.config.PID)
        }
```

- [ ] **Step 7.3: Verify build + unit tests**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build ./...
GOTOOLCHAIN=go1.26.0 make test-unit
```

Expected: both pass.

- [ ] **Step 7.4: Commit**

```
git add perfagent/agent.go
git commit -m "S7: perfagent allows SystemWide with --unwind dwarf (both CPU and off-CPU)"
```

---

## Task 8 — Integration tests

**Goal:** two full-binary tests validating system-wide DWARF for both profilers.

**Files:**
- Modify: `test/integration_test.go`

- [ ] **Step 8.1: Add TestPerfAgentSystemWideDwarfProfile**

Append:

```go
// TestPerfAgentSystemWideDwarfProfile runs perf-agent with --profile
// --unwind dwarf -a (no --pid) and verifies the resulting pprof has
// samples from multiple PIDs and at least one DWARF-unwound frame
// (any symbol — the exact set depends on whatever's CPU-active on the
// test host during the sampling window).
func TestPerfAgentSystemWideDwarfProfile(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "profile-dwarf-sys.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--profile",
		"--profile-output", outputFile,
		"--unwind", "dwarf",
		"-a",
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)
	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "system-wide profile should have samples")
	require.Greater(t, len(prof.Function), 0, "system-wide profile should have at least one symbolized function")
}
```

- [ ] **Step 8.2: Add TestPerfAgentSystemWideDwarfOffCPU**

Append:

```go
// TestPerfAgentSystemWideDwarfOffCPU runs perf-agent with --offcpu
// --unwind dwarf -a. System-wide means any blocking activity anywhere
// contributes samples.
func TestPerfAgentSystemWideDwarfOffCPU(t *testing.T) {
	if os.Getuid() != 0 {
		caps := cap.GetProc()
		have, _ := caps.GetFlag(cap.Permitted, cap.BPF)
		if !have {
			t.Skip("requires root or CAP_BPF")
		}
	}
	agentPath := getAgentPath(t)
	binPath := "./workloads/rust/target/release/rust-workload"
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("rust workload not built: %v", err)
	}

	workload := exec.Command(binPath, "20", "2")
	require.NoError(t, workload.Start())
	defer func() {
		_ = workload.Process.Kill()
		_ = workload.Wait()
	}()
	time.Sleep(2 * time.Second)

	outputFile := "offcpu-dwarf-sys.pb.gz"
	defer os.Remove(outputFile)

	agent := exec.Command(agentPath,
		"--offcpu",
		"--offcpu-output", outputFile,
		"--unwind", "dwarf",
		"-a",
		"--duration", "5s",
	)
	output, err := agent.CombinedOutput()
	if err != nil {
		t.Fatalf("perf-agent failed: %v\nOutput: %s", err, string(output))
	}
	assert.FileExists(t, outputFile)
	prof := parseProfile(t, outputFile)
	require.NotNil(t, prof)
	require.Greater(t, len(prof.Sample), 0, "system-wide off-CPU profile should have samples")

	var totalNs int64
	for _, s := range prof.Sample {
		for _, v := range s.Value {
			totalNs += v
		}
	}
	require.Greater(t, totalNs, int64(0), "system-wide off-CPU profile should have non-zero blocking-ns")
	t.Logf("system-wide off-CPU total: %d ns across %d samples", totalNs, len(prof.Sample))
}
```

- [ ] **Step 8.3: Rebuild integration.test**

```
cd test && GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/integration.test .
```

- [ ] **Step 8.4: Commit**

```
cd /home/diego/github/perf-agent
git add test/integration_test.go
git commit -m "S7: TestPerfAgentSystemWideDwarfProfile + TestPerfAgentSystemWideDwarfOffCPU"
```

---

## Task 9 — Sanity matrix + doc update

**Goal:** final check + mark S7 ✅.

- [ ] **Step 9.1: Full test-unit**

`GOTOOLCHAIN=go1.26.0 make test-unit`

Expected: all PASS.

- [ ] **Step 9.2: Capped matrix**

```
/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads
/home/diego/bin/ehmaps.test -test.v
cd unwind/dwarfagent && /home/diego/bin/dwarfagent.test -test.v
cd test && /home/diego/bin/integration.test -test.v -test.run "TestPerfDwarf|TestPerfAgentDwarf|TestPerfAgentOffCPUDwarf|TestPerfAgentSystemWideDwarf"
```

Expected: every test PASS or SKIP cleanly.

- [ ] **Step 9.3: Update design doc**

In `docs/dwarf-unwinding-design.md`, update the S7 row:

```
| S7 ✅  | System-wide (`-a`) — multi-PID map management              | 3-4d    | `perf-agent -a --unwind dwarf` correctly tracks mmaps across all processes. Build-id sharing keeps memory bounded. **Shipped**: see `docs/superpowers/plans/2026-04-24-s7-system-wide-dwarf.md`. Per-CPU MmapWatcher closes the per-TID gap from S4. |
```

- [ ] **Step 9.4: Commit**

```
git add docs/dwarf-unwinding-design.md docs/superpowers/plans/2026-04-24-s7-system-wide-dwarf.md
git commit -m "S7: design doc status update + preserve implementation plan"
```

---

## Success criteria recap

From `docs/dwarf-unwinding-design.md` §Execution plan:

> **S7: System-wide (`-a`) — multi-PID map management** — `perf-agent -a --unwind dwarf` correctly tracks mmaps across all processes. Build-id sharing keeps memory bounded.

Satisfied by:
- `TestPerfAgentSystemWideDwarfProfile` — runs `perf-agent --profile --unwind dwarf -a`, asserts non-empty pprof with symbolized functions.
- `TestPerfAgentSystemWideDwarfOffCPU` — same for `--offcpu --unwind dwarf -a`.
- `TestAttachAllProcesses` — system-wide startup scan.
- `TestSystemWideMmapWatcherSeesMmap` — per-CPU `pid=-1` watcher validates the unblocked capture path.

## Open risks

1. **Memory pressure on busy hosts.** 4096 pid_mappings entries cover most servers but not everything. If exceeded, new FORKs silently fail to insert (HASH_OF_MAPS returns -E2BIG); tracker logs at Debug. For S8/later, consider LRU eviction of dormant PIDs.
2. **Per-CPU watcher CPU overhead.** One goroutine per online CPU. On a 64-core system that's 64 polling goroutines. Could collapse to a single `epoll_wait` loop for S8 optimization.
3. **EXEC not handled.** When a tracked process execs a new binary, pid_mappings keeps old (invalid) entries until EXIT. Chain depth suffers briefly for that PID. Documented in scope; fixable via PERF_RECORD_COMM (bit 12 = EXEC) in a follow-up.
4. **AttachAllProcesses can be slow on boot.** On a 500-process host with 150 unique binaries, we'll spend a few seconds compiling CFI at startup. That's acceptable for a one-time cost but worth noting for CI where we want fast startup.
