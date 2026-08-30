package pyunwind

import (
	"errors"
	"fmt"
)

var errBadRead = errors.New("pyunwind: unreadable address")

// _frameowner values from CPython's internal/pycore_frame.h, in the
// numbering used by 3.12 and 3.13.
//
// CPython 3.14 renumbers this enum: it inserts FRAME_OWNED_BY_INTERPRETER
// before FRAME_OWNED_BY_CSTACK (internal/pycore_interpframe_structs.h in the
// 3.14.3 source), so CSTACK moves from 3 to 4 and the enum's ceiling moves
// from 3 to 4 with it. That is not a cosmetic renumbering: on 3.14 an owner
// byte of 3 is a live, legitimate value (FRAME_OWNED_BY_INTERPRETER), not an
// out-of-range one, and the CSTACK sentinel the walker stops at is 4, not 3.
// A single package-wide ceiling shared across versions would either reject
// real 3.14 CSTACK frames or misidentify an INTERPRETER frame as CSTACK.
// That per-version truth is carried on Offsets (FrameOwnerMax,
// FrameOwnerCStack), not in these constants -- these constants are only
// valid for 3.12 and 3.13, and Validate never uses them directly.
//
// FRAME_OWNED_BY_CSTACK is the entry frame: the walk stops there and hands
// back to native unwinding rather than running the chain to NULL, which
// would consume the whole Python stack in one go and terminate the trace
// with no native frames beneath it.
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
	// CPython 3.14 introduced _PyStackRef (internal/pycore_structs.h); on a
	// normal, non free-threaded build (Py_GIL_DISABLED undefined) the low
	// bit is Py_TAG_REFCNT = 1 (internal/pycore_stackref.h), which must be
	// masked off (value &^ 1) before the result is a valid PyObject*. A
	// reader that treats this field as a plain pointer on 3.14 will hold an
	// odd address one byte into the code object.
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

	// FrameOwnerCStack is this version's FRAME_OWNED_BY_CSTACK value -- the
	// entry-frame sentinel the walker stops at. 3 on 3.12/3.13, 4 on 3.14.
	FrameOwnerCStack uint8

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
		// 3 to 4 (see FrameOwnerMax/FrameOwnerCStack). PyThreadState kept
		// current_frame directly, at the same offset as 3.13.
		return Offsets{
			FramePrevious:            8,
			FrameExecutable:          0,
			FrameExecutableTagged:    true,
			FrameInstrPtr:            56,
			FrameOwner:               74,
			FrameOwnerMax:            4,
			FrameOwnerCStack:         4,
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
		return fmt.Errorf("pyunwind: validate: owner: %w", err)
	}
	if owner > o.FrameOwnerMax {
		return fmt.Errorf("pyunwind: validate: owner byte %#x is outside the _frameowner enum (max %d for this version); offsets are wrong for this build", owner, o.FrameOwnerMax)
	}
	prev, err := r.ReadU64(frame + uint64(o.FramePrevious))
	if err != nil {
		return fmt.Errorf("pyunwind: validate: previous: %w", err)
	}
	if prev != 0 && prev < 0x1000 {
		return fmt.Errorf("pyunwind: validate: previous %#x is neither NULL nor a plausible pointer", prev)
	}
	return nil
}
