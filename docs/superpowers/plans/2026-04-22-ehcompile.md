# `unwind/ehcompile/` Implementation Plan (Stage S1)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `unwind/ehcompile/` Go package that reads an ELF file's `.eh_frame` section, runs the DWARF Call Frame Information (CFI) bytecode interpreter, and produces two flat arrays (CFI entries + classification entries) suitable for loading into BPF maps for the Option A stack unwinder. **Supports x86_64 and arm64** from day one — arch-neutral output format, arch-specific register mapping in the interpreter.

**Architecture:** Parser + state-machine interpreter. `Compile(elfPath)` opens the ELF with stdlib `debug/elf`, reads the machine type to pick an `archInfo` (which register is FP, which column holds the return address), walks `.eh_frame` to enumerate CIEs and FDEs, and interprets each FDE's CFI bytecode on top of its CIE's initial state. Simple `CFA = SP|FP + offset` rules become `CFIEntry` rows; complex expression rules become `Classification{Mode: FALLBACK}` rows. The *output* struct is identical across arches; only the interpreter's mapping from DWARF register numbers to neutral slots (FP, RA) differs.

**Tech Stack:** Go 1.26, stdlib `debug/elf` + `encoding/binary`, `testify/assert` + `testify/require` for tests. Test fixtures produced with system `gcc`, `clang`, `readelf` (for golden-file generation only, not runtime). Optional `aarch64-linux-gnu-gcc` for cross-compiled arm64 fixtures; falls back to synthetic bytecode tests if unavailable.

**Out of scope for S1:**
- Hardcoding the two PLT `DW_CFA_expression` patterns Polar Signals calls out (stretch, later).
- BPF-map loading — that's S4 (`unwind/ehmaps/`).
- Integration with `profile/`, `offcpu/`, or the BPF programs.

---

## File Structure

```
unwind/ehcompile/
  ehcompile.go         # Public API: Compile(elfPath)
  types.go             # CFIEntry, Classification, Mode, CFAType, FPType
  arch.go              # archInfo struct + x86_64 / arm64 instances
  encoding.go          # ULEB128, SLEB128, DW_EH_PE_* pointer decode
  cie.go               # CIE + FDE structs and parsers
  opcodes.go           # DW_CFA_* constants + DWARF register numbers (both arches)
  interpreter.go       # CFI VM: state, opcode dispatch, emission (arch-parameterized)
  
  encoding_test.go     # unit: ULEB128/SLEB128/DW_EH_PE_*
  cie_test.go          # unit: CIE/FDE parsing on handcrafted bytes
  interpreter_test.go  # unit: opcode handling on synthetic programs (x86_64 and arm64)
  ehcompile_test.go    # integration: Compile() on real ELFs
  
  testdata/
    hello.c            # trivial C source used for x86_64 golden fixture
    hello              # compiled x86_64 binary (committed)
    hello.golden       # expected output
    hello_arm64        # cross-compiled arm64 binary (committed if available)
    hello_arm64.golden
    README.md          # how to regenerate fixtures
```

**Why this layout:**
- `ehcompile.go` is the one-function public API; everything else is package-internal.
- `arch.go` isolates per-architecture knowledge (which DWARF register is FP, which column is RA) so the interpreter doesn't sprinkle arch checks through its body.
- `types.go` isolates the output shape so the BPF-side struct (future `bpf/unwind_common.h`) can track it.
- `encoding.go`, `cie.go`, `opcodes.go`, `interpreter.go` split the parser into independently testable units.
- `testdata/` holds committed binaries + golden outputs for snapshot tests.

---

## Task 1: Package skeleton and output types

**Files:**
- Create: `unwind/ehcompile/types.go`
- Create: `unwind/ehcompile/ehcompile.go`
- Create: `unwind/ehcompile/ehcompile_test.go`

- [ ] **Step 1: Create the types file with the output structs.**

Write to `unwind/ehcompile/types.go`:

```go
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
```

- [ ] **Step 2: Create the public API skeleton.**

Write to `unwind/ehcompile/ehcompile.go`:

```go
package ehcompile

import (
	"errors"
)

// ErrNoEHFrame is returned when the ELF file has no usable .eh_frame section.
var ErrNoEHFrame = errors.New("ehcompile: no .eh_frame section")

// ErrUnsupportedArch is returned when the ELF's machine type is not
// x86_64 or arm64. Other architectures can be added later.
var ErrUnsupportedArch = errors.New("ehcompile: unsupported ELF machine type")

// Compile reads the ELF at elfPath and produces flat CFI + Classification
// tables. Both slices are sorted by PCStart. Adjacent rows with identical
// rules are coalesced.
//
// The ELF's machine type (x86_64 vs aarch64) is auto-detected and the
// appropriate archInfo is used for register-number translation.
//
// Not safe for concurrent calls per instance; callers should serialize.
func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, err error) {
	return nil, nil, errors.New("ehcompile: not implemented")
}
```

- [ ] **Step 3: Write a placeholder test.**

Write to `unwind/ehcompile/ehcompile_test.go`:

```go
package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompile_NotImplemented(t *testing.T) {
	_, _, err := Compile("/dev/null")
	require.Error(t, err)
}
```

- [ ] **Step 4: Run tests, confirm pass.**

Run: `go test ./unwind/ehcompile/ -v`
Expected: PASS (stub returns error, which the test requires).

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/types.go unwind/ehcompile/ehcompile.go unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: package skeleton with arch-neutral output types"
```

---

## Task 2: ULEB128 / SLEB128 decoders

**Files:**
- Create: `unwind/ehcompile/encoding.go`
- Create: `unwind/ehcompile/encoding_test.go`

- [ ] **Step 1: Write failing tests for ULEB128 decoding.**

Write to `unwind/ehcompile/encoding_test.go`:

```go
package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeULEB128(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		want     uint64
		consumed int
	}{
		{"single byte zero", []byte{0x00}, 0, 1},
		{"single byte 0x7f", []byte{0x7f}, 127, 1},
		{"two bytes 128", []byte{0x80, 0x01}, 128, 2},
		{"three bytes 16384", []byte{0x80, 0x80, 0x01}, 16384, 3},
		{"DWARF spec example 624485", []byte{0xe5, 0x8e, 0x26}, 624485, 3},
		{"max uint64", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, 0xFFFFFFFFFFFFFFFF, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n, err := decodeULEB128(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.consumed, n)
		})
	}
}

func TestDecodeULEB128_Truncated(t *testing.T) {
	_, _, err := decodeULEB128([]byte{0x80, 0x80, 0x80})
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestDecodeULEB128 -v`
Expected: FAIL with "undefined: decodeULEB128".

- [ ] **Step 3: Implement ULEB128.**

Write to `unwind/ehcompile/encoding.go`:

```go
package ehcompile

import (
	"errors"
)

var errTruncated = errors.New("ehcompile: truncated input")

// decodeULEB128 reads one ULEB128-encoded unsigned integer from b.
// Returns the value and the number of bytes consumed.
func decodeULEB128(b []byte) (uint64, int, error) {
	var result uint64
	var shift uint
	for i, by := range b {
		if i >= 10 {
			return 0, 0, errors.New("ehcompile: ULEB128 too long")
		}
		result |= uint64(by&0x7f) << shift
		if by&0x80 == 0 {
			return result, i + 1, nil
		}
		shift += 7
	}
	return 0, 0, errTruncated
}
```

- [ ] **Step 4: Verify tests pass.**

Run: `go test ./unwind/ehcompile/ -run TestDecodeULEB128 -v`
Expected: PASS all subtests.

- [ ] **Step 5: Add failing SLEB128 tests.**

Append to `unwind/ehcompile/encoding_test.go`:

```go
func TestDecodeSLEB128(t *testing.T) {
	tests := []struct {
		name     string
		in       []byte
		want     int64
		consumed int
	}{
		{"zero", []byte{0x00}, 0, 1},
		{"positive small", []byte{0x02}, 2, 1},
		{"negative small (-2)", []byte{0x7e}, -2, 1},
		{"positive 127", []byte{0xff, 0x00}, 127, 2},
		{"negative 128", []byte{0x80, 0x7f}, -128, 2},
		{"DWARF spec 129", []byte{0x81, 0x01}, 129, 2},
		{"DWARF spec -129", []byte{0xff, 0x7e}, -129, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, n, err := decodeSLEB128(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.consumed, n)
		})
	}
}
```

- [ ] **Step 6: Run, confirm failure.**

Run: `go test ./unwind/ehcompile/ -run TestDecodeSLEB128 -v`
Expected: FAIL — "undefined: decodeSLEB128".

- [ ] **Step 7: Implement SLEB128.**

Append to `unwind/ehcompile/encoding.go`:

```go
// decodeSLEB128 reads one SLEB128-encoded signed integer from b.
func decodeSLEB128(b []byte) (int64, int, error) {
	var result int64
	var shift uint
	for i, by := range b {
		if i >= 10 {
			return 0, 0, errors.New("ehcompile: SLEB128 too long")
		}
		result |= int64(by&0x7f) << shift
		shift += 7
		if by&0x80 == 0 {
			if shift < 64 && by&0x40 != 0 {
				result |= -(int64(1) << shift)
			}
			return result, i + 1, nil
		}
	}
	return 0, 0, errTruncated
}
```

- [ ] **Step 8: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run Encoding -v`
Expected: PASS.

