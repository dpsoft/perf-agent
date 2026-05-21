// Package bpfstack parses the raw layout produced by
// BPF_MAP_TYPE_STACK_TRACE: a fixed 127-slot buffer of little-endian
// u64 instruction pointers, terminated by a zero slot. Shared between
// profile/ (CPU sampling) and offcpu/ (sched_switch tracking).
package bpfstack

import "encoding/binary"

// MaxFrames is the depth of a BPF stackmap entry. Matches the BPF
// PERF_MAX_STACK_DEPTH macro in bpf/perf.bpf.c and bpf/offcpu.bpf.c.
const MaxFrames = 127

// kernelAddrThreshold is the low end of the x86_64 kernel half of
// the canonical address space. Anything at or above this is a
// kernel address. Same threshold the Linux kernel uses internally
// (the canonical-form rule: bits [63:48] must all match bit 47).
// arm64 uses 0xffff000000000000 by default which is also above
// this; the threshold is conservative for both architectures.
const kernelAddrThreshold uint64 = 0xffff800000000000

// SplitUserKernelIPs partitions a BPF user-stack buffer into
// genuine user IPs and stray kernel IPs. The leak happens when
// the sampled task is in kernel context (syscall, irq, fault) —
// bpf_get_stackid with BPF_F_USER_STACK can include kernel
// addresses in the user-stack buffer. Without this split the
// user symbolizer sees IPs it can't resolve and the kernel
// symbolizer never sees them.
//
// Order is preserved within each output slice so leaf-first stack
// semantics survive the split. Discovered via the self-profile
// scenario (bench-self iteration 2) where 40% of perf-agent's
// "user" CPU was actually kernel-range addresses.
func SplitUserKernelIPs(ips []uint64) (user, kernel []uint64) {
	for _, ip := range ips {
		if ip >= kernelAddrThreshold {
			kernel = append(kernel, ip)
		} else {
			user = append(user, ip)
		}
	}
	return user, kernel
}

// ExtractIPs decodes a BPF stackmap entry into a slice of instruction
// pointers, stopping at the first zero slot. Buffers shorter than
// MaxFrames*8 bytes are processed up to their length; buffers longer
// are truncated at MaxFrames.
func ExtractIPs(stack []byte) []uint64 {
	slots := len(stack) / 8
	if slots > MaxFrames {
		slots = MaxFrames
	}
	ips := make([]uint64, 0, slots)
	for j := 0; j < slots; j++ {
		ip := binary.LittleEndian.Uint64(stack[j*8 : j*8+8])
		if ip == 0 {
			break
		}
		ips = append(ips, ip)
	}
	return ips
}
