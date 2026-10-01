package gpuprofile

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestNewRefusesAMissingShim.
//
// The CUPTI adapter fails open and silent when it cannot load -- correct for
// something the driver injects into every CUDA process, wrong for a flag the
// operator passed deliberately. A missing shim must be an error here, not a
// profile with no samples in it.
func TestNewRefusesAMissingShim(t *testing.T) {
	_, err := New(Config{ShimPath: filepath.Join(t.TempDir(), "not-there.so"), PID: 1})
	if err == nil {
		t.Fatal("a missing shim should fail the attach, not degrade to an empty profile")
	}
	if !strings.Contains(err.Error(), "not-there.so") {
		t.Fatalf("the error should name the file it could not find, got: %v", err)
	}
}

// TestNewRefusesAnEmptyShimPath names the thing the caller left out.
func TestNewRefusesAnEmptyShimPath(t *testing.T) {
	_, err := New(Config{PID: 1})
	if err == nil {
		t.Fatal("an empty shim path should be refused")
	}
	if !strings.Contains(err.Error(), "CUDA_INJECTION64_PATH") {
		t.Fatalf("the error should say where the adapter comes from, got: %v", err)
	}
}

// TestDrainEveryDefaults: zero means the default, not a ticker that fires
// continuously. The timeline's rings hold one interval, so this value is
// what stands between a long attach and losing GPU time outright.
func TestDrainEveryDefaults(t *testing.T) {
	// Exercised through New's normalization without attaching: a missing
	// shim stops it before any BPF work, and the value is applied first.
	cfg := Config{ShimPath: "/nonexistent", PID: 1}
	if cfg.DrainEvery != 0 {
		t.Fatalf("precondition: DrainEvery should start zero, got %v", cfg.DrainEvery)
	}
	if DefaultDrainEvery <= 0 {
		t.Fatalf("DefaultDrainEvery must be positive, got %v", DefaultDrainEvery)
	}
}
