// Package ehcompile parses an ELF file's .eh_frame section and produces
// flat tables of unwind rules suitable for loading into BPF maps.
//
// Output:
//
//   - entries []CFIEntry: "for PC in [PCStart, PCStart+PCEndDelta),
//     CFA = <CFAType> + CFAOffset; FP saved per FPType/FPOffset;
//     RA saved per RAType/RAOffset."
//   - classifications []Classification: parallel rows tagging each PC
//     range as FP_SAFE (FP-based CFA), FP_LESS (SP-based CFA), or
//     FALLBACK (complex expression rule — BPF falls back to FP walking).
//
// Architectures (auto-detected from ELF machine type):
//
//   - x86_64 (EM_X86_64): SP=RSP, FP=RBP, RA column=16.
//   - arm64  (EM_AARCH64): SP=SP, FP=x29, RA column=30 (LR).
//   - Others rejected with ErrUnsupportedArch.
//
// CFI dialect supported:
//
//   - Simple CFA rules: def_cfa / def_cfa_register / def_cfa_offset /
//     def_cfa_offset_sf / def_cfa_sf. Only SP and FP (per-arch) produce
//     CFIEntry rows; other registers → FALLBACK classification.
//   - Register saves: offset / offset_extended / offset_extended_sf /
//     restore / restore_extended / same_value / undefined / register.
//     Only FP and RA are tracked; other register saves are ignored.
//   - State stack: remember_state / restore_state (16 deep).
//   - Expressions: def_cfa_expression / expression / val_expression
//     → FALLBACK for the covered PC range, no CFIEntry.
//   - PC advance: advance_loc (compressed), advance_loc1/2/4, set_loc.
//   - GNU extensions: GNU_args_size (consumed, no effect).
//   - arm64: DW_CFA_AArch64_negate_ra_state (no operand, no effect).
//
// Out of scope:
//
//   - DW_EH_PE_indirect pointer encoding.
//   - DW_CFA_val_offset register saving.
//   - .debug_frame (different layout from .eh_frame).
//
// See docs/dwarf-unwinding-design.md for the broader BPF-side architecture
// this package feeds.
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
// Layout mirrors bpf/unwind_common.h's `struct cfi_entry`
// — keep in sync with the BPF header. Arch-neutral: the same struct serves x86_64
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
