package test

import (
	"fmt"
	"strings"
	"testing"
)

// TestPythonSampleDefectsCatchesTheFailureShapes drives the end-to-end
// test's assertions with stacks that never came from a profiler.
//
// It exists because an assertion that has only ever been shown passing
// input is not known to be able to fail, and this branch has already
// produced seven assertions that could not. The stacks below are the
// failure shapes the walker's design is built to prevent -- most
// importantly the duplicated inner segment, which is what a walker that
// restarted from tstate->current_frame at the second eval-loop frame
// produces, and which reads as a perfectly plausible call path.
//
// It needs no BPF, no interpreter and no capabilities, so it also runs on
// the machines where the end-to-end test skips.
func TestPythonSampleDefectsCatchesTheFailureShapes(t *testing.T) {
	// One code object per workload function, at addresses that look like
	// the real ones (heap pointers) without being any particular process's.
	codes := map[string]uint64{}
	for i, name := range append(append([]string{}, pythonInnerSegment...), pythonOuterSegment...) {
		codes[name] = 0x7f0000000000 + uint64(i+1)*0x1000
	}
	py := func(name string) string { return fmt.Sprintf("python:%#x", codes[name]) }

	// The stack the design promises, leaf-first: an eval-loop native frame,
	// the inner Python segment, list.sort()'s C frames, the outer eval-loop
	// frame, the outer Python segment, and the thread bootstrap beneath.
	good := []string{
		"_PyLong_Multiply",
		"_PyEval_EvalFrameDefault",
		py("leaf"), py("inner"), py("key_fn"),
		"_Py_CheckFunctionResult", "list_sort_impl", "list_sort",
		"_PyEval_EvalFrameDefault",
		py("middle"), py("outer"), py("worker"),
		"_PyObject_VectorcallTstate", "thread_run", "pythread_wrapper", "start_thread",
	}
	if defects := pythonSampleDefects(good, codes); len(defects) != 0 {
		t.Fatalf("the intended stack was reported as defective: %v", defects)
	}

	cases := []struct {
		name  string
		names []string
		want  string
	}{
		{
			// THE Task 7 BUG. The inner segment is walked again at the
			// second eval-loop frame and filed beneath the outer one's
			// native position. Every frame is real, the order is plausible,
			// and three of them were never on this stack.
			name: "inner segment re-pushed beneath the outer one",
			names: []string{
				"_PyEval_EvalFrameDefault",
				py("leaf"), py("inner"), py("key_fn"),
				"list_sort_impl",
				"_PyEval_EvalFrameDefault",
				py("leaf"), py("inner"), py("key_fn"),
				py("middle"), py("outer"), py("worker"),
				"thread_run",
			},
			want: "appears 2 times",
		},
		{
			name: "segments adjacent, no native frame between them",
			names: []string{
				"_PyEval_EvalFrameDefault",
				py("leaf"), py("inner"), py("key_fn"),
				py("middle"), py("outer"), py("worker"),
				"thread_run",
			},
			want: "adjacent with no native frame",
		},
		{
			// Two unrelated stacks concatenated, or a walk that piled the
			// Python frames at one end: the segments come back swapped.
			name: "segments in the wrong order",
			names: []string{
				"_PyEval_EvalFrameDefault",
				py("middle"), py("outer"), py("worker"),
				"list_sort_impl",
				"_PyEval_EvalFrameDefault",
				py("leaf"), py("inner"), py("key_fn"),
				"thread_run",
			},
			want: "leaf-to-root order",
		},
		{
			name: "a frame missing from the chain",
			names: []string{
				"_PyEval_EvalFrameDefault",
				py("leaf"), py("key_fn"),
				"list_sort_impl",
				"_PyEval_EvalFrameDefault",
				py("middle"), py("outer"), py("worker"),
				"thread_run",
			},
			want: "leaf-to-root order",
		},
		{
			// The chain was run to NULL: the Python frames are all there
			// and the native stack they ran on is gone.
			name: "native frames consumed by the walk",
			names: []string{
				py("leaf"), py("inner"), py("key_fn"),
				py("middle"), py("outer"), py("worker"),
			},
			want: "no native frames left",
		},
		{
			name: "leafmost frame is Python",
			names: []string{
				py("leaf"), py("inner"), py("key_fn"),
				"list_sort_impl",
				"_PyEval_EvalFrameDefault",
				py("middle"), py("outer"), py("worker"),
				"thread_run",
			},
			want: "leafmost frame is a Python frame",
		},
		{
			// Plausible garbage: six Python frames in the right shape whose
			// code objects are not this workload's.
			name: "right shape, wrong code objects",
			names: []string{
				"_PyEval_EvalFrameDefault",
				"python:0xdead0001", "python:0xdead0002", "python:0xdead0003",
				"list_sort_impl",
				"_PyEval_EvalFrameDefault",
				"python:0xdead0004", "python:0xdead0005", "python:0xdead0006",
				"thread_run",
			},
			want: "leaf-to-root order",
		},
		{
			name:  "no Python frames at all",
			names: []string{"_PyEval_EvalFrameDefault", "list_sort_impl", "thread_run"},
			want:  "leaf-to-root order",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defects := pythonSampleDefects(tc.names, codes)
			if len(defects) == 0 {
				t.Fatalf("no defect reported for %q; the end-to-end assertions would pass on this stack", tc.name)
			}
			if !strings.Contains(strings.Join(defects, "\n"), tc.want) {
				t.Fatalf("defects %v do not mention %q", defects, tc.want)
			}
		})
	}
}

