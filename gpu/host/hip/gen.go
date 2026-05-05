package hip

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=hip hiplaunch ../../../bpf/gpu_hip_launch.bpf.c -- -I../../../bpf
