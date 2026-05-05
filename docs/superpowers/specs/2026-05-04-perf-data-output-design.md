# `perf.data` output — Design Spec

> **Status:** drafted, awaiting user review.
> **Supersedes:** `docs/superpowers/specs/2026-04-25-s10-pgo-converter-idea.md` — the Path A vs Path B brainstorm. This spec implements Path B and broadens the framing from "Rust PGO converter" to "perf.data emission, with PGO/AutoFDO as one of several downstream consumers."

## Problem

S9 left perf-agent's pprof output with everything a sample-based PGO pipeline needs — per-binary `Mapping` entries with absolute path + GNU build-id + file offsets, file-offset-keyed `Location` entries, expanded inline chains. Downstream consumers can already attribute samples back to ELF file offsets, which is the prerequisite for AutoFDO.

But pprof is the *wrong* serialization format for most of the Linux profiling ecosystem. AutoFDO's `create_llvm_prof`, `perf script`, `perf report`, FlameGraph (`stackcollapse-perf.pl`), hotspot, and magic-trace all consume the kernel's `perf.data` binary format. Today, anyone who wants to use perf-agent samples with one of these tools has no path: convert pprof back to perf.data is not a thing. The ecosystem assumes you ran `perf record`.

This spec adds a second output format alongside pprof. The capture stays the same; we serialize the same in-memory state to a different file.

## Goals

1. perf-agent emits standards-compliant `perf.data` files from its CPU profile pipeline. The kernel's [`perf.data` file format spec](https://github.com/torvalds/linux/blob/master/tools/perf/Documentation/perf.data-file-format.txt) is the contract.
2. Output passes through `perf script` without errors, and through `create_llvm_prof` for AutoFDO PGO use cases.
3. Works in cloud / virtualized environments (k8s pods, AWS/GCE VMs) where hardware PMU events are unavailable. The event-type strategy auto-detects and falls back gracefully.
4. Single new flag: `--perf-data-output <path>`. Combine with existing `--profile` (per-PID or `-a`); combine with `--profile-output` to also emit pprof from the same capture.
5. No BPF code changes. The data we already capture is the same data perf.data needs.

## Non-goals

- **LBR / branch-stack capture (`PERF_SAMPLE_BRANCH_STACK`).** Bare-metal-only feature; AutoFDO accuracy bonus on Intel SKX+/AMD Zen 3+. v2 spec.
- **Hardware-event opt-in flag.** v1 auto-detects (cycles → cpu-clock fallback) without user input. An explicit `--pgo-event=cycles|cpu-clock` flag belongs in v2 if any user ever asks.
- **Vendoring or wrapping external consumers.** `create_llvm_prof`, `perf`, FlameGraph, hotspot, magic-trace — all separate tools the user installs as needed. perf-agent ships the format, not the ecosystem.
- **Direct LLVM `.profdata` emission (Path A).** Reimplements well-trodden conversion logic in `create_llvm_prof` (function-relative line offsets, mangled-name preservation, callsite vs body samples, faithful inline expansion). Out of scope.
- **IR-level PGO support.** rustc / clang IR-level PGO is instrumentation-based; perf-agent fundamentally can't feed it. AutoFDO (sample-based) is the only PGO mode this spec enables.
- **Reimplementing pprof emission.** pprof already exists. perf.data is *additional*, not a replacement.

## Architecture

### Component map

```
internal/perfdata/         ← NEW: perf.data binary writer.
  perfdata.go              ← Writer struct: open/close/flush; record streaming.
  records.go               ← PERF_RECORD_* encoders (MMAP2, COMM, SAMPLE, FINISHED_ROUND).
  attr.go                  ← perf_event_attr serialization.
  feature.go               ← HEADER_BUILD_ID feature section.
  perfdata_test.go         ← Format-correctness fixtures + perf-script round-trip.

internal/perfevent/        ← TOUCHED: existing helper, gain auto-detect.
  perfevent.go             ← New: ProbeHardwareCycles() → (selectedEvent, error)
                             called by profile/dwarfagent constructors.

profile/profiler.go        ← TOUCHED: opens perfdata.Writer when configured;
                              fans samples out to both pprof and perfdata.
unwind/dwarfagent/agent.go ← TOUCHED: same pattern as profile/.
perfagent/options.go       ← TOUCHED: WithPerfDataOutput(path string).
perfagent/agent.go         ← TOUCHED: passes the option through to whichever
                              CPU profiler is constructed.
main.go                    ← TOUCHED: --perf-data-output flag.

docs/perf-data-output.md   ← NEW: user-facing recipes (perf script, FlameGraph,
                              create_llvm_prof for Rust/C++ AutoFDO, etc.)
```

