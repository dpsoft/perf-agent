# S9-Narrow: pprof Address + Mapping Fidelity — Design Spec

**Status:** design approved, implementation plan pending.
**Predecessor:** S3–S8 (DWARF unwinding feature, branch `feat/dwarf-unwinding`).
**Successor (out of scope here):** S10 — pprof → LLVM sample-profile converter for Rust PGO.

## 1. Problem

The current pipeline discards each sample's absolute PC after blazesym symbolization and collapses all samples into a single hardcoded `pprof.Mapping{ID: 1}`. Three consequences:

1. `profile.Location.Address` is always 0 — downstream tools cannot round-trip addresses back to binary offsets.
2. No per-binary `Mapping` identity (`Start`, `Limit`, `Offset`, `BuildID`) — ASLR-aware consumers (`llvm-profdata`, `create_llvm_prof`) can't match samples to ELF files.
3. Two PCs at the same source `(file, line, func)` collapse into one Location even when they are different instructions. Less common than the spec's framing suggests (we already dedup by line, so *different lines* already produce distinct Locations), but still a fidelity loss.

## 2. Goal

Preserve per-frame `Address` and produce a real per-binary `pprof.Mapping` table, so any pprof consumer (including the future S10 PGO converter) can attribute samples to ELF file offsets.

## 3. Non-goals

- No PGO-specific code paths, no LLVM sample-profile emission (that is S10).
- No `Discriminator` / column plumbing (spec §8; deferred until a consumer needs it).
- No persistent cross-run build-id cache.
- No changes to the FP or DWARF walker BPF programs; this is userspace-only.

## 4. Data Model

### 4.1 `pprof.Frame` additions (`pprof/pprof.go`)

All additions zero-valued by default; existing callers compile unchanged.

```go
type Frame struct {
    Name   string
    File   string
    Line   uint32
    Module string // binary/SO path; retained for existing fallback path

    Address  uint64 // absolute PC captured from the BPF stack
    BuildID  string // ELF GNU build-id hex; "" if mapping has no .note.gnu.build-id
    MapStart uint64 // mapping start in the target VA
    MapLimit uint64 // mapping end, exclusive
    MapOff   uint64 // PT_LOAD p_offset for the mapping
    IsKernel bool   // kernel frame sentinel — skips resolver, uses [kernel] mapping
}
```

### 4.2 Intern keys (`pprof/pprof.go`)

Replace the current `frameKey`/`functionKey` pair with an address-first scheme and a name-based fallback:

```go
type mappingKey struct {
    Path    string
    Start   uint64
    Limit   uint64
    Off     uint64
    BuildID string
}

type locationKey struct {
    MappingID uint64 // primary
    Address   uint64 // file offset (Address - MapStart + MapOff)
}

type locationFallbackKey struct {
    Name, File, Module string
    Line               uint32
} // used when Address == 0 OR resolver Lookup returns ok=false

type functionKey struct {
    MappingID uint64
    Name      string
}
```

**Semantic change:** `functionKey` gaining `MappingID` means a function name that appears in two binaries (e.g. `main.main` in both the target and a vendored tool invoked via exec) now dedups to two separate `profile.Function` entries. This is the pprof-correct behavior — functions belong to binaries — but it changes existing output for anyone diffing Function counts across runs.

### 4.3 `pprof.Mapping` flags

Set per-mapping based on the best symbolization seen inside that mapping:

| Flag               | True when                                     |
|--------------------|-----------------------------------------------|
| `HasFunctions`     | any frame in the mapping has `Name != ""`     |
| `HasFilenames`     | any frame has `File != ""`                    |
| `HasLineNumbers`   | any frame has `Line > 0`                      |

## 5. `unwind/procmap` — New Package

Single-purpose: parse `/proc/<pid>/maps` and ELF `.note.gnu.build-id`, cache per-PID, serve `Lookup(pid, addr) → Mapping`.

### 5.1 Public API