- [ ] **Step 9: Commit.**

```bash
git add unwind/ehcompile/encoding.go unwind/ehcompile/encoding_test.go
git commit -m "ehcompile: ULEB128 and SLEB128 decoders"
```

---

## Task 3: DW_EH_PE_* pointer decoder

**Files:**
- Modify: `unwind/ehcompile/encoding.go`
- Modify: `unwind/ehcompile/encoding_test.go`

- [ ] **Step 1: Add failing tests.**

Append to `unwind/ehcompile/encoding_test.go`:

```go
func TestDecodeEHPointer_udata4(t *testing.T) {
	b := []byte{0x34, 0x12, 0x00, 0x00}
	v, n, err := decodeEHPointer(b, 0x03, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x1234), v)
	assert.Equal(t, 4, n)
}

func TestDecodeEHPointer_sdata4_pcrel(t *testing.T) {
	b := []byte{0xf0, 0xff, 0xff, 0xff}
	v, n, err := decodeEHPointer(b, 0x1B, 0x1000, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x0FF0), v)
	assert.Equal(t, 4, n)
}

func TestDecodeEHPointer_sdata4_datarel(t *testing.T) {
	b := []byte{0x10, 0x00, 0x00, 0x00}
	v, n, err := decodeEHPointer(b, 0x3B, 0, 0x2000)
	require.NoError(t, err)
	assert.Equal(t, uint64(0x2010), v)
	assert.Equal(t, 4, n)
}

func TestDecodeEHPointer_omit(t *testing.T) {
	v, n, err := decodeEHPointer(nil, 0xff, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), v)
	assert.Equal(t, 0, n)
}
```

- [ ] **Step 2: Run, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestDecodeEHPointer -v`
Expected: FAIL — "undefined: decodeEHPointer".

- [ ] **Step 3: Implement.**

Prepend import to `unwind/ehcompile/encoding.go` (modify the existing imports):

```go
import (
	"encoding/binary"
	"errors"
)
```

Append to the same file:

```go
// DWARF exception-handling pointer encoding byte.
// Low nibble = data format; bits 4-6 = relativity; bit 7 = indirect.
// (Bit 7 we don't support — real binaries don't use it for FDE pointers.)
const (
	dwEhPEAbsptr = 0x00
	dwEhPEOmit   = 0xff

	dwEhPEUleb128 = 0x01
	dwEhPEUdata2  = 0x02
	dwEhPEUdata4  = 0x03
	dwEhPEUdata8  = 0x04
	dwEhPESleb128 = 0x09
	dwEhPESdata2  = 0x0a
	dwEhPESdata4  = 0x0b
	dwEhPESdata8  = 0x0c

	dwEhPEPcrel   = 0x10
	dwEhPETextrel = 0x20
	dwEhPEDatarel = 0x30
	dwEhPEFuncrel = 0x40
	dwEhPEAligned = 0x50
)

// decodeEHPointer reads a DWARF EH pointer from b using encoding byte enc.
// pcPos = absolute address of b[0] (for DW_EH_PE_pcrel).
// dataBase = base for DW_EH_PE_datarel (typically address of .eh_frame_hdr).
// Returns the resolved address and bytes consumed.
//
// DW_EH_PE_omit (0xff) returns (0, 0, nil).
func decodeEHPointer(b []byte, enc byte, pcPos uint64, dataBase uint64) (uint64, int, error) {
	if enc == dwEhPEOmit {
		return 0, 0, nil
	}
	format := enc & 0x0f
	rel := enc & 0x70

	var raw int64
	var n int
	switch format {
	case dwEhPEUleb128:
		u, cn, err := decodeULEB128(b)
		if err != nil {
			return 0, 0, err
		}
		raw = int64(u)
		n = cn
	case dwEhPEUdata2:
		if len(b) < 2 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint16(b))
		n = 2
	case dwEhPEUdata4:
		if len(b) < 4 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint32(b))
		n = 4
	case dwEhPEUdata8:
		if len(b) < 8 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint64(b))
		n = 8
	case dwEhPESleb128:
		s, cn, err := decodeSLEB128(b)
		if err != nil {
			return 0, 0, err
		}
		raw = s
		n = cn
	case dwEhPESdata2:
		if len(b) < 2 {
			return 0, 0, errTruncated
		}
		raw = int64(int16(binary.LittleEndian.Uint16(b)))
		n = 2
	case dwEhPESdata4:
		if len(b) < 4 {
			return 0, 0, errTruncated
		}
		raw = int64(int32(binary.LittleEndian.Uint32(b)))
		n = 4
	case dwEhPESdata8:
		if len(b) < 8 {
			return 0, 0, errTruncated
		}
		raw = int64(binary.LittleEndian.Uint64(b))
		n = 8
	default:
		return 0, 0, errors.New("ehcompile: unknown DW_EH_PE format")
	}

	var base int64
	switch rel {
	case dwEhPEAbsptr:
		base = 0
	case dwEhPEPcrel:
		base = int64(pcPos)
	case dwEhPEDatarel:
		base = int64(dataBase)
	default:
		return 0, 0, errors.New("ehcompile: unsupported DW_EH_PE relativity")
	}

	return uint64(base + raw), n, nil
}
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestDecodeEHPointer -v`
Expected: PASS all four.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/encoding.go unwind/ehcompile/encoding_test.go
git commit -m "ehcompile: DW_EH_PE_* pointer decoder"
```

---

## Task 4: DWARF opcode constants and register numbers for both arches

**Files:**
- Create: `unwind/ehcompile/opcodes.go`

- [ ] **Step 1: Write the opcode + register table.**

Write to `unwind/ehcompile/opcodes.go`:

```go
package ehcompile

// DWARF CFI opcodes. Upper 2 bits zero for the "primary" set; non-zero
// high bits encode the three compressed opcodes in the low 6 bits.
const (
	cfaAdvanceLoc = 0x40 // top 2 bits == 01; low 6 bits = delta
	cfaOffset     = 0x80 // top 2 bits == 10; low 6 bits = register
	cfaRestore    = 0xc0 // top 2 bits == 11; low 6 bits = register

	cfaNop                = 0x00
	cfaSetLoc             = 0x01
	cfaAdvanceLoc1        = 0x02
	cfaAdvanceLoc2        = 0x03
	cfaAdvanceLoc4        = 0x04
	cfaOffsetExtended     = 0x05
	cfaRestoreExtended    = 0x06
	cfaUndefined          = 0x07
	cfaSameValue          = 0x08
	cfaRegister           = 0x09
	cfaRememberState      = 0x0a
	cfaRestoreState       = 0x0b
	cfaDefCFA             = 0x0c
	cfaDefCFARegister     = 0x0d
	cfaDefCFAOffset       = 0x0e
	cfaDefCFAExpression   = 0x0f
	cfaExpression         = 0x10
	cfaOffsetExtendedSF   = 0x11
	cfaDefCFASF           = 0x12
	cfaDefCFAOffsetSF     = 0x13
	cfaValOffset          = 0x14
	cfaValOffsetSF        = 0x15
	cfaValExpression      = 0x16

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
	x86RBP = 6  // FP on x86_64
	x86RSP = 7  // SP on x86_64
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
```

- [ ] **Step 2: Verify it compiles.**

Run: `go build ./unwind/ehcompile/`
Expected: no errors.

- [ ] **Step 3: Commit.**

```bash
git add unwind/ehcompile/opcodes.go
git commit -m "ehcompile: DW_CFA opcode constants + x86_64 and arm64 register numbers"
```

---

## Task 5: `archInfo` struct for arch dispatch

**Files:**
- Create: `unwind/ehcompile/arch.go`
- Create: `unwind/ehcompile/arch_test.go`

- [ ] **Step 1: Write failing tests.**

Write to `unwind/ehcompile/arch_test.go`:

```go
package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArchX86_64(t *testing.T) {
	a := archX86_64()
	assert.Equal(t, uint8(x86RSP), a.spReg)
	assert.Equal(t, uint8(x86RBP), a.fpReg)
	assert.Equal(t, uint8(x86RIP), a.raReg)
	assert.Equal(t, "x86_64", a.name)
}

func TestArchARM64(t *testing.T) {
	a := archARM64()
	assert.Equal(t, uint8(arm64SP), a.spReg)
	assert.Equal(t, uint8(arm64X29), a.fpReg)
	assert.Equal(t, uint8(arm64X30), a.raReg)
	assert.Equal(t, "arm64", a.name)
}

func TestCFATypeFromReg(t *testing.T) {
	a := archX86_64()
	assert.Equal(t, CFATypeSP, a.cfaTypeFor(x86RSP))
	assert.Equal(t, CFATypeFP, a.cfaTypeFor(x86RBP))
	assert.Equal(t, CFATypeUndefined, a.cfaTypeFor(x86R8))
}

func TestArchFromELFMachine(t *testing.T) {
	a, err := archFromELFMachine(62) // EM_X86_64
	require.NoError(t, err)
	assert.Equal(t, "x86_64", a.name)

	a, err = archFromELFMachine(183) // EM_AARCH64
	require.NoError(t, err)
	assert.Equal(t, "arm64", a.name)

	_, err = archFromELFMachine(8) // EM_MIPS
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestArch -v`
Expected: FAIL — "undefined: archX86_64".

