# perf-agent

eBPF-based performance monitoring agent for Linux. Supports CPU profiling with stack traces, off-CPU profiling, and PMU hardware counter collection.

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                           USER SPACE (Go)                               │
│                                                                         │
│                           ┌──────────┐                                  │
│                           │ main.go  │                                  │
│                           └────┬─────┘                                  │
│                   ┌────────────┼────────────┐                           │
│                   ▼            ▼            ▼                           │
│  ┌──────────────────┐  ┌──────────────┐  ┌──────────────────┐           │
│  │   CPU Profiler   │  │ PMU Monitor  │  │ Off-CPU Profiler │           │
│  │                  │  │              │  │                  │           │
│  │  profile/        │  │  cpu/        │  │  offcpu/         │           │
│  │  perf_event.go   │  │  ring buffer │  │  sched_switch    │           │
│  │  blazesym        │  │  histograms  │  │  blazesym        │           │
│  └────────┬─────────┘  └──────┬───────┘  └────────┬─────────┘           │
│           │                   │                   │                     │
│           ▼                   ▼                   ▼                     │
│  ┌────────────────────────────────────────────────────────────────┐     │
│  │                    pprof/ (Profile Builder)                    │     │
│  └────────────────────────────────────────────────────────────────┘     │
└───────────┬───────────────────┬───────────────────┬─────────────────────┘
            │                   │                   │
════════════╪═══════════════════╪═══════════════════╪══════════════════════
            │    eBPF Load      │                   │
            ▼                   ▼                   ▼
┌─────────────────────────────────────────────────────────────────────────┐
│                         KERNEL SPACE (eBPF)                             │
│                                                                         │
│  ┌──────────────────┐  ┌──────────────┐  ┌──────────────────┐           │
│  │   perf.bpf.c     │  │  cpu.bpf.c   │  │  offcpu.bpf.c    │           │
│  │                  │  │              │  │                  │           │
│  │  perf_event hook │  │ sched_switch │  │  sched_switch    │           │
│  │  stack capture   │  │ sched_wakeup │  │  off-CPU time    │           │
│  │                  │  │ HW counters  │  │  stack capture   │           │
│  └────────┬─────────┘  └──────┬───────┘  └────────┬─────────┘           │
│           │                   │                   │                     │
│           └───────────────────┼───────────────────┘                     │
│                               ▼                                         │
│                    ┌─────────────────────┐                              │
│                    │     eBPF Maps       │                              │
│                    │                     │                              │
│                    │  • stackmap         │                              │
│                    │  • ring buffer      │                              │
│                    │  • pid_filter       │                              │
│                    │  • hw_counters      │                              │
│                    └─────────────────────┘                              │
└─────────────────────────────────────────────────────────────────────────┘
                                │
                                ▼
                  ┌─────────────────────────────────────────┐
                  │               OUTPUT                    │
                  │                                         │
                  │  *-on-cpu.pb.gz    *-off-cpu.pb.gz      │
                  │  (CPU stacks)      (blocking time)      │
                  │                                         │
                  │  Console / File    (PMU metrics)        │
                  └─────────────────────────────────────────┘
