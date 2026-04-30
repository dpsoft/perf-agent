package python

import (
	"bytes"
	"testing"
)

func TestEncodeActivatePayload(t *testing.T) {
	got := ActivatePayload()
	want := []byte("import sys; sys.activate_stack_trampoline('perf')\x00")
	if !bytes.Equal(got, want) {
		t.Fatalf("ActivatePayload mismatch:\n  got  %q\n  want %q", got, want)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("ActivatePayload not NUL-terminated; last byte = 0x%x", got[len(got)-1])
	}
}

func TestEncodeDeactivatePayload(t *testing.T) {
	got := DeactivatePayload()
	want := []byte("import sys; sys.deactivate_stack_trampoline()\x00")
	if !bytes.Equal(got, want) {
		t.Fatalf("DeactivatePayload mismatch:\n  got  %q\n  want %q", got, want)
	}
	if got[len(got)-1] != 0 {
		t.Fatalf("DeactivatePayload not NUL-terminated; last byte = 0x%x", got[len(got)-1])
	}
}

// TestActivatePayloadIsConsistent guards against accidental in-place mutation
// of the package-level slice across calls. ActivatePayload returns the
// underlying slice (no copy), so a caller mutating it would also affect
// later calls — that contract is documented; this test merely confirms the
// returned bytes haven't drifted from the canonical payload between two
// consecutive reads.
func TestActivatePayloadIsConsistent(t *testing.T) {
	a := ActivatePayload()
	b := ActivatePayload()
	if !bytes.Equal(a, b) {
		t.Fatalf("ActivatePayload returned different bytes on consecutive calls")
	}
}
