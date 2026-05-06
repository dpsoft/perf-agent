# Debuginfod-backed symbolization for perf-agent

**Author:** D. Parra
**Date:** 2026-05-06
**Status:** Draft (post-brainstorm)
**Supersedes:** SPEC-blazedebuginfod.md (PoC-era draft, 2026-05-05)

## Summary

Add a new in-tree package, `symbolize/debuginfod`, that wraps blazesym with a
debuginfod-protocol fetcher so perf-agent can resolve native symbols against a
remote symbol server when local debug info is unavailable.

Today perf-agent calls `blazesym.Symbolizer.SymbolizeProcessAbsAddrs(...)` from
three places (`profile/profiler.go`, `offcpu/profiler.go`,
`unwind/dwarfagent/common.go`), each constructing its own
`*blazesym.Symbolizer`. We replace those concrete handles with a
`symbolize.Symbolizer` interface owned by `perfagent.Agent`. Two
implementations are shipped:

- `LocalSymbolizer` — preserves today's behavior bit-for-bit. Selected when no
  debuginfod URL is configured.
- `DebuginfodSymbolizer` (in `symbolize/debuginfod/`) — wires blazesym's
  per-mapping `process_dispatch` callback to a Go-side debuginfod client with
  a build-id-indexed on-disk cache.

The dispatcher uses a hybrid routing strategy: it prefers `/debuginfo` (small
DWARF-only blobs placed in `debug_dirs` for blazesym to find on its own), and
only falls back to `/executable` when the agent cannot read the binary on
disk (e.g., sidecar without `shareProcessNamespace`). HTTP traffic is
suppressed entirely whenever local resolution would succeed.

## Motivation

perf-agent today symbolizes native code by reading binaries directly from
`/proc/<pid>/root/...`. Two production scenarios make this insufficient:

- **Stripped production binaries.** Deployed images strip debug info to reduce
  size; the corresponding `.debug` lives on a CI/build artifact server, not on
  the node. Without it, function names and source-line attribution are lost.
- **Sidecar without `shareProcessNamespace`.** The agent can read its own
  filesystem but not the peer container's. Even non-stripped binaries are
  unreachable.

A debuginfod-protocol symbol server addresses both by serving artifacts keyed
by GNU build-id over HTTP. blazesym now exposes
`blaze_symbolizer_opts.process_dispatch` (capi commit
[`1f2d983`][bz-dispatch] / `8891e70`) — a per-mapping callback that lets the
caller supply an alternative ELF path. This spec wires that hook into a
perf-agent-resident HTTP fetcher and cache.

The deployment story we're optimizing for is **DaemonSet on host PID
namespace with stripped binaries**. The sidecar story is supported (`/executable`
fallback), but not the primary motivation: perf-agent already requires
`/proc/<pid>/maps` for unwinding, which the sidecar-without-`shareProcessNamespace`
case also blocks.

[bz-dispatch]: https://github.com/libbpf/blazesym/commit/1f2d983

## Non-goals

- Replacing blazesym. We wrap it.
- Replacing `unwind/procmap.Resolver`. It stays the source of truth for
  `/proc/<pid>/maps` parsing and build-id caching; we read from its cache when
  available.
- JIT/perf-map symbolization. Continues to work via `pprof.decodePerfMapFrame`
  unchanged. Anonymous mappings never reach the dispatcher.
- Reimplementing DWARF parsing in Go. blazesym handles it.
- Push-to-backend (Parca-style) normalized stack emission. We design the cache
  key around build-id so this is implementable later, but it is not v1.
- Cross-architecture symbolize, source-file fetching, content negotiation
  beyond the three standard endpoints.

## Constraints from perf-agent

- **Go 1.26+** (matches `go.mod`).
- **blazesym pin**: minimum upstream commit `1f2d983 capi: Add callback for
  symbolizer` (or its review fixup `8891e70`). At the time of writing, no
  released `blazesym-c` tag contains the dispatcher (`0.1.7` is the latest
  release). Re-pin to the tag once `0.1.8` ships. The local checkout at
  `/home/diego/github/blazesym/` must be advanced past `1f2d983`; the
  Makefile's `blazesym` target should error if the header lacks
  `process_dispatch`.
