package pyunwind

import (
	"errors"
	"fmt"
)

var errBadRead = errors.New("pyunwind: unreadable address")

// ErrOffsetsImplausible means Validate read the frame successfully but what
// came back cannot come from a live CPython frame -- an owner byte outside
// the _frameowner enum, or a previous pointer that is neither NULL nor a
// plausible userspace address. It is the "this table is wrong for this
// build" signal, distinct from ErrOffsetsUnreadable: the read worked, the
// content didn't.
var ErrOffsetsImplausible = errors.New("pyunwind: offsets are implausible for this build")

// ErrOffsetsUnreadable wraps a FrameReader failure encountered while
// validating: an address could not be read at all, so Validate learned
// nothing about whether the table is right or wrong. Callers that need to
// tell "the table is wrong" (ErrOffsetsImplausible) apart from "the read
// failed" (this) can use errors.Is against either sentinel; the concrete
// error from the FrameReader is still available via further unwrapping.
var ErrOffsetsUnreadable = errors.New("pyunwind: could not read memory to validate offsets")

// _frameowner values from CPython's internal/pycore_frame.h, in the
// numbering used by 3.12 and 3.13.
//
// CPython 3.14 renumbers this enum: it inserts FRAME_OWNED_BY_INTERPRETER
// before FRAME_OWNED_BY_CSTACK (internal/pycore_interpframe_structs.h in the
// 3.14.3 source), so CSTACK moves from 3 to 4 and the enum's ceiling moves
// from 3 to 4 with it. That is not a cosmetic renumbering: on 3.14 an owner
// byte of 3 is a live, legitimate value (FRAME_OWNED_BY_INTERPRETER), not an
// out-of-range one. A single package-wide ceiling shared across versions
// would reject real 3.14 frames. That per-version truth is carried on
// Offsets (FrameOwnerMax, FrameOwnerCStack, FrameOwnerBoundary), not in
// these constants -- these constants are only valid for 3.12 and 3.13, and
// Validate never uses them directly.
//
// The frame the walk stops at is the interpreter's entry frame: it executes
// no Python code and hands back to native unwinding, rather than running the
// chain to NULL, which would consume the whole Python stack in one go and
// terminate the trace with no native frames beneath it. Which owner value
// marks it moved with the renumbering -- CSTACK (3) on 3.12/3.13,
// INTERPRETER (3) on 3.14 -- so the stop condition is
// FrameOwnerBoundary, not equality with FrameOwnerCStack. See
// Offsets.FrameOwnerBoundary.
const (
	FrameOwnedByThread      uint8 = 0
	FrameOwnedByGenerator   uint8 = 1
	FrameOwnedByFrameObject uint8 = 2
	FrameOwnedByCStack      uint8 = 3 // 3.12/3.13 numbering only -- see the comment above.
)

