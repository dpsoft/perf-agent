package codec

import (
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

func TestDecodeLaunchLine(t *testing.T) {
	line := []byte(`{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":100}`)

	ev, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if ev.Kind != KindLaunch {
		t.Fatalf("kind=%q want %q", ev.Kind, KindLaunch)
	}
	if ev.Launch.KernelName != "flash_attn_fwd" {
		t.Fatalf("kernel=%q", ev.Launch.KernelName)
	}
	if ev.Launch.TimeNs != 100 {
		t.Fatalf("time_ns=%d", ev.Launch.TimeNs)
	}
	if ev.Launch.Correlation != (gpu.CorrelationID{Backend: "stream", Value: "c1"}) {
		t.Fatalf("correlation=%+v", ev.Launch.Correlation)
	}
	if ev.Launch.ClockDomain != gpu.ClockDomainCPUMonotonic {
		t.Fatalf("clock_domain=%v", ev.Launch.ClockDomain)
	}
}

func TestDecodeExecLine(t *testing.T) {
	line := []byte(`{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","start_ns":120,"end_ns":200}`)

	ev, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if ev.Kind != KindExec {
		t.Fatalf("kind=%q want %q", ev.Kind, KindExec)
	}
	if ev.Exec.KernelName != "flash_attn_fwd" {
		t.Fatalf("kernel=%q", ev.Exec.KernelName)
	}
	if ev.Exec.StartNs != 120 || ev.Exec.EndNs != 200 {
		t.Fatalf("exec=%+v", ev.Exec)
	}
}

func TestDecodeSampleLine(t *testing.T) {
	line := []byte(`{"kind":"sample","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":150,"stall_reason":"memory_throttle","weight":7}`)

	ev, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if ev.Kind != KindSample {
		t.Fatalf("kind=%q want %q", ev.Kind, KindSample)
	}
	if ev.Sample.KernelName != "flash_attn_fwd" {
		t.Fatalf("kernel=%q", ev.Sample.KernelName)
	}
	if ev.Sample.StallReason != "memory_throttle" {
		t.Fatalf("stall=%q", ev.Sample.StallReason)
	}
	if ev.Sample.Weight != 7 {
		t.Fatalf("weight=%d", ev.Sample.Weight)
	}
}

func TestDecodeTimelineEventLine(t *testing.T) {
	line := []byte(`{"kind":"event","event":{"backend":"linuxdrm","kind":"submit","family":"amdgpu","name":"amdgpu-cs","time_ns":130,"duration_ns":13,"pid":4242,"tid":4243,"source":"replay"}}`)

	ev, err := DecodeLine(line)
	if err != nil {
		t.Fatalf("DecodeLine: %v", err)
	}
	if ev.Kind != KindEvent {
		t.Fatalf("kind=%q want %q", ev.Kind, KindEvent)
	}
	if ev.Event.Kind != gpu.TimelineEventSubmit {
		t.Fatalf("event kind=%q", ev.Event.Kind)
	}
	if ev.Event.Family != "amdgpu" {
		t.Fatalf("family=%q", ev.Event.Family)
	}
	if ev.Event.Name != "amdgpu-cs" {
		t.Fatalf("name=%q", ev.Event.Name)
	}
	if ev.Event.DurationNs != 13 {
		t.Fatalf("duration=%d", ev.Event.DurationNs)
	}
	if ev.Event.ClockDomain != gpu.ClockDomainCPUMonotonic {
		t.Fatalf("clock_domain=%v", ev.Event.ClockDomain)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kind":"launch"`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsUnknownKind(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kind":"mystery","time_ns":100}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsMissingKind(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kernel_name":"flash_attn_fwd","time_ns":100}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsMissingKindPayload(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kind":"launch","time_ns":100}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsBadTimestampType(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kind":"launch","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","time_ns":"100"}`))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeRejectsUnsupportedClockDomain(t *testing.T) {
	_, err := DecodeLine([]byte(`{"kind":"exec","correlation":{"backend":"stream","value":"c1"},"kernel_name":"flash_attn_fwd","clock_domain":"gpu-device","start_ns":120,"end_ns":200}`))
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "unsupported clock domain") {
		t.Fatalf("err=%v", err)
	}
}