- **Single shared symbolizer per Agent.** The three call sites take a
  `symbolize.Symbolizer` interface; the Agent constructs one impl at startup
  and threads it down. No per-profiler caches or dispatcher contexts.
- **Today's behavior preserved when feature is off.** Users who don't set
  `--debuginfod-url` see no change. The new package is reachable only via
  explicit opt-in.

## Architecture

```
                                                      ┌───────────────────────────┐
                                                      │   debuginfod server(s)    │
                                                      │   (DEBUGINFOD_URLS list)  │
                                                      └─────────────┬─────────────┘
                                                                    │ HTTP
                                                                    │ /buildid/<id>/debuginfo
                                                                    │ /buildid/<id>/executable
                                                                    │
  ┌───────────────────────────────────────────────────────────────┐ │
  │  perf-agent (single process)                                  │ │
  │                                                               │ │
  │   perfagent.Agent                                             │ │
  │     ├── procmap.Resolver         (mappings, build-id cache)   │ │
  │     ├── symbolize.Symbolizer ◄───┐  (interface)               │ │
  │     │                            │                            │ │
  │     ├── profile.Profiler ────────┘                            │ │
  │     ├── offcpu.Profiler  ────────┐                            │ │
  │     └── dwarfagent.session ──────┘                            │ │
  │                                                               │ │
  │   symbolize/debuginfod/  (new package)                        │ │
  │     ├── DebuginfodSymbolizer  (implements Symbolizer)         │ │
  │     │     ├── *blazesym handle (cgo)                          │ │
  │     │     ├── dispatcher (cgo callback → Go)                  │ │
  │     │     ├── debug_dirs = [cacheDir]                         │ │
  │     │     └── fetcher (HTTP + cache + singleflight) ──────────┼─┘
  │     ├── LocalSymbolizer       (today's behavior; no HTTP)     │
  │     └── Cache (.build-id/<NN>/<rest>{.debug,} on disk)        │
  └───────────────────────────────────────────────────────────────┘
```

`procmap.Resolver`, `pprof.ProfileBuilder`, the BPF programs, and the
`internal/perfdata.Writer` are unchanged.

## Public API

### `symbolize/` (new) — interface, value types, and `LocalSymbolizer`

```go
package symbolize

// Symbolizer resolves abs addresses in a process's address space to
// symbolic frames. Implementations are safe for concurrent use.
type Symbolizer interface {
    SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error)
    Close() error
}

// Frame is a single symbolized stack frame. Name is always populated when
// resolution succeeded; other fields are filled when the underlying resolver
// has them. Inlined holds the inline-expansion chain caller-most-to-callee
// when the resolver supports it.
type Frame struct {
    Address uint64
    Name    string
    Module  string
    BuildID string
    File    string
    Line    int
    Column  int
    Offset  uint64
    Inlined []Frame
    Reason  FailureReason // when Name == ""
}

type FailureReason uint8

const (
    FailureNone FailureReason = iota
    FailureUnmapped
    FailureInvalidFileOffset
    FailureMissingComponent
    FailureMissingSymbols
    FailureUnknownAddress
    FailureFetchError
    FailureNoBuildID
)

// LocalSymbolizer wraps blazesym's Process source with no debuginfod hooks —
// preserves today's perf-agent behavior. Used when no debuginfod URL is
// configured.
type LocalSymbolizer struct{ /* unexported: *blazesym.Symbolizer */ }

func NewLocalSymbolizer() (*LocalSymbolizer, error)
func (s *LocalSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]Frame, error)
func (s *LocalSymbolizer) Close() error

// ToProfFrames adapts []Frame to []pprof.Frame for callers that still
// produce pprof builders directly (currently dwarfagent).
func ToProfFrames([]Frame) []pprof.Frame
```

Module path: `github.com/dpsoft/perf-agent/symbolize`. `LocalSymbolizer` lives
in this package (no `symbolize/local/` subpackage).

### `symbolize/debuginfod/` (new) — `DebuginfodSymbolizer`

