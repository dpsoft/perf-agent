# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **CPython frames on the per-PID DWARF path** ([#83](https://github.com/dpsoft/perf-agent/issues/83)).
  `--profile --pid <n> --unwind dwarf` now walks the interpreter's own
  `_PyInterpreterFrame` chain from BPF and splices the Python frames into the
  native stack at the position of the `_PyEval_EvalFrameDefault` frame they were
  running in, so a Python -> C extension -> Python stack comes back interleaved
  rather than as C frames alone. Nothing is injected into the target and no new
  capability is required, though enrolling a process briefly ptrace-stops one of
  its threads to read the TLS base the interpreter's thread-state lookup needs
  (once, at attach; the sampling path itself reads it from the kernel).

  **What it does not do yet.** Python frames are **not symbolized**: each renders
  as `python:0x<code object address>` until the fingerprint/name recovery slice
  lands. It is **amd64 + glibc only** (the pthread-TSD offsets it needs have been
  measured there and nowhere else; a musl target is refused by name), covers
  **CPython 3.12, 3.13 and 3.14** GIL builds (a free-threaded `Py_GIL_DISABLED`
  build is refused by name), and is wired for **`--pid` captures only** —
  system-wide (`-a`) still produces native-only stacks and says so in the log.
  The GPU launch path (`gpuprobe`) enrols interpreters the same way and inherits
  the same walker, but **that combination has not been validated on hardware**:
  no CI machine has a GPU. An interpreter that cannot be
  walked is always refused with a reason on stderr, never walked with guessed
  offsets; `py_walk_counters` is reported at shutdown so a run that walked
  nothing is distinguishable from a run with no Python in it.

### Removed

