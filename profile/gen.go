package profile

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -go-package=profile Perf ../bpf/perf.bpf.c
