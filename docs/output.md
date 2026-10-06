# Output

## Output file naming

Output files are auto-named by process name + timestamp + profile type:

| Mode | Per-PID example | System-wide example |
|------|----------------|---------------------|
| `--profile` | `myapp-202604021430-on-cpu.pb.gz` | `202604021430-on-cpu.pb.gz` |
| `--offcpu` | `myapp-202604021430-off-cpu.pb.gz` | `202604021430-off-cpu.pb.gz` |
| `--pmu-output auto` | `myapp-202604021430-pmu.txt` | `202604021430-pmu.txt` |
| `--flamegraph-output auto` | `myapp-202604021430-on-cpu.html` | `202604021430-on-cpu.html` |

Process name comes from `/proc/<pid>/comm`. Override with `--profile-output` / `--offcpu-output`.

## pprof fidelity

CPU and off-CPU profiles are full-fidelity pprof: every `Mapping` carries the absolute path, GNU build-id, and file offsets; every `Location` is keyed by file offset (not symbol name) so cross-run diffing and sample-PGO converters work. `[kernel]` and `[jit]` sentinels handle the special cases. Tags from `--tag key=value` land as profile-level comments; k8s identity labels (when running in a pod) attach per-sample.

```bash
go tool pprof myapp-202604021430-on-cpu.pb.gz
```

With `--debuginfod-url` configured, pprof comes back fully symbolized —
function names + source `:line` — even when debug info isn't present
locally. See [docs/debuginfod-symbolization.md](debuginfod-symbolization.md).

## PMU output

On-CPU time, runqueue latency, context-switch reasons, hardware counters (cycles, instructions, cache misses), and derived metrics (IPC, cache miss rate).

Example:
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

---
