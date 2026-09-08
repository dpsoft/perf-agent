package cpu

// The include path is -I../bpf, the same as every other package here.
//
// It used to be `-I../bpf/libbpf -I../bpf/vmlinux/`, and neither directory has
// ever existed in any branch of this repository. clang ignores a missing -I
// silently, so the flags compiled fine and did nothing for as long as they were
// there: `#include "vmlinux.h"` resolves relative to bpf/cpu.bpf.c on its own,
// and `<bpf/bpf_helpers.h>` came from the system include path regardless.
//
// They were the residue of vendoring the libbpf headers, which issue #117
// proposed as the durable fix for BPF objects not being byte-reproducible.
// That is NOT the fix and the investigation settled it: the divergence is
// clang's BTF forward-declaration ordering, which follows the malloc addresses
// of debug-info nodes (a pointer-keyed std::map in BTFDebug::endModule), and
// what actually determines it is the BUILD DIRECTORY STRING. See the
// generate-container target in the Makefile, which reproduces CI's objects
// byte for byte by building at CI's checkout path.
//
// Left in place, dead flags that name a vendoring directory are worse than
// nothing: the next person to read #117 would see them and conclude the
// vendoring had been done.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=cpu cpu ../bpf/cpu.bpf.c -- -I../bpf
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=cpu cpu ../bpf/cpu.bpf.c -- -I../bpf
