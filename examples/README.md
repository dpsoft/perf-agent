<!-- examples/README.md -->
# perf-agent examples

End-to-end runnable demonstrations of what perf-agent can do beyond a
quick CPU profile. Each example is a self-contained directory with its
own README, source workload, and driver script.

| Example | What it shows |
|---|---|
| [`rust-pgo/`](rust-pgo/) | Rust AutoFDO PGO. Build → profile → `create_llvm_prof` → rebuild → strip → measure speedup. |
| [`cpp-pgo/`](cpp-pgo/) | C++ AutoFDO PGO. Same shape via clang's `-fprofile-sample-use`. |
| [`flamegraph/`](flamegraph/) | Render a Brendan-Gregg flame graph from a perf-agent capture. |

All three depend on `perf-agent` being built and on PATH with the standard
capability set (`setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`).

## Why these are here

The README describes what perf-agent emits. These examples prove the
workflows end-to-end: prerequisites you actually need to install, scripts
that run unattended, expected output you can compare against. If a workflow
documented in the README doesn't have a runnable example here, treat that
as a documentation bug.