```go
package procmap

type Mapping struct {
    Path    string
    Start   uint64
    Limit   uint64
    Offset  uint64
    BuildID string
    IsExec  bool
}

type Resolver struct { /* ... */ }

func NewResolver(opts ...Option) *Resolver
func (r *Resolver) Lookup(pid uint32, addr uint64) (Mapping, bool)
func (r *Resolver) Invalidate(pid uint32)
func (r *Resolver) InvalidateAddr(pid uint32, addr uint64)
func (r *Resolver) Close()

// Options
func WithProcRoot(path string) Option // defaults to "/proc"; for unit tests
```

### 5.2 Internal shape

```go
type pidEntry struct {
    once     sync.Once
    err      error
    mappings []Mapping // sorted by Start; binary-searched on Lookup
}

type Resolver struct {
    mu       sync.RWMutex
    cache    map[uint32]*pidEntry
    buildIDs sync.Map // map[string]string — path → build-id hex, global
    procRoot string
}
```

### 5.3 Behavior rules

- **Lazy populate:** first `Lookup` for a PID triggers `/proc/<pid>/maps` parse under the PID's `sync.Once`.
- **Build-id read:** only for executable mappings with `Path != ""` and not already in `buildIDs`. Reads the `.note.gnu.build-id` section via `debug/elf`. Failures produce `BuildID=""`, never a Lookup failure.
- **`Invalidate(pid)`:** drops the entry; next Lookup re-parses. Called on EXIT events (DWARF path) and on PID-reuse suspicion.
- **`InvalidateAddr(pid, addr)`:** forces re-parse if `addr` falls outside any cached mapping; a no-op otherwise. Called on MMAP2 events (DWARF path).
- **Missing PID (`ESRCH` on /proc read):** caches an empty `mappings` slice; subsequent Lookups return `ok=false` fast without re-reading /proc.

### 5.4 Concurrency

`RWMutex` guards `cache`. Hot path (`Lookup`) takes RLock, binary-searches the pre-sorted slice, releases. Cold path (first populate) takes RLock to find the entry, then the entry's `sync.Once` serializes the one-time fill. `buildIDs` is `sync.Map` because writes are rare and bursty (one per unique binary path seen).

## 6. Symbolize Path Integration

### 6.1 `blazeSymToFrames` — thread the PC through

Both copies (`profile/profiler.go:53`, `unwind/dwarfagent/symbolize.go:17`) change signature to accept the absolute PC. The outer real frame and every inlined frame receive the same `Address` — pprof's convention for a Location that spans multiple lines.

```go
func blazeSymToFrames(s blazesym.Sym, addr uint64) []pprof.Frame
```

Callers:

- `profile/profiler.go:215` — `bpfstack.ExtractIPs(stack)` already returns `[]uint64` parallel to `symbols`. Zip them.
- `unwind/dwarfagent/symbolize.go:41` — `symbolizePID` already receives `ips []uint64`. Zip them.

### 6.2 `[unknown]` and perf-map frames

**`[unknown]`** (blazesym resolved nothing): emit the frame with `Address` set to the original IP from the stack. Dedup uses `locationKey{MappingID, Address}` when the resolver can still attribute the IP to a mapping (common — blazesym may fail for reasons other than a missing mapping); falls back to `locationFallbackKey` otherwise.

**Perf-map runtime frames (Python/Node JIT):** `decodePerfMapFrame` zeros `Address` when it recognizes a perf-map format. JIT'd code lives in anonymous mmaps whose file offset is meaningless as a Location key, so these frames always use `locationFallbackKey` and attach to a sentinel `[jit]` mapping (one per profile).

### 6.3 Kernel frames

blazesym marks kernel modules with `Module` prefixed `[kernel]` or similar. `blazeSymToFrames` sets `IsKernel=true` when `Module` matches known kernel prefixes (or when the PC sits in the kernel half of the VA split on the target arch). Kernel frames bypass the resolver and get a synthetic sentinel mapping.

## 7. `pprof.ProfileBuilder` Changes

### 7.1 Wire the resolver

```go
type BuildersOptions struct {
    SampleRate    int64
    PerPIDProfile bool
    Comments      []string
    Resolver      *procmap.Resolver // S9: nil → fallback to current single-mapping behavior
}
```

