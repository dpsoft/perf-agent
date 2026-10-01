package perfagent

import (
	"strings"
	"testing"
)

// TestGPUAloneIsAValidRun pins the guard that would otherwise refuse it.
//
// Config.validate() required one of CPU/off-CPU/PMU. Adding a fourth
// collector without adding it to that list makes `--gpu` on its own fail
// with "at least one of ... must be enabled", which names every mode except
// the one the operator asked for.
func TestGPUAloneIsAValidRun(t *testing.T) {
	c := DefaultConfig()
	c.EnableCPUProfile = false
	c.EnableOffCPUProfile = false
	c.EnablePMU = false
	c.EnableGPU = true
	c.GPUShimPath = "/some/libperfagent-gpu-nvidia.so"
	c.PID = 1234

	if err := c.validate(); err != nil {
		t.Fatalf("--gpu alone should be a valid run: %v", err)
	}
}

// TestGPUNeedsTheShimPath: the uprobe attaches to the adapter's inode, so
// there is nothing to attach to without it.
func TestGPUNeedsTheShimPath(t *testing.T) {
	c := DefaultConfig()
	c.EnableGPU = true
	c.PID = 1234

	err := c.validate()
	if err == nil {
		t.Fatal("GPU profiling without a shim path should be refused")
	}
	if !strings.Contains(err.Error(), "CUDA_INJECTION64_PATH") {
		t.Fatalf("the error should say where the path comes from, got: %v", err)
	}
}

// TestGPUIsRefusedSystemWide.
//
// PID 0 is not a weaker attach, it is the broader one, and it IS what the
// node collector uses -- but that needs hostPID and the shim-inode
// machinery gpu-cuda-profile's collector mode carries. Accepting it here
// would attach, find nothing, and report success, which is the failure
// shape this repo keeps finding (#156, #161, #165).
func TestGPUIsRefusedSystemWide(t *testing.T) {
	c := DefaultConfig()
	c.EnableGPU = true
	c.GPUShimPath = "/some/shim.so"
	c.SystemWide = true
	c.PID = 0

	err := c.validate()
	if err == nil {
		t.Fatal("system-wide GPU profiling should be refused rather than silently finding nothing")
	}
	if !strings.Contains(err.Error(), "collector") {
		t.Fatalf("the error should point at the mode that does support it, got: %v", err)
	}
}

// TestGPUOptionsReachTheConfig covers the wiring from the With* options,
// which is the half a CLI flag depends on.
func TestGPUOptionsReachTheConfig(t *testing.T) {
	a, err := New(
		WithPID(1234),
		WithGPU("/opt/shim.so"),
		WithGPUProfilePath("/tmp/gpu.pb.gz"),
		WithGPUFlamegraph("/tmp/gpu.html"),
		WithGPUKeepInstrumentationFrames(),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := a.config
	if !c.EnableGPU {
		t.Error("WithGPU did not enable the collector")
	}
	if c.GPUShimPath != "/opt/shim.so" {
		t.Errorf("GPUShimPath = %q", c.GPUShimPath)
	}
	if c.GPUProfilePath != "/tmp/gpu.pb.gz" {
		t.Errorf("GPUProfilePath = %q", c.GPUProfilePath)
	}
	if c.GPUFlamegraphPath != "/tmp/gpu.html" {
		t.Errorf("GPUFlamegraphPath = %q", c.GPUFlamegraphPath)
	}
	if !c.GPUKeepInstrumentationFrames {
		t.Error("WithGPUKeepInstrumentationFrames did not take")
	}
}
