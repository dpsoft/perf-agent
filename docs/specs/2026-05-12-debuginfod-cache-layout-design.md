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
- **G2**: System libraries (libc, libstdc++, libpthread, ld-linux) continue to resolve through their existing distro debuginfo paths when available — we do not refetch what's already on disk. Two on-disk sources are honored: (a) `.gnu_debuglink` resolvable in standard search paths (blazesym's built-in DebugFileIter walks `/usr/lib/debug/<linkee>` and `/usr/lib/debug/.build-id/NN/REST.debug` from the debug-link entry point — distro-built libs have debug-link in the main package); (b) a `.debug` file present at `/usr/lib/debug/.build-id/NN/REST.debug` even when the binary lacks `.gnu_debuglink` (e.g., user-installed debuginfo for a stripped binary they built themselves). The classifier checks (b) explicitly before deciding to fetch.
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
├─ procmapClassifier.classify(pid)            ← re-snapshot /proc/<pid>/maps every call
│   │                                           (live mmap/dlopen/exec change mappings;
│   │                                            fresh snapshot is the correct invariant)
│   └─ for each mapping: inspect binary using map_files-derived path first,
│      fall back to symbolic path —
│       ├─ has local DWARF or resolvable .gnu_debuglink → ROUTE: process-mode
│       └─ build-id only, no local DWARF                → ROUTE: file-mode
│           └─ ensure cached .debug (fetch if miss); on fetch fail → process-mode
│
├─ split ips by mapping route
│
├─ Batch 1 — process-mode addresses:
│       blaze_symbolize_process_abs_addrs(csym, pid, ips_batch1)
│       (blazesym walks /proc/<pid>/maps + opens binaries. Dispatcher
│        provides Cases 1/2/4; Case 3 becomes a no-fetch fail-open
│        returning "" — see Dispatcher section.)
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

`procmapClassifier` runs **per-`Symbolize` call**, against a fresh `/proc/<pid>/maps` snapshot. It walks each executable mapping (`r-xp` or `r--p` with executable bit) and inspects the backing ELF.

**Path resolution (sidecar / mount-namespace safe)** — every ELF read goes through a two-step path resolution that mirrors the existing `symbolize/debuginfod/buildid.go::readBuildID` policy:

```
resolveELFPath(pid, mapping) → path:
  mapFilesPath := /proc/<pid>/map_files/<start>-<limit>
  if open(mapFilesPath) succeeds:        return mapFilesPath
  if open(mapping.SymbolicPath) succeeds: return mapping.SymbolicPath
  return ""   // unreachable from agent namespace
```

`/proc/<pid>/map_files/<va>-<va>` is a kernel-resolved symlink that points at the open file descriptor — it works even when the target's filesystem isn't reachable from the agent's mount namespace (sidecar case). The symbolic path is the fallback for older kernels or PIDs where map_files is restricted.

To make this available without threading `pid` everywhere, **`procmap.Mapping` gains a `MapFiles string` field** populated at parse time. Callers that need the dual-lookup ask for `mapping.OpenablePath()` (helper that returns the first openable of MapFiles / Path).

**Classification logic**:

```
classify(mapping) → route:
  if mapping.Path is empty or [vdso] or [stack] or [vsyscall] or [heap]:
      return skip
  path := mapping.OpenablePath()    // map_files → symbolic → ""
  if path == "":
      return skip  (unreachable from agent — can't even read PHDRs)

  if hasDwarf(path):                            return process-mode
  if hasResolvableDebuglink(path, []):          return process-mode
      // covers distro libs: blazesym walks /usr/lib/debug/.build-id/NN/REST.debug
      // via its built-in BuildId DebugFileIter state, reached because a debug-link
      // exists. Local distro debuginfo resolves with no fetch.

  buildID := readBuildID(mapping.MapFiles, mapping.Path)
  if buildID == "":
      return process-mode
      // no off-box option; .symtab may still resolve via blazesym defaults

  // 1. Distro debuginfo at standard path, even without debug-link in the binary
  systemPath := "/usr/lib/debug/.build-id/" + buildID[0:2] + "/" + buildID[2:] + ".debug"
  if exists(systemPath):
      return file-mode(path = systemPath)

  // 2. Our cache already has it
  if cache.Has(buildID, KindDebuginfo):
      return file-mode(path = cache.AbsPath(buildID, KindDebuginfo))

  // 3. Recent fetch failure — don't retry every batch
  if negFetch.IsActive(buildID):
      return process-mode

  // 4. Fetch via debuginfod
  abs, err := singleflightFetcher.fetchAndStore(ctx, "debuginfo", buildID)
  if err != nil:
      negFetch.Set(buildID, 5*Minute)
      return process-mode
  return file-mode(path = abs)
```