// Offsets is one CPython minor version's struct layout. Every *Offset field
// is a byte offset into a struct in the target process; the accompanying
// bool fields record layout facts that a byte offset alone cannot express.
//
// Every number here was measured with offsetof() compiled against that
// exact micro version's own internal headers inside python:X.Y.Z-slim
// (podman), not read off a diagram or carried over from an adjacent
// version. See offsets_fixture_test.go, which re-measures and compares
// against these literals whenever podman is available; see also
// offsets_test.go's divergence tests, which pin the specific ways 3.12 and
// 3.14 disagree with 3.13.
//
// The autoTSSkey offset is deliberately absent: it is parsed per binary by
// ParseAutoTSSKeyOffset, so it needs no table entry and survives distro
// patching. See the spec.
type Offsets struct {
	// _PyInterpreterFrame
	FramePrevious uint16 // struct _PyInterpreterFrame *previous

	// FrameExecutable is the offset of the code-object reference: named
	// f_code on 3.12, renamed f_executable on 3.13 and 3.14.
	FrameExecutable uint16

	// FrameExecutableTagged is true when the value at FrameExecutable is a
	// tagged _PyStackRef (3.14) rather than a plain PyObject* (3.12, 3.13).
	// CPython 3.14 introduced _PyStackRef (internal/pycore_structs.h) and the
	// TWO low bits carry the tag on an ordinary GIL build: Py_TAG_BITS is 3
	// there (Include/internal/pycore_stackref.h:446, in the `#else //
	// Py_GIL_DISABLED / With GIL` arm, read out of python:3.14.3-slim), made
	// of Py_TAG_REFCNT = 1 for a deferred/immortal reference and Py_INT_TAG =
	// 3 for a tagged int.
	//
	// The mask to apply is therefore &^ 3, and the authority for that is
	// CPython's own out-of-process reader rather than either bit's
	// definition: Modules/_remote_debugging_module.c:45 defines
	// CLEAR_PTR_TAG(ptr) as ((uintptr_t)(ptr) & ~Py_TAG_BITS) and line 2186
	// reads interpreter_frame.executable through it. bpf/python_walk.h's
	// PY_STACKREF_TAG_BITS is the same 3, cited the same way. (&^ 1 happens
	// to give the same pointer for a REFCNT-tagged code object, since
	// pointers are 8-aligned, which is exactly why picking the wrong mask is
	// invisible until it is not.)
	//
	// A reader that treats this field as a plain pointer on 3.14 holds an odd
	// address one byte into the code object. The free-threaded build's
	// Py_TAG_BITS is 1, not 3 -- irrelevant here, because such a build is
	// refused outright at attach (ErrFreeThreaded).
	FrameExecutableTagged bool

	// FrameInstrPtr is the offset of the currently-executing bytecode unit:
	// named prev_instr on 3.12, renamed instr_ptr on 3.13 and 3.14. Not
	// consumed by Validate; carried for the walker slice that reads names.
	FrameInstrPtr uint16

	FrameOwner uint16 // char owner

	// FrameOwnerMax is the highest valid raw value of the _frameowner enum
	// for this version. Validate refuses any owner byte above it. See the
	// package-level enum comment for why this cannot be a shared constant:
	// it is 3 on 3.12/3.13 and 4 on 3.14.
	FrameOwnerMax uint8

	// FrameOwnerCStack is this version's FRAME_OWNED_BY_CSTACK value. 3 on
	// 3.12/3.13, 4 on 3.14. It is NOT the walker's stop condition -- see
	// FrameOwnerBoundary, which is.
	FrameOwnerCStack uint8

	// FrameOwnerBoundary is the lowest owner value that means "this frame is
	// the interpreter's boundary with C, not Python code". The walker stops
	// at owner >= FrameOwnerBoundary and hands back to native unwinding.
	//
	// It is 3 on all three supported versions, and that is a coincidence of
	// two changes cancelling, not a constant: 3.14 inserts
	// FRAME_OWNED_BY_INTERPRETER at 3 (pushing FRAME_OWNED_BY_CSTACK to 4)
	// AND moves the entry frame onto the new enumerator, so the value stays
	// 3 while the enumerator it is measured from changes.
	//
	//	3.12.14  Python/ceval.c:702   entry_frame.owner = FRAME_OWNED_BY_CSTACK      (3)
	//	3.13.15  Python/ceval.c:719   entry_frame.owner = FRAME_OWNED_BY_CSTACK      (3)
	//	3.14.3   Python/ceval.c:1162  entry.frame.owner = FRAME_OWNED_BY_INTERPRETER (3)
	//
	// FRAME_OWNED_BY_CSTACK == 4 is assigned nowhere in the 3.14 tree, so a
	// walker testing owner == FrameOwnerCStack walks straight past the
	// boundary on 3.14 and consumes the native stack beneath the
	// interpreter. CPython's own readers test the range instead --
	// Python/ceval.c:260 (owner >= FRAME_OWNED_BY_INTERPRETER) and
	// Modules/_remote_debugging_module.c:2148-2149 (== CSTACK || ==
	// INTERPRETER) -- and so does bpf/python_walk.h's py_push_frames, which
	// this field feeds via py_proc_info.frame_owner_boundary.
	//
	// Carried per version rather than hardcoded as 3 in the walker for the
	// same reason every other number in this table is: the next renumbering
	// is a table edit re-measured against a real interpreter
	// (offsets_fixture_test.go measures this field too), not a hunt through
	// BPF C for a literal.
	FrameOwnerBoundary uint8

	// PyThreadState

	// ThreadStateFrame locates the current _PyInterpreterFrame from a
	// PyThreadState.
	//
	// On 3.13 and 3.14, PyThreadState has a current_frame field directly,
	// and ThreadStateFrame is its offset: tstate + ThreadStateFrame holds
	// the frame pointer.
	//
	// On 3.12, PyThreadState has no current_frame field. The frame pointer
	// is one level deeper, in tstate->cframe->current_frame
	// (cpython/pystate.h: struct _PyCFrame, removed in 3.13). ThreadStateFrame
	// is then the offset of the `cframe` pointer field, ThreadStateFrameIndirect
	// is true, and current_frame sits at offset 0 of *cframe -- so the
	// walker must read a pointer at tstate+ThreadStateFrame, then read the
	// frame pointer from offset 0 of what that pointer points to, rather
	// than treating tstate+ThreadStateFrame as the frame pointer itself.
	ThreadStateFrame uint16

	// ThreadStateFrameIndirect is true only for 3.12; see ThreadStateFrame.
	ThreadStateFrameIndirect bool

	// PyCodeObject, for the fingerprint (slice 3 reads the names)
	CodeArgCount       uint16
	CodeKwOnlyArgCount uint16
	CodeFlags          uint16
	CodeFirstLineNo    uint16
}