```go
package debuginfod

// Symbolizer is a symbolize.Symbolizer that fetches debug info from a
// debuginfod-protocol server when local resolution would fail.
//
// Per-mapping decision (see §"Dispatcher decision tree" for full table):
//   - cached executable present                → return cache path
//   - local DWARF or debug_dirs covers it      → NULL (default path; no HTTP)
//   - binary on disk, no DWARF                 → fetch /debuginfo, NULL
//   - binary not on disk                       → fetch /executable, return path
type Symbolizer struct{ /* unexported */ }

func New(opts Options) (*Symbolizer, error)
func (s *Symbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error)
func (s *Symbolizer) Stats() Stats
func (s *Symbolizer) Close() error

type Options struct {
    // URLs is the ordered list of debuginfod servers. First 200 wins;
    // 404 falls through to the next URL.
    URLs []string

    // CacheDir is where fetched artifacts are stored under a
    // .build-id/<NN>/<rest>{.debug,} layout. Default: /tmp/perf-agent-debuginfod.
    CacheDir string

    // CacheMaxBytes caps total on-disk cache size. LRU by access time.
    // Default: 2 GiB (sized for the /tmp/tmpfs default).
    CacheMaxBytes int64

    // FetchTimeout per artifact. Default: 30s.
    FetchTimeout time.Duration

    // FailClosed: when true, frames whose mapping had a failed debuginfod
    // fetch are forced to FailureFetchError regardless of what blazesym
    // resolved them to from a stale local file. Default: false.
    FailClosed bool

    // Resolver lets the symbolizer reuse procmap.Resolver's build-id cache.
    // Optional; if nil the dispatcher reads build-ids directly.
    Resolver *procmap.Resolver

    // HTTPClient overrides the default. Use this for mTLS, auth headers, or
    // a transport sized for the cluster.
    HTTPClient *http.Client

    // Logger receives operational events. Default: discard.
    Logger *slog.Logger

    // Symbolizer behavior knobs. Defaults: all true.
    Demangle, InlinedFns, CodeInfo bool
}

type Stats struct {
    CacheHits, CacheMisses, CacheEvictions uint64
    FetchSuccessDebuginfo, FetchSuccessExecutable uint64
    Fetch404s, FetchErrors uint64
    FetchBytesTotal uint64
    FetchLatencyP50, FetchLatencyP99 time.Duration
    InFlightFetches int64
    DispatcherCalls, DispatcherSkippedLocal uint64
    DispatcherPanics uint64
}
```

### Constructor at the Agent layer

```go
func chooseSymbolizer(cfg Config, res *procmap.Resolver, logger *slog.Logger) (symbolize.Symbolizer, error) {
    urls := cfg.DebuginfodURLs
    if len(urls) == 0 {
        for u := range strings.FieldsSeq(os.Getenv("DEBUGINFOD_URLS")) {
            urls = append(urls, u)
        }
    }
    if len(urls) == 0 {
        return symbolize.NewLocalSymbolizer()
    }
    return debuginfod.New(debuginfod.Options{
        URLs:          urls,
        CacheDir:      cmp.Or(cfg.SymbolCacheDir, "/tmp/perf-agent-debuginfod"),
        CacheMaxBytes: cmp.Or(cfg.SymbolCacheMax, int64(2<<30)),
        FetchTimeout:  cmp.Or(cfg.SymbolFetchTimeout, 30*time.Second),
        FailClosed:    cfg.SymbolFailClosed,
        Resolver:      res,
        Logger:        logger,
        Demangle:      true,
        InlinedFns:    true,
        CodeInfo:      true,
    })
}
```

## Internals

### Dispatcher decision tree