- [ ] **Step 3: Implement.**

Write to `unwind/ehcompile/arch.go`:

```go
package ehcompile

import (
	"debug/elf"
	"fmt"
)

// archInfo holds per-architecture metadata the CFI interpreter needs:
// which DWARF register is the stack pointer (SP), frame pointer (FP),
// and which register column the CIE uses for the return address (RA).
type archInfo struct {
	name   string
	spReg  uint8
	fpReg  uint8
	raReg  uint8
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
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestArch -v`
Expected: PASS all four.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/arch.go unwind/ehcompile/arch_test.go
git commit -m "ehcompile: archInfo with x86_64 and arm64 register maps"
```

---

## Task 6: CIE parser

**Files:**
- Create: `unwind/ehcompile/cie.go`
- Create: `unwind/ehcompile/cie_test.go`

- [ ] **Step 1: Write failing tests for CIE parsing.**

Write to `unwind/ehcompile/cie_test.go`:

```go
package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Canonical x86_64 CIE:
//   length = 0x14
//   CIE_id = 0
//   version = 1
//   augmentation = "zR\0"
//   code_alignment_factor = 1 (uleb128)
//   data_alignment_factor = -8 (sleb128 = 0x78)
//   return_address_column = 16 (uleb128)
//   z augmentation length = 1
//   R augmentation data = 0x1B (DW_EH_PE_pcrel|sdata4)
//   initial instructions:
//     DW_CFA_def_cfa(7, 8)
//     DW_CFA_offset(16, 1)   // RA at CFA + 1 * -8 = -8
//     DW_CFA_nop
func sampleCIEx86() []byte {
	return []byte{
		0x14, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00,
		0x01,
		'z', 'R', 0x00,
		0x01,
		0x78,
		0x10,
		0x01,
		0x1b,
		0x0c, 0x07, 0x08,
		0x90, 0x01,
		0x00,
	}
}

func TestParseCIE_Basic(t *testing.T) {
	c, err := parseCIE(sampleCIEx86(), 0)
	require.NoError(t, err)
	assert.Equal(t, byte(1), c.version)
	assert.Equal(t, "zR", c.augmentation)
	assert.Equal(t, uint64(1), c.codeAlign)
	assert.Equal(t, int64(-8), c.dataAlign)
	assert.Equal(t, uint64(16), c.raColumn)
	assert.Equal(t, byte(0x1b), c.fdePointerEnc)
	assert.NotEmpty(t, c.initialInstructions)
}

func TestParseCIE_UnknownAugmentation(t *testing.T) {
	b := sampleCIEx86()
	b[10] = 'X'
	_, err := parseCIE(b, 0)
	require.Error(t, err)
}
```

- [ ] **Step 2: Run, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestParseCIE -v`
Expected: FAIL — "undefined: parseCIE".

- [ ] **Step 3: Implement.**

Write to `unwind/ehcompile/cie.go`:

```go
package ehcompile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

type cie struct {
	version             byte
	augmentation        string
	codeAlign           uint64
	dataAlign           int64
	raColumn            uint64
	fdePointerEnc       byte
	hasSignalFrame      bool
	initialInstructions []byte
}

// parseCIE reads a CIE from raw[0:]. filePos is raw[0]'s absolute file
// offset, used to resolve augmentation-data pcrel pointers (which we
// skip — we just need to advance past them).
func parseCIE(raw []byte, filePos uint64) (*cie, error) {
	if len(raw) < 4 {
		return nil, errTruncated
	}
	length := binary.LittleEndian.Uint32(raw[:4])
	if length == 0xFFFFFFFF {
		return nil, errors.New("ehcompile: 64-bit .eh_frame not supported")
	}
	if length == 0 {
		return nil, errors.New("ehcompile: zero-length CIE (EOF sentinel)")
	}
	body := raw[4 : 4+length]
	if len(body) < 9 {
		return nil, errTruncated
	}
	if binary.LittleEndian.Uint32(body[:4]) != 0 {
		return nil, errors.New("ehcompile: non-zero CIE_id")
	}
	pos := 4
	version := body[pos]
	pos++
	if version != 1 && version != 3 {
		return nil, fmt.Errorf("ehcompile: unsupported CIE version %d", version)
	}

	nul := bytes.IndexByte(body[pos:], 0)
	if nul < 0 {
		return nil, errTruncated
	}
	augmentation := string(body[pos : pos+nul])
	pos += nul + 1

	codeAlign, n, err := decodeULEB128(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n

	dataAlign, n, err := decodeSLEB128(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n

	raCol, n, err := decodeULEB128(body[pos:])
	if err != nil {
		return nil, err
	}
	pos += n

	c := &cie{
		version:      version,
		augmentation: augmentation,
		codeAlign:    codeAlign,
		dataAlign:    dataAlign,
		raColumn:     raCol,
	}

	if len(augmentation) > 0 && augmentation[0] == 'z' {
		augLen, n, err := decodeULEB128(body[pos:])
		if err != nil {
			return nil, err
		}
		pos += n
		augData := body[pos : pos+int(augLen)]
		pos += int(augLen)
		if err := c.parseAugmentationData(augmentation[1:], augData); err != nil {
			return nil, err
		}
	} else if augmentation == "" {
		// no augmentation data
	} else if augmentation == "eh" {
		return nil, errors.New("ehcompile: legacy 'eh' augmentation not supported")
	} else {
		return nil, fmt.Errorf("ehcompile: unknown augmentation %q", augmentation)
	}

	c.initialInstructions = body[pos:]
	return c, nil
}

func (c *cie) parseAugmentationData(augChars string, data []byte) error {
	pos := 0
	for _, ch := range augChars {
		switch ch {
		case 'R':
			if pos >= len(data) {
				return errTruncated
			}
			c.fdePointerEnc = data[pos]
			pos++
		case 'S':
			c.hasSignalFrame = true
		case 'P':
			if pos >= len(data) {
				return errTruncated
			}
			enc := data[pos]
			pos++
			_, n, err := decodeEHPointer(data[pos:], enc, 0, 0)
			if err != nil {
				return err
			}
			pos += n
		case 'L':
			if pos >= len(data) {
				return errTruncated
			}
			pos++
		case 'B':
			// arm64 pointer auth — no operand in CIE aug data.
		default:
			return fmt.Errorf("ehcompile: unknown augmentation char %c", ch)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestParseCIE -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/cie.go unwind/ehcompile/cie_test.go
git commit -m "ehcompile: CIE parser with augmentation handling"
```

---

## Task 7: FDE parser

**Files:**
- Modify: `unwind/ehcompile/cie.go`
- Modify: `unwind/ehcompile/cie_test.go`

- [ ] **Step 1: Add failing test.**

Append to `unwind/ehcompile/cie_test.go`:

```go
// FDE for the sample CIE:
//   length = 0x10
//   CIE_pointer = 0x1C (backward offset from this field to CIE start)
//   initial_location = 0x100 (sdata4 pcrel)
//   address_range = 0x20
//   augmentation length = 0
//   instructions = DW_CFA_nop * 2
func sampleFDE(ciePos uint64, fdePos uint64) []byte {
	return []byte{
		0x10, 0x00, 0x00, 0x00,
		0x1c, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x00,
		0x20, 0x00, 0x00, 0x00,
		0x00,
		0x00, 0x00, 0x00,
	}
}

func TestParseFDE_Basic(t *testing.T) {
	cieRaw := sampleCIEx86()
	c, err := parseCIE(cieRaw, 0)
	require.NoError(t, err)

	fdeRaw := sampleFDE(0, uint64(len(cieRaw)))
	f, err := parseFDE(fdeRaw, uint64(len(cieRaw)), c)
	require.NoError(t, err)

	wantPC := uint64(len(cieRaw)) + 8 + 0x100
	assert.Equal(t, wantPC, f.initialLocation)
	assert.Equal(t, uint64(0x20), f.addressRange)
	assert.NotEmpty(t, f.instructions)
}
```

- [ ] **Step 2: Run, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestParseFDE -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

Append to `unwind/ehcompile/cie.go`:

