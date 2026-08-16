package bpfstack

import (
	"reflect"
	"testing"
)

// TestSplitUserKernelIPs_KernelLeakAtTop covers the canonical case:
// the BPF user-stack walker captured a kernel IP at index 0
// (process was in syscall context at sample time) and user frames
// follow. Verifies kernel IP is split out, order preserved.
func TestSplitUserKernelIPs_KernelLeakAtTop(t *testing.T) {
	ips := []uint64{
		0xffffffffa7f0cfe5, // kernel
		0x55fa00001234,     // user
		0x55fa00005678,     // user
	}
	user, kernel := SplitUserKernelIPs(ips)
	wantUser := []uint64{0x55fa00001234, 0x55fa00005678}
	wantKernel := []uint64{0xffffffffa7f0cfe5}
	if !reflect.DeepEqual(user, wantUser) {
		t.Errorf("user = %x, want %x", user, wantUser)
	}
	if !reflect.DeepEqual(kernel, wantKernel) {
		t.Errorf("kernel = %x, want %x", kernel, wantKernel)
	}
}

// TestSplitUserKernelIPs_NoLeak: a clean user stack should pass
// through unchanged with an empty kernel partition.
func TestSplitUserKernelIPs_NoLeak(t *testing.T) {
	ips := []uint64{0x55fa00001234, 0x55fa00005678, 0x7fa9c0000abc}
	user, kernel := SplitUserKernelIPs(ips)
	if !reflect.DeepEqual(user, ips) {
		t.Errorf("user = %x, want unchanged %x", user, ips)
	}
	if len(kernel) != 0 {
		t.Errorf("kernel = %x, want empty", kernel)
	}
}

// TestSplitUserKernelIPs_AllKernel: pathological case — every IP
// is kernel. Should yield empty user, full kernel.
func TestSplitUserKernelIPs_AllKernel(t *testing.T) {
	ips := []uint64{0xffffffffa7f0cfe5, 0xffffffffa9144ee2}
	user, kernel := SplitUserKernelIPs(ips)
	if len(user) != 0 {
		t.Errorf("user = %x, want empty", user)
	}
	if !reflect.DeepEqual(kernel, ips) {
		t.Errorf("kernel = %x, want %x", kernel, ips)
	}
}

// TestSplitUserKernelIPs_BoundaryExact: an IP exactly at the
// kernel threshold counts as kernel.
func TestSplitUserKernelIPs_BoundaryExact(t *testing.T) {
	ips := []uint64{kernelAddrThreshold - 1, kernelAddrThreshold}
	user, kernel := SplitUserKernelIPs(ips)
	if !reflect.DeepEqual(user, []uint64{kernelAddrThreshold - 1}) {
		t.Errorf("user = %x, want [threshold-1]", user)
	}
	if !reflect.DeepEqual(kernel, []uint64{kernelAddrThreshold}) {
		t.Errorf("kernel = %x, want [threshold]", kernel)
	}
}
