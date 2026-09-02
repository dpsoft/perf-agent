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

// fakeReader answers only the addresses it is given and errors (errBadRead)
// on anything else. That sparseness is exactly right for testing "a read
// failed" -- it is wrong for testing "the table is wrong", because a
// shifted/corrupted offset then gets refused for having no mapping at all,
// not for reading an implausible value. Use denseReader for the latter.
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

// denseReader answers every address, as a real BPF reader over a live
// process would: there is no such thing as "unmapped" inside a mapped
// frame, only bytes that hold something. Addresses not given an explicit
// override read back as defaultU64/defaultU8. This is the reader to use
// when the claim under test is "Validate rejects this value", not
// "Validate rejects this unreadable address" -- a sparse map would let the
// missing-mapping error do the rejecting instead of the value check.
type denseReader struct {
	defaultU64 uint64
	defaultU8  uint8
	u64        map[uint64]uint64
	u8         map[uint64]uint8
}

func (d denseReader) ReadU64(a uint64) (uint64, error) {
	if v, ok := d.u64[a]; ok {
		return v, nil
	}
	return d.defaultU64, nil
}

func (d denseReader) ReadU8(a uint64) (uint8, error) {
	if v, ok := d.u8[a]; ok {
		return v, nil
	}
	return d.defaultU8, nil
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

// The point of validation: a table whose owner offset reads a byte that is
// not an owner enum value must be refused, and refused because the value
// is implausible -- not because some other address happened to be
// unmapped. denseReader closes that gap: every address answers something,
// so the only way Validate can fail here is the value check itself.
func TestValidateRefusesAnImplausibleOwner(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := denseReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0},
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): 0x5a}, // not an owner
	}
	err := o.Validate(r, frame)
	if err == nil {
		t.Fatal("an owner byte outside the enum must be refused")
	}
	if !errors.Is(err, ErrOffsetsImplausible) {
		t.Fatalf("refusal must be ErrOffsetsImplausible, got %v", err)
	}
	if errors.Is(err, ErrOffsetsUnreadable) {
		t.Fatalf("this is a value refusal, not a read failure: %v", err)
	}
}

// TestValidateRefusesAnImplausiblePrevious is the FramePrevious half of the
// same check: a previous pointer that is neither NULL nor a plausible
// userspace address must be refused too, against a dense reader so nothing
// else can be blamed for the failure.
func TestValidateRefusesAnImplausiblePrevious(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := denseReader{
		u64: map[uint64]uint64{frame + uint64(o.FramePrevious): 0x42}, // below 0x1000, not NULL
		u8:  map[uint64]uint8{frame + uint64(o.FrameOwner): FrameOwnedByCStack},
	}
	err := o.Validate(r, frame)
	if err == nil {
		t.Fatal("a previous pointer that is neither NULL nor plausible must be refused")
	}
	if !errors.Is(err, ErrOffsetsImplausible) {
		t.Fatalf("refusal must be ErrOffsetsImplausible, got %v", err)
	}
}

// TestValidateUnreadableAddressIsDistinguishable confirms the other half of
// I1: a read failure (the reader errors, rather than answering with an
// implausible value) is ErrOffsetsUnreadable, not ErrOffsetsImplausible.
// Task 6 needs to tell "the table is wrong" apart from "the read failed",
// and until this sentinel existed it could not.
func TestValidateUnreadableAddressIsDistinguishable(t *testing.T) {
	o, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000
	r := fakeReader{} // answers nothing; every read fails
	err := o.Validate(r, frame)
	if err == nil {
		t.Fatal("an unreadable frame must be refused")
	}
	if !errors.Is(err, ErrOffsetsUnreadable) {
		t.Fatalf("a read failure must be ErrOffsetsUnreadable, got %v", err)
	}
	if errors.Is(err, ErrOffsetsImplausible) {
		t.Fatalf("this is a read failure, not a value refusal: %v", err)
	}
	if !errors.Is(err, errBadRead) {
		t.Fatalf("the underlying reader error must still be reachable via errors.Is: %v", err)
	}
}

// TestValidateIsAPlausibilityScreenNotAWrongTableDetector is the honest
// replacement for what used to be called a "mutation check" here. Against
// a fakeReader (sparse map), shifting FrameOwner by one byte "worked" --
// but only because the shifted address had nothing mapped, so errBadRead
// did the rejecting, not the owner check. Against a denseReader, which
// answers every address the way a real process's memory does, the shifted
// read lands on a byte that happens to still be a valid enum value (this
// table's default filler, 0, is FRAME_OWNED_BY_THREAD) and Validate has no
// way to tell that it is reading the wrong field. It accepts the corrupted
// table.
//
// That is not a bug in Validate: a one-byte plausibility screen cannot
// distinguish "the right field, a legitimate value" from "the wrong field,
// a coincidentally legitimate value". It is a real, permanent limit on
// what cheap self-consistency checks can catch. The actual wrong-table
// gate is offsets_fixture_test.go, which re-derives every offset from a
// real interpreter's own headers instead of inferring correctness from the
// shape of one frame. Task 6 must not treat a passing Validate call as
// proof the table is right -- only as proof this one frame wasn't
// obviously wrong.
func TestValidateIsAPlausibilityScreenNotAWrongTableDetector(t *testing.T) {
	good, _ := TableFor(Version{3, 12, 14})
	const frame = 0x7f0000000000

	r := denseReader{
		defaultU64: 0, // every unlisted address reads as a NULL previous pointer
		defaultU8:  0, // every unlisted address reads as owner=FRAME_OWNED_BY_THREAD
		u64:        map[uint64]uint64{frame + uint64(good.FramePrevious): 0},
		u8:         map[uint64]uint8{frame + uint64(good.FrameOwner): FrameOwnedByCStack},
	}
	if err := good.Validate(r, frame); err != nil {
		t.Fatalf("sanity: the correct table must validate its own frame: %v", err)
	}

	// A corrupted copy: FrameOwner is shifted by one byte. Against a real
	// process's memory (modeled here by the dense reader's default), that
	// byte is not garbage -- it is whatever the neighboring field holds,
	// and here it happens to decode as a legitimate owner value.
	bad := good
	bad.FrameOwner = good.FrameOwner + 1
	if err := bad.Validate(r, frame); err != nil {
		t.Fatalf("known limitation stopped being a limitation: expected the shifted table to be (wrongly) accepted, got refusal %v -- update this test and the comment above if Validate's checks changed", err)
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

	// owner=4 is CSTACK on 3.14 and must validate there... (fakeReader is
	// deliberately sparse here: this test's claim is only "refused",
	// never "refused because the value is implausible", so there is no
	// honesty gap in letting an unmapped address do the refusing when
	// o312's differently-located FrameOwner falls outside what this
	// 3.14-shaped fixture defines.)
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
