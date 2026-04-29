//go:build amd64

package ptraceop

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetupCallFrame_amd64(t *testing.T) {
	orig := unix.PtraceRegs{
		Rip: 0xdeadbeef00,
		Rdi: 0x11,
		Rsi: 0x22,
		Rsp: 0x7ffffff00000,
		Rax: 0xaaaa,
	}
	const fnAddr = 0x12340000
	const arg1 = 0xCAFEBABE
	const payloadAddr = 0x7ffffff00100 // 16-byte aligned

	frame, err := setupCallFrame(orig, fnAddr, arg1, payloadAddr)
	if err != nil {
		t.Fatalf("setupCallFrame error: %v", err)
	}
	if frame.Rip != fnAddr {
		t.Errorf("RIP = 0x%x, want 0x%x", frame.Rip, fnAddr)
	}
	if frame.Rdi != arg1 {
		t.Errorf("RDI = 0x%x, want 0x%x", frame.Rdi, arg1)
	}
	if frame.Rsp != payloadAddr-8 {
		t.Errorf("RSP = 0x%x, want 0x%x", frame.Rsp, payloadAddr-8)
	}
	if frame.Rsp%16 != 8 {
		t.Errorf("RSP alignment broken: 0x%x %% 16 = %d", frame.Rsp, frame.Rsp%16)
	}
	// Other registers should be inherited from orig.
	if frame.Rsi != orig.Rsi {
		t.Errorf("RSI clobbered: got 0x%x, want 0x%x", frame.Rsi, orig.Rsi)
	}
	if frame.Rax != orig.Rax {
		t.Errorf("RAX clobbered: got 0x%x, want 0x%x", frame.Rax, orig.Rax)
	}
}

func TestSetupCallFrame_amd64_BadAlignment(t *testing.T) {
	orig := unix.PtraceRegs{Rsp: 0x7ffffff00000}
	const payloadAddr = 0x7ffffff00104 // NOT 16-byte aligned
	_, err := setupCallFrame(orig, 0x1000, 0, payloadAddr)
	if err == nil {
		t.Fatal("expected alignment error; got nil")
	}
}

func TestExtractReturn_amd64(t *testing.T) {
	post := unix.PtraceRegs{Rax: 0xCAFEF00D}
	got := extractReturn(post)
	if got != 0xCAFEF00D {
		t.Errorf("extractReturn = 0x%x, want 0xCAFEF00D", got)
	}
}

func TestStackPointer_amd64(t *testing.T) {
	r := unix.PtraceRegs{Rsp: 0xC0DEFACE}
	if got := stackPointer(r); got != 0xC0DEFACE {
		t.Errorf("stackPointer = 0x%x, want 0xC0DEFACE", got)
	}
}
