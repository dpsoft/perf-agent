package offcpu

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=offcpu Offcpu ../bpf/offcpu.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=offcpu Offcpu ../bpf/offcpu.bpf.c -- -I../bpf
