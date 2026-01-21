# perf-agent

eBPF-based performance monitoring agent for Linux. Supports CPU profiling with stack traces, off-CPU profiling, and PMU hardware counter collection.

## Requirements

- Linux kernel 5.8+ (for BTF and CO-RE support)
- Root privileges or `CAP_SYS_ADMIN` + `CAP_PERFMON` capabilities

## Usage

```bash
# CPU profiling (stack traces + pprof output)
sudo ./perf-agent --profile --pid <PID>

# Off-CPU profiling (blocking/sleep time with stack traces)
sudo ./perf-agent --offcpu --pid <PID>

# Combined on-CPU + off-CPU profiling
sudo ./perf-agent --profile --offcpu --pid <PID>

# PMU only (hardware counters: cycles, instructions, cache misses)
sudo ./perf-agent --pmu --pid <PID>

# System-wide profiling (all processes)
sudo ./perf-agent --profile -a --duration 30s

# All features with tags for metadata
sudo ./perf-agent --profile --offcpu --pmu --pid <PID> --duration 30s \
    --tag env=production \
    --tag version=1.2.3 \
    --tag service=api
```

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--profile` | Enable CPU profiling with stack traces | `false` |
| `--offcpu` | Enable off-CPU profiling with stack traces | `false` |
| `--pmu` | Enable PMU hardware counters | `false` |
| `--pid <PID>` | Target process ID to monitor | - |
| `-a, --all` | System-wide profiling (all processes) | `false` |
| `--per-pid` | Show per-PID breakdown (only with `-a --pmu`) | `false` |
| `--duration` | Collection duration | `10s` |
| `--tag key=value` | Add tag to profile (repeatable) | - |

Either `--pid` or `-a/--all` is required. At least one of `--profile`, `--offcpu`, or `--pmu` must be specified.

## Output

### Profile Mode (`--profile`)

Writes `profile.pb.gz` in pprof format. Shows where CPU time is spent.

```bash
go tool pprof profile.pb.gz
```

### Profile Tags (`--tag`)

Tags are stored as comments in the pprof file. View them with:

```bash
go tool pprof profile.pb.gz
(pprof) comments
env=production
version=1.2.3
service=api
```

### Off-CPU Mode (`--offcpu`)

Writes `offcpu.pb.gz` in pprof format. Shows where time is spent blocked/sleeping (I/O, locks, sleep calls, etc.). Values are in nanoseconds.

```bash
go tool pprof offcpu.pb.gz
```

### PMU Mode (`--pmu`)

Prints to stdout:
- Scheduling latency histogram (min, max, mean, percentiles)
- Hardware counters (cycles, instructions, cache misses)
- Derived metrics (IPC, cache miss rate)

## Building

```bash
go generate ./...
go build
```

## Testing

perf-agent includes a comprehensive test suite with unit tests, integration tests, and multi-language test workloads (Go, Rust, Python).

```bash
# Run all tests (unit + integration)
make test

# Run only unit tests (no root required)
make test-unit

# Run only integration tests (requires root)
sudo make test-integration
```

For detailed testing documentation, see [TESTING.md](TESTING.md).
