//go:build arm64

package ptraceop

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetupCallFrame_arm64(t *testing.T) {
	var orig unix.PtraceRegs
	orig.Pc = 0xdeadbeef00
	orig.Sp = 0x7ffffff00000
	orig.Regs[0] = 0x11
	orig.Regs[1] = 0x22
	orig.Regs[30] = 0xCCCC // some other LR
	const fnAddr = 0x12340000
	const arg1 = 0xCAFEBABE
	const payloadAddr = 0x7ffffff00100 // 16-byte aligned

	frame, err := setupCallFrame(orig, fnAddr, arg1, payloadAddr)
	if err != nil {
		t.Fatalf("setupCallFrame error: %v", err)
	}
	if frame.Pc != fnAddr {
		t.Errorf("PC = 0x%x, want 0x%x", frame.Pc, fnAddr)
	}
	if frame.Regs[0] != arg1 {
		t.Errorf("X0 = 0x%x, want 0x%x", frame.Regs[0], arg1)
	}
	if frame.Regs[30] != 0 {
		t.Errorf("LR (X30) = 0x%x, want 0 (sentinel)", frame.Regs[30])
	}
	if frame.Sp != payloadAddr-16 {
		t.Errorf("SP = 0x%x, want 0x%x", frame.Sp, payloadAddr-16)
	}
	if frame.Sp%16 != 0 {
		t.Errorf("SP not 16-aligned: 0x%x", frame.Sp)
	}
	if frame.Regs[1] != orig.Regs[1] {
		t.Errorf("X1 clobbered: 0x%x, want 0x%x", frame.Regs[1], orig.Regs[1])
	}
}

func TestSetupCallFrame_arm64_BadAlignment(t *testing.T) {
	var orig unix.PtraceRegs
	orig.Sp = 0x7ffffff00000
	const payloadAddr = 0x7ffffff00108 // NOT 16-aligned
	_, err := setupCallFrame(orig, 0x1000, 0, payloadAddr)
	if err == nil {
		t.Fatal("expected alignment error; got nil")
	}
}

func TestExtractReturn_arm64(t *testing.T) {
	var post unix.PtraceRegs
	post.Regs[0] = 0xCAFEF00D
	got := extractReturn(post)
	if got != 0xCAFEF00D {
		t.Errorf("extractReturn = 0x%x, want 0xCAFEF00D", got)
	}
}

func TestStackPointer_arm64(t *testing.T) {
	var r unix.PtraceRegs
	r.Sp = 0xC0DEFACE
	if got := stackPointer(r); got != 0xC0DEFACE {
		t.Errorf("stackPointer = 0x%x, want 0xC0DEFACE", got)
	}
}
