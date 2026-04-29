package gpu

import (
	"encoding/json"
	"testing"
)

func TestCapabilityConstantsStable(t *testing.T) {
	got := CapabilityNames()
	want := []GPUCapability{
		"launch-trace",
		"exec-timeline",
		"device-counters",
		"gpu-pc-sampling",
		"stall-reasons",
		"gpu-source-correlation",
		"lifecycle-timeline",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cap[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestBackendIDConstantsStable(t *testing.T) {
	got := []GPUBackendID{
		BackendLinuxDRM,
		BackendLinuxKFD,
		BackendAMDSample,
		BackendStream,
		BackendReplay,
		BackendHIP,
		BackendHostReplay,
	}
	want := []GPUBackendID{
		"linuxdrm",
		"linuxkfd",
		"amdsample",
		"stream",
		"replay",
		"hip",
		"host-replay",
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("backend[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestLaunchRoundTripJSON(t *testing.T) {
	in := GPUKernelLaunch{
		Correlation: CorrelationID{Backend: "replay", Value: "corr-1"},
		KernelName:  "flash_attn_fwd",
		Launch:      LaunchContext{PID: 42, TID: 43},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out GPUKernelLaunch
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.KernelName != in.KernelName || out.Correlation != in.Correlation {
		t.Fatalf("round-trip mismatch: %#v vs %#v", out, in)
	}
}