The new package follows the same shape as `internal/perfevent` and `internal/k8slabels` — small, single-responsibility, no external deps, stdlib only.

### CLI surface

One new flag, orthogonal to existing output flags:

```bash
perf-agent --profile --pid <PID> --duration 60s --perf-data-output app.perf.data
```

Combine with `--profile-output app.pb.gz` to emit *both* pprof and perf.data from one capture (the BPF samples are read once; we write them to two formats). Works with `--pid <N>` or `-a/--all`. Off-CPU and PMU paths don't emit perf.data — the format is per the kernel's CPU sampling shape and only the CPU profile pipeline produces samples that fit it.

The flag has no default — when unset, no perf.data is written, and behaviour is identical to today.

### Event-type auto-detect

At startup, before opening per-CPU perf events for the CPU profiler, `internal/perfevent.ProbeHardwareCycles()` runs:

1. Open one `perf_event_open` with `type=PERF_TYPE_HARDWARE`, `config=PERF_COUNT_HW_CPU_CYCLES`, `pid=-1`, `cpu=0`, `flags=PERF_FLAG_FD_CLOEXEC`. Disabled at open.
2. On success: close the probe fd, return `cyclesEvent{type: PERF_TYPE_HARDWARE, config: PERF_COUNT_HW_CPU_CYCLES}`.
3. On `EOPNOTSUPP`, `ENOENT`, `EACCES`, or `EINVAL`: return `softwareEvent{type: PERF_TYPE_SOFTWARE, config: PERF_COUNT_SW_CPU_CLOCK}`.
4. Log at INFO level which event was selected: `"perf-agent: using hardware cycles event"` or `"perf-agent: PMU not available, using software cpu-clock event"`.

The selected event is recorded in the perf.data `attr` section so any consumer (`create_llvm_prof`, `perf report`) interprets samples correctly. The probe runs once per agent lifetime; the result is cached and used for both per-CPU perf event setup and the perf.data attr writing.

For the existing pprof path (no `--perf-data-output`), the probe still runs because the same per-CPU `perf_event_open` calls happen — we just consume the result silently. This is a behaviour change from today (where perf-agent always uses the software event); the upgrade benefits bare-metal users and is invisible to virt users (graceful fallback).

### perf.data record set

The kernel format is dense; the subset we emit is the canonical AutoFDO/`perf script`-readable shape:

| Element | Source | Notes |
|---|---|---|
| File header | static | Magic `PERFILE2` + 8-byte size + attr/data offsets + feature bitmap. Size 104 bytes. |
| `perf_event_attr` (one) | `internal/perfevent` probe | `type` + `config` + `sample_type = IP \| TID \| TIME \| CALLCHAIN \| PERIOD \| CPU` + `sample_period` (matches `--sample-rate` Hz via `freq` mode) + `read_format = 0`. |
| `attr_id` table | static | One id (we have one event), small. |
| `data` section: | | |
| `PERF_RECORD_COMM` | `/proc/<pid>/comm` | Per-PID, emitted at first sample for that PID. Records process name. |
| `PERF_RECORD_MMAP2` | `unwind/procmap.Resolver` | Per (pid, mapping) tuple, emitted before first sample referencing it. Carries pid, tid, addr, len, pgoff, maj/min/ino, prot/flags, build-id (or zeros), filename. |
| `PERF_RECORD_SAMPLE` | BPF stack ringbuf | Per sample: pid, tid, time (monotonic ns), CPU, IP (leaf), callchain (BPF stack walk), period. |
| `PERF_RECORD_FINISHED_ROUND` | per-N-records | Synchronization marker; consumers tolerate either way. We emit one per second. |
| Feature sections (after data): | | |
| `HEADER_BUILD_ID` | `procmap.Resolver` | Build-id table for every binary referenced by an MMAP2 record. The join key for `create_llvm_prof --binary=`. |
| `HEADER_HOSTNAME` | `os.Hostname()` | Helps consumers correlate captures from multiple nodes. |
| `HEADER_OSRELEASE` | `uname -r` | For diagnostic output. |
| `HEADER_NRCPUS` | `cpuonline.Get()` | Number of online CPUs at capture time. |

What we deliberately *don't* emit in v1:
- `PERF_RECORD_FORK` / `PERF_RECORD_EXIT` — we don't watch fork/exit lifecycle today; capturing it is a separate feature.
- `PERF_RECORD_AUX` / `PERF_RECORD_AUX_OUTPUT_HW_ID` — for hardware tracing (Intel PT, ARM CoreSight); not in our pipeline.
- `PERF_SAMPLE_BRANCH_STACK` — LBR; v2 non-goal above.
- `PERF_SAMPLE_REGS_USER` / `PERF_SAMPLE_STACK_USER` — userspace stack frames captured via raw register dumps; we already do callchain via BPF, this is redundant.
- `HEADER_TRACING_DATA` — we don't emit tracepoint records.

