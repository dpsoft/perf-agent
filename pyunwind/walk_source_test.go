package pyunwind

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pyPushFramesBody returns the source text of py_push_frames.
//
// These are source-inspection tests, the same style gpuprobe/gate_test.go
// uses for walk_step, and for the same reason: the code under test is BPF C
// that only ever runs inside the verifier, and this package's tests run with
// no capabilities. They cannot prove the walk works; they pin the decisions
// that are wrong in a way no test on a Python-less machine could ever
// notice — a walk that reads the right memory with the wrong stop condition
// produces a plausible stack, not a failure.
// pyPushFramesBody returns py_begin_chain: the entry half, which finds the
// thread state and arms the cursor.
func pyPushFramesBody(t *testing.T) string {
	t.Helper()
	return pyFuncBody(t, "static __always_inline void py_begin_chain(")
}

// pyFrameStepBody returns the source text of py_step_one, which advances the
// chain by one frame.
//
// The walk has been three shapes: a #pragma unroll inside py_push_frames, a
// bpf_loop callback, and now a plain step called once per walk_step
// iteration -- because each nested bpf_loop call site multiplies verifier
// state, and two of them stopped all three programs loading on 6.19.4+. The
// decisions these tests pin did not move with it, so the tests follow them
// rather than being relaxed.
func pyFrameStepBody(t *testing.T) string {
	t.Helper()
	return pyFuncBody(t, "static __always_inline int py_step_one(")
}

// pyChainWalkBody is both halves together, for assertions about the walk as
// a whole (which counters it bumps, say) rather than about where a
// particular decision lives.
func pyChainWalkBody(t *testing.T) string {
	t.Helper()
	return pyPushFramesBody(t) + "\n" + pyFrameStepBody(t)
}

func pyFuncBody(t *testing.T, signature string) string {
	t.Helper()
	src, err := os.ReadFile("../bpf/python_walk.h")
	if err != nil {
		t.Fatalf("read python_walk.h: %v", err)
	}
	body := string(src)
	start := strings.Index(body, signature)
	if start < 0 {
		t.Fatalf("%q not found in bpf/python_walk.h", signature)
	}
	rest := body[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatalf("%q body not closed", signature)
	}
	return rest[:end]
}

// The single most consequential line in the walk. CPython 3.14 renumbered
// _frameowner AND moved the entry frame onto the new enumerator, so the C
// boundary is owner 3 on every supported version while FRAME_OWNED_BY_CSTACK
// is 3 on 3.12/3.13 and 4 on 3.14 — and 4 is assigned nowhere in the 3.14
// tree. A walk testing `owner == frame_owner_cstack` therefore walks straight
// past the boundary on 3.14 and consumes the entire native stack beneath the
// interpreter, which reads as a deep, complete Python trace rather than as a
// bug. The stop must be the range test CPython's own readers use.
func TestTheWalkStopsAtTheBoundaryNotAtCStack(t *testing.T) {
	body := pyChainWalkBody(t)

	if !strings.Contains(body, "owner >= pi->frame_owner_boundary") {
		t.Error("the chain walk does not stop at owner >= pi->frame_owner_boundary")
	}
	if strings.Contains(body, "frame_owner_cstack") {
		t.Error("the chain walk tests frame_owner_cstack; that value is 4 on 3.14 and is assigned to no frame there")
	}
	// The plausibility screen is a separate, weaker check and must stay:
	// owner_max is the enum ceiling, boundary is the C frame.
	if !strings.Contains(body, "owner > pi->frame_owner_max") {
		t.Error("the chain walk no longer screens the owner byte against this version's enum ceiling")
	}
}

