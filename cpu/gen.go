package cpu

// DO NOT "CLEAN UP" THE INCLUDE FLAGS BELOW. They are dead, and removing them
// costs a day.
//
// `-I../bpf/libbpf -I../bpf/vmlinux/` name two directories that have never
// existed in any branch of this repository. clang ignores a missing -I
// silently, so they have never affected include resolution: `#include
// "vmlinux.h"` resolves relative to bpf/cpu.bpf.c on its own, and
// `<bpf/bpf_helpers.h>` comes from the system include path. This package has no
// working include path of its own and never has. They are residue of vendoring
// the libbpf headers, which #117 proposed as the durable fix for BPF objects
// not being byte-reproducible and which is not the fix.
//
// The obvious tidy-up -- replace both with -I../bpf, what every other package
// passes -- was tried on 2026-09-08 and REVERTED, because it broke CI:
//
//	make generate-container   all 14 objects byte-identical, before and after
//	CI (ubuntu-24.04, clang-18)   cpu_{x86,arm64} differ, 47128 both sides
//
// Same source, same clang version, same flags, and the two environments landed
// on different BTF orderings. That is #117's malloc lottery: the forward
// declarations get their type IDs in the iteration order of a pointer-keyed
// std::map in BTFDebug::endModule, so anything that shifts clang's allocation
// pattern permutes them. The Makefile's generate-container reproduces CI's
// objects for the command lines that are committed TODAY; it is not robust to
// changing one, and there is no way to find that out except by pushing.
//
// So the flags stay. Their only cost is looking like vendoring that was never
// done, which this comment fixes for free. Changing them means regenerating
// against whatever CI produces and giving up local reproduction until the two
// agree again -- for nothing but tidiness.
//
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=cpu cpu ../bpf/cpu.bpf.c -- -I../bpf/libbpf -I../bpf/vmlinux/
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target arm64 -cc clang -cflags "-O2 -Wall -Werror -fpie -Wno-unused-variable -Wno-unused-function" -go-package=cpu cpu ../bpf/cpu.bpf.c -- -I../bpf/libbpf -I../bpf/vmlinux/