// TableFor returns the layout for v, or ErrUnsupportedVersion.
//
// Provenance for every literal below: offsetof() compiled with
// -DPy_BUILD_CORE=1 against that exact micro version's own
// internal/pycore_frame.h (3.12, 3.13) or internal/pycore_interpframe_structs.h
// (3.14) and cpython/pystate.h, inside podman run --rm
// python:<exact-micro>-slim. offsets_fixture_test.go re-runs this
// measurement and fails the build if a header ever disagrees with these
// numbers.
//
// A test asserting a Go constant equals the literal it was written from
// proves nothing. These numbers are pinned against real interpreters, not
// against themselves.
func TableFor(v Version) (Offsets, error) {
	if !v.Supported() {
		return Offsets{}, fmt.Errorf("%w: %s", ErrUnsupportedVersion, v)
	}
	switch v.Minor {
	case 12:
		// Measured against python:3.12.14-slim (PY_VERSION "3.12.14").
		// struct _PyInterpreterFrame { PyCodeObject *f_code; struct
		// _PyInterpreterFrame *previous; ...; _Py_CODEUNIT *prev_instr;
		// int stacktop; uint16_t return_offset; char owner; ... }.
		// PyThreadState has no current_frame; it has `_PyCFrame *cframe`
		// at offset 56, and _PyCFrame's own current_frame is its first
		// field (offset 0).
		return Offsets{
			FramePrevious:            8,
			FrameExecutable:          0,
			FrameExecutableTagged:    false,
			FrameInstrPtr:            56,
			FrameOwner:               70,
			FrameOwnerMax:            3,
			FrameOwnerCStack:         3,
			FrameOwnerBoundary:       3,  // FRAME_OWNED_BY_CSTACK
			ThreadStateFrame:         56, // offset of `cframe`, not of a frame pointer
			ThreadStateFrameIndirect: true,
			CodeArgCount:             52,
			CodeKwOnlyArgCount:       60,
			CodeFlags:                48,
			CodeFirstLineNo:          68,
		}, nil
	case 13:
		// Measured against python:3.13.15-slim (PY_VERSION "3.13.15").
		// struct _PyInterpreterFrame { PyObject *f_executable; struct
		// _PyInterpreterFrame *previous; ...; _Py_CODEUNIT *instr_ptr;
		// int stacktop; uint16_t return_offset; char owner; ... }.
		// _PyCFrame is gone; PyThreadState has current_frame directly.
		return Offsets{
			FramePrevious:            8,
			FrameExecutable:          0,
			FrameExecutableTagged:    false,
			FrameInstrPtr:            56,
			FrameOwner:               70,
			FrameOwnerMax:            3,
			FrameOwnerCStack:         3,
			FrameOwnerBoundary:       3, // FRAME_OWNED_BY_CSTACK
			ThreadStateFrame:         72,
			ThreadStateFrameIndirect: false,
			CodeArgCount:             52,
			CodeKwOnlyArgCount:       60,
			CodeFlags:                48,
			CodeFirstLineNo:          68,
		}, nil
	case 14:
		// Measured against python:3.14.3-slim (PY_VERSION "3.14.3").
		// struct _PyInterpreterFrame moved to
		// internal/pycore_interpframe_structs.h; f_executable is now a
		// tagged _PyStackRef (see FrameExecutableTagged). `int stacktop`
		// was replaced by `_PyStackRef *stackpointer`, which pushes owner
		// from offset 70 to 74. The _frameowner enum gained
		// FRAME_OWNED_BY_INTERPRETER=3, pushing FRAME_OWNED_BY_CSTACK from
		// 3 to 4 (see FrameOwnerMax/FrameOwnerCStack) -- and ceval.c:1162
		// moved the entry frame onto FRAME_OWNED_BY_INTERPRETER at the same
		// time, which is why FrameOwnerBoundary is still 3 here while
		// FrameOwnerCStack is 4. PyThreadState kept current_frame directly,
		// at the same offset as 3.13.
		return Offsets{
			FramePrevious:            8,
			FrameExecutable:          0,
			FrameExecutableTagged:    true,
			FrameInstrPtr:            56,
			FrameOwner:               74,
			FrameOwnerMax:            4,
			FrameOwnerCStack:         4,
			FrameOwnerBoundary:       3, // FRAME_OWNED_BY_INTERPRETER, not CSTACK (4)
			ThreadStateFrame:         72,
			ThreadStateFrameIndirect: false,
			CodeArgCount:             52,
			CodeKwOnlyArgCount:       60,
			CodeFlags:                48,
			CodeFirstLineNo:          68,
		}, nil
	}
	return Offsets{}, fmt.Errorf("%w: %s", ErrUnsupportedVersion, v)
}