- **The Python perf-trampoline injector (`--inject-python`) and the `inject/`
  tree behind it.** The injector ptraced into a running CPython 3.12+ target and
  remote-called `sys.activate_stack_trampoline('perf')`. It was removed because
  it mutated the process it measured (and left trampoline overhead behind), only
  worked on CPython 3.12+, required `CAP_SYS_PTRACE` to ptrace into the target,
  and could not profile an already-running process without injecting into it.

  **This removes a shipped capability with no replacement yet.** Until
  [#83](https://github.com/dpsoft/perf-agent/issues/83) lands, perf-agent
  produces **no Python-level frames of its own** — Python processes still
  profile, but with C interpreter frames (`_PyEval_EvalFrameDefault`, …) rather
  than Python qualnames. #83 replaces the injector by walking the interpreter's
  frame chain from BPF (no injection, CPython 3.6+, no new capability). Its first
  slice is now in this release -- see **Added** above -- with Python frames
  correctly placed but rendered as addresses rather than qualnames.

  Unaffected: if the interpreter is started with `python -X perf` (3.12+) by
  whoever launches it, CPython writes `/tmp/perf-<pid>.map` itself and
  perf-agent still reads and decodes those `py::` entries as before.

## [1.2.1] - 2026-08-13

### Fixed

- Kernel-stack symbolization (`--kernel-stacks`) now works under kernel `lockdown=integrity` (Secure Boot). The v1.2.0 symbolizer relied on blazesym probing `/proc/kcore`, which is `CAP_SYS_RAWIO`-gated and absent from the standard `cap_perfmon`/`cap_bpf` set, so every batch returned `BLAZE_ERR_PERMISSION_DENIED` and kernel frames vanished from the pprof. Resolved by bumping blazesym to v0.2.4, which no longer reads `/proc/kcore` for the KASLR offset unless a vmlinux DWARF resolver is present ([#25](https://github.com/dpsoft/perf-agent/pull/25), [#26](https://github.com/dpsoft/perf-agent/pull/26)).
- On symbolization failure, kernel frames are now preserved as raw addresses (`Name: "0x<hex>"`, `Module: "[kernel.kallsyms]"`) instead of being dropped, so kernel context survives into the pprof and hot frames stay decodable via `/proc/kallsyms` ([#25](https://github.com/dpsoft/perf-agent/pull/25)).
- `--perf-data-output` now emits a `PERF_RECORD_MMAP2` per executable mapping of the target PID, so `perf script` / `perf report` resolve user-space frames instead of showing `[unknown]`. System-wide (`-a`) userspace mmaps remain a documented follow-up ([#25](https://github.com/dpsoft/perf-agent/pull/25)).

### Changed

- Bumped blazesym to v0.2.4 and removed the pure-Go `/proc/kallsyms` fallback introduced during hardening — the newer blazesym resolves lockdown-class hosts directly, so the fallback (and its `PERFAGENT_FORCE_KERNEL_FALLBACK` escape hatch and `KernelFallbackEngaged` counter) is no longer needed. `KernelLockdownEPERM` / `KernelOtherErr` / `KernelRawAddrFrames` counters remain for observability ([#26](https://github.com/dpsoft/perf-agent/pull/26)).

## [1.2.0] - 2026-05-15

### Added

- Opt-in kernel-mode stack capture and symbolization (`--kernel-stacks`). Interleaves kernel and user frames in the same pprof stack; off by default ([#21](https://github.com/dpsoft/perf-agent/pull/21)).

### Fixed

- Off-box symbolization for stripped binaries that lack `.gnu_debuglink` (the common Rust/Go release-build case). The v1.1.0 dispatcher relied on blazesym's split-debug lookup, which silently no-op'd without a debug-link. v1.2.0 adds a per-mapping classifier that normalizes addresses to file-VAs and symbolizes against the fetched `.debug` directly via blazesym's elf-virt API ([#22](https://github.com/dpsoft/perf-agent/pull/22)).

## [1.1.0] - 2026-05-08

### Added

- DWARF-based stack unwinding (`--unwind dwarf`) for binaries built without frame pointers ([#7](https://github.com/dpsoft/perf-agent/pull/7)).
- `--unwind auto` (default) with lazy CFI compilation — per-binary CFI is deferred until the first BPF miss notification, dramatically reducing startup cost on large fleets ([#11](https://github.com/dpsoft/perf-agent/pull/11)).
- Python perf-trampoline injector (`--inject-python`) — activates `sys.activate_stack_trampoline('perf')` on running CPython 3.12+ targets via `ptrace`, producing native + Python interleaved stacks ([#12](https://github.com/dpsoft/perf-agent/pull/12)).
- Namespace-aware `--pid` translation — target-namespace PIDs are translated to host PIDs for sidecar / `shareProcessNamespace` deployments. pprof samples carry k8s identity labels (`pod_uid`, `container_id`, `cgroup_path`, plus best-effort `pod_name` / `namespace` / `container_name`) parsed from the cgroup, with no kubelet API calls ([#14](https://github.com/dpsoft/perf-agent/pull/14)).
- Kernel-format `perf.data` emitter (`--perf-data-output`) — output is consumable by `perf script`, `perf report`, FlameGraph, hotspot, AutoFDO `create_llvm_prof`, etc. Requires `--profile` ([#17](https://github.com/dpsoft/perf-agent/pull/17)).
- Debuginfod-backed off-box symbolization (`--debuginfod-url`) — fetches DWARF on demand from `debuginfod`-protocol servers, keyed by GNU build-id, with a SQLite-indexed local cache and LRU eviction. Uses blazesym's `process_dispatch` hook for per-mapping routing ([#19](https://github.com/dpsoft/perf-agent/pull/19)).
- Benchmark infrastructure: scenario harness, fleet driver, and before/after report tool under `bench/` ([#9](https://github.com/dpsoft/perf-agent/pull/9)).
- Community files: LICENSE, CONTRIBUTING, CODE_OF_CONDUCT, SECURITY ([#15](https://github.com/dpsoft/perf-agent/pull/15)).

### Changed

- pprof frame model refactor for cleaner inline expansion ([#8](https://github.com/dpsoft/perf-agent/pull/8)).
- `internal/perfevent` extracted as a reusable per-CPU `perf_event_open` + `AttachRawLink` helper ([#13](https://github.com/dpsoft/perf-agent/pull/13)).
- README rewrite + intro / use-case / architecture trim ([#15](https://github.com/dpsoft/perf-agent/pull/15), [#16](https://github.com/dpsoft/perf-agent/pull/16)).

### Fixed

- PGO examples: `create_llvm_prof` + rustc invocations so the cycle works end-to-end ([#18](https://github.com/dpsoft/perf-agent/pull/18)).

[Unreleased]: https://github.com/dpsoft/perf-agent/compare/v1.2.1...HEAD
[1.2.1]: https://github.com/dpsoft/perf-agent/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/dpsoft/perf-agent/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/dpsoft/perf-agent/compare/v1.0.5...v1.1.0
[1.0.5]: https://github.com/dpsoft/perf-agent/releases/tag/v1.0.5
