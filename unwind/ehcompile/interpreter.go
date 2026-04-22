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