### Data flow

```
                 ┌──────────────────┐
                 │ perfagent.Agent  │
                 │  (Start)         │
                 └────────┬─────────┘
                          │ resolveTarget(): hostPID, labels
                          ↓
        ┌─────────────────┴─────────────────────┐
        │                                       │
   ┌────▼────────────┐               ┌──────────▼──────────────┐
   │ profile.        │               │ dwarfagent.             │
   │ NewProfiler     │               │ NewProfilerWithMode     │
   │ (FP path)       │               │ (DWARF hybrid)          │
   └────┬────────────┘               └──────────┬──────────────┘
        │                                       │
        │ both open:                            │
        │   pprof.ProfileBuilder                │
        │   perfdata.Writer (when --perf-data-output set)
        │                                       │
        └───────────────┬───────────────────────┘
                        │
                        │ each BPF sample fans out to both:
                        ↓
              ┌─────────┴──────────┐
              │ pprofBuilder.Add() │
              │ perfdataWriter.Add()│
              └─────────┬──────────┘
                        │ on Stop():
                        │   pprof → gzipped pb.gz
                        │   perfdata → flushed perf.data
                        │
                        ↓
         file output: app.pb.gz, app.perf.data
```

Sample fanout is a function call, not a goroutine — the perf.data writer accumulates records in memory and flushes on close. Memory cost is bounded by sample rate × duration × bytes-per-record (~64 bytes for a typical sample with 16-frame callchain). For a 60s @ 99Hz capture: ~6000 samples × 64 bytes ≈ 400 KB. Negligible.

### Integration into perfagent.Agent

The library option:

```go
// WithPerfDataOutput writes a perf.data file alongside any pprof output.
// Compatible with --profile (--pid or -a). Off-CPU and PMU modes ignore
// this option; only CPU samples are written to perf.data.
func WithPerfDataOutput(path string) Option
```

The CLI flag:

```go
flagPerfDataOutput = flag.String("perf-data-output", "",
    "Write a perf.data file (kernel format) alongside the pprof output. "+
    "Consumable by perf script, perf report, create_llvm_prof (AutoFDO PGO), FlameGraph, hotspot, etc.")
```

`perfagent.Agent.Start` passes the path through to whichever CPU profiler is constructed; the profiler opens the writer in its constructor and arranges the fan-out. NSpid translation, k8s labels, `--inject-python` — all unchanged. perf.data records carry the **host PID** so consumers that join against `/proc/<host-pid>/maps` work correctly.

### Documentation

`docs/perf-data-output.md` — new file. Sections:

1. **What this is** (one paragraph). perf-agent emits the same data in a different format. Link to the kernel format spec.
2. **What perf-agent emits**. The record set table above, simplified. Note about software vs hardware event auto-detect.
3. **Common consumer recipes** (ordered by likely use):
   - `perf script` / `perf report` — viewing the capture directly.
   - FlameGraph — `perf script | stackcollapse-perf.pl | flamegraph.pl`.
   - AutoFDO PGO (Rust): `create_llvm_prof` install + invocation, then `RUSTFLAGS="-C profile-use=..."`.
   - AutoFDO PGO (C++): same converter, then `clang -fprofile-sample-use=...`.
   - hotspot — GUI flame graph viewer; just open the `.perf.data`.
4. **Capture protocol guidance**. Representativeness (real workload, not test suites), duration (60s+ for AutoFDO accuracy), sample rate suggestions.
5. **What about Go PGO?** One paragraph: Go consumes pprof natively (`go build -pgo=app.pb.gz`); use `--profile-output` directly, no perf.data needed.
6. **Troubleshooting**:
   - `perf script: invalid file format` → check magic bytes; rerun with the latest perf-agent.
   - `create_llvm_prof: build-id mismatch` → binary was rebuilt between capture and conversion.
   - Empty / sparse profile → workload didn't run during capture window; check sample count with `perf script | wc -l`.
   - `cycles event not supported` log → expected in cloud VMs; software cpu-clock fallback is in effect.

## Data flow / lifecycle

For a single capture with `--perf-data-output app.perf.data --profile-output app.pb.gz`:

