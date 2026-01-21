package profile

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=profile Perf ../bpf/perf.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=profile Perf ../bpf/perf.bpf.c -- -I../bpf
