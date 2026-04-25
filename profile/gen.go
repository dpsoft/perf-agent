package profile

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile perf ../bpf/perf.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=profile perf ../bpf/perf.bpf.c -- -I../bpf

// perf_dwarf builds for both arches. The source uses PT_REGS_* macros from
// libbpf's bpf_tracing.h, and bpf2go's -target flag picks the right vmlinux
// header (vmlinux.h on amd64, vmlinux_arm64.h on arm64) via __TARGET_ARCH_*.
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile perf_dwarf ../bpf/perf_dwarf.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=profile perf_dwarf ../bpf/perf_dwarf.bpf.c -- -I../bpf

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile offcpu_dwarf ../bpf/offcpu_dwarf.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=profile offcpu_dwarf ../bpf/offcpu_dwarf.bpf.c -- -I../bpf
