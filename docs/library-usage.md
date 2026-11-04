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
