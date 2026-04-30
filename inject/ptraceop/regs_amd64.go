//go:build amd64

package ptraceop

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setupCallFrame builds a register frame for one remote function call on
// amd64 System V. The frame:
//   - RIP = fnAddr (entry of remote function)
//   - RDI = arg1   (first integer/pointer arg)
//   - RSP = payloadAddr - 8, 16-byte aligned post-write of return-addr sentinel
//   - *(RSP) gets a 0 written to it by the caller, serving as the return
//     address; when the function returns, target jumps to 0 → SIGSEGV →
//     ptrace catches it.
//   - Orig_rax = ^uint64(0) (i.e. -1) to disable kernel syscall-restart logic:
//     when ptrace attaches to a target mid-syscall (e.g. nanosleep), the kernel
//     records the syscall number in orig_rax; on PtraceCont the kernel runs
//     syscall-return processing which clobbers RAX based on that number. The
//     pyrasite/py-spy/manhole convention is to set orig_rax = -1 to mark "not
//     in a syscall", which prevents that clobbering and lets our remote call
//     return its actual value.
//
// All other registers are inherited from orig — we only edit what we must.
func setupCallFrame(orig unix.PtraceRegs, fnAddr, arg1, payloadAddr uint64) (unix.PtraceRegs, error) {
	frame := orig
	frame.Rip = fnAddr
	frame.Rdi = arg1
	frame.Orig_rax = ^uint64(0) // -1: suppress kernel syscall-restart on PtraceCont
	// SP layout: ... [return-addr sentinel = 0] [payload string]
	// We choose RSP = payloadAddr - 8 so *(RSP) holds the sentinel.
	// Then ensure 16-byte alignment after the simulated CALL push: in System V,
	// at function entry, RSP must be 8 mod 16 (CALL pushes the return addr
	// making RSP %16 == 8). We achieve that by ensuring (payloadAddr - 8) % 16 == 8,
	// i.e., payloadAddr % 16 == 0. The caller chose payloadAddr aligned to 16,
	// so we're good.
	frame.Rsp = payloadAddr - 8
	if frame.Rsp%16 != 8 {
		return unix.PtraceRegs{}, fmt.Errorf("RSP alignment broken: 0x%x %% 16 = %d (want 8)",
			frame.Rsp, frame.Rsp%16)
	}
	return frame, nil
}

// extractReturn reads the integer return value from a post-call register set.
// On amd64 System V, integer/pointer returns are in RAX.
func extractReturn(post unix.PtraceRegs) uint64 {
	return post.Rax
}

// stackPointer returns the SP register value for arch-generic code.
func stackPointer(r unix.PtraceRegs) uint64 {
	return r.Rsp
}

// instructionPointer returns the IP register value for arch-generic code.
func instructionPointer(r unix.PtraceRegs) uint64 {
	return r.Rip
}
