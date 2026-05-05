package amdsample

import (
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

func TestNewRejectsMissingReader(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBackendIDAndCapabilities(t *testing.T) {
	b, err := New(Config{Reader: strings.NewReader("")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := b.ID(); got != gpu.BackendAMDSample {
		t.Fatalf("ID()=%q want %q", got, gpu.BackendAMDSample)
	}
	if got := b.EventBackends(); got != nil {
		t.Fatalf("EventBackends()=%v want nil", got)
	}
	if got := b.Capabilities(); len(got) == 0 {
		t.Fatal("expected capabilities")
	}
}