// TestParsePythonFrameNameRejectsNativeNames pins the one string test the
// whole file rests on. Every assertion above is written in terms of "is
// this frame a Python frame", and a matcher that answered yes to a native
// symbol -- or no to a real Python frame -- would quietly turn each of them
// into a different question.
func TestParsePythonFrameNameRejectsNativeNames(t *testing.T) {
	if addr, ok := parsePythonFrameName("python:0x7f1234567890"); !ok || addr != 0x7f1234567890 {
		t.Fatalf("python frame not recognised: got %#x, %v", addr, ok)
	}
	for _, name := range []string{
		"_PyEval_EvalFrameDefault",
		"list_sort_impl",
		"[libpython3.12.so.1.0]+0x1969a9",
		"pythread_wrapper",    // starts with "python" but is not the prefix
		"python:not-a-number", // the prefix without a usable address
		"",
	} {
		if addr, ok := parsePythonFrameName(name); ok {
			t.Errorf("%q was read as a Python frame at %#x", name, addr)
		}
	}
}

// Both renderings must be understood, and which one a run produces is a
// property of the interpreter, not of the walk: pyunwind resolves a frame by
// reading co_qualname out of the live process and only has measured
// code-object offsets for some CPython versions. On one it has not been
// measured against, the walk works exactly as well and the frames stay
// addresses.
//
// This exists because the opposite happened: naming the frames turned
// "python:0x7f…" into "outer_function (target.py:2)", and an assertion that
// understood only the address form reported "only 0 Python frames in 478
// samples" -- a walker failure -- for what was a naming improvement.
func TestPythonFrameNamesAreRecognisedInBothRenderings(t *testing.T) {
	byAddr := map[uint64]string{0x7f1234567890: "leaf"}

	// Unresolved: named through the address map.
	if fn, ok := pythonFuncName("python:0x7f1234567890", byAddr); !ok || fn != "leaf" {
		t.Errorf("unresolved form: got (%q, %v), want (leaf, true)", fn, ok)
	}
	// Unresolved and unknown: still a Python frame, just unnamed.
	if fn, ok := pythonFuncName("python:0xdead", byAddr); !ok || fn != "" {
		t.Errorf("unknown address: got (%q, %v), want (\"\", true)", fn, ok)
	}
	// Resolved: the qualified name reduces to the bare function.
	for in, want := range map[string]string{
		"leaf (workload.py:12)":                    "leaf",
		"Widget.method_here (target.py:4)":         "method_here",
		"Module._wrapped_call_impl (module.py:17)": "_wrapped_call_impl",
	} {
		if fn, ok := pythonFuncName(in, byAddr); !ok || fn != want {
			t.Errorf("resolved form %q: got (%q, %v), want (%q, true)", in, fn, ok, want)
		}
	}
	// Native frames must not be mistaken for Python ones, or the lower bound
	// this feeds would pass on a run with no Python frames at all.
	for _, in := range []string{
		"at::native::add_kernel(at::TensorIteratorBase&)",
		"libcuda.so.1+0x4b71c6",
		"main",
		"0x7f2c945b2c2b",
		"some_helper (thing.cc:12)",
	} {
		if fn, ok := pythonFuncName(in, byAddr); ok {
			t.Errorf("native frame %q was read as Python (%q)", in, fn)
		}
	}
}
