package profile

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile perf ../bpf/perf.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=profile perf ../bpf/perf.bpf.c -- -I../bpf

// perf_dwarf is x86_64-only for now. The program reads user-space registers
// from the perf event context, which requires arch-specific pt_regs fields.
// bpf/vmlinux.h is an x86_64 BTF dump; arm64 BPF support lands in a later
// stage with its own vmlinux_arm64.h + separate source file. The userspace
// ehcompile package already handles both arches for CFI parsing — only the
// BPF walker is scoped narrow.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile perf_dwarf ../bpf/perf_dwarf.bpf.c -- -I../bpf
