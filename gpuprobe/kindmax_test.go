package gpuprobe

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKindMaxPinsTheBPFDropAccountingArrays is the drift guard for the one
// constant on this ABI that is duplicated in two languages and whose
// disagreement is silent in both directions.
//
// kindMax mirrors KIND_MAX in bpf/gpu_usdt.bpf.c, which sizes the `dropped`
// and `stacks_missing` BPF arrays AND bounds count_drop / count_stack_missing.
// Neither end errors when they disagree:
//
//   - Go too large: Consumer.Stats() reads slots the map does not have. The
//     lookups fail, the counts read zero, and a drop storm reports green.
//   - Go too small: the highest kinds' drops are never read at all. Same
//     symptom, different cause — and the C side would still have refused
//     nothing, because count_drop's `kind >= KIND_MAX` guard uses the C
//     constant, not this one.
//
// A mis-sized drop array is therefore a counter that cannot go non-zero,
// which spec §6.1 forbids outright. So the pin is against the *embedded
// object* — the bytecode that actually ships — and not against a re-read of
// the C source, which would only prove that two files agree about a number
// neither of them compiled.
//
// The C source IS also read, but for a different claim: that KIND_MAX is
// still the literal the maps are declared with, so a future edit that
// decouples the map size from the constant fails here rather than passing
// vacuously.
func TestKindMaxPinsTheBPFDropAccountingArrays(t *testing.T) {
	spec, err := loadGpuusdt()
	require.NoError(t, err)

	for _, name := range []string{"dropped", "stacks_missing"} {
		require.Containsf(t, spec.Maps, name, "the %s accounting array is gone", name)
		assert.Equalf(t, uint32(kindMax), spec.Maps[name].MaxEntries,
			"Go kindMax=%d but the embedded BPF object sizes %s at %d entries. "+
				"KIND_MAX in bpf/gpu_usdt.bpf.c and kindMax in gpuprobe/consumer.go must "+
				"move in the SAME commit, and `make generate` must have been re-run: a "+
				"mismatch mis-sizes the drop accounting silently, so a drop storm reads "+
				"as zero drops", kindMax, name, spec.Maps[name].MaxEntries)
	}

	// The two arrays must stay distinct objects: a failed stack capture is
	// not a dropped record, and folding them would report phantom loss.
	assert.NotSame(t, spec.Maps["dropped"], spec.Maps["stacks_missing"])

	// And the C constant itself, so the maps cannot quietly stop being
	// declared in terms of KIND_MAX.
	src, err := os.ReadFile("../bpf/gpu_usdt.bpf.c")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^#define\s+KIND_MAX\s+(\d+)`).FindSubmatch(src)
	require.NotNil(t, m, "no #define KIND_MAX in bpf/gpu_usdt.bpf.c")
	cKindMax, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	assert.Equal(t, kindMax, cKindMax, "kindMax must mirror KIND_MAX literally")

	for _, decl := range []string{"dropped", "stacks_missing"} {
		re := regexp.MustCompile(`__uint\(max_entries, KIND_MAX\);[\s\S]{0,400}?\}\s+` + decl + `\s+SEC\("\.maps"\)`)
		assert.Truef(t, re.Match(src),
			"the %s map must be declared with max_entries KIND_MAX, so this pin cannot pass vacuously", decl)
	}

	// Every kind the Go side can name must be addressable in those arrays.
	// count_drop drops anything >= KIND_MAX on the floor without counting it
	// anywhere, so a cookie past the end is loss with no counter at all.
	for _, probe := range knownProbeNames {
		k := cookieFor(probe)
		require.NotZerof(t, k, "cookieFor(%q) returned 0: the probe would not be attached at all", probe)
		assert.Lessf(t, uint32(k), uint32(kindMax),
			"%s is kind %d, outside the %d-slot drop array; count_drop discards its drops uncounted",
			probe, k, kindMax)
	}
}

// knownProbeNames is every probe cookieFor knows. Kept beside the pin above
// because the failure it guards against is adding a kind and forgetting the
// array that counts its drops.
var knownProbeNames = []string{
	"gpu_launch_v1",
	"gpu_exec_v1",
	"gpu_module_load_v1",
	"gpu_pc_sample_batch_v1",
	"gpu_launch_sampled_v1",
	"gpu_kernel_name_v1",
	"gpu_stall_reason_map_v1",
	"gpu_sampling_window_v1",
	"gpu_config_v1",
}