```go
type fde struct {
	cie             *cie
	initialLocation uint64
	addressRange    uint64
	instructions    []byte
}

// parseFDE reads an FDE starting at raw[0]. filePos is raw[0]'s absolute
// file offset. Caller is responsible for locating the matching CIE.
func parseFDE(raw []byte, filePos uint64, c *cie) (*fde, error) {
	if len(raw) < 8 {
		return nil, errTruncated
	}
	length := binary.LittleEndian.Uint32(raw[:4])
	if length == 0xFFFFFFFF || length == 0 {
		return nil, errors.New("ehcompile: unsupported FDE length")
	}
	body := raw[4 : 4+length]
	pos := 4 // skip CIE pointer (caller already resolved it)

	encPos := filePos + 8 // position of initial_location in absolute terms
	initLoc, n, err := decodeEHPointer(body[pos:], c.fdePointerEnc, encPos, 0)
	if err != nil {
		return nil, err
	}
	pos += n

	// address_range uses the same data format as initial_location but no
	// relativity.
	rangeEnc := c.fdePointerEnc & 0x0f
	addrRange, n, err := decodeEHPointer(body[pos:], rangeEnc, 0, 0)
	if err != nil {
		return nil, err
	}
	pos += n

	if len(c.augmentation) > 0 && c.augmentation[0] == 'z' {
		augLen, n, err := decodeULEB128(body[pos:])
		if err != nil {
			return nil, err
		}
		pos += n + int(augLen)
	}

	return &fde{
		cie:             c,
		initialLocation: initLoc,
		addressRange:    addrRange,
		instructions:    body[pos:],
	}, nil
}
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestParseFDE -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/cie.go unwind/ehcompile/cie_test.go
git commit -m "ehcompile: FDE parser"
```

---

## Task 8: `.eh_frame` section walker

**Files:**
- Modify: `unwind/ehcompile/cie.go`
- Modify: `unwind/ehcompile/cie_test.go`

- [ ] **Step 1: Add failing test.**

Append to `unwind/ehcompile/cie_test.go`:

```go
func TestWalkEHFrame_CIEAndFDE(t *testing.T) {
	cieRaw := sampleCIEx86()
	fdeRaw := sampleFDE(0, uint64(len(cieRaw)))
	section := append(append([]byte{}, cieRaw...), fdeRaw...)

	var cies, fdes int
	err := walkEHFrame(section, 0, func(off uint64, c *cie, f *fde) error {
		if c != nil {
			cies++
		}
		if f != nil {
			fdes++
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, cies)
	assert.Equal(t, 1, fdes)
}
```

- [ ] **Step 2: Run, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestWalkEHFrame -v`
Expected: FAIL.

- [ ] **Step 3: Implement.**

Append to `unwind/ehcompile/cie.go`:

```go
// walkEHFrame iterates CIE/FDE records in an .eh_frame section.
// sectionPos is section[0]'s absolute file offset. The callback receives
// exactly one of (c, f) non-nil per invocation; c is passed BEFORE any
// of its FDEs.
func walkEHFrame(section []byte, sectionPos uint64, cb func(off uint64, c *cie, f *fde) error) error {
	cies := make(map[uint64]*cie)

	var pos uint64
	for int(pos)+4 <= len(section) {
		length := binary.LittleEndian.Uint32(section[pos : pos+4])
		if length == 0 {
			return nil // EOF sentinel
		}
		if length == 0xFFFFFFFF {
			return errors.New("ehcompile: 64-bit .eh_frame not supported")
		}
		recordEnd := pos + 4 + uint64(length)
		if recordEnd > uint64(len(section)) {
			return errTruncated
		}
		secondWord := binary.LittleEndian.Uint32(section[pos+4 : pos+8])

		if secondWord == 0 {
			c, err := parseCIE(section[pos:recordEnd], sectionPos+pos)
			if err != nil {
				return fmt.Errorf("CIE at +%#x: %w", pos, err)
			}
			cies[pos] = c
			if err := cb(pos, c, nil); err != nil {
				return err
			}
		} else {
			cieOff := pos + 4 - uint64(secondWord)
			c, ok := cies[cieOff]
			if !ok {
				return fmt.Errorf("FDE at +%#x references unknown CIE at +%#x", pos, cieOff)
			}
			f, err := parseFDE(section[pos:recordEnd], sectionPos+pos, c)
			if err != nil {
				return fmt.Errorf("FDE at +%#x: %w", pos, err)
			}
			if err := cb(pos, nil, f); err != nil {
				return err
			}
		}
		pos = recordEnd
	}
	return nil
}
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -v`
Expected: all existing tests pass.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/cie.go unwind/ehcompile/cie_test.go
git commit -m "ehcompile: walk CIE/FDE records in .eh_frame"
```

---

## Task 9: CFI interpreter skeleton — `nop` and `advance_loc`

**Files:**
- Create: `unwind/ehcompile/interpreter.go`
- Create: `unwind/ehcompile/interpreter_test.go`

- [ ] **Step 1: Write failing test.**

Write to `unwind/ehcompile/interpreter_test.go`:

```go
package ehcompile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCIE() *cie {
	return &cie{
		version:   1,
		codeAlign: 1,
		dataAlign: -8,
		raColumn:  16, // x86_64 convention
	}
}

func TestInterpret_AdvanceLocOnly(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		0x40 | 5,
		cfaAdvanceLoc1,
		10,
		cfaNop,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x100F, program)
	require.NoError(t, err)
	assert.Empty(t, s.entries)
	assert.Empty(t, s.classifications)
}
```

- [ ] **Step 2: Run, confirm compile failure.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret -v`
Expected: FAIL.

- [ ] **Step 3: Implement interpreter skeleton.**

Write to `unwind/ehcompile/interpreter.go`:

```go
package ehcompile

import (
	"encoding/binary"
	"errors"
	"fmt"
)

type ruleKind uint8

const (
	ruleUndefined ruleKind = iota
	ruleSameValue
	ruleOffset
	ruleRegister
	ruleExpression
)

// regRule describes how to recover a register's caller value.
type regRule struct {
	kind     ruleKind
	offset   int64
	register uint8
}

// interpreter runs the CFI state machine. It's parameterized by an
// archInfo so the same code handles x86_64 and arm64.
type interpreter struct {
	cie  *cie
	arch archInfo

	// Current state.
	pc        uint64
	cfaType   CFAType
	cfaOffset int64
	cfaRule   ruleKind
	fpRule    regRule // rule for arch.fpReg (RBP / x29)
	raRule    regRule // rule for cie.raColumn (x86 column 16 / arm64 x30)

	// Snapshot of last-emitted state for dedup.
	lastEmittedPC uint64
	lastState     emittedState

	// Output.
	entries         []CFIEntry
	classifications []Classification

	// State stack for DW_CFA_remember_state/restore_state. 16 deep is more
	// than enough (real code rarely exceeds 2).
	stack [16]savedState
	sp    int
}

type emittedState struct {
	cfaType   CFAType
	cfaOffset int64
	cfaRule   ruleKind
	fpRule    regRule
	raRule    regRule
}

type savedState struct {
	cfaType   CFAType
	cfaOffset int64
	cfaRule   ruleKind
	fpRule    regRule
	raRule    regRule
}

func newInterpreter(c *cie, arch archInfo) *interpreter {
	return &interpreter{
		cie:     c,
		arch:    arch,
		cfaType: CFATypeUndefined,
		cfaRule: ruleUndefined,
		fpRule:  regRule{kind: ruleUndefined},
		raRule:  regRule{kind: ruleUndefined},
	}
}

// run executes the CFI program. [startPC, endPC) is the PC range this
// program describes; snapshot emits rows as state advances across the range.
func (s *interpreter) run(startPC, endPC uint64, program []byte) error {
	s.pc = startPC
	s.lastEmittedPC = startPC

	for pos := 0; pos < len(program); {
		op := program[pos]
		pos++

		if op&cfaOpcodeMask != 0 {
			switch op & cfaOpcodeMask {
			case cfaAdvanceLoc:
				delta := uint64(op&cfaOperandMask) * s.cie.codeAlign
				s.snapshotAndAdvance(delta)
				continue
			case cfaOffset:
				return errors.New("ehcompile: DW_CFA_offset not yet implemented")
			case cfaRestore:
				return errors.New("ehcompile: DW_CFA_restore not yet implemented")
			}
		}

		switch op {
		case cfaNop:
			// no-op
		case cfaAdvanceLoc1:
			if pos >= len(program) {
				return errTruncated
			}
			delta := uint64(program[pos]) * s.cie.codeAlign
			pos++
			s.snapshotAndAdvance(delta)
		case cfaAdvanceLoc2:
			if pos+2 > len(program) {
				return errTruncated
			}
			delta := uint64(binary.LittleEndian.Uint16(program[pos:])) * s.cie.codeAlign
			pos += 2
			s.snapshotAndAdvance(delta)
		case cfaAdvanceLoc4:
			if pos+4 > len(program) {
				return errTruncated
			}
			delta := uint64(binary.LittleEndian.Uint32(program[pos:])) * s.cie.codeAlign
			pos += 4
			s.snapshotAndAdvance(delta)
		default:
			return fmt.Errorf("ehcompile: unhandled opcode 0x%02x at pos %d", op, pos-1)
		}
	}
	s.snapshot(endPC)
	return nil
}

func (s *interpreter) snapshotAndAdvance(delta uint64) {
	s.snapshot(s.pc + delta)
	s.pc += delta
}

