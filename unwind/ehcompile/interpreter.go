package ehcompile

import (
	"encoding/binary"
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

// snapshot emits a row covering [lastEmittedPC, newPC) with the current
// state. Adjacent rows with identical rules are coalesced.
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
// edge cases but matters little for CFA+FP+RA tracking.
func (s *interpreter) restoreRegInitial(reg uint8) {
	s.setRegRule(reg, regRule{kind: ruleUndefined})
}