```

## Requirements

- Linux kernel 5.8+ (for BTF and CO-RE support)
- Root privileges or the following capabilities: `CAP_SYS_ADMIN` + `CAP_BPF` + `CAP_PERFMON` + `CAP_SYS_PTRACE` + `CAP_CHECKPOINT_RESTORE`

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
| `--sample-rate` | CPU profiling sample rate in Hz | `99` |
| `--profile-output` | Output path for CPU profile | auto-generated |
| `--offcpu-output` | Output path for off-CPU profile | auto-generated |
| `--pmu-output` | Output path for PMU metrics (`auto` for auto-named) | stdout |
| `--tag key=value` | Add tag to profile (repeatable) | - |

Either `--pid` or `-a/--all` is required. At least one of `--profile`, `--offcpu`, or `--pmu` must be specified.

## Output

### Output File Naming

Output files are auto-named based on process name, timestamp, and profile type:

| Mode | Per-PID example | System-wide example |
|------|----------------|---------------------|
| `--profile` | `myapp-202604021430-on-cpu.pb.gz` | `202604021430-on-cpu.pb.gz` |
| `--offcpu` | `myapp-202604021430-off-cpu.pb.gz` | `202604021430-off-cpu.pb.gz` |
| `--pmu-output auto` | `myapp-202604021430-pmu.txt` | `202604021430-pmu.txt` |

Process name is read from `/proc/<pid>/comm`. Override with `--profile-output` or `--offcpu-output`.

### Profile Mode (`--profile`)

Writes pprof format. Shows where CPU time is spent.

```bash
go tool pprof myapp-202604021430-on-cpu.pb.gz
```

Tags (`--tag key=value`) are stored as comments in the pprof file.

### Off-CPU Mode (`--offcpu`)

Writes pprof format. Shows where time is spent blocked/sleeping (I/O, locks, sleep calls). Values are in nanoseconds.

```bash
go tool pprof myapp-202604021430-off-cpu.pb.gz
```

### PMU Mode (`--pmu`)

Prints to stdout (or to file with `--pmu-output <path>`):

- **On-CPU Time**: Time slice per context switch (min, max, mean, percentiles)
  - Measures how long a process runs on CPU before being switched out
- **Runqueue Latency**: Time waiting for CPU after becoming runnable (min, max, mean, percentiles)
  - Measures scheduling delay: time from `sched_wakeup` to actually running
- **Context Switch Reasons**: Breakdown of why tasks were switched out
  - **Preempted (running)**: Task was running and got preempted by scheduler
  - **Voluntary (sleep/mutex)**: Task voluntarily yielded (sleep, mutex wait)
  - **I/O Wait (D state)**: Task blocked on I/O (uninterruptible sleep)
- **Hardware Counters**: Cycles, instructions, cache misses
- **Derived Metrics**: IPC (instructions per cycle), cache miss rate

Example output:
```
=== PMU Metrics (PID: 84228) ===
Samples: 26358

On-CPU Time (time slice per context switch):
  Min:    0.003 ms
  P50:    0.071 ms
  P99:    9.183 ms

Runqueue Latency (time waiting for CPU):
  Min:    0.001 ms
  P50:    0.012 ms
  P99:    0.850 ms

Context Switch Reasons:
  Preempted (running):     45.2%  (11912 times)
  Voluntary (sleep/mutex): 42.1%  (11095 times)
  I/O Wait (D state):      12.7%  (3351 times)

Hardware Counters:
  IPC (Instr/Cycle):  2.342
  Cache Misses/1K:    0.022
```

## Library Usage

`perf-agent` can be used as a Go library via the `perfagent` package:

```go
package main

import (
    "context"
    "log"
    "time"
    "github.com/dpsoft/perf-agent/perfagent"
)

func main() {
    agent, err := perfagent.New(
        perfagent.WithPID(12345),
        perfagent.WithCPUProfile("profile.pb.gz"),
        perfagent.WithPMU(),
    )
    if err != nil {
        log.Fatal(err)
    }
    defer agent.Close()

    ctx := context.Background()
    agent.Start(ctx)
    time.Sleep(10 * time.Second)
    agent.Stop(ctx)
}
```

### In-Memory Collection

```go
var buf bytes.Buffer
agent, _ := perfagent.New(
    perfagent.WithCPUProfileWriter(&buf), // gzip-compressed pprof
)
// After Stop(), buf contains ready-to-use .pb.gz data
```

### Custom Metrics Export

```go
agent, _ := perfagent.New(
    perfagent.WithPMU(),
    perfagent.WithMetricsExporter(&MyExporter{}),
)
```

See [perfagent package documentation](perfagent/) for all available options.

## Building

Requires Go 1.25+, Clang/LLVM, Linux headers, and [blazesym](https://github.com/libbpf/blazesym) (Rust C library for symbolization).

```bash
make build
```

See [BUILDING.md](BUILDING.md) for detailed setup instructions.

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