// snapshot is filled in by later tasks; skeleton just tracks lastEmittedPC.
func (s *interpreter) snapshot(newPC uint64) {
	if newPC <= s.lastEmittedPC {
		return
	}
	if s.cfaType == CFATypeUndefined && s.cfaRule != ruleExpression {
		s.lastEmittedPC = newPC
		return
	}
	s.lastEmittedPC = newPC
}
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret -v`
Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/interpreter.go unwind/ehcompile/interpreter_test.go
git commit -m "ehcompile: interpreter skeleton parameterized by archInfo"
```

---

## Task 10: `def_cfa` family and row emission

**Files:**
- Modify: `unwind/ehcompile/interpreter.go`
- Modify: `unwind/ehcompile/interpreter_test.go`

- [ ] **Step 1: Add failing test for def_cfa + row emission.**

Append to `unwind/ehcompile/interpreter_test.go`:

```go
func TestInterpret_DefCFAEmitsRow(t *testing.T) {
	// def_cfa(rsp,8); advance(4); def_cfa_offset(16); advance(8).
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		0x40 | 4,
		cfaDefCFAOffset, 16,
		0x40 | 8,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x1000, 0x100C, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 2)

	assert.Equal(t, uint64(0x1000), s.entries[0].PCStart)
	assert.Equal(t, uint32(4), s.entries[0].PCEndDelta)
	assert.Equal(t, CFATypeSP, s.entries[0].CFAType)
	assert.Equal(t, int16(8), s.entries[0].CFAOffset)

	assert.Equal(t, uint64(0x1004), s.entries[1].PCStart)
	assert.Equal(t, uint32(8), s.entries[1].PCEndDelta)
	assert.Equal(t, CFATypeSP, s.entries[1].CFAType)
	assert.Equal(t, int16(16), s.entries[1].CFAOffset)
}

func TestInterpret_DefCFAWithFPOnARM64(t *testing.T) {
	// arm64 CIE: raColumn=30; def_cfa(x29, 16) → FP-based CFA.
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64X29, 16,
		0x40 | 4,
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x2000, 0x2004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	assert.Equal(t, CFATypeFP, s.entries[0].CFAType)
	assert.Equal(t, int16(16), s.entries[0].CFAOffset)
}
```

- [ ] **Step 2: Run, confirm failure.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret_DefCFA -v`
Expected: FAIL (opcodes unhandled).

- [ ] **Step 3: Add opcodes and rewrite snapshot.**

Modify `unwind/ehcompile/interpreter.go`. Add to the primary-opcode switch before the `default:` line:

```go
case cfaDefCFA:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	off, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setCFA(uint8(reg), int64(off))
case cfaDefCFASF:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	off, n, err := decodeSLEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setCFA(uint8(reg), off*s.cie.dataAlign)
case cfaDefCFARegister:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setCFAReg(uint8(reg))
case cfaDefCFAOffset:
	off, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.cfaOffset = int64(off)
	if s.cfaType != CFATypeUndefined {
		s.cfaRule = ruleSameValue
	}
case cfaDefCFAOffsetSF:
	off, n, err := decodeSLEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.cfaOffset = off * s.cie.dataAlign
	if s.cfaType != CFATypeUndefined {
		s.cfaRule = ruleSameValue
	}
```

And add helper methods and a real `snapshot()`:

```go
func (s *interpreter) setCFA(reg uint8, off int64) {
	s.setCFAReg(reg)
	s.cfaOffset = off
}

func (s *interpreter) setCFAReg(reg uint8) {
	s.cfaType = s.arch.cfaTypeFor(reg)
	if s.cfaType == CFATypeUndefined {
		s.cfaRule = ruleExpression // arch register we can't express
	} else {
		s.cfaRule = ruleSameValue
	}
}

func (s *interpreter) snapshot(newPC uint64) {
	if newPC <= s.lastEmittedPC {
		return
	}
	if s.cfaType == CFATypeUndefined && s.cfaRule != ruleExpression {
		s.lastEmittedPC = newPC
		return
	}

	cur := emittedState{
		cfaType:   s.cfaType,
		cfaOffset: s.cfaOffset,
		cfaRule:   s.cfaRule,
		fpRule:    s.fpRule,
		raRule:    s.raRule,
	}
	delta := uint32(newPC - s.lastEmittedPC)

	// Coalesce: if the last emitted row matches current state, extend it.
	if s.cfaRule != ruleExpression && len(s.entries) > 0 && cur == s.lastState {
		s.entries[len(s.entries)-1].PCEndDelta += delta
		s.classifications[len(s.classifications)-1].PCEndDelta += delta
		s.lastEmittedPC = newPC
		return
	}
	if s.cfaRule == ruleExpression && len(s.classifications) > 0 && cur == s.lastState {
		s.classifications[len(s.classifications)-1].PCEndDelta += delta
		s.lastEmittedPC = newPC
		return
	}

	if s.cfaRule == ruleExpression {
		s.classifications = append(s.classifications, Classification{
			PCStart:    s.lastEmittedPC,
			PCEndDelta: delta,
			Mode:       ModeFallback,
		})
	} else {
		s.entries = append(s.entries, CFIEntry{
			PCStart:    s.lastEmittedPC,
			PCEndDelta: delta,
			CFAType:    s.cfaType,
			FPType:     fpRuleToType(s.fpRule),
			CFAOffset:  int16(s.cfaOffset),
			FPOffset:   int16(s.fpRule.offset),
			RAType:     raRuleToType(s.raRule),
			RAOffset:   int16(s.raRule.offset),
		})
		mode := ModeFPLess
		if s.cfaType == CFATypeFP {
			mode = ModeFPSafe
		}
		s.classifications = append(s.classifications, Classification{
			PCStart:    s.lastEmittedPC,
			PCEndDelta: delta,
			Mode:       mode,
		})
	}
	s.lastState = cur
	s.lastEmittedPC = newPC
}

func fpRuleToType(r regRule) FPType {
	switch r.kind {
	case ruleOffset:
		return FPTypeOffsetCFA
	case ruleSameValue:
		return FPTypeSameValue
	case ruleRegister:
		return FPTypeRegister
	default:
		return FPTypeUndefined
	}
}

func raRuleToType(r regRule) RAType {
	switch r.kind {
	case ruleOffset:
		return RATypeOffsetCFA
	case ruleSameValue:
		return RATypeSameValue
	case ruleRegister:
		return RATypeRegister
	default:
		return RATypeUndefined
	}
}
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret -v`
Expected: both x86 and arm64 def_cfa tests pass.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/interpreter.go unwind/ehcompile/interpreter_test.go
git commit -m "ehcompile: def_cfa opcodes and row emission (x86_64 + arm64)"
```

---

## Task 11: Register-save opcodes — `offset`, `restore`, `same_value`, `undefined`, `register`

**Files:**
- Modify: `unwind/ehcompile/interpreter.go`
- Modify: `unwind/ehcompile/interpreter_test.go`

Track both FP (caller's frame pointer) AND RA (caller's return address) register-save rules.

- [ ] **Step 1: Add failing tests covering both FP and RA tracking.**

Append to `unwind/ehcompile/interpreter_test.go`:

```go
func TestInterpret_OffsetFPAndRA_x86(t *testing.T) {
	// def_cfa(rsp,16); offset(rbp,2); offset(RIP,1); advance(4).
	// data_align = -8, factor 2 → -16 for RBP; factor 1 → -8 for RA.
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 16,
		0x80 | x86RBP, 2,
		0x80 | x86RIP, 1,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x3000, 0x3004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	e := s.entries[0]
	assert.Equal(t, CFATypeSP, e.CFAType)
	assert.Equal(t, int16(16), e.CFAOffset)
	assert.Equal(t, FPTypeOffsetCFA, e.FPType)
	assert.Equal(t, int16(-16), e.FPOffset)
	assert.Equal(t, RATypeOffsetCFA, e.RAType)
	assert.Equal(t, int16(-8), e.RAOffset)
}

func TestInterpret_OffsetFPAndRA_arm64(t *testing.T) {
	// arm64 CIE: raColumn=30. def_cfa(sp,16); offset(x29,2); offset(x30,1); advance(4).
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64SP, 16,
		0x80 | arm64X29, 2,
		0x80 | arm64X30, 1,
		0x40 | 4,
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x4000, 0x4004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
	e := s.entries[0]
	assert.Equal(t, int16(-16), e.FPOffset)
	assert.Equal(t, int16(-8), e.RAOffset)
}
```

- [ ] **Step 2: Run, confirm failure.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret_OffsetFPAndRA -v`
Expected: FAIL — `cfaOffset` / `cfaRestore` still stubs.

- [ ] **Step 3: Implement.**

In `unwind/ehcompile/interpreter.go`, replace the compressed-opcode `cfaOffset` and `cfaRestore` stubs:

```go
case cfaOffset:
	reg := op & cfaOperandMask
	factor, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setRegOffset(reg, int64(factor)*s.cie.dataAlign)
	continue
case cfaRestore:
	s.restoreRegInitial(op & cfaOperandMask)
	continue
