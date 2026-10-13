# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.3.0] - 2026-10-09

The GPU and Python release. perf-agent can now attribute CUDA kernel time to the
CPU stacks that launched it, and name Python frames walked out of the live
interpreter — neither of which modifies the application being measured.

### Added

- **GPU profiling for CUDA workloads.** A CUPTI adapter is injected into the
  target by the CUDA driver through `CUDA_INJECTION64_PATH`, emits USDT probes,
  and an eBPF consumer joins each kernel execution to the CPU stack that
  launched it, keyed on CUPTI's correlation id. A profile reads as one tower:
  the application's own frames above `cudaLaunchKernel`, and the kernel
  beneath. Validated on an RTX 3090: 505/505 sampled launches carry a stack
  that reaches a real root, joins cannot cross a process, and no stack is lost
  to batch ordering ([#30](https://github.com/dpsoft/perf-agent/issues/30), [#34](https://github.com/dpsoft/perf-agent/issues/34), [#37](https://github.com/dpsoft/perf-agent/issues/37),
  [#38](https://github.com/dpsoft/perf-agent/issues/38), [#40](https://github.com/dpsoft/perf-agent/issues/40), [#43](https://github.com/dpsoft/perf-agent/issues/43)).

  Launches are sampled one in N. The agent does not set that: the **adapter**
  reads `PERFAGENT_GPU_SAMPLE_PERIOD` (default 8) from the *target's*
  environment, so it must be set alongside `CUDA_INJECTION64_PATH` before the
  workload starts. `cmd/gpu-cuda-profile` additionally exposes it as `-period`
  for runs it launches itself.

  Attachment uses the `uprobe_multi` BPF link rather than the `perf_uprobe`
  PMU, which is what lets it run without `CAP_SYS_ADMIN` — measured: the PMU
  path fails `EACCES` under `CAP_BPF`+`CAP_PERFMON`. That costs a Linux 6.6
  floor for the GPU path; below it the PMU path works and needs
  `CAP_SYS_ADMIN`.

- **Python frames, walked from BPF and named** ([#83](https://github.com/dpsoft/perf-agent/issues/83),
  [#130](https://github.com/dpsoft/perf-agent/issues/130), [#132](https://github.com/dpsoft/perf-agent/issues/132)). A capture walks the interpreter's own
  `_PyInterpreterFrame` chain and splices Python frames into the native stack
  at the `_PyEval_EvalFrameDefault` frame they were running in, so a
  Python → C extension → Python stack comes back interleaved rather than as C
  frames alone. Frames carry qualname and file:line — `pa_train_step
  (torch_workload.py:76)`, not an address. A frame whose code object cannot be
  resolved keeps the `python:0x…` form rather than guessing.

  Nothing is injected into the target and no new capability is required, though
  enrolling a process briefly ptrace-stops one thread to read the TLS base the
  interpreter's thread-state lookup needs (once, at attach; the sampling path
  reads it from the kernel). An interpreter that cannot be walked is refused by
  name on stderr, never walked with guessed offsets.

  Covers **CPython 3.12–3.14** GIL builds on **amd64 + glibc**, including
  distro interpreters whose eval loop is split by LTO and reachable only
  through `.gnu_debugdata` ([#170](https://github.com/dpsoft/perf-agent/issues/170), [#190](https://github.com/dpsoft/perf-agent/issues/190)). Limits are
  listed under **Known limitations** below.

- **GPU PC sampling, two tiers.** `-gpu-pc-sampling=continuous` is measurably
  free (−0.03%, and −0.006%/+0.017% on two earlier independent runs).
  `serialized` reaches instruction-level attribution with stall reasons but
  perturbs the workload, so it requires an explicit
  `-gpu-pc-sampling-acknowledge-perturbation` and is duty-capped at 5% — the
  arm measured at +4.31% ([#103](https://github.com/dpsoft/perf-agent/issues/103), [#104](https://github.com/dpsoft/perf-agent/issues/104), [#107](https://github.com/dpsoft/perf-agent/issues/107)).

- **Collector deployment: one agent per node.** `-mode=collector` installs the
  shim it carries into a `hostPath` directory, attaches once with `PID: 0`, and
  profiles every process that maps that inode — including ones that start
  later. Measured on hardware rather than assumed: a workload started *after*
  the link existed, never discovered and with rediscovery disabled, contributed
  120,000 samples; a byte-identical copy at another path contributed none
  ([#147](https://github.com/dpsoft/perf-agent/issues/147), [#148](https://github.com/dpsoft/perf-agent/issues/148)).

  This is **not** zero-touch. The application pod still declares a volume mount
  and `CUDA_INJECTION64_PATH`; a `hostPath` shim changes which inode that names
  rather than removing the variable. Injecting those without editing the pod
  needs a mutating admission webhook, which is not built.

- **Attach to a running process.** `-pid` and `-discover` profile processes this
  command did not start, which is what a sidecar and a node collector both
  need. Late attach is reported rather than papered over: the adapter captures
  module bytes only while a consumer is present, so modules loaded before the
  attach carry no source line and every attached run says so
  ([#136](https://github.com/dpsoft/perf-agent/issues/136), [#138](https://github.com/dpsoft/perf-agent/issues/138)).

- **Published artifacts.** Release tags publish `ghcr.io/dpsoft/perf-agent` and
  `ghcr.io/dpsoft/perf-agent-shim` as multi-arch images, plus
  `gpu-cuda-profile-linux-{amd64,arm64}` binaries. Both images carry the same
  tag deliberately — the shim's USDT record layouts are frozen per version and
  the agent decodes them — though nothing at runtime refuses a mismatched pair
  ([#149](https://github.com/dpsoft/perf-agent/issues/149), [#151](https://github.com/dpsoft/perf-agent/issues/151), [#153](https://github.com/dpsoft/perf-agent/issues/153)).

- **The GPU shim builds on aarch64.** The agent itself has shipped `linux/arm64`
  since v1 — binaries, BPF objects and CI all cover both architectures, and
  that is unchanged. What was missing is the GPU *shim*, which had never
  compiled on aarch64: it bound its USDT probe arguments to `rdi`/`rsi`/`rdx`
  by name with no architecture guards, and its own test had been skipping with
  "not x86-64" the whole time. The probes now bind the first three
  integer-argument registers of the platform ABI, and `probe_args_test` proves
  the binding on a native arm64 runner by trapping its own probe and reading
  the registers back ([#152](https://github.com/dpsoft/perf-agent/issues/152)).

- **An injection self-check.** `CUDA_INJECTION64_PATH` fails open and silent, so
  a broken install is otherwise indistinguishable from a workload that launched
  no kernels. Every run now says whether injection happened ([#135](https://github.com/dpsoft/perf-agent/issues/135)).

- **`--gpu-ring-bytes`, and GPU record loss is no longer silent**
  ([#204](https://github.com/dpsoft/perf-agent/issues/204)). `gpuprobe.Stats` counted every kind of loss and nothing
  printed it; `gpuprofile.Profiler` exposed no `Stats()` at all, so the agent
  could not read the counters even in principle. A capture that lost most of
  its GPU work still reported only `GPU profile written (N samples)`, and N
  alone cannot tell a quiet GPU from a lossy capture.

  A run now prints what arrived and, when any loss counter is non-zero, a
  `gpu: LOST` line naming each one. The loss is real and scales with the event
  rate: measured on an RTX 3090, the same workload lost 5.5% of records at the
  default sampling period and 90% at period 1, nearly all of it the BPF event
  ringbuf overflowing. What overflows is the CUPTI kernel *activity* records,
  which carry the GPU time itself — so lowering the period to buy attribution
  shrank the profile rather than coarsening it. `--gpu-ring-bytes` sizes that
  buffer; the compiled-in 4 MB default is unchanged.

- **Several profiles drawn as one flame graph** (`flamegraph -fuse`), which is
  how a CPU profile and a GPU profile become a single tree ([#158](https://github.com/dpsoft/perf-agent/issues/158)).

- **Flame graph rework.** A right-click menu (pin, zoom, hide a binary, copy
  frame/path/module), a frame-details panel, dark theme, a favicon, and runs of
  unnamed vendor frames collapsed to one frame named for the library. Hiding
  moves no time: widths are absolute in the markup, so hiding changes only
  depth, and a note above the chart names what is hidden ([#133](https://github.com/dpsoft/perf-agent/issues/133),
  [#142](https://github.com/dpsoft/perf-agent/issues/142), [#144](https://github.com/dpsoft/perf-agent/issues/144), [#145](https://github.com/dpsoft/perf-agent/issues/145), [#203](https://github.com/dpsoft/perf-agent/issues/203)).

- **`-keep-instrumentation-frames`.** By default a capture collapses CUPTI's
  callback machinery — 7 frames per sampled stack — to one marker, and the
  elision happens before the profile is written. The flag keeps them, for
  profiling perf-agent itself ([#145](https://github.com/dpsoft/perf-agent/issues/145)).

- **Go PGO actually works.** `pprof` now populates `Function.start_line`, which
  is what `go build -pgo=` reads; without it the toolchain silently inlined
  nothing ([#171](https://github.com/dpsoft/perf-agent/issues/171), [#180](https://github.com/dpsoft/perf-agent/issues/180)).

- **Per-sample pprof labels**, folded into the dedup hash, so a label set
  cannot silently merge two distinct samples
  ([#28](https://github.com/dpsoft/perf-agent/issues/28)). The Kubernetes pod
  labels that ride on them are not new — they shipped before v1.2.1.

- **`--no-debuginfod`** to disable network symbolization explicitly
  ([#119](https://github.com/dpsoft/perf-agent/issues/119)).

### Changed

- **`CAP_CHECKPOINT_RESTORE` is required rather than degraded around.** Without
  it every user-space frame resolves to a bare hex address; the agent now says
  so and refuses instead of producing a profile that looks fine and names
  nothing ([#41](https://github.com/dpsoft/perf-agent/issues/41)).

- **Debuginfod stops dominating short captures.** The fetch timeout went 30s →
  5s, after a 5s capture spent 87s in debuginfod because every build-id the
  servers cannot serve cost a full timeout synchronously ([#110](https://github.com/dpsoft/perf-agent/issues/110)).
  The *dial* is now bounded separately at 300 ms, so an unreachable server
  fails fast instead of burning the fetch budget on a connection that will
  never open — an air-gapped capture went 5.004 s → 301 ms ([#199](https://github.com/dpsoft/perf-agent/issues/199)).

- **MiniDebugInfo symbols are cached** per (dev, ino, size, mtime), which
  [#190](https://github.com/dpsoft/perf-agent/issues/190) made worth doing: `EvalRangesForFile` went 21.3 ms → 716 µs
  ([#197](https://github.com/dpsoft/perf-agent/issues/197)).

- **Interpreter enrolment no longer sleeps through the ptrace stop.** A flat
  1 ms poll dominated the stop; a bounded spin took the median from 1.088 ms to
  25 µs, and CI measures 5–13 µs ([#198](https://github.com/dpsoft/perf-agent/issues/198)).

### Fixed

- **pprof samples were stored root-first**, so `go tool pprof` read every
  profile this agent ever wrote inside out ([#164](https://github.com/dpsoft/perf-agent/issues/164)), and GPU projected
  frames had the same inversion ([#172](https://github.com/dpsoft/perf-agent/issues/172)).

- **Two defects in the default DWARF unwinder**: stack order, and lost
  userspace frames ([#155](https://github.com/dpsoft/perf-agent/issues/155), [#156](https://github.com/dpsoft/perf-agent/issues/156), [#157](https://github.com/dpsoft/perf-agent/issues/157)).

- **arm64 unwinding of large frames.** `CFAOffset` was `int16`, and AAPCS64
  keeps the CFA SP-rooted as the frame grows, so a 72,032-byte frame wrapped to
  6,496 and every arm64 walk ended three frames deep. x86-64 was immune because
  gcc pins the offset at 16 via `DW_CFA_def_cfa_register %rbp`
  ([#185](https://github.com/dpsoft/perf-agent/issues/185)).

- **A zero return address ends the walk and says so**, instead of being pushed
  as a `0x0` frame ([#187](https://github.com/dpsoft/perf-agent/issues/187), [#195](https://github.com/dpsoft/perf-agent/issues/195)).

- **A correlation-less PC sample kept its producing pid**, which three join
  counters needed to be reachable at all ([#184](https://github.com/dpsoft/perf-agent/issues/184), [#196](https://github.com/dpsoft/perf-agent/issues/196)).

- **A store-less `ehmaps` tracker crashed** instead of reporting, when a
  synthetic pid happened to exist ([#192](https://github.com/dpsoft/perf-agent/issues/192)).

- **`perf.data` now writes the `sample_id` trailer its attr promises**
  ([#161](https://github.com/dpsoft/perf-agent/issues/161), [#162](https://github.com/dpsoft/perf-agent/issues/162)).

- **The shared symbolizer is serialized** — concurrent use aborted the process
  from inside Rust ([#173](https://github.com/dpsoft/perf-agent/issues/173)).

- **Symbolization recovers names when blazesym fails a whole batch**
  ([#125](https://github.com/dpsoft/perf-agent/issues/125), [#127](https://github.com/dpsoft/perf-agent/issues/127)), and NVIDIA's stripped libraries get their
  symbols fetched ([#128](https://github.com/dpsoft/perf-agent/issues/128)).

### Removed

- **The Python perf-trampoline injector (`--inject-python`) and the `inject/`
  tree behind it.** It ptraced into a running CPython 3.12+ target and
  remote-called `sys.activate_stack_trampoline('perf')`. It mutated the process
  it measured, left trampoline overhead behind, required `CAP_SYS_PTRACE`, and
  could not profile an already-running process without injecting into it. Its
  replacement — walking the interpreter's frame chain from BPF — is in this
  release under **Added**.

  Unaffected: if the interpreter is started with `python -X perf` (3.12+) by
  whoever launches it, CPython writes `/tmp/perf-<pid>.map` itself and
  perf-agent still reads and decodes those `py::` entries as before.

- **Design documents and SDD reports left the tree** ([#201](https://github.com/dpsoft/perf-agent/issues/201)).

### Known limitations

- **Python enrolment is per-PID.** A `--pid` capture walks the interpreter; a
  system-wide `-a` capture does **not** enrol at all, so every Python process
  in it gets native-only stacks. The agent logs this rather than leaving it to
  be discovered from the profile. Enrolment was measured at ~6 ms per
  interpreter (~627 ms for 100), so the cost is affordable; what is undecided
  is the trigger ([#194](https://github.com/dpsoft/perf-agent/issues/194)).

- **Python frames are amd64 + glibc only.** The pthread-TSD offsets the walk
  needs were measured on Fedora glibc 2.43 and are applied to every glibc with
  no version gate — long-stable values, checked at attach by reading a real
  frame, but not checked against the libc. A musl target is refused by name,
  as is a free-threaded (`Py_GIL_DISABLED`) build.

- **Off-CPU stacks stay native.** The off-CPU profiler exposes no Python maps,
  so `--offcpu` carries no Python frames.

- **Python + GPU is not validated on hardware by CI.** `gpuprobe` enrols
  interpreters the same way and inherits the same walker, and it has been run
  by hand on an RTX 3090, but no CI machine has a GPU.

- **The CUPTI integration is untested on ARM**, for want of an arm64 GPU node.
  That part is arch-independent C++ calling CUPTI APIs rather than anything
  register-level, but it has not been run.

- **`DEBUGINFOD_URLS` still enables network symbolization implicitly** on
  distributions that set it (Fedora does). `--no-debuginfod` turns it off; what
  the default should be is undecided ([#111](https://github.com/dpsoft/perf-agent/issues/111)).

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

[Unreleased]: https://github.com/dpsoft/perf-agent/compare/v1.3.0...HEAD
[1.3.0]: https://github.com/dpsoft/perf-agent/compare/v1.2.1...v1.3.0
[1.2.1]: https://github.com/dpsoft/perf-agent/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/dpsoft/perf-agent/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/dpsoft/perf-agent/compare/v1.0.5...v1.1.0
[1.0.5]: https://github.com/dpsoft/perf-agent/releases/tag/v1.0.5