The cgo callback runs once per file-backed mapping during
`SymbolizeProcessAbsAddrs`. It returns either a `char*` path (override) or
`NULL` (use blazesym's default).

```go
//export goDispatchCb
func goDispatchCb(mapsFile, symbolicPath *C.char, ctx unsafe.Pointer) (ret *C.char) {
    h := cgo.Handle(uintptr(ctx))
    s := h.Value().(*Symbolizer)

    s.inflight.Add(1)
    defer s.inflight.Done()
    defer func() {
        if r := recover(); r != nil {
            s.stats.dispatcherPanics.Add(1)
            ret = nil
        }
    }()

    return s.dispatch(C.GoString(mapsFile), C.GoString(symbolicPath))
}

func (s *Symbolizer) dispatch(mapsFile, symbolicPath string) *C.char {
    s.stats.dispatcherCalls.Add(1)

    buildID, ok := s.readBuildID(mapsFile, symbolicPath)
    if !ok {
        return nil
    }

    // Case 1: cached executable from prior fetch
    if p, ok := s.cache.executablePath(buildID); ok {
        return C.CString(p)
    }

    // Case 2: blazesym's default path will work
    if s.localResolutionPossible(symbolicPath, buildID) {
        s.stats.dispatcherSkippedLocal.Add(1)
        return nil
    }

    // Case 3: binary on disk, no DWARF locally → fetch /debuginfo into debug_dirs
    if s.binaryReadable(symbolicPath) {
        if err := s.cache.fetchDebuginfo(s.fetchCtx(), buildID); err != nil {
            s.recordFetchFailure(buildID)
        }
        return nil
    }

    // Case 4: binary not on disk → fetch /executable, return that path
    p, err := s.cache.fetchExecutable(s.fetchCtx(), buildID)
    if err != nil {
        s.recordFetchFailure(buildID)
        return nil
    }
    return C.CString(p)
}
```

`readBuildID` tries `mapsFile` first (kernel-resolved
`/proc/<pid>/map_files/...` symlink, present even when the symbolic path
isn't), falls back to `symbolicPath`. If `Options.Resolver` is set, consults
its build-id cache before parsing the ELF.

`localResolutionPossible` is the cheap pre-check that keeps the common case
HTTP-free:

```go
func (s *Symbolizer) localResolutionPossible(path, buildID string) bool {
    if s.cache.hasDebuginfo(buildID)        { return true }
    if hasDwarf(path)                       { return true }
    if hasResolvableDebuglink(path)         { return true }
    return false
}
```

All three checks are O(open + section-header read). Results memoized per
`(inode, mtime)` so re-symbolizing the same mapping is a hashmap lookup.

### cgo bridge

```c
// cgo preamble
extern char* goDispatchCb(char* maps_file, char* symbolic_path, void* ctx);
typedef char* (*blaze_dispatch_fn)(const char*, const char*, void*);

static void install_dispatch(blaze_symbolizer_opts* opts,
                             blaze_symbolizer_dispatch* slot,
                             void* ctx) {
    slot->dispatch_cb = (blaze_dispatch_fn)goDispatchCb;
    slot->ctx = ctx;
    opts->process_dispatch = slot;
}
```

Go side:

```go
type Symbolizer struct {
    csym     *C.blaze_symbolizer
    dispatch C.blaze_symbolizer_dispatch
    handle   cgo.Handle
    cache    *cache
    fetcher  *fetcher
    stats    atomicStats
    closing  atomic.Bool
    inflight sync.WaitGroup
    failed   sync.Map // buildID → struct{}{} (per-call, cleared between calls)
    opts     Options
}

func New(opts Options) (*Symbolizer, error) {
    s := &Symbolizer{opts: opts /* … */}
    s.handle = cgo.NewHandle(s)
    var copts C.blaze_symbolizer_opts
    // fill type_size, code_info, inlined_fns, demangle, debug_dirs=[cacheDir]
    C.install_dispatch(&copts, &s.dispatch, unsafe.Pointer(uintptr(s.handle)))
    s.csym = C.blaze_symbolizer_new_opts(&copts)
    if s.csym == nil {
        s.handle.Delete()
        return nil, errSymbolizer
    }
    return s, nil
}
```

**Memory contract.** The C ABI says blazesym `free()`s the returned `char*`
via libc. `C.CString` allocates via libc `malloc`, which matches. **Never**
return a pointer into a Go-managed slice — blazesym will free it.

**Multi-instance.** `cgo.Handle` maps a C `void*` back to a Go object safely.
The PoC's `ctx = NULL` + static dispatch struct breaks the moment two
`Symbolizer`s exist (e.g., in tests); we pay one `cgo.Handle` per Symbolizer
to fix that. In production there's one per Agent — trivial cost.

**Panic safety.** `recover()` at the top of `goDispatchCb` is non-negotiable:
a Go panic crossing a cgo boundary is undefined behavior. Recover, record,
return `NULL`.

### Fetch path

```go
type fetcher struct {
    client *http.Client
    urls   []string
    sf     singleflight.Group
}

// kind ∈ {"debuginfo", "executable"}
func (f *fetcher) fetch(ctx context.Context, kind, buildID string) (string, error) {
    key := kind + ":" + buildID
    res, err, _ := f.sf.Do(key, func() (any, error) {
        return f.fetchOnce(ctx, kind, buildID)
    })
    if err != nil {
        return "", err
    }
    return res.(string), nil
}

func (f *fetcher) fetchOnce(ctx context.Context, kind, buildID string) (string, error) {
    var lastErr error
    for _, base := range f.urls {
        url := base + "/buildid/" + buildID + "/" + kind
        req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
        if err != nil {
            return "", err
        }
        resp, err := f.client.Do(req)
        if err != nil {
            lastErr = err
            continue
        }
        switch resp.StatusCode {
        case http.StatusOK:
            defer resp.Body.Close()
            return f.cache.writeAtomic(kind, buildID, resp.Body)
        case http.StatusNotFound:
            resp.Body.Close()
            continue
        default:
            resp.Body.Close()
            lastErr = &httpError{status: resp.StatusCode, url: url}
        }
    }
    if lastErr == nil {
        return "", errNotFound
    }
    return "", lastErr
}
```

**No automatic retries** in v1 (M1/M2). Per-URL fallback handles "try N
servers"; transient 5xx becomes the next collection cycle's problem.
Exponential backoff is M3.

**Context.** Each fetch derives a context with `Options.FetchTimeout`.
Cancelling the parent (Agent shutdown) cancels in-flight fetches.

**Singleflight collapse.** Concurrent dispatchers asking for the same
build-id collapse to one HTTP call.

### Cache layout, atomic writes, LRU eviction

```
<cacheDir>/
   .build-id/
      aa/
         bbccddeeff...debug      ← from /debuginfo (works as debug_dirs source)
         bbccddeeff...           ← from /executable (returned to dispatcher directly)
   index.db                       ← SQLite (or index.json with -tags noindex_sqlite)
```

The `.build-id/<NN>/<rest>` layout is what gdb, eu-readelf, and blazesym's
`debug_dirs` walker recognize natively. The same file on disk serves both
Case 3 (blazesym finds it via `debug_dirs`) and Case 1 (the dispatcher hands
the path back).

**Atomic writes.** Tmp file in the same directory + `os.Rename`. Singleflight
already prevents two concurrent fetches for the same build-id; cross-build-id
fetches never collide.

**Eviction.** A small `index` interface tracks
`(build_id, kind, size, last_access_ns)`. A goroutine wakes on cache write,
sums sizes, and evicts LRU entries until total ≤ `CacheMaxBytes`.

**Index backend selection.** Build-tag opt-out:

```
go build                     → SQLite via modernc.org/sqlite (default)
go build -tags noindex_sqlite → JSON-only (no SQLite linked in)
```

```go
// symbolize/debuginfod/cache/index.go (always compiled)
type index interface {
    Touch(buildID, kind string, size int64)
    Evict(maxBytes int64) []entry
    Iter(yield func(entry) bool)
    Close() error
}

// symbolize/debuginfod/cache/index_sqlite.go  //go:build !noindex_sqlite
// symbolize/debuginfod/cache/index_json.go    //go:build noindex_sqlite
```

Both impls covered by the cache test suite via build-tag matrix in CI. JSON
impl is a single `index.json` rewritten on cache write, mutex-guarded; fine
at the 2 GiB cap (low-thousands of entries).

**Pre-warm on startup.** `New(...)` scans the cache directory and
re-populates the index from disk (recovers from index loss; lets a fresh
process inherit a populated cache).

### Lifecycle

```go
func (s *Symbolizer) Close() error {
    if !s.closing.CompareAndSwap(false, true) {
        return errors.New("already closed")
    }
    s.inflight.Wait()                       // drain dispatcher invocations
    C.blaze_symbolizer_free(s.csym)         // safe: no more callbacks possible
    s.handle.Delete()                       // safe: blazesym is gone
    s.fetcher.client.CloseIdleConnections()
    return s.cache.Close()
}
```

Order matters: free blazesym **before** the cgo.Handle, and only after
in-flight callbacks have returned. `SymbolizeProcess` checks `s.closing` and
returns `errClosed` immediately as a defense against caller races; the
contract is "close after all profilers are stopped."

**FailClosed semantics.** When `Options.FailClosed=true`:

- The dispatcher still returns `NULL` on fetch failure (the C ABI has no
  "fail this mapping" signal). Frames may come back resolved from a stale
  local file.
- `SymbolizeProcess` tracks per-call failed build-ids in `s.failed`. After
  blazesym returns, frames whose mapping's build-id is in the set are
  forced to `Reason=FailureFetchError, Name=""`.
- The set is cleared at the start of each `SymbolizeProcess` call.

This is the only place fail-closed deviates from "let blazesym figure it
out." If the implementation reveals it's gnarlier than the sketch suggests,
we drop the option and reintroduce in M2.

## Integration with perf-agent

### Agent wiring

`perfagent.Agent` constructs the symbolizer and passes the interface to each
profiler:

```go
func (a *Agent) Start(ctx context.Context) error {
    a.resolver = procmap.NewResolver()

    sym, err := chooseSymbolizer(a.cfg, a.resolver, a.logger)
    if err != nil {
        return fmt.Errorf("symbolizer: %w", err)
    }
    a.symbolizer = sym

    if a.cfg.ProfileEnabled {
        a.cpu, err = profile.New(profile.Options{
            // existing options, plus:
            Symbolizer: a.symbolizer,
        })
        if err != nil {
            return err
        }
    }
    // offcpu, dwarfagent — same pattern
    return nil
}

func (a *Agent) Close() error {
    return errors.Join(
        closeIfNotNil(a.cpu),
        closeIfNotNil(a.offcpu),
        closeIfNotNil(a.dwarf),
        closeIfNotNil(a.symbolizer),
        closeIfNotNil(a.resolver),
    )
}
```

### Per-call-site changes

All three sites get the same diff: `*blazesym.Symbolizer` →
`symbolize.Symbolizer`.

**`profile/profiler.go`** — replace the `*blazesym.Symbolizer` field;
`SymbolizeProcessAbsAddrs(ips, pid, opts...)` becomes
`SymbolizeProcess(pid, ips)` returning `[]symbolize.Frame`. The
`blazeSymToFrames` translator is gone — its job moves into
`LocalSymbolizer.SymbolizeProcess` so the Sym→Frame translation lives in one
place.

**`offcpu/profiler.go`** — same diff, same conversion.

**`unwind/dwarfagent/`** — `common.go:186`'s `blazesym.NewSymbolizer(...)` is
removed; the session takes the shared interface from the Agent.
`symbolizePID` becomes:

```go
func symbolizePID(sym symbolize.Symbolizer, pid uint32, ips []uint64) []pprof.Frame {
    frames, err := sym.SymbolizeProcess(pid, ips)
    if err != nil {
        log.Printf("symbolize: %v", err)
        return nil
    }
    return symbolize.ToProfFrames(frames)
}
```

### CLI flags

```
--debuginfod-url=URL          repeatable; ordered fallback list. Falls back to
                               DEBUGINFOD_URLS env (space-separated) if unset.
                               If both empty, debuginfod is disabled and
                               LocalSymbolizer is used (today's behavior).
--symbol-cache-dir=DIR         default: /tmp/perf-agent-debuginfod
--symbol-cache-max=BYTES       default: 2 GiB
--symbol-fetch-timeout=DUR     default: 30s
--symbol-fail-closed           default: false
```

Repeatable flag follows the existing `--tag` pattern in `main.go` (`flag.Var` with a custom slice type).

## Error handling

Three boundaries, three policies:

- **`debuginfod.New(...)` fails at agent startup** (bad URL, unwritable cache
  dir, blazesym constructor returns NULL): Agent fails fast with an explicit
  error. **Does not** silently downgrade to `LocalSymbolizer` — if the user
  asked for debuginfod, they get debuginfod or an error.
- **A specific fetch fails during symbolize** (404, 5xx, timeout): governed
  by `FailClosed`. Default (false): dispatcher returns `NULL`, blazesym
  attempts its default for that mapping, frames may end up `Name=""` if local
  resolution also fails. Stats record the outcome.
- **The dispatcher itself panics** (programmer error, unexpected nil deref):
  `defer recover()` at the top of `goDispatchCb`, increment
  `dispatcher_panics_total`, return `NULL`. Letting the panic propagate
  through cgo is undefined behavior.

## Metrics

Wired through the existing `metrics.Exporter` interface. `Stats() Stats` on
the symbolizer; the exporter polls on its existing tick.

```
perfagent_symbolize_dispatcher_calls_total
perfagent_symbolize_dispatcher_skipped_local_total
perfagent_symbolize_dispatcher_panics_total
perfagent_symbolize_cache_hits_total
perfagent_symbolize_cache_misses_total
perfagent_symbolize_cache_evictions_total
perfagent_symbolize_cache_size_bytes
perfagent_symbolize_fetch_total{kind="debuginfo|executable",outcome="success|404|error"}
perfagent_symbolize_fetch_bytes_total{kind="debuginfo|executable"}
perfagent_symbolize_fetch_duration_seconds  // histogram
perfagent_symbolize_inflight_fetches        // gauge
```

## Testing strategy

### Unit (no network, no root)

- `readBuildID` fixture set: PIE, non-PIE, stripped, `--build-id=none`,
  ARM64.
- `localResolutionPossible` against fixtures (with/without DWARF, with/without
  resolvable debug-link).
- `fetcher.fetch` against `httptest.Server`: 200 / 404 fall-through / 500 →
  next URL / context-cancellation / timeout.
- Singleflight: 100 goroutines, same build-id → assert one HTTP call.
- LRU eviction: fill past `CacheMaxBytes`, assert oldest evicted.
- Index backends: cache test suite runs under both build tags in CI.
- Use `t.Context()` everywhere (Go 1.24+); `b.Loop()` for any benchmarks.

### Integration (cgo, network, no root)

Reuses the `docker-compose.yml` from the PoC archive. Cases:

1. Spawn a fixture process, `SymbolizeProcess(pid, addrs)`, assert function
   names match `nm` output.
2. Cache hit: run twice, assert second has zero HTTP requests (test
   transport counter).
3. FailClosed: server returns 404, assert frames carry
   `FailureFetchError`.
4. Sidecar simulation: rename the binary so `symbolic_path` doesn't open
   → assert dispatcher returns `/executable` cache path.

### Concurrency / race

- 10 goroutines × 100 addrs each, `go test -race`. Assert no panics, no
  cgo.Handle / fd leaks.
- Multi-instance: open 2 `Symbolizer`s in the same test process, symbolize
  concurrently, assert callbacks route to the right instance (test the PoC's
  static-singleton failure mode).

### End-to-end in perf-agent's existing suite

`test/integration_test.go` adds one variant: today's profiling fixture, but
the binary is stripped and a debuginfod docker container runs alongside.
Assert resolved frames match the unstripped baseline.

## Build & dependencies

- **blazesym pin:** documented minimum commit in `BUILDING.md`. Makefile's
  `blazesym` target errors when the local checkout's
  `capi/include/blazesym.h` lacks `process_dispatch`.
- **Go modules added:**
  - `golang.org/x/sync/singleflight`
  - `modernc.org/sqlite` (default builds only; pure Go, no cgo)
- **Build tags:**
  - `noindex_sqlite` — drops SQLite, falls back to JSON index. Used in
    restrictive-environment builds.
- **No new system packages.**

## Phasing

### M1 — minimum viable (5-7 days)

- `symbolize.Symbolizer` interface + `LocalSymbolizer`.
- Migrate the three call sites to the interface (no functional change).
- `symbolize/debuginfod`: cgo dispatcher, `readBuildID`, fetcher with
  singleflight, four-case dispatcher logic.
- Cache with SQLite index (default) + JSON index (build-tag opt-out), LRU
  eviction.
- CLI flags + env-var fallback.
- Unit tests + the docker-compose integration test from the PoC.

### M2 — production hardening (5-7 days)

- `FailClosed` per-call failed-build-id tracking + tests.
- Multi-URL ordered fallback tests.
- Metrics exporter wiring.
- Race tests, FD-leak tests, multi-instance test.
- BUILDING.md update (blazesym pin + build-tag matrix).
- End-to-end stripped-binary test in `test/integration_test.go`.

### M3 — operational polish (3-5 days)

- Background prefetch (skip first cache miss latency on agent restart).
- Optional content verification (reject blobs whose actual build-id
  doesn't match the request).
- Conditional GET (`If-None-Match`) for shared-server dedup.
- Exponential backoff on 5xx.

## Future directions (tracked, not v1)

- **Option B — Parca / otel-ebpf-profiler-style PID resolution and normalized
  stack emission.** Drive normalization in perf-agent (walk
  `/proc/<pid>/maps`, convert abs IPs → `(build-id, file_offset)` per
  mapping, emit those tuples instead of resolved frames). This unlocks
  push-to-backend pipelines, fleet-wide symbol caches, symbolize-on-read
  storage, robustness to short-lived process exit, and cross-architecture
  symbolize. The dispatcher path (this spec) keeps `Source::Process` and
  symbolizes locally — fine for perf-agent's current "produce a symbolized
  pprof at the end of a session" contract, but blocks those richer
  topologies. The cache key (build-id) and the in-tree package boundary are
  designed so a `NormalizeOnly` mode can be added on top without rewriting
  the per-call-site integration. Reconsider when push-to-backend, fleet-scale
  symbol storage, or short-lived-process workloads become a target.

## Risks

- **cgo overhead per dispatcher invocation.** One Go↔C transition per
  mapping (~10-50 per process), not per frame. Singleflight bounds the HTTP
  cost. Verified in the PoC.
- **`cgo.Handle` lifetime.** If a caller closes the Symbolizer mid-symbolize,
  the handle deref panics. Mitigation: `inflight.Wait()` in `Close()` and
  ordering free-blazesym before handle-delete.
- **Cache write contention.** Many concurrent same-build-id fetches collide
  on rename(2). Mitigation: singleflight + per-fetch tmp file + atomic
  rename.
- **Server-side DOS surface.** New-image rollouts spike requests at the
  symbol server. Mitigation: deferred to M3 (per-pod throttle in `Options`,
  exponential backoff on 5xx).
- **`FailClosed` correctness.** The post-symbolize override pass is fiddly.
  Mitigation: drop the option in M1 if implementation review reveals it's
  worse than the sketch suggests; reintroduce in M2.

## Success criteria

- perf-agent emits pprof profiles with native function names resolved via
  the symbol server, with debug info NOT present locally.
- Cache hit ratio ≥ 95% in steady state for stable workloads.
- No measurable latency regression vs. local-disk symbolization on cache
  hit (within 5% of baseline).
- `go test -race` passes with 1000-iteration concurrency tests.
- A build with `-tags noindex_sqlite` produces a binary that does not link
  `modernc.org/sqlite` (verifiable via `go list -deps`).
- BUILDING.md documents the blazesym pin, the build-tag, and the cache
  semantics.

## Resolved open questions

The PoC-era draft listed four open questions. With perf-agent's actual
structure in view:

- **#1 `/executable` vs `/debuginfo`.** **Hybrid.** `/debuginfo` is the
  primary path, fetched into `debug_dirs`-indexed cache and consumed by
  blazesym's default resolver — small blobs, no dispatcher override.
  `/executable` is the fallback for the binary-not-on-disk case (sidecar)
  and is what the dispatcher returns directly. Per-mapping decision at
  runtime.
- **#2 fork blazesym-go vs cgo.** **Stay with cgo.** The official
  `github.com/libbpf/blazesym/go` binding does not expose `process_dispatch`;
  our package does its own cgo against `libblazesym_c`. This package's
  `LocalSymbolizer` continues to use the upstream binding for the simple
  cases.
- **#3 in-tree vs separate repo.** **In-tree** under `symbolize/debuginfod/`.
  Same CI, same build, same tests. Extract to a separate repo if reuse
  emerges.
- **#4 Resolver reuse.** **Optional `Options.Resolver` field.** When set,
  the dispatcher consults `procmap.Resolver`'s build-id cache before
  parsing the ELF; cuts ELF parses by ~95% in steady state.

## References

- PoC: `Archive.zip` (this conversation), `client/main.go` validates the cgo
  dispatcher path end-to-end against a `debuginfod` server.
- blazesym C API:
  https://github.com/libbpf/blazesym/blob/main/capi/include/blazesym.h
- blazesym capi dispatcher commit:
  https://github.com/libbpf/blazesym/commit/1f2d983
- blazesym `sym-debuginfod` Rust example:
  https://github.com/libbpf/blazesym/tree/main/examples/sym-debuginfod
- elfutils debuginfod:
  https://sourceware.org/elfutils/Debuginfod.html
- Public debuginfod aggregator:
  https://debuginfod.elfutils.org/