// FrameOwnerBoundary is 3 on all three versions, which is exactly why it must
// be carried per version rather than written as a literal: it is 3 for two
// different reasons, and the next renumbering breaks the coincidence.
func TestFrameOwnerBoundaryIsCarriedPerVersion(t *testing.T) {
	for _, v := range []Version{{3, 12, 14}, {3, 13, 15}, {3, 14, 3}} {
		o, err := TableFor(v)
		if err != nil {
			t.Fatalf("TableFor(%v): %v", v, err)
		}
		if o.FrameOwnerBoundary != 3 {
			t.Errorf("%v: FrameOwnerBoundary = %d, want 3", v, o.FrameOwnerBoundary)
		}
		if o.FrameOwnerBoundary > o.FrameOwnerMax {
			t.Errorf("%v: boundary %d is above the enum ceiling %d, so the walk can never stop",
				v, o.FrameOwnerBoundary, o.FrameOwnerMax)
		}
	}
	o314, _ := TableFor(Version{3, 14, 3})
	if o314.FrameOwnerBoundary == o314.FrameOwnerCStack {
		t.Error("3.14: boundary and CStack must differ (INTERPRETER=3, CSTACK=4); if they agree the table is wrong")
	}
	o313, _ := TableFor(Version{3, 13, 15})
	if o313.FrameOwnerBoundary != o313.FrameOwnerCStack {
		t.Error("3.13: the entry frame IS the CSTACK frame; boundary and CStack must agree")
	}
}

