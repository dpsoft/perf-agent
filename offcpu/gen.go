package offcpu

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package=offcpu Offcpu ../bpf/offcpu.bpf.c
