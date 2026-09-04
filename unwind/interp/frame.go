package interp

import (
	"fmt"
	"sync"
)

// ----- Rendering an interpreter frame.
//
// A frame this walker did not produce from an instruction pointer cannot be
// symbolized like one. It occupies TWO consecutive pcs[] slots carrying
// whatever pair of words its unwinder chose, both tagged with that unwinder's
// id, and NEITHER is an address in the process's text. Handing either to a
// native symbolizer asks it to name something that is not code: it will place
// the value in whatever mapping it happens to fall in and return a frame, so
// what reaches the user is a plausible, wrong native frame, silently.
//
// So the tag routes the pair to the unwinder that wrote it, and that unwinder
// names it. This is the one place where a generic tag legitimately needs a
// language-specific renderer, and it is deliberately a SEPARATE registry from
// the module one above: symbolization happens in a different phase of the
// process than attach, often with no BPF session alive at all (a saved profile
// being converted, a test decoding a fixture), so it must not require a
// driver, a loaded object, or any privilege.

var (
	renderMu  sync.RWMutex
	renderers = map[uint32]func(a, b uint64) string{}
	// namers resolve a frame against the live process it came from. See
	// RegisterNamer for why this is separate from renderers.
	namers = map[uint32]func(pid uint32, a, b uint64) (string, bool){}
)

// RegisterRenderer records how to name one unwinder's frames. Call it from a
// module's package init, so a binary that links the module can name its frames
// whether or not it ever loads its BPF object.
func RegisterRenderer(id uint32, render func(a, b uint64) string) {
	renderMu.Lock()
	defer renderMu.Unlock()
	renderers[id] = render
}

// FrameName renders one interpreter frame pair.
//
// The fallback matters as much as the hit. A tag with no registered renderer
// means a record produced by a build that had a module this one does not --
// an agent downgraded, a profile carried between hosts. Naming it
// "interp1:0x7f…" says exactly that: the frame is real, it is at the right
// position in the call path, and this binary cannot name it. Dropping it
// instead would silently shorten the stack, and symbolizing it as native would
// invent one.
// RegisterNamer records how to name one unwinder's frames FOR A GIVEN PROCESS.
//
// Separate from RegisterRenderer because naming an interpreter frame properly
// means reading the target's memory -- a Python code object only says
// "Widget.method_here" if something reads the PyCodeObject out of the process
// -- and that needs a pid, which the renderer signature has no room for and
// should not: a renderer must keep working on a saved profile converted on a
// laptop, where there is no process to read.
//
// A namer returns false when it cannot answer, and the caller then falls back
// to the renderer's address form. Both are registered: the namer is used
// during a capture, the renderer for everything after it.
func RegisterNamer(id uint32, namer func(pid uint32, a, b uint64) (string, bool)) {
	renderMu.Lock()
	defer renderMu.Unlock()
	namers[id] = namer
}

// FrameNameFor names one interpreter frame in the context of the process it
// came from, falling back to FrameName when no namer can answer.
//
// Call this from a capture path, where the target is still alive. After it
// exits only FrameName's address form is honest, and a namer that cached what
// it read during the capture will still answer -- which is how a resolved name
// outlives the process that explained it.
func FrameNameFor(pid uint32, id uint32, a, b uint64) string {
	renderMu.RLock()
	n := namers[id]
	renderMu.RUnlock()
	if n != nil {
		if s, ok := n(pid, a, b); ok {
			return s
		}
	}
	return FrameName(id, a, b)
}

func FrameName(id uint32, a, b uint64) string {
	renderMu.RLock()
	f := renderers[id]
	renderMu.RUnlock()
	if f != nil {
		return f(a, b)
	}
	return fmt.Sprintf("interp%d:%#x", id, a)
}

// FrameTagNative mirrors FRAME_TAG_NATIVE in bpf/unwind_record.h: a single
// pcs[] slot holding one instruction pointer. Any other tag value is an
// unwinder id and marks the first of a PAIR.
const FrameTagNative = 0

// Slot is one decoded pcs[]/tags[] position: a native instruction pointer, or
// an interpreter frame with the two words that describe it folded back
// together and the id of the unwinder that wrote them.
type Slot struct {
	// Unwinder is 0 for a native frame, else the id of the unwinder that
	// pushed this pair.
	Unwinder uint32
	// PC is the native instruction pointer, or the interpreter frame's first
	// word (for CPython, the code object's address).
	PC uint64
	// Extra is the second word of an interpreter pair, unused for a native
	// frame.
	Extra uint64
}

// IsInterp reports whether this slot came from an interpreter unwinder rather
// than the native walk.
func (s Slot) IsInterp() bool { return s.Unwinder != FrameTagNative }

// Name renders an interpreter slot. Only meaningful when IsInterp.
func (s Slot) Name() string { return FrameName(s.Unwinder, s.PC, s.Extra) }

// NameFor is Name with the process in hand, so an interpreter frame can be
// resolved from the live target rather than rendered as an address.
func (s Slot) NameFor(pid uint32) string { return FrameNameFor(pid, s.Unwinder, s.PC, s.Extra) }

// SplitSlots folds the parallel pcs[]/tags[] arrays into one ordered list of
// frames -- ONE entry per frame, not per slot.
//
// ORDER IS PRESERVED AND THAT IS THE WHOLE POINT. Separating native frames
// from interpreter frames into two lists would lose which native frame each
// interpreter frame sat above, and a stack whose interpreter frames are all
// piled at one end is a plausible call path that never happened.
//
// A nil or short tags slice is treated as all-native from that point on, which
// is what a record written before tags existed looks like.
//
// truncatedPair reports a trailing interpreter slot whose partner word fell
// off the end of the walk. The half frame is dropped rather than half-read: a
// code object with a garbage second word is worse than no frame.
//
// This function is PURE: it touches no counter. It is called from more than
// one place, and a counter inside it would report a number that depends on how
// many callers happen to exist rather than on how many walks were truncated.
func SplitSlots(pcs []uint64, tags []uint8) (slots []Slot, truncatedPair bool) {
	slots = make([]Slot, 0, len(pcs))
	for i := 0; i < len(pcs); i++ {
		if i >= len(tags) || tags[i] == FrameTagNative {
			slots = append(slots, Slot{PC: pcs[i]})
			continue
		}
		if i+1 >= len(pcs) {
			return slots, true
		}
		slots = append(slots, Slot{Unwinder: uint32(tags[i]), PC: pcs[i], Extra: pcs[i+1]})
		i++
	}
	return slots, false
}

// NativeIPs returns the instruction pointers from slots, in order. These are
// the only words that may reach a symbolizer or a perf.data user-IP stream.
func NativeIPs(slots []Slot) []uint64 {
	out := make([]uint64, 0, len(slots))
	for _, sl := range slots {
		if !sl.IsInterp() {
			out = append(out, sl.PC)
		}
	}
	return out
}