`Resolver==nil` preserves today's semantics for tests and any caller that hasn't migrated. When set, the builder uses it for every `addLocation` call.

### 7.2 `addLocation` rewrite

```
1. frame = decodePerfMapFrame(frame)          // unchanged perf-map decode
2. If frame.IsKernel:
       mapping := b.addMapping(kernelSentinel)
       return b.addLocationByAddr(mapping, frame)
3. If frame.Address != 0 and b.resolver != nil:
       m, ok := b.resolver.Lookup(sample.Pid, frame.Address)
       if ok:
           // Fill MapStart/Limit/Off/BuildID so addMapping sees full identity.
           frame.MapStart, frame.MapLimit, frame.MapOff, frame.BuildID =
               m.Start, m.Limit, m.Offset, m.BuildID
           mapping := b.addMapping(m.Path, frame)
           return b.addLocationByAddr(mapping, frame)
4. Fallback: current name-based path using b.Profile.Mapping[0]
```

`addMapping` interns by `mappingKey`; updates `HasFunctions`/`HasFilenames`/`HasLineNumbers` flags when the frame supplies evidence.

`addLocationByAddr` sets `profile.Location.Address = frame.Address - frame.MapStart + frame.MapOff` — the binary-relative file offset. Absolute VAs aren't portable across runs; file offsets are.

`sample.Pid` propagation: `AddSample` already has access to the `ProfileSample.Pid`; pass it through to `CreateSample`/`CreateSampleOrAddValue` so `addLocation` can call `resolver.Lookup(pid, addr)`.

### 7.3 Sentinel mappings

```go
var kernelSentinel = procmap.Mapping{Path: "[kernel]"}
var jitSentinel    = procmap.Mapping{Path: "[jit]"}
```

Exactly one `pprof.Mapping` per sentinel per builder. Kernel VA space is shared across PIDs; JIT anonymous mappings are per-PID but their file-offset identity is meaningless, so they collapse into one sentinel for the whole profile.

## 8. Profiler Wiring

### 8.1 FP profiler (`profile/profiler.go`)

- `NewProfiler` creates `procmap.Resolver`, stores on `Profiler`.
- `Collect` threads the resolver into `BuildersOptions`.
- No invalidation logic — symbolize happens in one Collect pass per run; PID reuse within a window is not a realistic concern.
- `Close` calls `resolver.Close`.

### 8.2 DWARF profiler (`unwind/dwarfagent/`)

- `session` (in `common.go`) grows `resolver *procmap.Resolver`; created in `newSession`, shared by both CPU and off-CPU profilers embedding `*session`.
- `runTracker` already processes MMAP2/EXIT/FORK events for ehmaps; add a sibling dispatch that calls `resolver.InvalidateAddr(pid, addr)` on MMAP2 and `resolver.Invalidate(pid)` on EXIT.
- FORK events copy parent state lazily — no resolver action needed; the child PID populates on first Lookup.

## 9. Graceful Degradation Matrix

| Case                                  | Outcome                                                        |
|---------------------------------------|----------------------------------------------------------------|
| PID exited before symbolize           | Lookup returns `ok=false` → fallback mapping; frame still emitted |
| ELF has no `.note.gnu.build-id`       | `BuildID=""`; Mapping otherwise complete                       |
| blazesym returns `Module==""`         | Resolver Lookup returns `Path` — fill from there               |
| Kernel frame                          | `IsKernel=true` → `[kernel]` sentinel mapping                  |
| Synthetic `[unknown]` frame           | Address still set from IP; fallback key if resolver misses     |
| Perf-map runtime frame (Python/Node)  | `decodePerfMapFrame` zeros Address; fallback key; `[jit]` sentinel mapping |

## 10. Test Plan

### 10.1 Unit — `unwind/procmap/`

- `TestResolverParsesMaps` — fake `/proc` root with hand-crafted `maps` + binaries → verify sorted entries, exec flag, lookup by addr.
- `TestResolverBuildID` — ELF fixture with `.note.gnu.build-id` → hex matches expected.
- `TestResolverInvalidate` — populate, `Invalidate(pid)`, re-populate from modified maps.
- `TestResolverMissingPID` — Lookup on non-existent PID returns `ok=false`, no panic.
- `TestResolverConcurrent` — N goroutines Lookup same PID → `sync.Once` ensures single parse.

