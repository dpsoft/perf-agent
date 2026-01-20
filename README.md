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

# All features
sudo ./perf-agent --profile --offcpu --pmu --pid <PID> --duration 30s
```

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--profile` | Enable CPU profiling with stack traces | `false` |
| `--offcpu` | Enable off-CPU profiling with stack traces | `false` |
| `--pmu` | Enable PMU hardware counters | `false` |
| `--pid` | Target process ID to monitor (required) | - |
| `--duration` | Collection duration | `10s` |

At least one of `--profile`, `--offcpu`, or `--pmu` must be specified.

## Output

### Profile Mode (`--profile`)

Writes `profile.pb.gz` in pprof format. Shows where CPU time is spent.

```bash
go tool pprof profile.pb.gz
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
