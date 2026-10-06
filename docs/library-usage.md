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

---
