# Library usage

`perf-agent` is also a Go library via the `perfagent` package:

```go
agent, _ := perfagent.New(
    perfagent.WithPID(12345),
    perfagent.WithCPUProfile("profile.pb.gz"),
    perfagent.WithPMU(),
)
defer agent.Close()
agent.Start(ctx); time.Sleep(10*time.Second); agent.Stop(ctx)
```

See the [`perfagent` package docs](../perfagent/) for in-memory output, custom label enrichers, and metrics exporters.

GPU sensors are an independent finite collector, usable without `Agent.Start`:

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
collector := gputelemetry.Collector{
    Source: gputelemetry.NewNVML(),
    Interval: time.Second,
    Exporters: []metrics.Exporter{
        metrics.NewConsoleExporter(false),
        &metrics.GPUJSONLExporter{Writer: output},
    },
}
if err := collector.Run(ctx); err != nil {
    return err
}
```

Import `github.com/dpsoft/perf-agent/gputelemetry` and
`github.com/dpsoft/perf-agent/metrics`. Set `UUIDs` to select physical devices.
`output` is an `io.Writer` owned and closed by the caller. A custom exporter
receives an optional `MetricsSnapshot.GPU` independently of `Processes` and
`SystemWide`; CPU/PMU snapshots leave it nil. `Run` owns the source until it
returns, exports serially and emits final readings after context cancellation.
Do not call it concurrently on the same source or share an unsynchronized
exporter with another collector.

---

### Continuous telemetry and OTLP ownership

`gputelemetry.Collector.RunContinuous(ctx, time.Minute)` collects consecutive
summary windows until cancellation, reopening NVML between windows. The same
exporters persist across windows. Local output failures terminate collection;
source initialization failures back off and retry.

The optional `metrics/otlp.New(ctx, otlp.Options{...})` adapter implements
`metrics.Exporter` and exports GPU samples asynchronously. Construct it only after
explicit application opt-in. The caller owns its lifecycle: stop collection first,
then call `Shutdown` with a fresh bounded context (the CLI uses five seconds),
and inspect `Stats`. Do not tie the final flush to an already-cancelled capture
context or close a shared exporter from inside the collector. `Shutdown` returning
a deadline error means the worker was cancelled; ownership is released when its
in-flight HTTP request exits. Device enumeration and requests have separate
lifetimes. See the [CLI contract](usage.md#continuous-gpu-telemetry-and-otlp) for
bounded buffering, availability, cooperative locks and intentional instance IDs.
