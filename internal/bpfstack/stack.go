// Package bpfstack parses the raw layout produced by
// BPF_MAP_TYPE_STACK_TRACE: a fixed 127-slot buffer of little-endian
// u64 instruction pointers, terminated by a zero slot. Shared between
// profile/ (CPU sampling) and offcpu/ (sched_switch tracking).
package bpfstack

import "encoding/binary"

// MaxFrames is the depth of a BPF stackmap entry. Matches the BPF
// PERF_MAX_STACK_DEPTH macro in bpf/perf.bpf.c and bpf/offcpu.bpf.c.
const MaxFrames = 127

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
