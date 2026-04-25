//go:build linux && arm64

package perfreader

// Register indices from linux/perf_regs.h (arch/arm64/include/uapi/asm/perf_regs.h).
const (
	PerfRegARM64X0  = 0
	PerfRegARM64X1  = 1
	PerfRegARM64X2  = 2
	PerfRegARM64X3  = 3
	PerfRegARM64X4  = 4
	PerfRegARM64X5  = 5
	PerfRegARM64X6  = 6
	PerfRegARM64X7  = 7
	PerfRegARM64X8  = 8
	PerfRegARM64X9  = 9
	PerfRegARM64X10 = 10
	PerfRegARM64X11 = 11
	PerfRegARM64X12 = 12
	PerfRegARM64X13 = 13
	PerfRegARM64X14 = 14
	PerfRegARM64X15 = 15
	PerfRegARM64X16 = 16
	PerfRegARM64X17 = 17
	PerfRegARM64X18 = 18
	PerfRegARM64X19 = 19
	PerfRegARM64X20 = 20
	PerfRegARM64X21 = 21
	PerfRegARM64X22 = 22
	PerfRegARM64X23 = 23
	PerfRegARM64X24 = 24
	PerfRegARM64X25 = 25
	PerfRegARM64X26 = 26
	PerfRegARM64X27 = 27
	PerfRegARM64X28 = 28
	PerfRegARM64X29 = 29 // frame pointer (FP) per AAPCS
	PerfRegARM64LR  = 30 // x30, link register
	PerfRegARM64SP  = 31
	PerfRegARM64PC  = 32
)

// SampleRegsUser captures X0..X30 (all GPRs including LR), SP, and PC.
// DWARF CFI on arm64 may reference any of them for frame restoration.
const SampleRegsUser = uint64(0) |
	(1 << PerfRegARM64X0) | (1 << PerfRegARM64X1) | (1 << PerfRegARM64X2) |
	(1 << PerfRegARM64X3) | (1 << PerfRegARM64X4) | (1 << PerfRegARM64X5) |
	(1 << PerfRegARM64X6) | (1 << PerfRegARM64X7) | (1 << PerfRegARM64X8) |
	(1 << PerfRegARM64X9) | (1 << PerfRegARM64X10) | (1 << PerfRegARM64X11) |
	(1 << PerfRegARM64X12) | (1 << PerfRegARM64X13) | (1 << PerfRegARM64X14) |
	(1 << PerfRegARM64X15) | (1 << PerfRegARM64X16) | (1 << PerfRegARM64X17) |
	(1 << PerfRegARM64X18) | (1 << PerfRegARM64X19) | (1 << PerfRegARM64X20) |
	(1 << PerfRegARM64X21) | (1 << PerfRegARM64X22) | (1 << PerfRegARM64X23) |
	(1 << PerfRegARM64X24) | (1 << PerfRegARM64X25) | (1 << PerfRegARM64X26) |
	(1 << PerfRegARM64X27) | (1 << PerfRegARM64X28) | (1 << PerfRegARM64X29) |
	(1 << PerfRegARM64LR) | (1 << PerfRegARM64SP) | (1 << PerfRegARM64PC)

// RegIP returns the instruction pointer (PC on arm64).
func RegIP(regs []uint64) uint64 { return regs[regIndexIP] }
func RegSP(regs []uint64) uint64 { return regs[regIndexSP] }
func RegBP(regs []uint64) uint64 { return regs[regIndexBP] } // X29 / FP

var (
	regIndexIP = sparseBitIndex(SampleRegsUser, PerfRegARM64PC)
	regIndexSP = sparseBitIndex(SampleRegsUser, PerfRegARM64SP)
	regIndexBP = sparseBitIndex(SampleRegsUser, PerfRegARM64X29)
)

func sparseBitIndex(mask uint64, which int) int {
	count := 0
	for i := 0; i < which; i++ {
		if mask&(1<<i) != 0 {
			count++
		}
	}
	return count
}
