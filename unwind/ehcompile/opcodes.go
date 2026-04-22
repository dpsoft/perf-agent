package ehcompile

// DWARF CFI opcodes. Upper 2 bits zero for the "primary" set; non-zero
// high bits encode the three compressed opcodes in the low 6 bits.
const (
	cfaAdvanceLoc = 0x40 // top 2 bits == 01; low 6 bits = delta
	cfaOffset     = 0x80 // top 2 bits == 10; low 6 bits = register
	cfaRestore    = 0xc0 // top 2 bits == 11; low 6 bits = register

	cfaNop              = 0x00
	cfaSetLoc           = 0x01
	cfaAdvanceLoc1      = 0x02
	cfaAdvanceLoc2      = 0x03
	cfaAdvanceLoc4      = 0x04
	cfaOffsetExtended   = 0x05
	cfaRestoreExtended  = 0x06
	cfaUndefined        = 0x07
	cfaSameValue        = 0x08
	cfaRegister         = 0x09
	cfaRememberState    = 0x0a
	cfaRestoreState     = 0x0b
	cfaDefCFA           = 0x0c
	cfaDefCFARegister   = 0x0d
	cfaDefCFAOffset     = 0x0e
	cfaDefCFAExpression = 0x0f
	cfaExpression       = 0x10
	cfaOffsetExtendedSF = 0x11
	cfaDefCFASF         = 0x12
	cfaDefCFAOffsetSF   = 0x13
	cfaValOffset        = 0x14
	cfaValOffsetSF      = 0x15
	cfaValExpression    = 0x16

	cfaGnuArgsSize               = 0x2e
	cfaGnuNegativeOffsetExtended = 0x2f

	cfaOpcodeMask  = 0xc0
	cfaOperandMask = 0x3f
)

// DWARF register numbers — x86_64 (System V AMD64 ABI, Figure 3.36).
// Only the ones we care about for CFA/FP/RA tracking are named.
const (
	x86RAX = 0
	x86RDX = 1
	x86RCX = 2
	x86RBX = 3
	x86RSI = 4
	x86RDI = 5
	x86RBP = 6 // FP on x86_64
	x86RSP = 7 // SP on x86_64
	x86R8  = 8
	x86R15 = 15
	x86RIP = 16 // conventional "return address column" on x86_64
)

// DWARF register numbers — arm64 (AArch64 ABI).
const (
	arm64X0  = 0
	arm64X29 = 29 // FP on arm64
	arm64X30 = 30 // LR — conventional "return address column" on arm64
	arm64SP  = 31 // SP on arm64
)
