package dwarfagent

import (
	"sync/atomic"

	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/interp"
)

// Issue #83: the walk this agent consumes is no longer a flat, uniformly
// native PC array. A frame produced by an interpreter unwinder occupies TWO
// consecutive pcs[] slots -- for CPython the code object's address and an
// encoded instruction word -- both tagged with that unwinder's id, and NEITHER
// is an instruction pointer. Handing either to blazesym asks it to name an
// address that is not code, and it will place it in whatever mapping the
// address happens to fall in and return a frame: two plausible, wrong native
// frames, silently.
//
// The folding and the naming both live in unwind/interp, which knows about
// tagged slots and nothing about any language. This file is the counting: the
// units below are per-SESSION accounting that only this agent can do.

// InterpFrameStats summarizes what the tagged-slot decoder has seen across
// this process. Package-level rather than per-session because the decoder is a
// stateless function with no session to hang counters on.
type InterpFrameStats struct {
	// Frames is the number of interpreter frames seen, counted once per
	// SAMPLE (in the ringbuf aggregator, where each sample is decoded exactly
	// once) rather than once per unique stack at collect time. That unit is
	// chosen so this number is directly comparable to the BPF side's
	// per-frame push counter, which is also per frame per sample; counting at
	// collect time would weight a hot stack the same as a stack seen once.
	// It is what answers "is the handoff reaching the DWARF path at all",
	// which no other number here can say.
	Frames uint64
	// TruncatedPairs counts walks that ended between the two slots of an
	// interpreter frame -- MAX_FRAMES landed mid-pair, so the first word
	// arrived and its partner did not. The half frame is dropped rather than
	// half-read, and this stops that drop being silent. Same unit as Frames:
	// once per sample.
	TruncatedPairs uint64
	// SymbolizerCountMismatch counts stacks whose splice fell back to
	// placeholders because the symbolizer returned a different number of
	// frames than it was given IPs. symbolize/local.go documents one Frame
	// per IP; if that ever stops holding, a positional splice would drop the
	// tail natives from the call path, and a short stack with nothing saying
	// so is worse than no stack.
	//
	// Unit: once per UNIQUE STACK, at collect time -- symbolization happens
	// there, not per sample. Deliberately different from the two above and
	// labelled rather than silently mixed.
	SymbolizerCountMismatch uint64
}

var (
	interpFrames            atomic.Uint64
	interpTruncatedPairs    atomic.Uint64
	interpSymbolizerCountMM atomic.Uint64
)

// InterpFrameCounters returns a snapshot of the tagged-slot decoder's
// counters.
func InterpFrameCounters() InterpFrameStats {
	return InterpFrameStats{
		Frames:                  interpFrames.Load(),
		TruncatedPairs:          interpTruncatedPairs.Load(),
		SymbolizerCountMismatch: interpSymbolizerCountMM.Load(),
	}
}

// countInterpSlots records one decoded sample against the per-sample counters.
// Called from the ringbuf aggregators, which see every sample exactly once --
// not from collect, which sees each unique stack once regardless of how many
// samples hit it.
func countInterpSlots(slots []interp.Slot, truncatedPair bool) {
	if truncatedPair {
		interpTruncatedPairs.Add(1)
	}
	var n uint64
	for _, sl := range slots {
		if sl.IsInterp() {
			n++
		}
	}
	if n > 0 {
		interpFrames.Add(n)
	}
}

// interpSymbolizeFrame is the symbolize.Frame an interpreter slot becomes.
//
// NameFor rather than Name: naming a Python frame means reading the code
// object out of the live process, and this runs during collect while the
// target may still be there. What it reads is cached, so the name survives the
// process; what it cannot read stays the address form.
//
// Reason follows what actually happened. A frame the resolver named IS
// resolved, and marking it FailureMissingSymbols anyway would render
// "Widget.method_here (train.py:42)" hatched as unsymbolized -- and, worse,
// let pprof's builder overwrite the name with module+offset, since that path
// fires on the unresolved bit alone.
func interpSymbolizeFrame(pid uint32, sl interp.Slot) symbolize.Frame {
	name := sl.NameFor(pid)
	reason := symbolize.FailureMissingSymbols
	if name != sl.Name() {
		reason = symbolize.FailureNone
	}
	return symbolize.Frame{
		Address: sl.PC,
		Name:    name,
		Reason:  reason,
	}
}
