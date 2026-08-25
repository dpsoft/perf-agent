package gpuprobe_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
)

// nopSink accepts everything. Attach only checks that a sink is present, so
// nothing here is ever called on these error paths.
type nopSink struct{}

func (nopSink) EmitLaunch(gpu.GPUKernelLaunch) error           { return nil }
func (nopSink) EmitExec(gpu.GPUKernelExec) error               { return nil }
func (nopSink) EmitPCSample(gpu.GPUPCSample) error             { return nil }
func (nopSink) EmitModule(gpu.GPUModule) error                 { return nil }
func (nopSink) EmitEvent(gpu.GPUTimelineEvent) error           { return nil }
func (nopSink) EmitSamplingWindow(gpu.GPUSamplingWindow) error { return nil }

// Attach must return an error, never panic, when there is no such file.
func TestAttachNonexistentShimReturnsError(t *testing.T) {
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: filepath.Join(t.TempDir(), "no-such-shim.so"),
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     nopSink{},
	})
	require.Error(t, err)
	assert.Nil(t, c)
}

// A well-formed ELF that simply carries no perfagent probes is the other
// pre-load rejection. The running test binary is exactly such an ELF.
func TestAttachELFWithoutPerfagentProbesReturnsError(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: self,
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     nopSink{},
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "no perfagent probes found")
	assert.Nil(t, c)
}

// A missing sink is rejected before anything is opened.
func TestAttachWithoutSinkReturnsError(t *testing.T) {
	c, err := gpuprobe.Attach(gpuprobe.Config{ShimPath: "/proc/self/exe"})
	require.Error(t, err)
	assert.Nil(t, c)
}

// The regression that motivated this test: every failure *after* the Consumer
// is constructed runs Attach's cleanup defer, and a `return nil, err` there
// nils the named result before the defer fires — so the defer called Close on
// a nil receiver and panicked in the caller, leaking the loaded programs and
// maps on the way out. Without CAP_BPF the object load is exactly such a
// failure, so an unprivileged run walks the path that used to panic.
func TestAttachFailingInsideTheCleanupDeferDoesNotPanic(t *testing.T) {
	if hasBPFAndPerfmon() {
		t.Skip("this exercises the unprivileged load failure; run it without CAP_BPF")
	}
	stub := filepath.Join("..", "shim", "perfagent-gpu-stub")
	requireBuilt(t, stub)

	// Attach discovers real probes here, so it gets past the pre-load checks
	// and into the deferred region before the BPF load is refused.
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: stub,
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     nopSink{},
	})
	if err == nil {
		// Some kernels permit the load unprivileged; then there is no failure
		// to observe, but there is also nothing to leak.
		t.Cleanup(func() { _ = c.Close() })
		t.Skip("the BPF load succeeded without CAP_BPF; nothing to assert")
	}
	t.Logf("attach failed as expected: %v", err)
	// The failure has to be one raised *inside* the deferred region, or this
	// test is checking the pre-load rejections the other cases already cover
	// and the regression could come back unnoticed.
	assert.NotContains(t, err.Error(), "no perfagent probes found",
		"the stub must carry probes, or this never reaches the cleanup defer")
	assert.NotContains(t, err.Error(), "parse usdt notes",
		"the stub must parse, or this never reaches the cleanup defer")
	assert.Nil(t, c, "a failed Attach returns no consumer")
}

// Close is called by Attach's cleanup defer on whatever the named result
// holds, and callers routinely `defer c.Close()` next to a checked error.
// Both must survive a nil receiver.
func TestCloseOnNilConsumerIsSafe(t *testing.T) {
	var c *gpuprobe.Consumer
	assert.NotPanics(t, func() {
		assert.NoError(t, c.Close())
	})
}
