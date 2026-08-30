// pyunwind/offsets_test.go
package pyunwind

import (
	"errors"
	"testing"
)

func TestTableForCoversTheSupportedRange(t *testing.T) {
	for _, v := range []Version{{3, 12, 14}, {3, 13, 15}, {3, 14, 3}} {
		if _, err := TableFor(v); err != nil {
			t.Fatalf("%v: %v", v, err)
		}
	}
	if _, err := TableFor(Version{3, 11, 16}); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("3.11 must be refused with ErrUnsupportedVersion, got %v", err)
	}
}

// fakeReader returns a frame chain whose owner byte and previous pointer we
// control, so validation can be driven into both outcomes.
type fakeReader struct {
	u64 map[uint64]uint64
	u8  map[uint64]uint8
}

func (f fakeReader) ReadU64(a uint64) (uint64, error) {
	v, ok := f.u64[a]
	if !ok {
		return 0, errBadRead
	}
	return v, nil
}

func (f fakeReader) ReadU8(a uint64) (uint8, error) {
	v, ok := f.u8[a]
	if !ok {
		return 0, errBadRead
	}
	return v, nil
}

func TestValidateAcceptsASelfConsistentFrame(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := fakeReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): FrameOwnedByCStack},
	}
	if err := o.Validate(r, frame); err != nil {
		t.Fatalf("a self-consistent frame must validate: %v", err)
	}
}

// The point of validation: a table whose owner offset is wrong reads a byte
// that is not an owner enum, and must be refused rather than walked.
func TestValidateRefusesAnImplausibleOwner(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := fakeReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): 0x5a}, // not an owner
	}
	if err := o.Validate(r, frame); err == nil {
		t.Fatal("an owner byte outside the enum must be refused")
	}
}

// TestValidateRefusesAnImplausiblePrevious is the FramePrevious half of the
// same mutation check: a previous pointer that is neither NULL nor a
// plausible userspace address must be refused too.
func TestValidateRefusesAnImplausiblePrevious(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := fakeReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0x42}, // below 0x1000, not NULL
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): FrameOwnedByCStack},
	}
	if err := o.Validate(r, frame); err == nil {
		t.Fatal("a previous pointer that is neither NULL nor plausible must be refused")
	}
}

// TestValidateMutationCheck corrupts the owner offset in a table copy (as a
// wrong-offset table would be) and confirms Validate refuses it against a
// frame laid out for the correct table. A validator that cannot fail on any
// input is the defect this task exists to prevent.
func TestValidateMutationCheck(t *testing.T) {
	good, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000

	// A real frame, laid out at good's offsets: previous is NULL, owner is
	// CSTACK.
	r := fakeReader{
		u64: map[uint64]uint64{frame + uint64(good.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(good.FrameOwner): FrameOwnedByCStack},
	}
	if err := good.Validate(r, frame); err != nil {
		t.Fatalf("sanity: the correct table must validate its own frame: %v", err)
	}

	// A corrupted copy: FrameOwner now points at some other field's bytes,
	// which the fakeReader has nothing mapped for.
	bad := good
	bad.FrameOwner = good.FrameOwner + 1
	if err := bad.Validate(r, frame); err == nil {
		t.Fatal("a table with a corrupted FrameOwner offset must be refused, not silently accepted")
	}
}

// TestOwnerEnumRenumberedOn314 pins down the most consequential divergence
// this task found: CPython 3.14 inserted FRAME_OWNED_BY_INTERPRETER before
// FRAME_OWNED_BY_CSTACK (pycore_interpframe_structs.h, CPython 3.14.3), so
// the enum's ceiling and the CSTACK sentinel both move. A single shared
// "owner <= 3" bound -- correct for 3.12/3.13 -- would silently misclassify
// 3.14 frames: it would refuse a real CSTACK frame (owner 4) as
// out-of-range, and would accept an INTERPRETER frame (owner 3) as if it
// were the CSTACK entry frame. TableFor must carry the bound and the
// sentinel per version, not as shared package constants.
func TestOwnerEnumRenumberedOn314(t *testing.T) {
	o312, _ := TableFor(Version{3, 12, 14})
	o314, _ := TableFor(Version{3, 14, 3})

	if o312.FrameOwnerMax != 3 || o312.FrameOwnerCStack != 3 {
		t.Fatalf("3.12: want max=3 cstack=3, got max=%d cstack=%d", o312.FrameOwnerMax, o312.FrameOwnerCStack)
	}
	if o314.FrameOwnerMax != 4 || o314.FrameOwnerCStack != 4 {
		t.Fatalf("3.14: want max=4 cstack=4 (FRAME_OWNED_BY_INTERPRETER inserted at 3), got max=%d cstack=%d", o314.FrameOwnerMax, o314.FrameOwnerCStack)
	}

	const frame = 0x7f0000000000

	// owner=4 is CSTACK on 3.14 and must validate there...
	r4 := fakeReader{
		u64: map[uint64]uint64{frame + uint64(o314.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(o314.FrameOwner): 4},
	}
	if err := o314.Validate(r4, frame); err != nil {
		t.Fatalf("owner=4 (CSTACK on 3.14) must validate against the 3.14 table: %v", err)
	}
	// ...but is out of range on 3.12, where the enum tops out at 3.
	if err := o312.Validate(r4, frame); err == nil {
		t.Fatal("owner=4 must be refused against the 3.12 table -- 3.12's enum has no value 4")
	}
}

// TestThreadStateFrameIndirectionOnlyOn312 pins the second divergence: 3.12
// has no PyThreadState.current_frame field (removed cframe indirection
// landed in 3.13). A table that quietly assumed the 3.13/3.14 shape for
// 3.12 would have the walker read a *_PyCFrame value and treat it as a
// *_PyInterpreterFrame directly.
func TestThreadStateFrameIndirectionOnlyOn312(t *testing.T) {
	o312, _ := TableFor(Version{3, 12, 14})
	o313, _ := TableFor(Version{3, 13, 15})
	o314, _ := TableFor(Version{3, 14, 3})

	if !o312.ThreadStateFrameIndirect {
		t.Fatal("3.12: ThreadStateFrame must be marked indirect (tstate->cframe->current_frame)")
	}
	if o313.ThreadStateFrameIndirect || o314.ThreadStateFrameIndirect {
		t.Fatal("3.13/3.14: ThreadStateFrame is tstate->current_frame directly, not indirect")
	}
}

// TestFrameExecutableTaggedOnlyOn314 pins the third divergence: 3.14's
// f_executable is a _PyStackRef, a tagged union, not a plain PyObject*. On
// a normal (non free-threaded) build the low bit is a refcount tag
// (Py_TAG_REFCNT, pycore_stackref.h) that must be masked off before the
// value is a usable pointer.
func TestFrameExecutableTaggedOnlyOn314(t *testing.T) {
	o312, _ := TableFor(Version{3, 12, 14})
	o313, _ := TableFor(Version{3, 13, 15})
	o314, _ := TableFor(Version{3, 14, 3})

	if o312.FrameExecutableTagged || o313.FrameExecutableTagged {
		t.Fatal("3.12/3.13: f_code / f_executable are plain PyObject*, not tagged")
	}
	if !o314.FrameExecutableTagged {
		t.Fatal("3.14: f_executable is a tagged _PyStackRef")
	}
}
