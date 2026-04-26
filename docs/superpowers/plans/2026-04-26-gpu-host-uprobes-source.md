# GPU Host Uprobes Source Implementation Plan

This plan is superseded.

The original `CUDA`-first `gpu/host/uprobes` milestone was the wrong next step after review.

Use [2026-04-26-gpu-linux-observability-core.md](/home/diego/github/perf-agent/.worktrees/gpu-profiling-spec/docs/superpowers/plans/2026-04-26-gpu-linux-observability-core.md:1) as the active plan instead.

Reason for the pivot:

- the plan review showed that `uprobes`-first was overcommitting to a proprietary runtime path too early
- the Linux-first investigation reframed the base product correctly as an `eBPF`-centered, DRM-aware event timeline profiler
- the next milestone should establish truthful cross-vendor lifecycle telemetry before deeper runtime or vendor-specific correlation paths