```

And add the primary-opcode cases (before the `default:`):

```go
case cfaOffsetExtended:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	factor, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setRegOffset(uint8(reg), int64(factor)*s.cie.dataAlign)
case cfaOffsetExtendedSF:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	factor, n, err := decodeSLEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setRegOffset(uint8(reg), factor*s.cie.dataAlign)
case cfaUndefined:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setRegRule(uint8(reg), regRule{kind: ruleUndefined})
case cfaSameValue:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setRegRule(uint8(reg), regRule{kind: ruleSameValue})
case cfaRegister:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	other, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.setRegRule(uint8(reg), regRule{kind: ruleRegister, register: uint8(other)})
case cfaRestoreExtended:
	reg, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	s.restoreRegInitial(uint8(reg))
```

And add helpers:

```go
// setRegOffset is the common path for DW_CFA_offset / offset_extended /
// offset_extended_sf. Updates rule only for registers we track (FP and RA).
func (s *interpreter) setRegOffset(reg uint8, offset int64) {
	s.setRegRule(reg, regRule{kind: ruleOffset, offset: offset})
}

// setRegRule routes a rule to the right slot based on which register
// we're tracking. Only FP (arch.fpReg) and RA (cie.raColumn) matter.
func (s *interpreter) setRegRule(reg uint8, r regRule) {
	switch {
	case reg == s.arch.fpReg:
		s.fpRule = r
	case uint64(reg) == s.cie.raColumn:
		s.raRule = r
	}
}

// restoreRegInitial resets the register's rule to undefined. Simplification:
// tracking CIE's initial rules precisely would help correctness in a few
// edge cases but matters little for CFA+FP+RA tracking. See corsix note.
func (s *interpreter) restoreRegInitial(reg uint8) {
	s.setRegRule(reg, regRule{kind: ruleUndefined})
}
```

- [ ] **Step 4: Run tests, confirm pass.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret -v`
Expected: PASS for all interpreter tests, including arm64.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/interpreter.go unwind/ehcompile/interpreter_test.go
git commit -m "ehcompile: offset/restore/same/undefined/register opcodes (FP + RA)"
```

---

## Task 12: `remember_state` / `restore_state` and expression opcodes

**Files:**
- Modify: `unwind/ehcompile/interpreter.go`
- Modify: `unwind/ehcompile/interpreter_test.go`

- [ ] **Step 1: Add failing tests.**

Append to `unwind/ehcompile/interpreter_test.go`:

```go
func TestInterpret_RememberRestoreState(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		0x40 | 2,
		cfaRememberState,
		cfaDefCFAOffset, 64,
		0x40 | 3,
		cfaRestoreState,
		0x40 | 5,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x5000, 0x500A, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 3)
	assert.Equal(t, int16(8), s.entries[0].CFAOffset)
	assert.Equal(t, int16(64), s.entries[1].CFAOffset)
	assert.Equal(t, int16(8), s.entries[2].CFAOffset)
}

func TestInterpret_ExpressionProducesFallback(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFAExpression, 1, 0x90,
		0x40 | 16,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x6000, 0x6010, program)
	require.NoError(t, err)
	assert.Empty(t, s.entries)
	require.Len(t, s.classifications, 1)
	assert.Equal(t, ModeFallback, s.classifications[0].Mode)
}

func TestInterpret_GnuArgsSizeIsNoop(t *testing.T) {
	c := newTestCIE()
	program := []byte{
		cfaDefCFA, x86RSP, 8,
		cfaGnuArgsSize, 0x10,
		0x40 | 4,
	}
	s := newInterpreter(c, archX86_64())
	err := s.run(0x7000, 0x7004, program)
	require.NoError(t, err)
	require.Len(t, s.entries, 1)
}
```

- [ ] **Step 2: Run, confirm failure.**

Run: `go test ./unwind/ehcompile/ -run TestInterpret -v`
Expected: FAIL on the three new tests.

- [ ] **Step 3: Implement.**

Add to the primary-opcode switch in `unwind/ehcompile/interpreter.go`:

```go
case cfaRememberState:
	if s.sp >= len(s.stack) {
		return errors.New("ehcompile: remember_state stack overflow")
	}
	s.stack[s.sp] = savedState{
		cfaType:   s.cfaType,
		cfaOffset: s.cfaOffset,
		cfaRule:   s.cfaRule,
		fpRule:    s.fpRule,
		raRule:    s.raRule,
	}
	s.sp++
case cfaRestoreState:
	if s.sp == 0 {
		return errors.New("ehcompile: restore_state on empty stack")
	}
	s.sp--
	ss := s.stack[s.sp]
	s.cfaType = ss.cfaType
	s.cfaOffset = ss.cfaOffset
	s.cfaRule = ss.cfaRule
	s.fpRule = ss.fpRule
	s.raRule = ss.raRule
case cfaDefCFAExpression:
	length, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n + int(length)
	s.cfaRule = ruleExpression
	s.cfaType = CFATypeUndefined
case cfaExpression, cfaValExpression:
	_, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	length, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n + int(length)
	s.cfaRule = ruleExpression
case cfaSetLoc:
	newPC, n, err := decodeEHPointer(program[pos:], s.cie.fdePointerEnc, 0, 0)
	if err != nil {
		return err
	}
	pos += n
	if newPC < s.pc {
		return errors.New("ehcompile: set_loc moves backward")
	}
	s.snapshotAndAdvance(newPC - s.pc)
case cfaGnuArgsSize:
	_, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
case cfaValOffset, cfaValOffsetSF:
	_, n, err := decodeULEB128(program[pos:])
	if err != nil {
		return err
	}
	pos += n
	if op == cfaValOffset {
		_, n, err = decodeULEB128(program[pos:])
	} else {
		_, n, err = decodeSLEB128(program[pos:])
	}
	if err != nil {
		return err
	}
	pos += n
	s.cfaRule = ruleExpression
```

- [ ] **Step 4: Run, confirm pass.**

Run: `go test ./unwind/ehcompile/ -v`
Expected: all interpreter tests pass.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/interpreter.go unwind/ehcompile/interpreter_test.go
git commit -m "ehcompile: remember/restore_state, expressions, GNU args_size, set_loc"
```

---

## Task 13: `Compile()` with arch dispatch and ELF parsing

**Files:**
- Modify: `unwind/ehcompile/ehcompile.go`
- Modify: `unwind/ehcompile/ehcompile_test.go`

- [ ] **Step 1: Add a failing end-to-end test using a system binary.**

Append to `unwind/ehcompile/ehcompile_test.go`:

```go
import (
	"os"

	"github.com/stretchr/testify/assert"
)

func TestCompile_SystemBinary(t *testing.T) {
	entries, classes, err := Compile("/bin/true")
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	assert.NotEmpty(t, classes)
	for i := 1; i < len(entries); i++ {
		assert.LessOrEqual(t, entries[i-1].PCStart, entries[i].PCStart,
			"entry %d out of order", i)
	}
}
```

- [ ] **Step 2: Run, confirm failure ("not implemented").**

Run: `go test ./unwind/ehcompile/ -run TestCompile_SystemBinary -v`
Expected: FAIL.

- [ ] **Step 3: Implement `Compile()`.**

Replace `unwind/ehcompile/ehcompile.go`:

```go
package ehcompile

import (
	"debug/elf"
	"errors"
	"fmt"
	"sort"
)

var ErrNoEHFrame = errors.New("ehcompile: no .eh_frame section")
var ErrUnsupportedArch = errors.New("ehcompile: unsupported ELF machine type")

func Compile(elfPath string) (entries []CFIEntry, classifications []Classification, err error) {
	f, err := elf.Open(elfPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open elf: %w", err)
	}
	defer f.Close()

	arch, err := archFromELFMachine(f.Machine)
	if err != nil {
		return nil, nil, err
	}

	sec := f.Section(".eh_frame")
	if sec == nil {
		return nil, nil, ErrNoEHFrame
	}
	data, err := sec.Data()
	if err != nil {
		return nil, nil, fmt.Errorf("read .eh_frame: %w", err)
	}
	sectionPos := sec.Addr

	var allEntries []CFIEntry
	var allClasses []Classification

	err = walkEHFrame(data, sectionPos, func(off uint64, c *cie, fd *fde) error {
		if fd == nil {
			return nil
		}
		interp := newInterpreter(fd.cie, arch)
		// CIE's initial instructions seed state without emitting rows
		// (they're evaluated with PC == initialLocation, which equals
		// the interpreter's lastEmittedPC).
		if err := interp.run(fd.initialLocation, fd.initialLocation, fd.cie.initialInstructions); err != nil {
			return fmt.Errorf("CIE init at PC 0x%x: %w", fd.initialLocation, err)
		}
		interp.lastEmittedPC = fd.initialLocation
		if err := interp.run(fd.initialLocation, fd.initialLocation+fd.addressRange, fd.instructions); err != nil {
			return fmt.Errorf("FDE at PC 0x%x: %w", fd.initialLocation, err)
		}
		allEntries = append(allEntries, interp.entries...)
		allClasses = append(allClasses, interp.classifications...)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	sort.Slice(allEntries, func(i, j int) bool { return allEntries[i].PCStart < allEntries[j].PCStart })
	sort.Slice(allClasses, func(i, j int) bool { return allClasses[i].PCStart < allClasses[j].PCStart })

	return allEntries, allClasses, nil
}
```