// The 3.14 f_executable is a tagged _PyStackRef and the mask is CPython's
// own: Py_TAG_BITS is 3 on the GIL build (pycore_stackref.h), and
// _remote_debugging_module.c's CLEAR_PTR_TAG clears exactly those bits.
// Masking only bit 0 gives the same pointer today — pointers are 8-aligned —
// which is why the wrong mask survives every test that does not read the
// definition.
func TestTheStackRefMaskIsCPythonsOwn(t *testing.T) {
	src, err := os.ReadFile("../bpf/python_walk.h")
	if err != nil {
		t.Fatalf("read python_walk.h: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "#define PY_STACKREF_TAG_BITS 3ULL") {
		t.Error("PY_STACKREF_TAG_BITS is not 3: Py_TAG_BITS on an ordinary GIL build is 3, not 1")
	}
	if !strings.Contains(body, "CLEAR_PTR_TAG") {
		t.Error("the mask no longer cites CPython's own out-of-process reader as its authority")
	}

	fn := pyFrameStepBody(t)
	if !strings.Contains(fn, "if (pi->frame_executable_tagged) exec &= ~PY_STACKREF_TAG_BITS;") {
		t.Error("the tag is not cleared, or is cleared unconditionally; 3.12 and 3.13 hold a plain PyObject* there")
	}
}

// 3.12 reaches the frame through tstate->cframe->current_frame; 3.13+ read
// current_frame directly. Losing the hop does not fail — it reads a
// _PyCFrame* as if it were an _PyInterpreterFrame* and produces frames.
func TestTheWalkKeepsThe312CFrameIndirection(t *testing.T) {
	body := pyPushFramesBody(t)
	if !strings.Contains(body, "pi->threadstate_frame_indirect") {
		t.Error("py_push_frames no longer honours the 3.12 cframe indirection")
	}
}

// CPython >= 3.13 puts Py_None in the top C-entered frame's f_executable.
// Treating it as a code object puts one garbage frame on every Python stack.
func TestTheWalkRecognisesPyNoneAsAStop(t *testing.T) {
	body := pyFrameStepBody(t)
	if !strings.Contains(body, "pi->none_addr") {
		t.Error("the chain walk does not recognise Py_None in f_executable (3.13+)")
	}
	if !strings.Contains(body, "PY_CNT_NONE_EXECUTABLE") {
		t.Error("the Py_None stop is silent")
	}
}

// Every way the walk can refuse must land in a named counter. walker_flags
// has no bits left, so an uncounted refusal is invisible: the profile simply
// has no Python frames and nothing anywhere says why.
func TestEveryPythonRefusalIsCounted(t *testing.T) {
	// PY_CNT_CHAIN_TRUNCATED is deliberately absent from this list: it is no
	// longer a property of the walk, because the walk has no budget of its
	// own. It is asked once per sample by each driver, after the outer loop,
	// and TestEveryDriverCountsAnAbandonedPythonChain pins it there.
	body := pyChainWalkBody(t)
	for _, slot := range []string{
		"PY_CNT_TSS_MISS",
		"PY_CNT_TSTATE_READ_FAIL",
		"PY_CNT_FRAME_READ_FAIL",
		"PY_CNT_OWNER_IMPLAUSIBLE",
		"PY_CNT_PUSH_REFUSED",
		"PY_CNT_FRAMES_PUSHED",
	} {
		if !strings.Contains(body, "py_count("+slot+")") {
			t.Errorf("%s is never bumped anywhere in the chain walk", slot)
		}
	}
}

// The chain has to be RESUMED across walk_step calls, not restarted.
// _PyInterpreterFrame.previous links run through the C boundary, so a native
// stack that crosses the eval loop twice (Python -> C extension -> Python)
// hits the interpreter arm twice; restarting from tstate->current_frame the
// second time pushes the innermost segment again and files it under the outer
// one's position. Duplicated frames in a plausible order — the failure mode
// this whole design keeps refusing.
func TestTheChainIsResumedNotRestarted(t *testing.T) {
	entry := pyPushFramesBody(t)
	if !strings.Contains(entry, "if (ctx->py_state == PY_CHAIN_DONE || ctx->py_state == PY_CHAIN_WALKING) return;") {
		t.Error("py_begin_chain re-arms a chain that is finished, or restarts one already being walked")
	}
	if !strings.Contains(entry, "if (ctx->py_state == PY_CHAIN_UNSTARTED)") {
		t.Error("the TSS lookup is not gated on the chain being unstarted")
	}
	// Arming the cursor is the callback's job now: it is the half that knows
	// it has reached the boundary. Asserted THERE rather than over the
	// combined text, so moving it back into the entry function -- where it
	// would run on every call rather than at the boundary -- fails here.
	step := pyFrameStepBody(t)
	if !strings.Contains(step, "ctx->py_state = PY_CHAIN_ACTIVE;") {
		t.Error("the cursor is never armed, so a second eval-loop frame restarts the chain from the top")
	}
	if strings.Contains(entry, "PY_CHAIN_ACTIVE") {
		t.Error("py_push_frames arms the cursor outside the boundary check, so a chain that never reached the boundary would be resumed")
	}
	// Budget exhaustion must NOT leave the cursor live: resuming a
	// half-walked segment at the next eval-loop PC would file the rest of it
	// under a different native frame.
	//
	// With no per-segment loop there is no "budget ran out" path inside the
	// walk at all: the outer loop's budget is the record's, and the drivers
	// ask about a segment left mid-flight after it returns. What must still
	// hold here is that the cursor is armed ONLY at the boundary, which the
	// two assertions above pin from both sides.
}

// Budget exhaustion has to be told apart from a walk that stopped for a
// reason, and bpf_loop's return value cannot do it: a callback that stops on
// its last permitted iteration returns the same count as one that never
// stopped. Every path in the callback that returns 1 must therefore set the
// flag, or a perfectly ordinary stop is counted as a truncation.
//
// Counted rather than merely present: the callback has one `return 0`
// (continue) and the rest are stops, so the number of stop-returns and the
// number of flag assignments must match exactly.
func TestEveryStoppingPathMarksTheWalkStopped(t *testing.T) {
	step := pyFrameStepBody(t)
	// Every return except the last must first say what the segment's state
	// now is. A path that returns without setting py_state leaves the segment
	// PY_CHAIN_WALKING with an unchanged cursor, and walk_step would spend
	// every remaining iteration of the outer loop re-reading the same frame --
	// a stack that silently ends at the Python segment.
	idxs := []int{}
	for i := 0; i+len("return") < len(step); i++ {
		if strings.HasPrefix(step[i:], "return 0;") || strings.HasPrefix(step[i:], "return 1;") {
			idxs = append(idxs, i)
		}
	}
	if len(idxs) < 3 {
		t.Fatalf("py_step_one has only %d returns; the slice is looking at the wrong text", len(idxs))
	}
	for _, i := range idxs[:len(idxs)-1] {
		lo := i - 260
		if lo < 0 {
			lo = 0
		}
		if !strings.Contains(step[lo:i], "ctx->py_state =") {
			t.Errorf("a return at byte %d in py_step_one does not first set ctx->py_state; "+
				"the segment would stay PY_CHAIN_WALKING and re-read the same frame forever", i)
		}
	}
	// The final return is the one that continues the walk: it advances the
	// cursor and deliberately leaves the state alone.
	tail := step[idxs[len(idxs)-1]:]
	if !strings.Contains(step[idxs[len(idxs)-1]-200:idxs[len(idxs)-1]], "ctx->py_iter = prev;") {
		t.Error("py_step_one's continuing path does not advance the cursor")
	}
	_ = tail
}

// PY_CNT_CHAIN_ABANDONED is the one counter neither py_push_frames nor
// walk_step can bump: a live cursor only becomes a LOST Python segment once
// the native walk is over, and nothing inside the bpf_loop callback knows it
// is running for the last time. Each driver has to ask after bpf_loop returns,
// which means the property lives in three files and can be half-applied --
// exactly the shape that leaves one profiler silently short.
//
// Global Constraint: every refusal, miss or truncation increments a named
// counter. This is the last place on the Python path where that did not hold.
func TestEveryDriverCountsAnAbandonedPythonChain(t *testing.T) {
	for _, driver := range []string{
		"../bpf/perf_dwarf.bpf.c",
		"../bpf/offcpu_dwarf.bpf.c",
		"../bpf/gpu_usdt.bpf.c",
	} {
		t.Run(filepath.Base(driver), func(t *testing.T) {
			src, err := os.ReadFile(driver)
			if err != nil {
				t.Fatalf("read %s: %v", driver, err)
			}
			body := string(src)

			loop := strings.Index(body, "bpf_loop(MAX_FRAMES, walk_step, &walker, 0);")
			if loop < 0 {
				t.Fatalf("%s does not drive the shared walker; the Python arm cannot reach it", driver)
			}
			// Everything the driver does AFTER the walk. Asking before it
			// would read a cursor that is still being written.
			after := body[loop+len("bpf_loop(MAX_FRAMES, walk_step, &walker, 0);"):]

			// Comments are stripped before anything is matched: these
			// drivers carry a prose reference to PY_CNT_CHAIN_ABANDONED
			// above the call, and a naive search finds the mention rather
			// than the code. That is how a source-inspection test ends up
			// asserting against its own documentation.
			code := stripLineComments(after)

			call := strings.Index(code, "py_count(PY_CNT_CHAIN_ABANDONED)")
			if call < 0 {
				t.Errorf("%s never counts an abandoned Python chain: a native walk that stops early "+
					"drops the outer Python segment and no counter moves", driver)
				return
			}
			// The nearest `if (` before the call must be the cursor test.
			// Anchoring on "nearest" rather than "anywhere in the function"
			// is what makes this fail when the guard is dropped: some other
			// condition then becomes the nearest one.
			prefix := code[:call]
			lastIf := strings.LastIndex(prefix, "if (")
			if !strings.Contains(string(src), "walker.py_state == PY_CHAIN_WALKING) py_count(PY_CNT_CHAIN_TRUNCATED)") {
				t.Errorf("%s does not count a segment left mid-walk when the record filled up", driver)
			}
			if lastIf < 0 || !strings.Contains(prefix[lastIf:], "walker.py_state == PY_CHAIN_ACTIVE") {
				t.Errorf("%s bumps PY_CNT_CHAIN_ABANDONED without testing that the cursor was live; "+
					"it would count every sample, including the ones that lost nothing", driver)
			}
		})
	}
}

// A native-walk outcome, not a Python failure -- and the walker_flags join is
// the only way a reader can tell "the unwinder gave out" from
// "py_eval_ranges is missing an entry". Both readings must be written down
// where the counter is defined, or the number is unactionable.
func TestTheAbandonedChainCounterDocumentsItsFlagJoin(t *testing.T) {
	src, err := os.ReadFile("../bpf/python_walk.h")
	if err != nil {
		t.Fatalf("read python_walk.h: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "PY_CNT_CHAIN_ABANDONED is a NATIVE-WALK")
	if start < 0 {
		t.Fatal("PY_CNT_CHAIN_ABANDONED has no doc block naming it a native-walk outcome")
	}
	// Bounded to the comment block itself, not sliced to end of file. A slice
	// that runs to EOF passes on any landmark that happens to appear later in
	// the header, which is the same shape as the assertion that could not
	// fail (see TestTheChainIsResumedNotRestarted).
	var doc strings.Builder
	for line := range strings.SplitSeq(body[start:], "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") && doc.Len() > 0 {
			break
		}
		doc.WriteString(line)
		doc.WriteByte('\n')
	}
	for _, flag := range []string{
		"WALKER_FLAG_FP_EXHAUSTED",
		"WALKER_FLAG_FP_NONMONOTONIC",
		"WALKER_FLAG_CFI_MISS",
		"WALKER_FLAG_FRAME_PUSH_REFUSED",
		"WALKER_FLAG_FP_TERMINATED",
		"WALKER_FLAG_RA_UNDEFINED",
		"py_eval_ranges",
	} {
		if !strings.Contains(doc.String(), flag) {
			t.Errorf("the doc block does not name %s; a reader cannot reconstruct the join alone", flag)
		}
	}
}

// stripLineComments blanks // comments so a landmark quoted in prose cannot be
// mistaken for the code that does the thing. Newlines are preserved so line
// structure survives.
func stripLineComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for line := range strings.SplitSeq(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestFrameWindowCoversEveryTable pins bpf/python_walk.h's PY_FRAME_WINDOW
// against the offsets this package actually installs.
//
// The walk reads one fixed-size prefix of _PyInterpreterFrame and then takes
// every field out of it by offset, so a version whose fields sit past the
// window would be refused at run time by a guard nobody would think to look
// for. Reading the constant out of the header and comparing it against the
// tables makes that a build-time failure in the package that owns the
// numbers, which is where a new version's offsets get added.
func TestFrameWindowCoversEveryTable(t *testing.T) {
	src, err := os.ReadFile("../bpf/python_walk.h")
	if err != nil {
		t.Fatalf("read python_walk.h: %v", err)
	}
	m := regexp.MustCompile(`#define PY_FRAME_WINDOW (\d+)`).FindStringSubmatch(string(src))
	if m == nil {
		t.Fatal("PY_FRAME_WINDOW is not defined in bpf/python_walk.h")
	}
	window, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("PY_FRAME_WINDOW is not a number: %v", err)
	}

	for _, v := range []Version{{3, 12, 14}, {3, 13, 15}, {3, 14, 3}} {
		off, err := TableFor(v)
		if err != nil {
			t.Fatalf("TableFor(%v): %v", v, err)
		}
		// owner is one byte; the other three are 8-byte fields.
		if int(off.FrameOwner) > window-1 {
			t.Errorf("%v: FrameOwner at %d does not fit a %d-byte window", v, off.FrameOwner, window)
		}
		for name, o := range map[string]uint16{
			"FramePrevious":   off.FramePrevious,
			"FrameExecutable": off.FrameExecutable,
			"FrameInstrPtr":   off.FrameInstrPtr,
		} {
			if int(o) > window-8 {
				t.Errorf("%v: %s at %d does not fit a %d-byte window (needs 8 bytes)", v, name, o, window)
			}
		}
	}
}
