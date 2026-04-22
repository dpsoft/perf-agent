package ehcompile

import (
	"debug/elf"
	"fmt"
)

// archInfo holds per-architecture metadata the CFI interpreter needs:
// which DWARF register is the stack pointer (SP), frame pointer (FP),
// and which register column the CIE uses for the return address (RA).
type archInfo struct {
	name  string
	spReg uint8
	fpReg uint8
	raReg uint8
}

func archX86_64() archInfo {
	return archInfo{
		name:  "x86_64",
		spReg: x86RSP,
		fpReg: x86RBP,
		raReg: x86RIP,
	}
}

func archARM64() archInfo {
	return archInfo{
		name:  "arm64",
		spReg: arm64SP,
		fpReg: arm64X29,
		raReg: arm64X30,
	}
}

// cfaTypeFor translates a DWARF register number into a neutral CFAType.
// Anything other than SP or FP returns CFATypeUndefined — the interpreter
// then classifies the range as FALLBACK.
func (a archInfo) cfaTypeFor(reg uint8) CFAType {
	switch reg {
	case a.spReg:
		return CFATypeSP
	case a.fpReg:
		return CFATypeFP
	default:
		return CFATypeUndefined
	}
}

// archFromELFMachine picks an archInfo from an ELF machine constant.
// elf.EM_X86_64 == 62; elf.EM_AARCH64 == 183.
func archFromELFMachine(m elf.Machine) (archInfo, error) {
	switch m {
	case elf.EM_X86_64:
		return archX86_64(), nil
	case elf.EM_AARCH64:
		return archARM64(), nil
	default:
		return archInfo{}, fmt.Errorf("%w: %v", ErrUnsupportedArch, m)
	}
}
