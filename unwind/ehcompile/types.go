// Package ehcompile parses an ELF file's .eh_frame section and produces
// flat tables of unwind rules suitable for loading into BPF maps.
//
// Arch-neutral output: the same struct shape describes unwind rules for
// x86_64 and arm64. CFAType uses SP / FP abstractions that the interpreter
// maps from the concrete DWARF register numbers per-arch. See arch.go.
package ehcompile

// CFAType names the base register of a CFA rule.
// On x86_64, SP == RSP (reg 7) and FP == RBP (reg 6).
// On arm64,  SP == SP  (reg 31) and FP == x29 (reg 29).
type CFAType uint8

const (
	CFATypeUndefined CFAType = 0
	CFATypeSP        CFAType = 1 // CFA = SP + offset
	CFATypeFP        CFAType = 2 // CFA = FP + offset
)

// FPType describes how the caller's frame pointer is recovered.
type FPType uint8

const (
	FPTypeUndefined FPType = 0 // FP is not tracked / not callee-saved here
	FPTypeOffsetCFA FPType = 1 // saved at [CFA + FPOffset]
	FPTypeSameValue FPType = 2 // caller's FP == current FP (unchanged)
	FPTypeRegister  FPType = 3 // saved in another register (rare; we FALLBACK)
)

// RAType describes how the return address is recovered. On x86_64 this
// is conventionally always `OffsetCFA` with RAOffset == -8, but we emit
// it explicitly to match arm64, where the LR register's save location
// varies per FDE.
type RAType uint8

const (
	RATypeUndefined RAType = 0
	RATypeOffsetCFA RAType = 1 // saved at [CFA + RAOffset]
	RATypeSameValue RAType = 2 // caller's RA is live in the RA register (leaf functions on arm64)
	RATypeRegister  RAType = 3 // saved in another register (rare)
)

// CFIEntry is one row of the flat unwind table. The range
// [PCStart, PCStart + PCEndDelta) shares the same CFA / FP / RA rules.
//
// Layout mirrors bpf/unwind_common.h's `struct cfi_entry` (to be written
// in S2) — keep in sync. Arch-neutral: the same struct serves x86_64
// and arm64 unwinders.
type CFIEntry struct {
	PCStart    uint64  // relative to the binary's load base
	PCEndDelta uint32  // PCEnd - PCStart
	CFAType    CFAType // SP or FP
	FPType     FPType
	CFAOffset  int16
	FPOffset   int16 // valid only when FPType == FPTypeOffsetCFA
	RAOffset   int16 // valid only when RAType == RATypeOffsetCFA
	RAType     RAType
	_          [5]uint8 // pad to 32 bytes
}

// Mode classifies a PC range for the hybrid unwinder.
type Mode uint8

const (
	ModeFPSafe   Mode = 0 // FP walk is expected to succeed; BPF prefers FP.
	ModeFPLess   Mode = 1 // FPs are not preserved here; BPF must use DWARF.
	ModeFallback Mode = 2 // Complex CFI; BPF falls back to FP (accept whatever it produces).
)

// Classification pairs a PC range with its hybrid-walker mode.
type Classification struct {
	PCStart    uint64
	PCEndDelta uint32
	Mode       Mode
	_          [3]uint8
}
