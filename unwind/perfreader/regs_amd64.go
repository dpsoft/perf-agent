//go:build linux && amd64

package perfreader

// Register indices from linux/perf_regs.h (arch/x86/include/uapi/asm/perf_regs.h).
// These are the positions in the perf_event sample's regs[] array and must
// match the order the kernel uses when populating PERF_SAMPLE_REGS_USER.
const (
	PerfRegX86AX    = 0
	PerfRegX86BX    = 1
	PerfRegX86CX    = 2
	PerfRegX86DX    = 3
	PerfRegX86SI    = 4
	PerfRegX86DI    = 5
	PerfRegX86BP    = 6
	PerfRegX86SP    = 7
	PerfRegX86IP    = 8
	PerfRegX86Flags = 9
	PerfRegX86CS    = 10
	PerfRegX86SS    = 11
	PerfRegX86DS    = 12
	PerfRegX86ES    = 13
	PerfRegX86FS    = 14
	PerfRegX86GS    = 15
	PerfRegX86R8    = 16
	PerfRegX86R9    = 17
	PerfRegX86R10   = 18
	PerfRegX86R11   = 19
	PerfRegX86R12   = 20
	PerfRegX86R13   = 21
	PerfRegX86R14   = 22
	PerfRegX86R15   = 23
)

// SampleRegsUser is the bitmask of registers we ask the kernel to capture
// per sample. Includes the minimum needed for DWARF CFI unwinding on x86_64
// (IP, SP, BP) plus the general-purpose registers libunwind needs to restore
// frame state (AX..DI, R8..R15). Flags/segment registers are excluded —
// DWARF never restores them and they'd just waste ring-buffer bandwidth.
const SampleRegsUser = uint64(0) |
	(1 << PerfRegX86AX) |
	(1 << PerfRegX86BX) |
	(1 << PerfRegX86CX) |
	(1 << PerfRegX86DX) |
	(1 << PerfRegX86SI) |
	(1 << PerfRegX86DI) |
	(1 << PerfRegX86BP) |
	(1 << PerfRegX86SP) |
	(1 << PerfRegX86IP) |
	(1 << PerfRegX86R8) |
	(1 << PerfRegX86R9) |
	(1 << PerfRegX86R10) |
	(1 << PerfRegX86R11) |
	(1 << PerfRegX86R12) |
	(1 << PerfRegX86R13) |
	(1 << PerfRegX86R14) |
	(1 << PerfRegX86R15)

// RegIP returns the instruction pointer from a captured regs slice.
// Index into regs[] follows the bit order in SampleRegsUser — the kernel
// packs enabled registers densely, skipping bits that weren't requested.
// We compute the index at package init from SampleRegsUser.
func RegIP(regs []uint64) uint64 { return regs[regIndexIP] }
func RegSP(regs []uint64) uint64 { return regs[regIndexSP] }
func RegBP(regs []uint64) uint64 { return regs[regIndexBP] }

var (
	regIndexIP = sparseBitIndex(SampleRegsUser, PerfRegX86IP)
	regIndexSP = sparseBitIndex(SampleRegsUser, PerfRegX86SP)
	regIndexBP = sparseBitIndex(SampleRegsUser, PerfRegX86BP)
)

// sparseBitIndex returns the position of bit `which` inside a mask where
// only set bits are counted. Used to translate a logical register id
// (e.g. PerfRegX86IP = 8) into its slot in the dense regs[] array the
// kernel emits.
func sparseBitIndex(mask uint64, which int) int {
	count := 0
	for i := 0; i < which; i++ {
		if mask&(1<<i) != 0 {
			count++
		}
	}
	return count
}
