package gpuprobe

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -go-package=gpuprobe gpuusdt ../bpf/gpu_usdt.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -go-package=gpuprobe gpuusdt ../bpf/gpu_usdt.bpf.c -- -I../bpf
