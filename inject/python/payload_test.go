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

func TestPayloadsAreImmutable(t *testing.T) {
	a := ActivatePayload()
	b := ActivatePayload()
	if !bytes.Equal(a, b) {
		t.Fatalf("ActivatePayload returned different slices on consecutive calls")
	}
}
