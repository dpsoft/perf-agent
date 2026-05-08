# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/dpsoft/perf-agent/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/dpsoft/perf-agent/compare/v1.0.5...v1.1.0
[1.0.5]: https://github.com/dpsoft/perf-agent/releases/tag/v1.0.5
