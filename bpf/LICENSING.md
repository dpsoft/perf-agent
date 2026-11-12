# Licensing of the eBPF programs

The rest of perf-agent is Apache-2.0. The eBPF sources in this directory are
licensed differently:

- **The eBPF programs and their headers** (`*.bpf.c`, `unwind_common.h`,
  `unwind_record.h`, `interp/python/*`) are licensed **GPL-2.0-only**. The full
  text is in `LICENSE`.
- **`vmlinux.h` and `vmlinux_arm64.h`** are generated from Linux kernel BTF
  and carry the kernel's GPL-2.0 type definitions.
- **`libbpf/bpf/*.h`** are libbpf 1.3.0 headers under
  `LGPL-2.1 OR BSD-2-Clause`. See `libbpf/LICENSE*`.

Each program declares `char LICENSE[] SEC("license") = "GPL"`. The kernel
requires a GPL-compatible declaration before a program may call GPL-only
helpers. The compiled objects (`*_bpfel.o`, embedded through the generated
`*_bpfel.go` files) are loaded into the kernel as separate programs. The Go
code that loads them stays Apache-2.0. This is the same arrangement used by
the OpenTelemetry eBPF profiler, Pyroscope and Parca.

The license is declared here rather than in a header in each file because the
committed objects record source lines in their BTF line info. Adding a header
to a source file would change every object built from it.

## Attribution

`perf.bpf.c` is adapted from the eBPF profiler in Grafana Pyroscope
(`ebpf/bpf/profile.bpf.c`, `stacks.h` and `profile.bpf.h`, Copyright Grafana
Labs). The `sample_key` and `pid_config` layouts and the stack-collection body
follow that code. This file has been modified.
