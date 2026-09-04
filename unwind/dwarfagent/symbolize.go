package dwarfagent

import (
	"log"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
	"github.com/dpsoft/perf-agent/unwind/interp"
)

// symbolizePID resolves one walk's slot words for pid and returns pprof
// frames in the same order. Failed IPs contribute a single synthetic
// "[unknown]" frame carrying the original PC as Address.
//
// slots is the walk decoded by interp.SplitSlots (issue #83), NOT a flat list of
// instruction pointers: a Python frame occupies two consecutive pcs[] slots
// holding a code-object address and an encoded instruction word, folded into
// one slot here, and handing either word to blazesym asks it to name an
// address that is not code — it will place it in whatever mapping it falls in
// and hand back a frame, so the stack gains two plausible, wrong native
// frames and nothing says so. Only native slots reach the symbolizer; Python
// slots are spliced back at their own position, rendered as addresses until
// slice 3 can read a code object.
//
// The splice happens at the symbolize.Frame level, where the correspondence
// with the native list is still one-to-one. After ToProfFrames it is not:
// that call expands inline chains, so there is no index left to splice
// against.
func symbolizePID(sym symbolize.Symbolizer, pid uint32, slots []interp.Slot) []pprof.Frame {
	if len(slots) == 0 {
		return nil
	}
	native := interp.NativeIPs(slots)

	var frames []symbolize.Frame
	// placeholders is carried explicitly rather than inferred later from an
	// empty Name: "the symbolizer gave us nothing usable" and "the symbolizer
	// named this frame with an empty string" are different facts, and a
	// sentinel that conflates them is how an unresolved frame starts reading
	// as a resolved one.
	placeholders := false
	if len(native) > 0 {
		var err error
		frames, err = sym.SymbolizeProcess(pid, native)
		if err != nil {
			log.Printf("dwarfagent: symbolize: %v", err)
		}
		// symbolize/local.go documents one Frame per IP, and the splice below
		// indexes positionally against `native` on that promise. A count that
		// disagrees is not something to splice around: it would drop the tail
		// natives from the call path, leaving a short stack in the right order
		// with nothing saying it is short. The condition is a superset of the
		// one this function used before it learned about tags (err != nil ||
		// len(frames) == 0), so the pre-existing failure rendering is
		// unchanged; only the genuinely new case is counted.
		if err != nil || len(frames) != len(native) {
			if err == nil && len(frames) != 0 {
				interpSymbolizerCountMM.Add(1)
			}
			placeholders = true
		}
	}

	out := make([]pprof.Frame, 0, len(slots))
	next := 0
	for _, sl := range slots {
		if sl.IsInterp() {
			// Not counted here: PythonFrameStats.Frames is a per-SAMPLE
			// number and collect() runs once per unique stack. The
			// aggregators count, via countInterpSlots.
			out = append(out, symbolize.ToProfFrames(
				[]symbolize.Frame{interpSymbolizeFrame(pid, sl)})...)
			continue
		}
		if placeholders {
			out = append(out, pprof.Frame{Name: "[unknown]", Address: sl.PC})
			continue
		}
		// ToProfFrames one frame at a time: it is a pure per-frame flattening
		// (symbolize/toprof.go), so this is identical to calling it on the
		// whole slice, and it is what keeps each expanded inline chain
		// anchored to the slot it came from.
		out = append(out, symbolize.ToProfFrames([]symbolize.Frame{frames[next]})...)
		next++
	}
	return out
}

// symbolizePIDWithKernel resolves both user-mode and kernel-mode IPs for a
// single sample. Kernel frames are leaf-side and are prepended to the user
// frames so the resulting chain is leaf-first (kernel → user-leaf → … →
// user-root). pprof.Reverse() later flips this to outermost-first.
//
// When kernelIPs is empty (the typical case with --kernel-stacks off or
// stale BPF stack-IDs), behaves identically to symbolizePID. When user-mode
// symbolization fails we still emit synthetic "[unknown]" placeholders so
// the kernel frames don't appear hanging off an unrelated stack.
func symbolizePIDWithKernel(sym symbolize.Symbolizer, kernelSym symbolize.KernelSymbolizer, pid uint32, userSlots []interp.Slot, kernelIPs []uint64) []pprof.Frame {
	userFrames := symbolizePID(sym, pid, userSlots)
	if len(kernelIPs) == 0 {
		return userFrames
	}
	kernelFrames, err := kernelSym.SymbolizeKernel(kernelIPs)
	if err != nil {
		log.Printf("dwarfagent: symbolize kernel: %v", err)
	}
	kf := symbolize.ToProfFramesKernel(kernelFrames)
	if len(kf) == 0 {
		return userFrames
	}
	out := make([]pprof.Frame, 0, len(kf)+len(userFrames))
	out = append(out, kf...)
	out = append(out, userFrames...)
	return out
}
