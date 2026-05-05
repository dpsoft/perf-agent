package linuxdrm

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=linuxdrm linuxdrm ../../../bpf/gpu_linux_observe.bpf.c -- -I../../../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=linuxdrm linuxdrm ../../../bpf/gpu_linux_observe.bpf.c -- -I../../../bpf
