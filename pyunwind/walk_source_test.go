package pyunwind

import (
	"os"
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
func pyPushFramesBody(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../bpf/python_walk.h")
	if err != nil {
		t.Fatalf("read python_walk.h: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "static __always_inline void py_push_frames(")
	if start < 0 {
		t.Fatal("py_push_frames not found in bpf/python_walk.h")
	}
	rest := body[start:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("py_push_frames body not closed")
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
	body := pyPushFramesBody(t)

	if !strings.Contains(body, "owner >= pi->frame_owner_boundary") {
		t.Error("py_push_frames does not stop at owner >= pi->frame_owner_boundary")
	}
	if strings.Contains(body, "frame_owner_cstack") {
		t.Error("py_push_frames tests frame_owner_cstack; that value is 4 on 3.14 and is assigned to no frame there")
	}
	// The plausibility screen is a separate, weaker check and must stay:
	// owner_max is the enum ceiling, boundary is the C frame.
	if !strings.Contains(body, "owner > pi->frame_owner_max") {
		t.Error("py_push_frames no longer screens the owner byte against this version's enum ceiling")
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

	fn := pyPushFramesBody(t)
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
	body := pyPushFramesBody(t)
	if !strings.Contains(body, "pi->none_addr") {
		t.Error("py_push_frames does not recognise Py_None in f_executable (3.13+)")
	}
	if !strings.Contains(body, "PY_CNT_NONE_EXECUTABLE") {
		t.Error("the Py_None stop is silent")
	}
}

// Every way the walk can refuse must land in a named counter. walker_flags
// has no bits left, so an uncounted refusal is invisible: the profile simply
// has no Python frames and nothing anywhere says why.
func TestEveryPythonRefusalIsCounted(t *testing.T) {
	body := pyPushFramesBody(t)
	for _, slot := range []string{
		"PY_CNT_TSS_MISS",
		"PY_CNT_TSTATE_READ_FAIL",
		"PY_CNT_FRAME_READ_FAIL",
		"PY_CNT_OWNER_IMPLAUSIBLE",
		"PY_CNT_CHAIN_TRUNCATED",
		"PY_CNT_PUSH_REFUSED",
		"PY_CNT_FRAMES_PUSHED",
	} {
		if !strings.Contains(body, "py_count("+slot+")") {
			t.Errorf("%s is never bumped in py_push_frames", slot)
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
	body := pyPushFramesBody(t)
	if !strings.Contains(body, "if (ctx->py_state == PY_CHAIN_DONE) return;") {
		t.Error("py_push_frames re-runs after the chain is finished")
	}
	if !strings.Contains(body, "ctx->py_state = PY_CHAIN_ACTIVE;") {
		t.Error("the cursor is never armed, so a second eval-loop frame restarts the chain from the top")
	}
	if !strings.Contains(body, "if (ctx->py_state == PY_CHAIN_UNSTARTED)") {
		t.Error("the TSS lookup is not gated on the chain being unstarted")
	}
	// Budget exhaustion must NOT leave the cursor live: resuming a
	// half-walked segment at the next eval-loop PC would file the rest of it
	// under a different native frame.
	//
	// The region examined is everything AFTER the chain loop's closing brace.
	// This assertion previously sliced from the LAST occurrence of
	// py_count(PY_CNT_CHAIN_TRUNCATED), which is the final statement of the
	// function -- so the slice was that one 33-character call and could not
	// contain PY_CHAIN_ACTIVE under any mutation. It passed against the very
	// defect it names. Slice from the loop instead, and assert the region is
	// the one intended (it must still contain the counter) so a future
	// reshape cannot silently empty it again.
	tail := pyPushFramesAfterChainLoop(t)
	if !strings.Contains(tail, "py_count(PY_CNT_CHAIN_TRUNCATED)") {
		t.Fatal("the post-loop region does not contain the truncation counter; the slice is looking at the wrong text")
	}
	if strings.Contains(tail, "PY_CHAIN_ACTIVE") {
		t.Error("a truncated segment leaves the cursor live and will be resumed at the wrong native frame")
	}
}

// pyPushFramesAfterChainLoop returns the text of py_push_frames that follows
// the #pragma unroll chain loop's CLOSING BRACE -- i.e. the path taken only
// when the loop ran out of budget without reaching the interpreter boundary.
//
// Brace-matched from the loop's opening brace rather than found by searching
// for a landmark string, because every landmark inside this function is one
// edit away from moving. Line comments are skipped so a brace in prose cannot
// throw the depth count off.
func pyPushFramesAfterChainLoop(t *testing.T) string {
	t.Helper()
	body := pyPushFramesBody(t)

	pragma := strings.Index(body, "#pragma unroll")
	if pragma < 0 {
		t.Fatal("py_push_frames no longer has a #pragma unroll chain loop")
	}
	open := strings.Index(body[pragma:], "{")
	if open < 0 {
		t.Fatal("no opening brace after #pragma unroll")
	}
	open += pragma

	depth := 0
	for i := open; i < len(body); i++ {
		if body[i] == '/' && i+1 < len(body) && body[i+1] == '/' {
			nl := strings.IndexByte(body[i:], '\n')
			if nl < 0 {
				break
			}
			i += nl
			continue
		}
		switch body[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return body[i+1:]
			}
		}
	}
	t.Fatal("the chain loop's closing brace was never reached; braces are unbalanced or the loop was reshaped")
	return ""
}
