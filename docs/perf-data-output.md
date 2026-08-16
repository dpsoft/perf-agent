# perf.data output

`perf-agent --profile --perf-data-output app.perf.data ...` writes a
[Linux kernel-format `perf.data`](https://github.com/torvalds/linux/blob/master/tools/perf/Documentation/perf.data-file-format.txt)
file alongside the existing pprof output. The same capture serializes into
two formats; you pick whichever consumer you want.

## What perf-agent emits

- `PERF_RECORD_COMM`, `PERF_RECORD_MMAP2` (with build-id), `PERF_RECORD_SAMPLE`,
  `PERF_RECORD_FINISHED_ROUND` records in the data section.
- Feature sections: `HEADER_BUILD_ID`, `HEADER_HOSTNAME`, `HEADER_OSRELEASE`,
  `HEADER_NRCPUS`.
- Sample event auto-detected: `PERF_TYPE_HARDWARE / PERF_COUNT_HW_CPU_CYCLES`
  on bare metal where the PMU is exposed; falls back to
  `PERF_TYPE_SOFTWARE / PERF_COUNT_SW_CPU_CLOCK` in cloud VMs / k8s pods
  without PMU passthrough. perf-agent logs which event was chosen at INFO.

## Capture

```bash
perf-agent --profile --pid <PID> --duration 60s --perf-data-output app.perf.data
```

Combine with `--profile-output app.pb.gz` to get pprof and perf.data from the
same capture. Use `-a/--all` for system-wide instead of `--pid`.

For AutoFDO-style training runs, longer durations (60s+) under a representative
workload give better profile accuracy than short, idle captures.

## Consumer recipes

### `perf script` / `perf report`

```bash
perf script -i app.perf.data
perf report -i app.perf.data
```

Sanity-check that samples landed and stacks resolved.

### FlameGraph

```bash
perf script -i app.perf.data | \
  ./stackcollapse-perf.pl | \
  ./flamegraph.pl > flame.svg
```

`stackcollapse-perf.pl` and `flamegraph.pl` come from
<https://github.com/brendangregg/FlameGraph>.

### Rust AutoFDO PGO

```bash
# 1. Build with debug info so create_llvm_prof can resolve symbols
RUSTFLAGS="-C debuginfo=2" cargo build --release

# 2. Capture a representative profile with perf-agent
perf-agent --profile --pid $(pgrep my-app) --duration 60s \
           --perf-data-output train.perf.data

# 3. Convert to LLVM .profdata via autofdo's create_llvm_prof
#    (https://github.com/google/autofdo#install). --use_lbr=false is
#    required: perf-agent samples cycles without branch records, and
#    create_llvm_prof's default LBR mode produces an empty profile.
create_llvm_prof --binary=./target/release/my-app \
                 --profile=train.perf.data \
                 --out=train.prof \
                 --use_lbr=false

# 4. Recompile with PGO. Stable rustc has no high-level AutoFDO flag
#    (`-C profile-use` is for instrumented PGO and rejects sample
#    profiles), so plumb the profile through to LLVM directly.
RUSTFLAGS="-Cllvm-args=-sample-profile-file=$(pwd)/train.prof" \
    cargo build --release
```

### C++ AutoFDO PGO

Same `create_llvm_prof` step, then:

```bash
clang++ -fprofile-sample-use=train.prof -O2 ...
```

### hotspot (GUI flame-graph viewer)

`hotspot --executable ./target/release/my-app app.perf.data`

## What about Go?

Go consumes pprof natively for PGO — you don't need perf.data:

```bash
perf-agent --profile --pid <PID> --duration 60s --profile-output train.pprof
go build -pgo=train.pprof ./...
```

`--perf-data-output` is for tools that don't speak pprof.

## Troubleshooting

- **`perf script: invalid file format`** — magic bytes don't match. Rerun with
  the latest perf-agent; verify the file isn't truncated (capture exited early).
- **`create_llvm_prof: build-id mismatch`** — the binary was rebuilt between
  capture and conversion. Re-run with the binary that produced the capture, or
  recapture against the new binary.
- **Empty / sparse profile** — workload didn't run during the capture window.
  Check sample count: `perf script -i app.perf.data | wc -l`. Below ~50 samples
  is unreliable for AutoFDO; capture longer or under heavier load.
- **`cycles event not supported` log message** — expected in cloud VMs and
  many k8s pods. Software cpu-clock is in effect; AutoFDO still works, just
  with slightly less accurate cycle attribution.
- **Missing debug info** in `perf script` output — rebuild with
  `-C debuginfo=2` (Rust) or `-g` (C++).

## What's not in this output (yet)

- LBR / branch records (bare-metal-only feature; v2 spec).
- `PERF_RECORD_FORK` / `PERF_RECORD_EXIT` lifecycle records.
- Hardware tracing data (Intel PT, ARM CoreSight).
