package flamegraph

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassifyCoversTheRealRTX3090Stack(t *testing.T) {
	// Every frame below is taken verbatim from the CPU+GPU profile in
	// /home/diego/gpu-cuda-45.pb.gz, root-first. If the classifier drifts,
	// the colours stop meaning what the legend says they mean.
	cases := []struct {
		name   string
		module string
		want   Domain
	}{
		{"_start", "", DomainSystem},
		{"__libc_start_main_alias_1", "", DomainSystem},
		{"__libc_start_call_main", "", DomainSystem},
		{"main", "", DomainApplication},
		{"__device_stub__Z14perfagent_axpyfPKfPfi(float, float const*, float*, int)", "", DomainApplication},
		{"cudaLaunchKernel", "", DomainVendorRuntime},
		{"0x7f2c958b71c6", "", DomainUnsymbolized},
		{"0x7f2c944de06b", "", DomainUnsymbolized},
		{"(anonymous namespace)::on_callback(void*, CUpti_CallbackDomain, unsigned int, void const*)", "", DomainProfilerShim},
		{"[gpu:launch]", "", DomainBoundary},
		{"[gpu:launch unsampled]", "", DomainBoundaryUnattributed},
		{"[gpu:kernel:_Z14perfagent_axpyfPKfPfi]", "", DomainGPUKernel},
		{"[gpu:kernel:_Z15perfagent_scalePffi]", "", DomainGPUKernel},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Classify(c.name, c.module), "frame %q", c.name)
	}
}

func TestClassifyPrefersTheProfilesMappingOverTheSymbolName(t *testing.T) {
	// The mapping is something the profile asserts; the name is something
	// we infer from. When both are available the profile wins.
	assert.Equal(t, DomainKernel, Classify("finish_task_switch.isra.0", "[kernel]"))
	assert.Equal(t, DomainVendorRuntime, Classify("someInternalThing", "/usr/lib/libcuda.so.550.54.14"))
	assert.Equal(t, DomainProfilerShim, Classify("emit_launch", "/opt/libperfagent-gpu-nvidia.so"))
	assert.Equal(t, DomainSystem, Classify("memcpy", "/usr/lib64/libc.so.6"))
}

func TestClassifyDoesNotBillOrdinaryCallbacksToTheProfiler(t *testing.T) {
	// "on_callback" alone is a name any program may use. Only the vendor
	// callback signatures perf-agent's shim actually registers count.
	assert.Equal(t, DomainApplication, Classify("myapp::on_callback(int)", ""))
	assert.Equal(t, DomainProfilerShim, Classify("on_callback(void*, CUpti_CallbackDomain)", ""))
	assert.Equal(t, DomainProfilerShim, Classify("on_callback(rocprofiler_record_t)", ""))
}

func TestClassifyUnknownFrame(t *testing.T) {
	assert.Equal(t, DomainUnsymbolized, Classify("[unknown]", ""))
	assert.Equal(t, DomainUnsymbolized, Classify("0xdeadbeef", ""))
	assert.Equal(t, DomainApplication, Classify("0xNotHex", ""))
	assert.Equal(t, DomainApplication, Classify("0x", ""))
}

func TestClassifyCUDADriverAPI(t *testing.T) {
	assert.Equal(t, DomainVendorRuntime, Classify("cuLaunchKernel", ""))
	assert.Equal(t, DomainVendorRuntime, Classify("cuMemcpyDtoH_v2", ""))
	assert.Equal(t, DomainVendorRuntime, Classify("hipLaunchKernel", ""))
	assert.Equal(t, DomainVendorRuntime, Classify("hsa_signal_wait_scacquire", ""))
	// "cu" followed by lowercase is not the driver API; do not over-claim.
	assert.Equal(t, DomainApplication, Classify("cutlass_gemm", ""))
	assert.Equal(t, DomainApplication, Classify("current_thread", ""))
}

func TestEveryDomainHasAFillAndALegendEntry(t *testing.T) {
	for d := Domain(0); d < numDomains; d++ {
		info := d.Info()
		assert.NotEmpty(t, info.Key, "domain %d", d)
		assert.NotEmpty(t, info.Label, "domain %d", d)
		assert.NotEmpty(t, info.Desc, "domain %d", d)
		assert.NotEmpty(t, info.Fill, "domain %d", d)
	}
}

func TestBoundaryAndUnattributedBoundaryLookDifferent(t *testing.T) {
	// One means "GPU time we have a CPU stack for", the other "GPU time we
	// do not". They must not be confusable at a glance.
	a := DomainBoundary.Info()
	b := DomainBoundaryUnattributed.Info()
	assert.NotEqual(t, a.Fill, b.Fill)
	assert.Empty(t, a.Overlay)
	assert.NotEmpty(t, b.Overlay, "unattributed GPU time must be hatched")
}
