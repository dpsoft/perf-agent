// Package ehcompile parses an ELF file's .eh_frame section and produces
// flat tables of unwind rules suitable for loading into BPF maps.
//
// Output:
//
//   - entries []CFIEntry: "for PC in [PCStart, PCStart+PCEndDelta),
//     CFA = <CFAType> + CFAOffset; FP saved per FPType/FPOffset;
//     RA saved per RAType/RAOffset."
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
//     CFIEntry rows; other registers emit NO row, which is how the walker
//     learns to fall back to the frame pointer for that range.
//   - Register saves: offset / offset_extended / offset_extended_sf /
//     restore / restore_extended / same_value / undefined / register.
//     Only FP and RA are tracked; other register saves are ignored.
//     restore / restore_extended revert to the rule the CIE's initial
//     instructions left in place (DWARF 5 6.4.2.3), not to a fixed default.
//   - Initial rules, before any instruction runs: the frame-pointer register
//     is SAME VALUE (callee-saved under the x86-64 psABI and AAPCS64, so a
//     frame that never mentions it has not changed it) and the
//     return-address column is UNDEFINED (DWARF 5 6.4.1, and the marker
//     producers use for an outermost frame).
//   - State stack: remember_state / restore_state (16 deep).
//   - Expressions: def_cfa_expression / expression / val_expression
//     → NO CFIEntry for the covered PC range, so the walker frame-pointer
//     walks it.
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
	// FPTypeUndefined means the CFI positively asserts the frame-pointer
	// register does not hold the caller's value and says nothing about where
	// it does: DW_CFA_undefined on %rbp / x29. The BPF walker zeroes the
	// frame pointer on such a step, so every FP-based frame above is lost.
	//
	// It is NOT what "the CFI carries no rule for the register" produces.
	// A callee-saved register with no rule is UNCHANGED under the x86-64
	// psABI and AAPCS64, so that compiles to FPTypeSameValue - see
	// archDefaultFPRule in interpreter.go and issue #45. Emitting
	// FPTypeUndefined for it truncated 12-27% of every shipped library's
	// code ranges. In practice producers never emit DW_CFA_undefined for the
	// frame-pointer register at all (0 occurrences across ~200k CFI rows in
	// five system binaries), so this value is close to unreachable in the
	// wild - which is exactly why conflating it with "no rule" was so
	// damaging.
	FPTypeUndefined FPType = 0
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
