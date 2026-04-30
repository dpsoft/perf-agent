//go:build arm64

package ptraceop

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setupCallFrame builds a register frame for one remote function call on
// arm64 AAPCS64. The frame:
//   - PC = fnAddr (entry of remote function)
//   - X0 = arg1 (first integer/pointer arg)
//   - X30 (LR) = 0 (sentinel — when function returns via RET, target
//     jumps to 0 → SIGSEGV → ptrace catches it)
//   - SP = payloadAddr - 16, 16-byte aligned (AAPCS64 requires 16-byte SP at
//     all public interfaces)
//
// All other registers are inherited from orig.
func setupCallFrame(orig unix.PtraceRegs, fnAddr, arg1, payloadAddr uint64) (unix.PtraceRegs, error) {
	frame := orig
	frame.Pc = fnAddr
	frame.Regs[0] = arg1
	frame.Regs[30] = 0 // LR (X30) — return address sentinel
	frame.Sp = payloadAddr - 16
	if frame.Sp%16 != 0 {
		return unix.PtraceRegs{}, fmt.Errorf("SP alignment broken: 0x%x %% 16 = %d (want 0)",
			frame.Sp, frame.Sp%16)
	}
	return frame, nil
}

// extractReturn reads the integer return value from a post-call register set.
// On arm64 AAPCS64, integer/pointer returns are in X0.
func extractReturn(post unix.PtraceRegs) uint64 {
	return post.Regs[0]
}

// stackPointer returns the SP register value for arch-generic code.
func stackPointer(r unix.PtraceRegs) uint64 {
	return r.Sp
}

// instructionPointer returns the PC register value for arch-generic code.
func instructionPointer(r unix.PtraceRegs) uint64 {
	return r.Pc
}