// FrameReader reads the target's memory. Implemented over BPF-read bytes in
// production and over a map in tests.
type FrameReader interface {
	ReadU64(addr uint64) (uint64, error)
	ReadU8(addr uint64) (uint8, error)
}

// Validate walks one frame and checks the result is self-consistent before
// the table is trusted for a process.
//
// This exists because a wrong offset does not fail loudly -- it produces a
// plausible stack of frames that are simply wrong, which is worse than no
// Python frames at all. Cheap checks that a wrong table fails: the owner
// byte must be inside its (per-version) enum, and the previous pointer must
// be either NULL or a plausible userspace address.
func (o Offsets) Validate(r FrameReader, frame uint64) error {
	owner, err := r.ReadU8(frame + uint64(o.FrameOwner))
	if err != nil {
		return fmt.Errorf("pyunwind: validate: owner: %w: %w", ErrOffsetsUnreadable, err)
	}
	if owner > o.FrameOwnerMax {
		return fmt.Errorf("pyunwind: validate: owner byte %#x is outside the _frameowner enum (max %d for this version); offsets are wrong for this build: %w", owner, o.FrameOwnerMax, ErrOffsetsImplausible)
	}
	prev, err := r.ReadU64(frame + uint64(o.FramePrevious))
	if err != nil {
		return fmt.Errorf("pyunwind: validate: previous: %w: %w", ErrOffsetsUnreadable, err)
	}
	if prev != 0 && prev < 0x1000 {
		return fmt.Errorf("pyunwind: validate: previous %#x is neither NULL nor a plausible pointer: %w", prev, ErrOffsetsImplausible)
	}
	return nil
}