- [ ] **Step 4: Run test.**

Run: `go test ./unwind/ehcompile/ -run TestCompile_SystemBinary -v`
Expected: PASS. Failure most likely comes from unhandled opcodes inside glibc's portions compiled into `/bin/true`; read the error message and extend the interpreter.

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/ehcompile.go unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: Compile() end-to-end with arch dispatch"
```

---

## Task 14: x86_64 snapshot test with committed fixture

**Files:**
- Create: `unwind/ehcompile/testdata/hello.c`
- Create: `unwind/ehcompile/testdata/README.md`
- Create: `unwind/ehcompile/testdata/hello` (committed x86_64 binary)
- Create: `unwind/ehcompile/testdata/hello.golden` (committed expected output)
- Modify: `unwind/ehcompile/ehcompile_test.go`

- [ ] **Step 1: Write the fixture source.**

Write to `unwind/ehcompile/testdata/hello.c`:

```c
// Fixture for ehcompile snapshot tests. Deliberately simple — one main
// that calls a leaf function — so the resulting .eh_frame is small and
// the golden file stays readable.
#include <stdio.h>

__attribute__((noinline))
int leaf(int x) {
    return x * 2 + 1;
}

int main(int argc, char **argv) {
    (void)argv;
    return leaf(argc);
}
```

Write to `unwind/ehcompile/testdata/README.md`:

```markdown
# ehcompile test fixtures

## x86_64: hello / hello.golden

Trivial C binary for `TestCompile_GoldenFile_x86`.

Regenerate:
```
gcc -O0 -fno-omit-frame-pointer -o testdata/hello testdata/hello.c
go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_x86 -update
```

## arm64: hello_arm64 / hello_arm64.golden

Cross-compiled arm64 binary. Regenerate (requires aarch64-linux-gnu-gcc):
```
aarch64-linux-gnu-gcc -O0 -o testdata/hello_arm64 testdata/hello.c
go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_arm64 -update
```

