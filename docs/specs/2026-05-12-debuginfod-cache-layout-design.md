# Debuginfod cache layout fix — design spec

**Date:** 2026-05-12
**Status:** Draft, pending implementation
**Scope:** v1.2.0 follow-up to v1.1.0 debuginfod work
**Related:** PR #19 (debuginfod M1), PR #21 (kernel-stacks M1)

## Problem statement

v1.1.0 shipped debuginfod-backed off-box symbolization (PR #19). In practice the path is broken for the most common case — a stripped binary whose only debug-info pointer is its build-id (no `.gnu_debuglink` section):

1. perf-agent's dispatcher fetches the `.debug` into `<cacheDir>/.build-id/NN/REST.debug`.
2. The dispatcher returns `""` to blazesym, expecting blazesym to find the cached file.
3. **blazesym never looks there.** Its debug-info lookup is gated entirely on `.gnu_debuglink` (`src/dwarf/resolver.rs::try_deref_debug_link`). The `BUILD_ID_DEBUG_DIR` build-id walker is hardcoded to `/usr/lib/debug/.build-id` (`src/elf/mod.rs:15`) and is only reached *via* the debug-link path. blazesym has no built-in debuginfod-protocol client.
4. Symbols come back unresolved as `[binary-name]` mapping fallback.

Rust and Go release builds do not emit `.gnu_debuglink` by default. The distro debuginfo-install workflow (which does add it) covers system packages but not user binaries — exactly the binaries operators want to profile.

## Goals

- **G1**: Off-box symbolization works end-to-end for stripped binaries with only `.note.gnu.build-id` (no `.gnu_debuglink`).
- **G2**: System libraries (libc, libstdc++, libpthread, ld-linux) continue to resolve through their existing distro debuginfo paths when available — we do not refetch what's already on disk.
- **G3**: No regressions for binaries that *do* have local DWARF or resolvable debug-link.
- **G4**: Integration tests cover the stripped-no-debuglink case with both a Rust and a Go workload, end-to-end through a local debuginfod server.

## Non-goals

- Kernel module off-box symbolization (separate concern, blazesym kernel API path).
- Synthesizing `.gnu_debuglink` sections into user binaries (the three reference projects all avoid this; we follow suit).
- Replacing blazesym with a custom DWARF parser (Pyroscope-style Lidia). Out of scope.
- Object-store / network cache (Parca/Pyroscope upload `.debug` to S3-compatible storage; we keep filesystem-only).

## Industry references

Research summary (full notes in commit history):

| Project | Stripped+build-id path |
|---|---|
| **Parca** | Normalizes addresses in Go (`pkg/symbolizer/normalize.go`), opens the fetched `.debug` directly, never touches `.gnu_debuglink`. |
| **Pyroscope** | Same: agent ships `(build-id, file-offset)`, backend symbolizes against the fetched `.debug` using their custom Lidia indexer. |
| **OpenTelemetry eBPF profiler** | Same: `pfelf.AddressMapper` (`libpf/pfelf/addressmapper.go`) does PHDR-based normalization including the page-alignment correction. The collector ships `(host.FileID, file-VA-address)` pairs to the backend. |

**Common pattern**: do address normalization in Go using `/proc/<pid>/maps` + ELF program headers, then symbolize against the cached `.debug` file directly. blazesym's debug-link/build-id resolver becomes irrelevant — the symbolizer just receives `(elf_path, file_va)` and produces frames.

We adopt this pattern (Option B from brainstorming) as the primary fix.

## Architecture

```
DebuginfodSymbolizer.Symbolize(pid, ips) → frames
│
├─ procmapClassifier.classify(pid)            ← read /proc/<pid>/maps once per PID
│   └─ for each mapping: inspect binary →
│       ├─ has local DWARF or resolvable .gnu_debuglink → ROUTE: process-mode
│       └─ build-id only, no local DWARF                → ROUTE: file-mode
│           └─ ensure cached .debug (fetch if miss)
│
├─ split ips by mapping route
│
├─ Batch 1 — process-mode addresses:
│       blaze_symbolize_process_abs_addrs(csym, pid, ips_batch1)
│       (blazesym walks /proc/<pid>/maps + opens binaries.
│        Dispatcher provides Cases 1/2/4 only — Case 3 is removed.)
│
├─ Batch 2 — file-mode addresses (one call per cached .debug):
│       for each mapping in batch2:
│           normalized = AddressMapper(mapping).normalize(ip)
│           blaze_symbolize_elf_virt_offsets(
│               csym,
│               &src{ path: cached_debug, debug_syms: true },
│               [normalized...])
│
└─ merge results ordered by original ip index
```

### Classification

`procmapClassifier` runs per-PID, lazily on first symbolize call for that PID. It reads `/proc/<pid>/maps`, walks each executable mapping (`r-xp` or `r--p` with executable bit), and inspects the backing ELF:

```
classify(mapping) → route:
  if mapping.path is empty or [vdso] or [stack]:    skip (no symbolization possible)
  if hasDwarf(mapping.path):                        process-mode
  if hasResolvableDebuglink(mapping.path, []):      process-mode
  if buildID(mapping.path) != "":                   file-mode
                                                    (fetch .debug if not cached)
  else:                                             skip
```

Helpers reuse existing `symbolize/debuginfod/resolution.go::hasDwarf` and `hasResolvableDebuglink`. The check order matches blazesym's resolution preference.

State is cached on the `Symbolizer` keyed by PID (`sync.Map[int]*pidClass`). Eviction hooks into the existing `procmap.Resolver` PID-exit signal.

### Address normalization

Port `AddressMapper` from `open-telemetry/opentelemetry-ebpf-profiler` (`libpf/pfelf/addressmapper.go`, Apache-2.0, ~70 LOC) into `unwind/procmap/addressmapper.go`. Inline credit comment:

```go
// AddressMapper is a port of pfelf.AddressMapper from
// github.com/open-telemetry/opentelemetry-ebpf-profiler
// (libpf/pfelf/addressmapper.go) — Apache-2.0, used per §4 of the license.
// Original copyright: Elasticsearch B.V. / OpenTelemetry Authors.
```

The mapper exposes:

```go
type AddressMapper struct {
    pageSize uint64
    loads    []ptLoad  // executable PT_LOADs, sorted by Off
}

func NewAddressMapper(path string) (*AddressMapper, error)
func (m *AddressMapper) FileOffsetToVirtualAddress(off uint64) (uint64, bool)
```

Symbolization-time computation per file-mode mapping:

```
elfVA, ok := mapper.FileOffsetToVirtualAddress(mapping.FileOffset)
if !ok { skip }
bias := mapping.Start - elfVA
fileVA := pid_pc - bias
```

**Three correctness details we preserve**:

1. **Page-align PT_LOAD `p_offset` before range comparison** (`libpf/pfelf/addressmapper.go:44-49`):
   ```go
   aligned := l.Off &^ (m.pageSize - 1)
   if off >= aligned && off < aligned + l.Filesz { … }
   ```
   Mirrors how the kernel/glibc mmap-aligns segment offsets; without this we silently misattribute frames near segment starts.

2. **Return-address `pc - 1` adjustment** for non-leaf frames (`manager.go:256-258`):
   ```go
   if !isLeaf { ip-- }
   ```
   So resolution lands at the call site, not the instruction after. Applied to all frames except the topmost. The current FP/DWARF unwinders pass raw IPs; this adjustment moves into the symbolize call site.

3. **PIE vs non-PIE**: same algorithm — the bias `mapping.Start - elfVA` handles both transparently.

Cache: one `AddressMapper` per unique `(path, st_ino)` per PID, stashed in the PID's `pidClass`.

### blazesym file-mode wrapper

New C glue in `symbolize/debuginfod/dispatcher.go`:

```c
static const blaze_syms* symbolize_elf_virt(
    blaze_symbolizer* sym,
    const char* path,
    const uint64_t* virt_offsets,
    size_t cnt) {
    blaze_symbolize_src_elf src;
    memset(&src, 0, sizeof(src));
    src.type_size = sizeof(src);
    src.path = path;
    src.debug_syms = true;
    return blaze_symbolize_elf_virt_offsets(sym, &src, virt_offsets, cnt);
}
```

Go wrapper alongside the existing `symbolizeProcess`:

```go
func (st *cgoState) symbolizeElfVirt(path string, virtOffsets []uint64) ([]symbolize.Frame, error)
```

Reuses the existing `sym_at` / `inlined_at` C indexers and the user-side `frameFromCSym` for the result conversion (function name, file, line, column, inlined chain).

### Dispatcher simplification

`dispatchWithBuildID` cases reduce from 4 to 3:

| Case | Status |
|---|---|
| 1 (`cache.Has(KindExecutable)`) | Keep |
| 2 (`localResolutionPossible`) | Keep |
| ~~3 (`binaryReadable` → fetch debuginfo, return "")~~ | **Remove** — file-mode owns this |
| 4 (binary not on disk → fetch executable, return abs) | Keep — sidecar case |

Replace Case 3 with a panic guard (`log.Panic("dispatcher Case 3 reached — file-mode classifier should have caught this")`). Process-mode batches only contain mappings classified as Case 1/2/4-eligible, so the guard is defense-in-depth, not user-visible.

Stats removed: `cacheMisses` for Case 3, `fetchSuccessDebuginfo`. New stats added (see Observability below).

### Cache layout

**Unchanged**: `<cacheDir>/.build-id/NN/REST.debug`. With file-mode, blazesym receives absolute paths directly; no `debug_dirs` wiring, no flat symlinks, no debug-link synthesis.

`index.db` SQLite (LRU eviction) — unchanged.

### Failure modes

| Failure | Fallback |
|---|---|
| Classification: can't read ELF | Route via process-mode (let blazesym try) |
| Classification: no build-id | Process-mode (no off-box option anyway) |
| `AddressMapper` can't find PT_LOAD for an offset | Skip that address — frame name = `[binary]:offset` (current behavior) |
| Debuginfod fetch fails (404, timeout, all URLs exhausted) | Mapping demoted to "best-effort process-mode" for this session. Negative result cached for 5 min so we don't retry every batch. One log line per `(pid, build-id)`. |
| Cached `.debug` is corrupt / `blaze_symbolize_elf_virt_offsets` returns NULL | Demote mapping to process-mode for this PID's lifetime. **Don't** poison the cache (the file may be valid for another PID; this is a blazesym-side parse issue). |
| `--symbol-fail-closed` is set | Refuse to emit any frame for this mapping; pprof gets `[unresolved]` instead of `[binary]:off`. (Existing flag, semantics extended naturally.) |

### Observability

New atomic counters on `Symbolizer.stats`:

```go
classifyProcessMode atomic.Uint64
classifyFileMode    atomic.Uint64
classifySkipped     atomic.Uint64       // classification failed (unreadable, no path)
fileModeCalls       atomic.Uint64
fileModeFetchFails  atomic.Uint64       // cache miss + fetch failed
fileModeParseFails  atomic.Uint64       // blazesym returned NULL on .debug
normalizationFails  atomic.Uint64       // AddressMapper miss for an address
```

Logged at agent shutdown alongside existing dispatcher counters. No new env vars or CLI flags.

### Concurrency

- `Symbolizer.pids sync.Map[int]*pidClass` — per-PID classification cache. Populated lazily, evicted on PID exit (hook into existing `procmap.Resolver`).
- `pidClass` holds:
  - `routes map[uint64]routeKind` — keyed by mapping start address
  - `mappers map[string]*AddressMapper` — keyed by ELF path
  - `negFetch map[string]time.Time` — keyed by build-id, 5-min TTL
- All maps are written once at classify time, read-only thereafter (per PID's lifetime). Reader/writer mutex on `pidClass` itself; no per-map locking needed.
- Fetch dedup remains in `singleflightFetcher`.

## Component breakdown

| New / changed file | Lines (est.) | Responsibility |
|---|---|---|
| `unwind/procmap/addressmapper.go` (NEW) | ~80 | Ported AddressMapper + page-alignment + tests |
| `unwind/procmap/addressmapper_test.go` (NEW) | ~120 | Unit tests for normalization |
| `symbolize/debuginfod/classifier.go` (NEW) | ~150 | `procmapClassifier`, `pidClass`, route decisions |
| `symbolize/debuginfod/classifier_test.go` (NEW) | ~200 | Table-driven classification tests |
| `symbolize/debuginfod/dispatcher.go` (MODIFY) | -30 / +80 | Remove Case 3, add `symbolize_elf_virt` cgo wrapper, add `symbolizeElfVirt` Go wrapper |
| `symbolize/debuginfod/symbolizer.go` (MODIFY) | +60 | Per-mapping routing in `Symbolize()`, batch splitting, result merging |
| `symbolize/debuginfod/stats.go` (MODIFY) | +20 | New counters |
| `test/integration_test.go` (MODIFY) | +180 | `TestStrippedRustOffBoxSymbolization`, `TestStrippedGoOffBoxSymbolization`, `TestStrippedCachedHitNoFetch`, `TestOffBoxLibcResolution` |
| `test/integration_strip_helpers.go` (NEW) | ~50 | Strip-binary + extract-debug helpers |
| `docs/specs/2026-05-12-debuginfod-cache-layout-design.md` (this doc) | - | - |

Net change: roughly +900 LOC, -30 LOC removed. New code is mostly testable in isolation; existing dispatcher tests are updated for the simplified case structure.

## Test plan

### Unit tests (no root, fast)

`unwind/procmap/addressmapper_test.go`:
- Address at first byte of segment → first byte of file-VA range
- Address at last byte of segment → last byte of file-VA range
- Address before any segment → returns `(0, false)`
- Address in gap between segments (multi-`PT_LOAD` ELF) → `(0, false)`
- Page-alignment: offset 0x1000 with `p_offset = 0x1234, filesz = 0x2000` → routes to that segment (without page-align trick, would fall outside)
- PIE binary (ET_DYN) and non-PIE (ET_EXEC) parsed identically

`symbolize/debuginfod/classifier_test.go`:
- Binary with `.debug_info` present → process-mode
- Binary with `.gnu_debuglink` resolvable in `/usr/lib/debug/` (mocked) → process-mode
- Binary stripped, build-id present, no debug-link → file-mode
- Binary stripped, no build-id → skipped
- Path is `[vdso]` / `[stack]` / `[anon]` → skipped
- Read error on ELF → process-mode (defensive fallback)
- Cache hit short-circuits re-classification within the same PID

`symbolize/debuginfod/dispatcher_test.go`:
- Existing tests updated for 3-case structure
- New test: assertion fires if Case 3 path is hit via the test entry point

### Integration tests (caps, hermetic debuginfod)

Reuse the existing `test/debuginfod/docker-compose.yml` + `upload.sh` harness. All new tests follow this pattern:

```
1. Build workload with debug info (in worktree-local paths, not /tmp)
2. Extract debug via upload.sh → <build-id>/.build-id/NN/REST.debug
3. objcopy --strip-all <workload>  (no debug-link, build-id only)
4. Wait ≤ 12s for debuginfod rescan; assert /buildid/<id>/debuginfo serves
5. Run perf-agent --kernel-stacks=off --profile --debuginfod-url=http://localhost:8002 \
                  --symbol-cache-dir=<tmp> --pid=<workload-pid> --duration=6s \
                  --profile-output=<out>.pb.gz
6. Parse pprof, assert specific user-side function names present
```

Test cases:

- **`TestStrippedRustOffBoxSymbolization`**: Rust workload (existing `test/workloads/rust/`). Strip-all, fetch via debuginfod. Assert `rust_workload::cpu_intensive_work` and `core::num::<impl u64>::wrapping_add` appear in pprof.

- **`TestStrippedGoOffBoxSymbolization`**: Go workload built with `go build -ldflags='-w -s'` then strip-all. Assert `main.cpuWork` (or equivalent) appears. Go's build-id format differs slightly (Go's own `.note.go.buildid` plus the optional GNU build-id from `-buildid=`); test confirms we read the GNU one correctly.

- **`TestStrippedCachedHitNoFetch`**: Run the Rust test twice. Verify second run's container access log shows no `GET /buildid/...` (or one cached 304-equivalent). Cache stats counter `cacheHits` increments.

- **`TestOffBoxLibcResolution`**: Profile a workload that calls into libc heavily. Assert libc functions (`malloc`, `__libc_start_main`, …) resolve through process-mode if `/usr/lib/debug/libc6-dbg` is installed on the host (skip test if not). Verifies we don't break the existing path.

- **`TestFileModeParseFailDemotes`**: Corrupt a cached `.debug` (truncate to 100 bytes). Run perf-agent. Assert the mapping demotes cleanly to process-mode and pprof still emits frames (just unsymbolized).

### Workload prep

New `test/integration_strip_helpers.go` (build-tag `integration`):

```go
// stripWorkload copies src to dst, runs `objcopy --strip-all dst`, returns build-id.
// Caller is responsible for cleanup. dst MUST be under the worktree
// (not /tmp — caps don't survive on nosuid mounts, see memory).
func stripWorkload(t *testing.T, src, dst string) (buildID string)

// uploadDebug runs the test/debuginfod/upload.sh helper. Returns the
// build-id and the path of the .debug file inside the store dir.
func uploadDebug(t *testing.T, srcWithDwarf string) (buildID, debugPath string)
```

## Migration / backward compatibility

- Existing v1.1.0 caches stay readable: layout unchanged.
- `--debuginfod-url`, `--symbol-cache-dir`, `--symbol-cache-max`, `--symbol-fetch-timeout`, `--symbol-fail-closed` flags — semantics unchanged.
- No CLI surface changes.
- No on-the-wire format changes (this is all internal to symbolization).
- Behavior change: stripped-no-debuglink binaries now resolve. v1.1.0 caches with files but never-used → after this PR, those files get used.

## Open follow-ups (out of scope here)

- Per-mapping debuginfod-fetch latency: today the first symbolize-batch for a new PID blocks on fetch (synchronous). Could move to "fetch ahead of time on attach" or "spawn background fetch + emit unresolved frames for the first batch".
- Object-store cache backend (Parca / Pyroscope use S3-compatible). Filesystem cache is fine for single-host; multi-replica K8s scenarios would benefit from shared storage.
- Lidia-style indexed DWARF (Pyroscope) — symbolize cost is currently `O(DWARF parse)` per first-use. Caching parsed DWARF (via blazesym's `blaze_symbolize_cache_elf`) would help; consider in M3.
- Kernel module off-box symbolization (separate concern, kernel-stacks M2).