The local `/usr/lib/debug/.build-id/NN/REST.debug` check addresses **G2**: distro-installed split-debug (e.g., `glibc-debugsource`) that happens to lack `.gnu_debuglink` is reused without remote fetch. The path is the elfutils-standard hardcoded location.

Helpers reuse existing `symbolize/debuginfod/resolution.go::hasDwarf`, `hasResolvableDebuglink`, and `symbolize/debuginfod/buildid.go::readBuildID`. The check order matches blazesym's resolution preference.

**No persistent PID classification cache.** Mappings change across `Symbolize` calls (mmap, dlopen, exec, mprotect — `dwarfagent` already invalidates its session resolver on these events at `unwind/dwarfagent/common.go:249`, but that doesn't reach into the symbolizer). Re-classifying each batch from a fresh `/proc/<pid>/maps` is the simplest correct invariant: classification is cheap (one ELF open per new mapping; PHDRs only, a few KB read), and stale routes are impossible by construction.

Two **content-addressed** caches survive across calls and are immutable for the lifetime of the file at that path:

- `mappers map[mapperKey]*AddressMapper` — keyed by `mapperKey{dev: uint64, ino: uint64}` from `Stat(openablePath)`. `AddressMapper` depends only on ELF program headers, which don't change for a given inode. Keying by `(dev, ino)` (not path) means two PIDs mapping the same shared library via different namespaces share one mapper; also avoids collisions if a deleted-and-recreated file reuses the same path with different content.
- `negFetch map[string]time.Time` — keyed by build-id, 5-min TTL. Avoids re-trying failed fetches every batch.

Both maps are bounded LRU (size from existing `--symbol-cache-max` budget; entries cheap, default 4096). Mutex on the symbolizer protects both.

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

// NewAddressMapper reads PHDRs from the ELF at path. Caller is responsible
// for picking a path that's readable from the agent's namespace — typically
// mapping.OpenablePath() which prefers /proc/<pid>/map_files/...
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

Cache: one `AddressMapper` per unique `(path, st_ino)`, stashed in the symbolizer's `mappers` LRU. Content-addressed by inode, so two PIDs mapping the same shared library share the same mapper.

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

`dispatchWithBuildID` keeps four cases but **Case 3 changes semantics**:

| Case | Behavior |
|---|---|
| 1 (`cache.Has(KindExecutable)`) | Keep — sidecar executable cache hit |
| 2 (`localResolutionPossible`) | Keep — let blazesym defaults handle it |
| **3 (stripped + build-id, no local DWARF/debuglink)** | **Become a no-fetch fallback: return "" without fetching.** The classifier owns the fetch decision; dispatcher is only called by blazesym's process-mode path. If process-mode reaches a build-id-only mapping, the classifier either (a) didn't classify it yet — should be rare, transient, and blazesym just emits unresolved frame names; or (b) the mapping was demoted from file-mode after a fetch/parse failure — same outcome, fail-open. No panic. |
| 4 (binary not on disk → fetch executable, return abs) | Keep — sidecar fallback |

Why no panic: file-mode failures legitimately demote mappings to process-mode for the remainder of the session (see Failure modes below). Those mappings WILL re-enter the dispatcher on subsequent batches. A panic would crash the agent on any failed `.debug`. Returning "" is the correct fail-open behavior — blazesym emits `[binary]:offset` for those frames, identical to current v1.1.0 behavior for any unresolved mapping.

Stats reframed: `cacheMisses` for Case 3 is removed (no fetch here anymore). `fetchSuccessDebuginfo` moves into the classifier (file-mode owns the fetch). New stats added in Observability below.

### Cache layout

**Unchanged**: `<cacheDir>/.build-id/NN/REST.debug`. With file-mode, blazesym receives absolute paths directly; no `debug_dirs` wiring, no flat symlinks, no debug-link synthesis.

`index.db` SQLite (LRU eviction) — unchanged.

### Failure modes

| Failure | Fallback |
|---|---|
| Classification: can't read ELF | Process-mode (defensive — let blazesym try its defaults) |
| Classification: path is `[vdso]`, `[stack]`, `[vsyscall]`, empty | Skip (no symbolization possible) |
| Classification: no build-id | Process-mode (no off-box option, but `.symtab` may still resolve) |
| Classification: build-id present but stripped, no debug-link, fetch fails (404 / timeout / all URLs exhausted) | Demote mapping to process-mode for this `Symbolize` batch. Record negative-fetch entry (build-id, 5-min TTL) so subsequent batches skip the fetch and go straight to process-mode. Dispatcher Case 3 returns "" (no panic). One log line per (PID, build-id). |
| `AddressMapper` can't find PT_LOAD for an offset | Skip that address — frame name = `[binary]:offset` (current behavior) |
| Cached `.debug` is corrupt / `blaze_symbolize_elf_virt_offsets` returns NULL | Demote that build-id to process-mode for the remainder of the session (no per-PID scope — file content is identical for all consumers). **Don't** poison the cache file itself — a future blazesym version may parse it correctly. Set negative-fetch with a longer TTL (1 hour) to avoid re-parsing every batch. |
| `--symbol-fail-closed` is set | **No change in this PR.** The flag's semantics remain as-of v1.1.0 (still M2-pending per `perfagent/options.go:102`). Wiring it into the new classifier + file-mode demote logic is a separate concern; deferred to a follow-up PR so this work stays scoped. |

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

- **No persistent per-PID state.** Each `Symbolize(pid, ips)` re-snapshots `/proc/<pid>/maps` and re-classifies. This is the correctness story for live mmap/dlopen/exec changes.
- Two content-addressed LRU caches live on the `Symbolizer` and survive across calls:
  - `mappers` — keyed by `mapperKey{path: string, ino: uint64}`. Value: `*AddressMapper`. Immutable for the inode's lifetime.
  - `negFetch` — keyed by build-id. Value: `negFetchEntry{ until: time.Time }` with 5-min TTL for fetch failures, 1-hour for parse failures. Bounded by `--symbol-cache-max` budget; default 4096 entries.
- Both caches behind one `sync.RWMutex` on the symbolizer. Reads on the hot path; writes only when a new mapping is seen or a fetch/parse outcome changes.
- Fetch dedup remains in `singleflightFetcher` (called from the classifier, not the dispatcher).

## Component breakdown

| New / changed file | Lines (est.) | Responsibility |
|---|---|---|
| `unwind/procmap/addressmapper.go` (NEW) | ~80 | Ported AddressMapper + page-alignment + tests |
| `unwind/procmap/addressmapper_test.go` (NEW) | ~120 | Unit tests for normalization |
| `symbolize/debuginfod/classifier.go` (NEW) | ~150 | `procmapClassifier`, route decisions, content-addressed mapper + negFetch caches |
| `symbolize/debuginfod/classifier_test.go` (NEW) | ~200 | Table-driven classification tests |
| `symbolize/debuginfod/dispatcher.go` (MODIFY) | -20 / +80 | Reframe Case 3 as no-fetch fallback (return ""), add `symbolize_elf_virt` cgo wrapper, add `symbolizeElfVirt` Go wrapper |
| `unwind/procmap/procmap.go` (MODIFY) | +15 | Add `Mapping.MapFiles string` + `Mapping.OpenablePath()` helper |
| `unwind/procmap/procmap_test.go` (MODIFY) | +30 | Tests for OpenablePath: map_files preferred, falls back to symbolic, returns "" when neither works |
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
- Binary stripped, build-id present, no debug-link, local `/usr/lib/debug/.build-id/.../...debug` exists (mocked) → file-mode with local path (no fetch)
- Binary stripped, build-id present, no debug-link, only cache hit → file-mode with cached path (no fetch)
- Binary stripped, build-id present, no debug-link, cache miss + fetch succeeds → file-mode with fetched path
- Binary stripped, build-id present, no debug-link, cache miss + fetch fails → process-mode (no panic), `negFetch` populated
- Binary stripped, no build-id → process-mode (defensive — `.symtab` may resolve)
- Path is `[vdso]` / `[stack]` / `[vsyscall]` / empty → skipped
- Read error on ELF → process-mode (defensive fallback)
- `AddressMapper` cache hit on second call with same `(path, ino)`

`symbolize/debuginfod/dispatcher_test.go`:
- Existing tests updated for the new Case 3 semantics (no-fetch fallback, return "")
- New test: Case 3 entry with a `negFetch` build-id returns "" without contacting `singleflightFetcher`
- New test: Case 3 entry with a fresh build-id (not in `negFetch`) returns "" — fetch ownership moved to classifier

### Integration tests (caps, hermetic debuginfod)

Reuse the existing `test/debuginfod/docker-compose.yml` + `upload.sh` harness. All new tests follow this pattern:

```
1. Build workload WITH DWARF (no -w/-s for Go; default release for Rust). Worktree-local
   paths, not /tmp (caps don't survive on nosuid mounts).
2. Extract debug via upload.sh → debuginfo-store/.build-id/NN/REST.debug
3. objcopy --strip-all <workload>   (no debug-link, build-id only)
4. Wait ≤ 12s for debuginfod rescan; assert curl /buildid/<id>/debuginfo serves
5. Run perf-agent --profile --debuginfod-url=http://localhost:8002 \
                  --symbol-cache-dir=<worktree-tmp> --pid=<workload-pid> --duration=6s \
                  --profile-output=<out>.pb.gz
   (--kernel-stacks omitted — defaults to false; tests don't exercise kernel symbols)
6. Parse pprof, assert specific user-side function names present
```

Test cases:

- **`TestStrippedRustOffBoxSymbolization`**: Rust workload (existing `test/workloads/rust/`). Build keeps `debug = true, strip = false`. Strip-all, fetch via debuginfod. Assert `rust_workload::cpu_intensive_work` and `core::num::<impl u64>::wrapping_add` appear in pprof.

- **`TestStrippedGoOffBoxSymbolization`**: Go workload built with plain `go build` (no `-ldflags` — Go emits DWARF by default; **do not pass `-w`**, which omits DWARF and would make the uploaded `.debug` useless). Then `objcopy --strip-all` removes DWARF + symtab from the binary, leaving build-id. Assert `main.cpuWork` (or equivalent) appears in pprof. Go embeds two build-id notes (`.note.go.buildid` and `.note.gnu.build-id`); the GNU one is what debuginfod indexes — test confirms we read it correctly.

- **`TestStrippedCachedHitNoFetch`**: Run the Rust test twice. Verify second run's container access log shows no `GET /buildid/...` (or one cached 304-equivalent). Cache stats counter `cacheHits` increments.

- **`TestOffBoxLibcResolution`**: Profile a workload that calls into libc heavily. Assert libc functions (`malloc`, `__libc_start_main`, …) resolve through process-mode if `/usr/lib/debug/libc6-dbg` is installed on the host (skip test if not). Verifies we don't break the existing path.

- **`TestFileModeParseFailDemotes`**: Corrupt a cached `.debug` (truncate to 100 bytes). Run perf-agent. Assert the mapping demotes cleanly to process-mode and pprof still emits frames (just unsymbolized).

- **`TestStrippedSidecarUnreachableSymbolicPath`**: Simulate the sidecar / mount-namespace case. Build a workload, get its symbolic path, then **delete the binary from its symbolic location while it's still running** (the process keeps it alive via the open file descriptor). The agent now sees the symbolic path as missing but `/proc/<pid>/map_files/...` still works. Assert symbols still resolve. (This is the simplest portable repro of the sidecar shareProcessNamespace + separate-mount-ns case.)

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

- **Wire `--symbol-fail-closed` end-to-end**: today the flag is parsed but inert (M2-pending). Extend it through classifier + file-mode demotion so users running profiling pipelines that must not silently lose frames can opt into hard failures. Separate PR.
- Per-mapping debuginfod-fetch latency: today the first symbolize-batch for a new PID blocks on fetch (synchronous). Could move to "fetch ahead of time on attach" or "spawn background fetch + emit unresolved frames for the first batch".
- Object-store cache backend (Parca / Pyroscope use S3-compatible). Filesystem cache is fine for single-host; multi-replica K8s scenarios would benefit from shared storage.
- Lidia-style indexed DWARF (Pyroscope) — symbolize cost is currently `O(DWARF parse)` per first-use. Caching parsed DWARF (via blazesym's `blaze_symbolize_cache_elf`) would help; consider in M3.
- Kernel module off-box symbolization (separate concern, kernel-stacks M2).
