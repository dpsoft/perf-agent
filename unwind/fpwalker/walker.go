// Package fpwalker unwinds a stack given captured registers and raw stack
// bytes from a PERF_SAMPLE_STACK_USER sample, assuming the target used
// frame pointers.
//
// This is the cheap leg of the --unwind auto hybrid strategy: called when
// the sampled PC is in FP-safe code. For FP-less code (libstd, stripped
// C++) a DWARF-based unwinder takes over.
//
// On x86_64 the frame-pointer convention is: each function prologue does
// `push rbp; mov rbp, rsp`, so at any PC inside the function, [rbp] holds
// the caller's rbp and [rbp+8] holds the return address. On aarch64 the
// layout is the same using x29 (FP) and x30 (LR) saved as a pair at [fp]
// and [fp+8]. The walker is therefore arch-agnostic — callers pass the
// correct register index via the bp argument.
package fpwalker

import (
	"encoding/binary"
)

// MaxFrames bounds the unwind loop. A real stack rarely exceeds ~64 frames;
// beyond that we assume a loop or garbage and stop.
const MaxFrames = 128

// Walk produces a leaf-first list of PCs by following the frame-pointer
// chain through the captured stack bytes.
//
// Parameters:
//   - ip:         the instruction pointer at the time of the sample. Becomes pcs[0].
//   - bp:         the frame pointer (rbp on x86_64, x29 on arm64).
//   - stackAddr:  the target-address-space address of stack[0] (RSP at sample time).
//   - stack:      raw bytes copied by the kernel via PERF_SAMPLE_STACK_USER.
//
// Termination: stops when saved-bp is zero, falls outside the captured
// stack range, or doesn't strictly increase (stack grows down, so walking
// toward callers means increasing addresses).
func Walk(ip, bp uint64, stackAddr uint64, stack []byte) []uint64 {
	pcs := make([]uint64, 0, 16)
	pcs = append(pcs, ip)

	stackEnd := stackAddr + uint64(len(stack))

	for i := 0; i < MaxFrames; i++ {
		// Need 16 bytes at bp: [bp] = saved_bp, [bp+8] = return_addr.
		if bp < stackAddr || bp+16 > stackEnd {
			break
		}
		off := bp - stackAddr
		savedBP := binary.LittleEndian.Uint64(stack[off:])
		retAddr := binary.LittleEndian.Uint64(stack[off+8:])

		pcs = append(pcs, retAddr)

		// Stop on null terminator or non-monotonic chain. savedBP == bp
		// means the walker is stuck on a self-referential frame; savedBP
		// < bp means the chain is going the wrong direction and can't be
		// a valid prior frame.
		if savedBP == 0 || savedBP <= bp {
			break
		}
		bp = savedBP
	}

	return pcs
}
