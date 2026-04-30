package ptraceop

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrRemoteCallNonZero_ErrorMessage(t *testing.T) {
	e := &ErrRemoteCallNonZero{Op: "PyRun_SimpleString", Result: 0xFFFFFFFF}
	got := e.Error()
	want := fmt.Sprintf("PyRun_SimpleString returned non-zero: %d", uint64(0xFFFFFFFF))
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// TestErrRemoteCallNonZero_ErrorsAs ensures callers can extract the typed
// error from a wrapped chain — that's how the perfagent bridge classifies
// PyRun_SimpleString returning -1 as ErrNoPerfTrampoline.
func TestErrRemoteCallNonZero_ErrorsAs(t *testing.T) {
	original := &ErrRemoteCallNonZero{Op: "PyRun_SimpleString", Result: 0xFFFFFFFF}
	wrapped := fmt.Errorf("RemoteActivate: %w", original)

	var got *ErrRemoteCallNonZero
	if !errors.As(wrapped, &got) {
		t.Fatal("errors.As failed to extract *ErrRemoteCallNonZero from wrapped error")
	}
	if int32(got.Result) != -1 {
		t.Errorf("int32(Result) = %d, want -1 (sign-extended)", int32(got.Result))
	}
}
