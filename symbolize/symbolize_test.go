package symbolize

import "testing"

func TestFrameZeroValue(t *testing.T) {
	var f Frame
	if f.Reason != FailureNone {
		t.Fatalf("zero Frame.Reason = %d, want %d", f.Reason, FailureNone)
	}
	if f.Name != "" {
		t.Fatalf("zero Frame.Name = %q, want empty", f.Name)
	}
}

func TestFailureReasonExhaustive(t *testing.T) {
	// Locks the iota order: changes here are deliberate API-shape changes.
	want := map[FailureReason]string{
		FailureNone:              "none",
		FailureUnmapped:          "unmapped",
		FailureInvalidFileOffset: "invalid_file_offset",
		FailureMissingComponent:  "missing_component",
		FailureMissingSymbols:    "missing_symbols",
		FailureUnknownAddress:    "unknown_address",
		FailureFetchError:        "fetch_error",
		FailureNoBuildID:         "no_build_id",
	}
	for r, name := range want {
		if r.String() != name {
			t.Fatalf("FailureReason(%d).String() = %q, want %q", r, r.String(), name)
		}
	}
}
