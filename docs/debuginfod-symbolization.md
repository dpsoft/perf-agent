# Debuginfod-backed symbolization

`perf-agent` integrates with the [debuginfod](https://sourceware.org/elfutils/Debuginfod.html)
protocol to resolve symbols from stripped production binaries. Pass
`--debuginfod-url=URL` (or set `DEBUGINFOD_URLS`) and the agent fetches
DWARF debug info and/or ELF executables on demand, keyed by GNU build-id,
and caches them on disk beside the local blazesym resolution path.
Without `--debuginfod-url` the feature is entirely off — no HTTP, no
cache, no change to existing behavior.

---

## When to enable it

**Stripped production binaries** are the primary use case. Most release
images strip debug info (and sometimes the binary itself is absent from the
node's filesystem). debuginfod lets the agent fetch what it needs from a
server rather than requiring debug packages to be installed on every host.

**DaemonSet on the host PID namespace** is the best-fit deployment: the
agent runs on the node, `/proc/<pid>/maps` is always visible, and the only
missing piece is DWARF. The dispatcher's Case 3 covers this path — it
leaves the binary in place and only fetches `/debuginfo`, dropping it into
the cache directory where blazesym finds it automatically.

**Sidecar without `shareProcessNamespace`** is supported but partial. The
dispatcher can't see the target's filesystem, so it falls into Case 4 and
fetches the full `/executable`. The agent still needs `/proc/<pid>/maps` to
build the address map — that file is always visible from the host regardless
of namespace — so symbolization works end-to-end even for binaries that
exist only in the container image.

---

## Quickstart

Spin up the bundled test server and populate it with a sample binary:

```bash
cd test/debuginfod
docker compose up -d debuginfod
./upload.sh sample/hello.full        # populate one build-id
```

Point `perf-agent` at it:

```bash
perf-agent --profile --pid <PID> --duration 10s \
  --debuginfod-url=http://localhost:8002
```

The agent fetches DWARF or the full ELF on the first collection cycle and
caches the result. Subsequent runs reuse the cached artifact (Case 1 in the
dispatcher). With no `--debuginfod-url` and no `DEBUGINFOD_URLS` env var,
the agent uses the local blazesym path unchanged.

---

## How the dispatcher decides

For each binary mapping encountered during symbolization, the dispatcher
runs a four-case routing table to decide whether to pass blazesym an
override path or let it use its default resolution logic.

| Case | Trigger | Action | Result |
|---|---|---|---|
| 1 | Cached executable already on disk | Return cache path to blazesym | blazesym uses cached `/executable` |
| 2 | Local DWARF or cached `.debug` covers the mapping | Return NULL | blazesym uses default (no HTTP) |
| 3 | Binary on disk but missing DWARF | Fetch `/debuginfo`, return NULL | blazesym finds it via `debug_dirs` |
| 4 | Binary not on disk | Fetch `/executable`, return cache path | blazesym uses fetched ELF |

Cases 2 and 3 are the common path on a DaemonSet node: binaries are on
disk (they came with the package or container layer) but DWARF was stripped.
Case 1 is a warm-cache hit after any prior fetch. Case 4 is the sidecar
fallback where the binary doesn't exist on the node filesystem at all.

The dispatcher only runs when a GNU build-id is present in the ELF
`.note.gnu.build-id` section. Mappings without a build-id are skipped
silently.

---

## Cache layout & tuning

The on-disk layout mirrors the standard `.build-id` directory structure
that blazesym's `debug_dirs` walker recognizes natively:

```
<CacheDir>/
├── .build-id/
│   └── <NN>/
│       ├── <rest>.debug      ← from /debuginfo
│       └── <rest>            ← from /executable
└── index.db                   ← SQLite LRU index
```

`<NN>` is the first two hex characters of the build-id; `<rest>` is the
remainder. This is the same layout used by `debuginfod-find` and
`eu-debuginfod-find`.

**Default cache dir:** `/tmp/perf-agent-debuginfod`. Note that `/tmp` is
`tmpfs` (RAM-backed) on most Linux distributions and systemd setups — the
cache is lost on reboot. For persistent storage across node restarts, use
`--symbol-cache-dir=/var/cache/perf-agent-debuginfod` (or any path on a
real filesystem).

**Default cap:** 2 GiB. LRU eviction runs automatically after each
successful fetch, deleting the least-recently-used artifacts until total
on-disk size falls back under the cap.

**Atomic writes:** artifacts are streamed to a `fetch-*.tmp` file in the
same directory as the final destination, then renamed into place with
`os.Rename`. This avoids partial writes being visible to the blazesym
`debug_dirs` walker.

**Prewarm on startup:** when the agent starts, it walks the cache directory
and re-indexes any existing artifacts. This means a restarted agent inherits
a populated cache from a previous run, even if the SQLite index was lost.

---

## Configuration

| Flag | Env var fallback | Default | Notes |
|---|---|---|---|
| `--debuginfod-url=URL` | `DEBUGINFOD_URLS` (space-separated) | unset (feature off) | Repeatable; ordered list, first 200 wins, 404 falls through to next. |
| `--symbol-cache-dir=DIR` | — | `/tmp/perf-agent-debuginfod` | |
| `--symbol-cache-max=BYTES` | — | `2147483648` (2 GiB) | LRU eviction cap. |
| `--symbol-fetch-timeout=DUR` | — | `30s` | Per-artifact HTTP timeout. |
| `--symbol-fail-closed` | — | `false` | Reserved for M2; flag exists, semantics not yet wired. |

When `--debuginfod-url` is unset and `DEBUGINFOD_URLS` is empty, the agent
uses the local blazesym resolution path (no HTTP, no cache).

---

## Running your own server

`test/debuginfod/` is the canonical example. It runs
[elfutils `debuginfod`](https://sourceware.org/elfutils/Debuginfod.html)
in Docker and exposes it on port 8002.

elfutils `debuginfod` is the de facto server implementation — it speaks the
protocol that `perf-agent` uses. Two ways to populate it:

1. **Drop pre-extracted files** into the `.build-id/<NN>/<rest>.debug`
   layout on the server's scan path. This is what `upload.sh` does: it
   copies the stripped binary and its companion `.debug` file into the store
   directory that the container mounts read-only.

2. **Federate to upstream servers.** Set `DEBUGINFOD_URLS` in the server's
   environment (the compose file already federates to
   `https://debuginfod.debian.net/` and `https://debuginfod.elfutils.org/`)
   and the server will proxy requests it can't answer locally.

The two endpoints `perf-agent` uses are:

- `GET /buildid/<hex>/debuginfo` — fetch the DWARF-bearing `.debug` file.
- `GET /buildid/<hex>/executable` — fetch the stripped ELF.

The source endpoint (`GET /buildid/<hex>/source/<path>`) is not used.

---

## Public debuginfod servers

Several distributions run public debuginfod servers you can point at
directly via `--debuginfod-url` or `DEBUGINFOD_URLS`:

- **`https://debuginfod.elfutils.org/`** — the upstream federation hub
  maintained by the elfutils project. Federates to distro servers and is
  generally what `DEBUGINFOD_URLS` defaults to on an elfutils install.
- **`https://debuginfod.debian.net/`** — Debian packages (including stable
  and testing).
- **`https://debuginfod.fedoraproject.org/`** — Fedora packages.
- **`https://debuginfod.ubuntu.com/`** — Ubuntu packages.

Public servers are read-only. For private build artifacts (your own Go
services, Rust binaries, etc.) run your own server and populate it with
your build products.

---

## Troubleshooting

**No build-id in mapping.** The dispatcher can't look up an artifact
without a GNU build-id. `DispatcherCalls` increments but `CacheMisses` does
not, meaning the mapping is silently skipped. Build with `-Wl,--build-id`
(the linker default for GCC and Clang on modern distributions) or
`RUSTFLAGS="-C link-arg=-Wl,--build-id"` for Rust. Verify with
`file <binary>` — look for `BuildID[sha1]=`.

**Server unreachable.** `FetchErrors` increments on each failed HTTP
request. The default behavior is to fall back to local blazesym resolution
(no override path returned). Check the URL, firewall rules, and that the
container is healthy (`docker compose ps`).

**Permission denied on cache dir.** Common when `/tmp/perf-agent-debuginfod`
was previously created by a different user (e.g., a prior `docker run` as
root). Fix with `rm -rf /tmp/perf-agent-debuginfod` or pass
`--symbol-cache-dir=/path/writable/by/current-user`.

**Stale cache.** Run with a fresh `--symbol-cache-dir` to force a clean
fetch. M2 will add an explicit cache-invalidation flag.

---

## What's not yet supported (M2)

- **`--symbol-fail-closed` semantics.** The flag is parsed and stored but
  does not yet override the resolved frames when a fetch fails. Today the
  dispatcher always falls back to local resolution on error.

- **Metrics exporter integration.** Counters exist internally (see
  `symbolize/debuginfod/stats.go`) and are accessible via the `Stats()`
  method, but the snapshot is not yet wired through the `metrics.Exporter`
  interface. Prometheus/OpenTelemetry export of cache hit rate, fetch
  latency, and error counts is deferred to M2.

- **Exponential backoff on 5xx.** Today each collection cycle retries
  naturally on the next dispatcher call. A retry budget with backoff is
  planned for M2.

- **Mapping blazesym's `reason` enum to `symbolize.FailureReason`.** Both
  the `LocalSymbolizer` and the debuginfod dispatcher currently flatline
  unresolvable frames to `FailureUnknownAddress`. Richer failure attribution
  (no build-id, fetch failed, DWARF corrupt, etc.) is M2 work.
