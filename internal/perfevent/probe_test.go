package perfevent

import "testing"

func TestEventSpec_String(t *testing.T) {
	hw := EventSpec{Type: PerfTypeHardware, Config: PerfCountHWCPUCycles}
	if got := hw.String(); got != "hardware/cpu-cycles" {
		t.Errorf("hw.String() = %q, want %q", got, "hardware/cpu-cycles")
	}
	sw := EventSpec{Type: PerfTypeSoftware, Config: PerfCountSWCPUClock}
	if got := sw.String(); got != "software/cpu-clock" {
		t.Errorf("sw.String() = %q, want %q", got, "software/cpu-clock")
	}
	other := EventSpec{Type: 99, Config: 42}
	if got := other.String(); got != "type=99/config=42" {
		t.Errorf("other.String() = %q, want %q", got, "type=99/config=42")
	}
}