1. **Agent.Start**: `internal/perfevent.ProbeHardwareCycles()` runs; result cached.
2. **Profile constructor**: opens both `pprof.ProfileBuilder` and `perfdata.Writer`. The writer writes the file header, attr, attr_id table to disk immediately (small, fixed-size); records section is appended-as-we-go.
3. **For each BPF sample**: profile aggregator calls `pprofBuilder.AddSample()` and (when configured) `perfdataWriter.AddSample()`. The latter resolves the PID's mapping table from `procmap.Resolver` (cached), emits a `PERF_RECORD_MMAP2` record per (pid, mapping) tuple it hasn't seen yet, then a `PERF_RECORD_SAMPLE` for this sample.
4. **For each new PID**: `procmap.Resolver` returns the mappings; the writer also emits a `PERF_RECORD_COMM` record (one per pid).
5. **Once per second**: `perfdataWriter.AddFinishedRound()` writes a `PERF_RECORD_FINISHED_ROUND` marker.
6. **On Agent.Stop**: pprof builder flushes via existing path; perf.data writer:
   a. Records the data section's end offset.
   b. Emits feature sections (HEADER_BUILD_ID — collected from `procmap.Resolver`, HOSTNAME, OSRELEASE, NRCPUS).
   c. Patches the file header's data section size + feature bitmap.
   d. Closes the file.

## Error handling

| Condition | Behaviour |
|---|---|
| `--perf-data-output` set but file open fails (permissions, ENOSPC) | Hard error before profiling starts; user fixes path. |
| Hardware cycles probe fails | Silent fallback to software cpu-clock. INFO-level log. |
| Sample arrives for a PID we have no mapping for | Emit the SAMPLE record anyway with empty MMAP2; log at debug. Consumers handle missing mappings (frame stays unsymbolized). |
| BPF ringbuf overrun (samples dropped) | Counter only; don't emit synthetic LOST records in v1. v2 may emit `PERF_RECORD_LOST`. |
| Disk full mid-capture | Detected at next write; surface error, abort cleanly, exit non-zero. The partial perf.data may still be parseable but truncated. |
| Format encoding bug (e.g. wrong record size) | Caught by unit tests against fixtures; CI gate via `perf script` parses-without-error integration test. |

## Testing strategy

- **Unit tests** (`internal/perfdata/perfdata_test.go`):
  - Encode-and-decode round trip: build a known record stream in memory, write to a buffer, parse it back, assert equivalence.
  - Byte-for-byte fixture tests: known input → known output bytes; catches format regressions.
  - Header / feature section invariants: bitmap matches the features actually written; offsets self-consistent.
- **Integration test** (`test/integration_test.go`):
  - Capture a perf.data on the existing `cpu_bound` test workload.
  - Assert `perf script <output>` parses without error and produces non-empty output.
  - Assert sample count is ≥ `degenerateSampleFloor` (existing threshold; reuse the same flake-tolerance pattern).
  - `perf` is already installed in the CI image (we use it nowhere else, but it's available).
- **Manual validation** (deferred from CI):
  - End-to-end `create_llvm_prof` round-trip on a Rust binary; document in `docs/perf-data-output.md`.
  - End-to-end FlameGraph render.
  - hotspot GUI parse.

## Open questions

1. **Should the writer be a goroutine or a synchronous call?** Synchronous is simpler and the in-memory accumulation cost is small (~1MB for a long capture). Goroutine adds backpressure-handling complexity for no obvious win. Decision: **synchronous in v1**.

2. **How do we encode kernel samples (PID 0)?** They land with `PERF_RECORD_MMAP2` showing `[kernel]`. v1 emits them faithfully; consumers that don't want kernel samples filter them on their side. Same as `perf record` default.

3. **`PERF_SAMPLE_CPU` — include or skip?** Yes, include. Cheap (4 bytes), helps consumers that want per-CPU breakdowns. Matches what `perf record` emits by default.

## v2 follow-ups (out of scope here)

- **LBR / `PERF_SAMPLE_BRANCH_STACK`.** Bare-metal-only. AutoFDO accuracy bonus on Intel SKX+/AMD Zen 3+. Separate spec; touches BPF program structure.
- **`--pgo-event=cycles|cpu-clock` explicit override flag.** Today's auto-detect handles 95% of cases; the override is for the 5% who want to force a specific event for cross-environment consistency.
- **`perf-agent verify-perf-data <file> <binary>` subcommand.** Build-id presence check, sample density sanity check, expected record types check. Useful for users debugging "why does create_llvm_prof reject my file."
- **`PERF_RECORD_LOST` records on BPF ringbuf overruns.** Today the loss counter exists but isn't surfaced in perf.data. Trivial to add later.
- **Hardware tracing (Intel PT, ARM CoreSight) ingestion.** Different BPF / kernel surface; out of scope.
