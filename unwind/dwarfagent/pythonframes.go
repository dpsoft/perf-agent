package dwarfagent

import (
	"fmt"
	"sync/atomic"

	"github.com/dpsoft/perf-agent/symbolize"
)

// Issue #83, ruling T7-R5: the interpreter arm lives inside walk_step, so
// perf_dwarf and offcpu_dwarf inherit it from the same bpf_loop gpu_usdt
// does. Every consumer of a walk therefore has to understand tagged slots,
// not just the GPU one.
//
// A Python frame occupies TWO consecutive pcs[] slots -- the code object's
// address in the target, then an encoded instruction word -- both tagged
// frameTagPython. Neither is an instruction pointer. Handing either to
// blazesym asks it to name an address that is not code, and it will place it
// in whatever mapping the address happens to fall in and return a frame: two
// plausible, wrong native frames, silently. That is the same defect closed on
// the GPU path in gpuprobe/consumer.go, and it is closed here the same way.

const (
	// frameTagNative and frameTagPython mirror FRAME_TAG_* in
	// bpf/unwind_common.h.
	frameTagNative = 0
	frameTagPython = 1
)

// frameSlot is one decoded pcs[]/tags[] position: a native instruction
// pointer, or a Python frame with the two words that describe it folded back
// together.
type frameSlot struct {
	python bool
	pc     uint64 // native PC, or the Python code object's address
	instr  uint64 // Python only: the encoded instruction word
}

// PythonFrameStats summarizes what the tagged-slot decoder has seen across
// this process. Package-level rather than per-session because the decoder is
// a stateless function, mirroring profile.FrameDecodeCounters.
type PythonFrameStats struct {
	// Frames is the number of Python frames seen, counted once per SAMPLE
	// (in the ringbuf aggregator, where each sample is decoded exactly once)
	// rather than once per unique stack at collect time. That unit is chosen
	// so this number is directly comparable to the BPF side's
	// PY_CNT_FRAMES_PUSHED, which is also per frame per sample; counting at
	// collect time would weight a hot stack the same as a stack seen once.
	// It is what answers "is the interpreter arm reaching the DWARF path at
	// all", which no other number here can say.
	Frames uint64
	// TruncatedPairs counts walks that ended between the two slots of a
	// Python frame -- MAX_FRAMES landed mid-pair, so the code object arrived
	// and its instruction word did not. The half frame is dropped rather
	// than half-read, and this stops that drop being silent. Same unit as
	// Frames: once per sample.
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
	pythonFrames            atomic.Uint64
	pythonTruncatedPairs    atomic.Uint64
	pythonSymbolizerCountMM atomic.Uint64
)

// PythonFrameCounters returns a snapshot of the tagged-slot decoder's
// counters.
func PythonFrameCounters() PythonFrameStats {
	return PythonFrameStats{
		Frames:                  pythonFrames.Load(),
		TruncatedPairs:          pythonTruncatedPairs.Load(),
		SymbolizerCountMismatch: pythonSymbolizerCountMM.Load(),
	}
}

// splitFrameSlots folds the parallel pcs[]/tags[] arrays into one ordered
// list of frames -- ONE entry per frame, not per slot.
//
// Order is preserved and that is the whole point: separating natives from
// Python frames into two lists would lose which native frame each Python
// frame sat above, and a stack whose Python frames are all piled at one end
// is a plausible call path that never happened.
//
// A nil or short tags slice is treated as all-native from that point on,
// which is what a record written before tags existed looks like.
//
// truncatedPair reports a trailing frameTagPython slot whose partner word
// fell off the end of the walk. The half frame is dropped rather than
// half-read -- a code object with a garbage instruction word is worse than no
// frame.
//
// This function is PURE: it touches no counter. It is called from more than
// one place (the ringbuf aggregator, and the perf.data export beside it), and
// a counter inside it would report a number that depends on how many callers
// happen to exist rather than on how many walks were truncated. The caller
// that decodes each sample exactly once does the counting -- see
// countPythonSlots.
func splitFrameSlots(pcs []uint64, tags []uint8) (slots []frameSlot, truncatedPair bool) {
	slots = make([]frameSlot, 0, len(pcs))
	for i := 0; i < len(pcs); i++ {
		if i >= len(tags) || tags[i] != frameTagPython {
			slots = append(slots, frameSlot{pc: pcs[i]})
			continue
		}
		if i+1 >= len(pcs) {
			return slots, true
		}
		slots = append(slots, frameSlot{python: true, pc: pcs[i], instr: pcs[i+1]})
		i++
	}
	return slots, false
}

// countPythonSlots records one decoded sample against the per-sample
// counters. Called from the ringbuf aggregators, which see every sample
// exactly once -- not from collect, which sees each unique stack once
// regardless of how many samples hit it.
func countPythonSlots(slots []frameSlot, truncatedPair bool) {
	if truncatedPair {
		pythonTruncatedPairs.Add(1)
	}
	var n uint64
	for _, sl := range slots {
		if sl.python {
			n++
		}
	}
	if n > 0 {
		pythonFrames.Add(n)
	}
}

// nativeIPs returns the instruction pointers from slots, in order. These are
// the only words that may reach the symbolizer or a perf.data user-IP stream.
func nativeIPs(slots []frameSlot) []uint64 {
	out := make([]uint64, 0, len(slots))
	for _, sl := range slots {
		if !sl.python {
			out = append(out, sl.pc)
		}
	}
	return out
}

// pythonFrameName renders an unsymbolized Python frame. Turning the code
// object into a file, a function and a line is slice 3; until then these
// carry the address, exactly as an unresolved native frame does. The prefix
// is what makes the frame legible as Python rather than as a stray pointer.
//
// Kept identical to gpuprobe's rendering on purpose: the same frame reaching
// a user through two different profilers must not have two different names.
func pythonFrameName(codeObject uint64) string {
	return fmt.Sprintf("python:%#x", codeObject)
}

// pythonSymbolizeFrame is the symbolize.Frame a Python slot becomes.
//
// Reason is deliberately not FailureNone: the frame is placed correctly and
// its address is real, but nothing named it, and pprof has no unsymbolized
// bit of its own to infer that from after the conversion.
func pythonSymbolizeFrame(sl frameSlot) symbolize.Frame {
	return symbolize.Frame{
		Address: sl.pc,
		Name:    pythonFrameName(sl.pc),
		Reason:  symbolize.FailureMissingSymbols,
	}
}
