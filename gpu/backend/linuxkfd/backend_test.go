package linuxkfd

import (
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

func TestNewRejectsMissingPID(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestBackendIDAndCapabilities(t *testing.T) {
	b, err := New(Config{PID: 123})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.ID(); got != gpu.BackendLinuxKFD {
		t.Fatalf("ID()=%q", got)
	}
	if got := b.EventBackends(); len(got) != 1 || got[0] != gpu.BackendLinuxKFD {
		t.Fatalf("EventBackends()=%v", got)
	}
	got := b.Capabilities()
	want := []gpu.GPUCapability{gpu.CapabilityLifecycleTimeline}
	if len(got) != len(want) {
		t.Fatalf("len(capabilities)=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cap[%d]=%q want %q", i, got[i], want[i])
		}
	}
}