### 10.2 Unit — `pprof/`

- `TestAddLocationAddressKeyed` — two frames same func/file/line but distinct Addresses → two Locations.
- `TestAddLocationFallback` — resolver returns `ok=false` → single-mapping fallback, location dedup by name.
- `TestMappingFlags` — DWARF-rich frame flips `HasFilenames/HasLineNumbers`; name-only frame flips only `HasFunctions`.
- `TestKernelSentinel` — kernel frames share one mapping regardless of source PID.

### 10.3 Integration — `test/integration_test.go`

- Extend `TestProfileMode` and `TestPerfAgentSystemWideDwarfProfile`:
  - Parse output pprof with `github.com/google/pprof/profile`.
  - Assert `len(profile.Mapping) >= 2` (target binary + at least libc).
  - Assert at least one `Mapping.BuildID != ""`.
  - Assert every non-kernel, non-`[unknown]` Location has `Address != 0`.
  - Assert schema compatibility: `go tool pprof -raw` decodes without error.

### 10.4 Regression

- `TestOffCPUMode`, `TestCombinedMode`, existing symbolization tests pass with richer but schema-compatible output.

## 11. Cleanup (bundled into the final plan tasks)

Two non-feature cleanups to land in the same branch, isolating them as their own commits:

### 11.1 Delete unused diagnostic binaries

- `cmd/perf-dwarf-test/` — S2-era raw sample dumper; superseded by integration tests.
- `cmd/perfreader-test/` — kernel-capture smoke test; superseded.
- `cmd/test_blazesym/` — blazesym smoke test; superseded by the symbolize unit tests.

None are wired into the Makefile, CI, or reachable from `main.go`. Removing the `cmd/` tree entirely.

### 11.2 Strip stage markers from code

~20 comment lines across the tree reference `S2`..`S8` — the internal stage numbering from the DWARF implementation plan. These are historical scaffolding; a future reader shouldn't need to know which stage shipped what. Each comment gets rewritten to describe what the code *does* today, not when it arrived.

Scope (from `rg -nE '\bS[2-9]\b'`):

- `bpf/offcpu_dwarf.bpf.c`, `bpf/perf_dwarf.bpf.c`, `bpf/unwind_common.h`
- `unwind/dwarfagent/sample.go`
- `unwind/ehmaps/ehmaps.go`, `store.go`, `tracker.go`, `tracker_test.go`
- `unwind/ehcompile/types.go`
- `profile/perf_dwarf_test.go`
- `perfagent/options.go`
- `test/integration_test.go`

## 12. Migration Order (informs the plan)

1. Land `unwind/procmap` package + unit tests.
2. Add `Frame` fields + intern keys in `pprof/`; keep `Resolver=nil` path for back-compat; unit tests for both branches.
3. Thread `Address` through both `blazeSymToFrames` variants; wire IP parallel arrays.
4. Wire `procmap.Resolver` into `profile.NewProfiler` and `dwarfagent.session`; add MMAP2/EXIT invalidation shim in the DWARF path.
5. Update integration tests to assert the new fidelity guarantees.
6. Delete `cmd/` diagnostic binaries.
7. Strip `SX` stage markers repo-wide.

Each step is independently shippable and independently testable.

## 13. Open Questions (record during implementation)

- Do we need a bound on `Resolver.cache` size for long-running system-wide captures? `Invalidate` on EXIT keeps it right-sized in the DWARF path; FP path could accumulate entries for exited PIDs if the symbolize pass straddles many short-lived processes. Defer until we see it in a real workload.
- Should `HasInlineFrames` become a fourth mapping flag? blazesym gives it to us for free (`s.Inlined != nil`). Nice-to-have; not load-bearing for PGO.
- Kernel frame detection heuristic — prefix-match `Module` or VA-split test? Start with prefix-match (cheap, blazesym-provided) and revisit if it misattributes on exotic kernels.
