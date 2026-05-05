package gpu

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCapabilityConstantsStable(t *testing.T) {
	got := CapabilityNames()
	want := []GPUCapability{
		CapabilityLaunchTrace,
		CapabilityExecTimeline,
		CapabilityDeviceCounters,
		CapabilityPCSampling,
		CapabilityStallReasons,
		CapabilitySourceMap,
		CapabilityLifecycleTimeline,
	}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d len(want)=%d", len(got), len(want))
	}
	wantNames := []string{
		"launch-trace",
		"exec-timeline",
		"device-counters",
		"gpu-pc-sampling",
		"stall-reasons",
		"gpu-source-correlation",
		"lifecycle-timeline",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cap[%d]=%v want %v", i, got[i], want[i])
		}
		if got[i].String() != wantNames[i] {
			t.Fatalf("cap[%d].String()=%q want %q", i, got[i].String(), wantNames[i])
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

func TestCapabilityRoundTripJSONUsesStableNames(t *testing.T) {
	in := []GPUCapability{
		CapabilityLaunchTrace,
		CapabilityPCSampling,
		CapabilitySourceMap,
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(buf)
	for _, want := range []string{
		"launch-trace",
		"gpu-pc-sampling",
		"gpu-source-correlation",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}

	var out []GPUCapability
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out)=%d len(in)=%d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("cap[%d]=%v want %v", i, out[i], in[i])
		}
	}
}

func TestCapabilityRejectsUnknownJSONValue(t *testing.T) {
	var out GPUCapability
	err := json.Unmarshal([]byte(`"totally-made-up-capability"`), &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown gpu capability") {
		t.Fatalf("err=%v", err)
	}
}

func TestClockDomainRoundTripJSONUsesStableNames(t *testing.T) {
	in := []ClockDomain{
		ClockDomainCPUMonotonic,
		ClockDomainSynced,
		ClockDomainGPUDevice,
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(buf)
	for _, want := range []string{
		"cpu-monotonic",
		"synced",
		"gpu-device",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}

	var out []ClockDomain
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("len(out)=%d len(in)=%d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("domain[%d]=%v want %v", i, out[i], in[i])
		}
	}
}

func TestClockDomainRejectsUnknownJSONValue(t *testing.T) {
	var out ClockDomain
	err := json.Unmarshal([]byte(`"not-a-clock-domain"`), &out)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown clock domain") {
		t.Fatalf("err=%v", err)
	}
}