If the arm64 toolchain isn't installed, the arm64 test skips cleanly.
```

- [ ] **Step 2: Compile the x86_64 fixture.**

Run:
```bash
gcc -O0 -fno-omit-frame-pointer -o unwind/ehcompile/testdata/hello unwind/ehcompile/testdata/hello.c
```
Expected: small ELF at that path.

- [ ] **Step 3: Add the golden test harness.**

Append to `unwind/ehcompile/ehcompile_test.go`:

```go
import (
	"encoding/json"
	"flag"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

type goldenFile struct {
	Entries         []CFIEntry       `json:"entries"`
	Classifications []Classification `json:"classifications"`
}

func runGolden(t *testing.T, elfPath, goldenPath string) {
	t.Helper()
	if _, err := os.Stat(elfPath); err != nil {
		t.Skipf("fixture missing: %s", elfPath)
	}
	entries, classes, err := Compile(elfPath)
	require.NoError(t, err)
	got := goldenFile{Entries: entries, Classifications: classes}

	if *updateGolden {
		f, err := os.Create(goldenPath)
		require.NoError(t, err)
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		require.NoError(t, enc.Encode(got))
		t.Logf("golden file updated: %s", goldenPath)
		return
	}

	raw, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden file missing — regenerate with -update")
	var want goldenFile
	require.NoError(t, json.Unmarshal(raw, &want))
	assert.Equal(t, want, got)
}

func TestCompile_GoldenFile_x86(t *testing.T) {
	runGolden(t, "testdata/hello", "testdata/hello.golden")
}
```

- [ ] **Step 4: Generate the initial golden file.**

Run: `go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_x86 -update -v`
Expected: PASS, `hello.golden` appears.

Cross-check by running `readelf --debug-dump=frames-interp unwind/ehcompile/testdata/hello` and confirming the `main` and `leaf` FDEs' CFA rules match the JSON.

- [ ] **Step 5: Re-run without -update.**

Run: `go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_x86 -v`
Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add unwind/ehcompile/testdata/
git add unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: x86_64 snapshot test with committed fixture"
```

---

## Task 15: arm64 cross-compiled fixture (optional — skips if toolchain absent)

**Files:**
- Create: `unwind/ehcompile/testdata/hello_arm64` (committed if toolchain available)
- Create: `unwind/ehcompile/testdata/hello_arm64.golden`
- Modify: `unwind/ehcompile/ehcompile_test.go`

- [ ] **Step 1: Attempt to cross-compile.**

Run:
```bash
which aarch64-linux-gnu-gcc || sudo dnf install -y gcc-aarch64-linux-gnu || echo "toolchain unavailable"
aarch64-linux-gnu-gcc -O0 -o unwind/ehcompile/testdata/hello_arm64 unwind/ehcompile/testdata/hello.c 2>&1 || echo "cross-compile failed"
ls -la unwind/ehcompile/testdata/hello_arm64 2>/dev/null || echo "no arm64 fixture produced"
```

- [ ] **Step 2: Add the arm64 golden test (skips if fixture missing).**

Append to `unwind/ehcompile/ehcompile_test.go`:

```go
func TestCompile_GoldenFile_arm64(t *testing.T) {
	runGolden(t, "testdata/hello_arm64", "testdata/hello_arm64.golden")
}
```

- [ ] **Step 3: If the fixture exists, generate golden and commit. Otherwise document the skip.**

If `unwind/ehcompile/testdata/hello_arm64` exists:
```bash
go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_arm64 -update -v
go test ./unwind/ehcompile/ -run TestCompile_GoldenFile_arm64 -v
git add unwind/ehcompile/testdata/hello_arm64 unwind/ehcompile/testdata/hello_arm64.golden
git add unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: arm64 snapshot test with cross-compiled fixture"
```

If not:
```bash
git add unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: arm64 snapshot test (skips without aarch64-linux-gnu-gcc)"
```

---

## Task 16: arm64 synthetic bytecode coverage (host-independent)

**Files:**
- Modify: `unwind/ehcompile/interpreter_test.go`

Even without a real arm64 fixture, exercise arm64 CIE/FDE through handcrafted bytes to confirm the interpreter behaves correctly for arm64 register numbers.

- [ ] **Step 1: Add failing tests.**

Append to `unwind/ehcompile/interpreter_test.go`:

```go
func TestInterpret_ARM64_TypicalPrologue(t *testing.T) {
	// Models a typical arm64 function prologue:
	//   stp x29, x30, [sp, #-16]!   (push FP + LR, decrement SP by 16)
	//   mov x29, sp                  (FP = SP)
	// CFI for this typically emits:
	//   def_cfa(sp, 0)
	//   advance_loc(N)
	//   def_cfa_offset_sf(2)          -- CFA = SP + 16 (2 * dataAlign=-8... wait, we want +16)
	//   offset(x29, 2)                -- x29 saved at CFA-16
	//   offset(x30, 1)                -- x30 saved at CFA-8
	//   advance_loc(M)
	//   def_cfa_register(x29)         -- CFA = x29 + 16
	//
	// data_alignment_factor on arm64 is -8 typically; with def_cfa_offset
	// the operand is unsigned, so we pass 16 directly.
	c := &cie{version: 1, codeAlign: 1, dataAlign: -8, raColumn: 30}
	program := []byte{
		cfaDefCFA, arm64SP, 0,
		0x40 | 4,                     // advance_loc(4)
		cfaDefCFAOffset, 16,
		0x80 | arm64X29, 2,           // offset(x29, 2) → -16
		0x80 | arm64X30, 1,           // offset(x30, 1) → -8
		0x40 | 4,                     // advance_loc(4)
		cfaDefCFARegister, arm64X29,
		0x40 | 16,                    // advance_loc(16)
	}
	s := newInterpreter(c, archARM64())
	err := s.run(0x5000, 0x5018, program)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(s.entries), 2)

	// One row should have CFA=FP (x29)+16 — that's the main body.
	var found bool
	for _, e := range s.entries {
		if e.CFAType == CFATypeFP && e.CFAOffset == 16 {
			found = true
			assert.Equal(t, FPTypeOffsetCFA, e.FPType)
			assert.Equal(t, int16(-16), e.FPOffset)
			assert.Equal(t, RATypeOffsetCFA, e.RAType)
			assert.Equal(t, int16(-8), e.RAOffset)
		}
	}
	assert.True(t, found, "expected a row with CFA=FP+16")
}
```

- [ ] **Step 2: Run, confirm pass (interpreter already handles all needed opcodes).**

Run: `go test ./unwind/ehcompile/ -run TestInterpret_ARM64 -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add unwind/ehcompile/interpreter_test.go
git commit -m "ehcompile: arm64 synthetic-bytecode coverage for prologue shape"
```

---

## Task 17: Integration test against system glibc

**Files:**
- Modify: `unwind/ehcompile/ehcompile_test.go`

- [ ] **Step 1: Add failing test.**

Append to `unwind/ehcompile/ehcompile_test.go`:

```go
func TestCompile_SystemGlibc(t *testing.T) {
	candidates := []string{
		"/lib64/libc.so.6",
		"/lib/x86_64-linux-gnu/libc.so.6",
		"/usr/lib64/libc.so.6",
	}
	var path string
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			path = p
			break
		}
	}
	if path == "" {
		t.Skip("no system libc found")
	}
	entries, classes, err := Compile(path)
	require.NoError(t, err)
	assert.Greater(t, len(entries), 1000)

	var fallback int
	for _, c := range classes {
		if c.Mode == ModeFallback {
			fallback++
		}
	}
	t.Logf("glibc: %d entries, %d classes, %d FALLBACK", len(entries), len(classes), fallback)
	assert.Less(t, float64(fallback)/float64(len(classes)), 0.02)
}
```

- [ ] **Step 2: Run.**

Run: `go test ./unwind/ehcompile/ -run TestCompile_SystemGlibc -v`
Expected: PASS with a log line. Failure points at unhandled opcode — read the error and fix in the interpreter.

- [ ] **Step 3: Commit.**

```bash
git add unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: integration test against system glibc"
```

---

## Task 18: Integration test against a Go binary

**Files:**
- Modify: `unwind/ehcompile/ehcompile_test.go`

- [ ] **Step 1: Add test.**

Append to `unwind/ehcompile/ehcompile_test.go`:

```go
func TestCompile_GoBinary(t *testing.T) {
	path := "/usr/bin/go"
	if _, err := os.Stat(path); err != nil {
		t.Skip("/usr/bin/go not found")
	}
	entries, _, err := Compile(path)
	require.NoError(t, err)
	assert.NotEmpty(t, entries)
	t.Logf("go binary: %d entries", len(entries))
}
```

- [ ] **Step 2: Run.**

Run: `go test ./unwind/ehcompile/ -run TestCompile_GoBinary -v`
Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add unwind/ehcompile/ehcompile_test.go
git commit -m "ehcompile: integration test against a Go binary"
```

---

## Task 19: Benchmark and package docstring

**Files:**
- Create: `unwind/ehcompile/ehcompile_bench_test.go`
- Modify: `unwind/ehcompile/types.go`

- [ ] **Step 1: Add benchmark.**

Write to `unwind/ehcompile/ehcompile_bench_test.go`:

```go
package ehcompile

import (
	"os"
	"testing"
)

func BenchmarkCompile_Glibc(b *testing.B) {
	path := "/lib64/libc.so.6"
	if _, err := os.Stat(path); err != nil {
		b.Skip("/lib64/libc.so.6 not found")
	}
	b.ResetTimer()
	for b.Loop() {
		_, _, err := Compile(path)
		if err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Run the benchmark.**

Run: `go test ./unwind/ehcompile/ -bench BenchmarkCompile_Glibc -benchmem -run=^$`
Expected: single-line report. Target: < 50 ms per Compile. Optimize only if significantly slower.

- [ ] **Step 3: Finalize the package docstring.**

In `unwind/ehcompile/types.go`, replace the package comment block at the top with:

```go
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
//     FALLBACK (complex rule — BPF falls back to FP walking).
//
// Architectures:
//
//   - x86_64 (EM_X86_64): SP=RSP, FP=RBP, RA column=16.
//   - arm64  (EM_AARCH64): SP=SP, FP=x29, RA column=30 (LR).
//   - Others rejected with ErrUnsupportedArch.
//
// CFI dialect supported:
//
//   - Simple CFA rules: def_cfa / def_cfa_register / def_cfa_offset /
//     def_cfa_offset_sf / def_cfa_sf. Only SP and FP (per-arch) produce
//     CFIEntry rows; other registers → FALLBACK.
//   - Register saves: offset / offset_extended / offset_extended_sf /
//     restore / restore_extended / same_value / undefined / register.
//     Only FP and RA are tracked; other registers are ignored.
//   - State stack: remember_state / restore_state (16 deep).
//   - Expressions: def_cfa_expression / expression / val_expression
//     → FALLBACK for the covered PC range, no CFIEntry.
//   - PC advance: advance_loc (compressed), advance_loc1/2/4, set_loc.
//   - GNU extensions: GNU_args_size (consumed, no effect).
//
// Out of scope:
//
//   - DW_EH_PE_indirect pointer encoding.
//   - DW_CFA_val_offset register saving.
//   - .debug_frame (different layout from .eh_frame).
//
// See docs/dwarf-unwinding-design.md for the broader BPF-side architecture.
package ehcompile
```

- [ ] **Step 4: Run all tests one final time.**

Run: `go test ./unwind/ehcompile/ -v`
Expected: all tests pass (or skip cleanly, for arm64 fixture if toolchain missing).

- [ ] **Step 5: Commit.**

```bash
git add unwind/ehcompile/ehcompile_bench_test.go unwind/ehcompile/types.go
git commit -m "ehcompile: benchmark and finalized package docstring"
```

---

## Self-Review

### Spec coverage

| Spec section | Task |
|---|---|
| Output types (CFIEntry with FP + RA fields; Classification; Mode) | 1 |
| archInfo + x86_64/arm64 register maps | 5 |
| ULEB128/SLEB128 | 2 |
| DW_EH_PE_* pointer encoding | 3 |
| DW_CFA_* opcode constants | 4 |
| DWARF register numbers (x86_64 + arm64) | 4 |
| CIE augmentation handling | 6 |
| FDE parsing (backward CIE pointer) | 7 |
| `.eh_frame` walking | 8 |
| Interpreter skeleton + advance_loc | 9 |
| def_cfa family (x86_64 + arm64) | 10 |
| FP + RA tracking via offset family | 11 |
| remember_state / restore_state | 12 |
| Expression opcodes → FALLBACK | 12 |
| GNU_args_size | 12 |
| set_loc | 12 |
| Compile() + arch dispatch | 13 |
| x86_64 golden fixture | 14 |
| arm64 cross-compiled fixture | 15 |
| arm64 synthetic bytecode tests | 16 |
| System glibc stress test | 17 |
| Go binary test | 18 |
| Benchmark | 19 |
| Row coalescing | integrated into Task 10's snapshot() |
| FP-safe/FP-less/FALLBACK classification per row | integrated into Task 10's snapshot() |

### Placeholder scan

No "TODO", "TBD", "fill in", or "similar to above". Every code block is complete. Every command has expected output. Arm64 fixture task explicitly handles the "toolchain not available" case rather than hand-waving.

### Type consistency

- `CFIEntry`: PCStart, PCEndDelta, CFAType, FPType, CFAOffset, FPOffset, RAOffset, RAType — used identically throughout.
- `Classification`: PCStart, PCEndDelta, Mode — consistent.
- Enum values: `CFATypeSP=1`, `CFATypeFP=2`, `FPTypeOffsetCFA=1`, `RATypeOffsetCFA=1`, `ModeFPSafe=0`, `ModeFPLess=1`, `ModeFallback=2` — all consistent across tasks.
- `archInfo` fields: name, spReg, fpReg, raReg — consistent.
- `interpreter` fields: pc, cfaType, cfaOffset, cfaRule, fpRule, raRule, arch, cie, entries, classifications, stack, sp, lastEmittedPC, lastState — consistent.
- Function names consistent: `newInterpreter`, `run`, `snapshot`, `snapshotAndAdvance`, `setCFA`, `setCFAReg`, `setRegOffset`, `setRegRule`, `restoreRegInitial`, `fpRuleToType`, `raRuleToType`, `archX86_64`, `archARM64`, `archFromELFMachine`, `cfaTypeFor`, `parseCIE`, `parseFDE`, `walkEHFrame`, `decodeULEB128`, `decodeSLEB128`, `decodeEHPointer`, `Compile`.

### Non-obvious correctness notes

1. **Task 13's Compile uses `arch` per-ELF**, not a package global. One `Compile()` invocation on an x86 binary and another on an arm64 binary in the same process both work independently.
2. **Task 11's `setRegRule` checks `reg == arch.fpReg` and `uint64(reg) == cie.raColumn`.** The FP register is arch-fixed; the RA column is CIE-declared. This separation is important — even within a single arch, theoretically different CIEs could use different RA columns (though in practice they don't).
3. **Task 12's `cfaDefCFAExpression` sets `cfaRule = ruleExpression` AND `cfaType = CFATypeUndefined`** — both are needed because `snapshot()` checks `cfaType == CFATypeUndefined && cfaRule != ruleExpression` to decide whether to emit anything.
4. **Task 10's `snapshot()` dedup uses `cur == s.lastState`** — Go's struct equality recurses into the nested `regRule` structs, giving exact "state unchanged since last emit" semantics.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-22-ehcompile.md`. Two execution options:

**1. Subagent-Driven (recommended)** — dispatch a fresh subagent per task, review between tasks, fast iteration. Good fit for 19 tasks.

**2. Inline Execution** — execute tasks in this session using executing-plans, batch execution with checkpoints.

**Which approach?**
