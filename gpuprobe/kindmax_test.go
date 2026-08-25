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
	"gpu_dropped_v1",
}

// Every probe a producer fires must have a cookie, and every cookie must name
// a probe some producer fires.
//
// The two failures this closes are both silent. A probe with no cookie is
// never attached, so its semaphore never arms, the shim's `enabled()` gate
// never opens and the records it would carry are simply never produced — the
// counters on both sides read zero and look healthy. That is exactly the state
// gpu_dropped_v1 was in until this phase: four drop classes defined in the
// header and no way for any of them to reach a consumer. The reverse, a cookie
// for a probe nobody fires, is a kind the BPF program sizes and the consumer
// decodes that can never arrive — dead wire surface that reads as coverage.
func TestEveryProducerProbeHasACookieAndViceVersa(t *testing.T) {
	re := regexp.MustCompile(`PERFAGENT_USDT_EMITTER\(\s*(\w+)\s*,\s*(\d+)\s*\)`)
	emitted := map[string]int{}
	for _, src := range []string{"../shim/stub/stub.cc", "../shim/nvidia/cupti_adapter.cc"} {
		b, err := os.ReadFile(src)
		require.NoError(t, err)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			size, err := strconv.Atoi(m[2])
			require.NoError(t, err)
			emitted[m[1]] = size
		}
	}
	require.NotEmpty(t, emitted)

	for probe := range emitted {
		assert.NotZerof(t, cookieFor(probe),
			"a producer fires %s but cookieFor does not know it: the probe is never "+
				"attached, its semaphore never arms, and every record it would carry "+
				"is silently never produced", probe)
	}
	// THE LIST IS EMPTY, and that is the state it was built to reach: every
	// probe the ABI defines is now fired by a producer, so no cookie names
	// dead wire surface that reads as coverage.
	//
	// It emptied one entry at a time and each time because this assertion
	// forced it, not because anyone remembered. gpu_module_load_v1 left when
	// Task 5 landed; gpu_sampling_window_v1 left when Tier A started emitting
	// a window around every burst. A future probe added ahead of its producer
	// goes back in here with the task that will fire it named, and comes out
	// the same way.
	notYetFired := map[string]string{}
	for _, probe := range knownProbeNames {
		if _, ok := emitted[probe]; ok {
			assert.NotContainsf(t, notYetFired, probe,
				"%s is fired by a producer now; drop it from notYetFired so the "+
					"list keeps meaning what it says", probe)
			continue
		}
		why, expected := notYetFired[probe]
		assert.Truef(t, expected,
			"cookieFor installs %s but no producer in shim/ fires it: the kind is "+
				"sized, decoded and counted, and can never arrive", probe)
		_ = why
	}
}

// The wire size each producer pins in its emitter must match what the BPF
// program copies for that kind. They are separate constants in separate
// languages; a disagreement makes the kernel read past the end of the record
// buffer or truncate every record in a batch, and errors nowhere.
func TestProducerWireSizesMatchTheBPFRecordSizes(t *testing.T) {
	re := regexp.MustCompile(`PERFAGENT_USDT_EMITTER\(\s*(\w+)\s*,\s*(\d+)\s*\)`)
	src, err := os.ReadFile("../shim/nvidia/cupti_adapter.cc")
	require.NoError(t, err)
	bpfSrc, err := os.ReadFile("../bpf/gpu_usdt.bpf.c")
	require.NoError(t, err)

	// kind -> REC_* name, mirroring record_size() in the BPF program.
	recName := map[uint64]string{
		kindLaunch: "REC_LAUNCH", kindExec: "REC_EXEC", kindModule: "REC_MODULE",
		kindPC: "REC_PC", kindLaunchSampled: "REC_LAUNCH_SAMPLED",
		kindKernelName: "REC_KERNEL_NAME", kindStallMap: "REC_STALL_MAP",
		kindSamplingWindow: "REC_SAMPLING_WINDOW", kindConfig: "REC_CONFIG",
		kindDropped: "REC_DROPPED",
	}
	checked := 0
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		probe := m[1]
		size, err := strconv.Atoi(m[2])
		require.NoError(t, err)
		name, ok := recName[cookieFor(probe)]
		require.Truef(t, ok, "no REC_* constant known for %s", probe)
		def := regexp.MustCompile(`(?m)^#define\s+` + name + `\s+(\d+)`).FindSubmatch(bpfSrc)
		require.NotNilf(t, def, "did not find #define %s", name)
		want, err := strconv.Atoi(string(def[1]))
		require.NoError(t, err)
		assert.Equalf(t, want, size, "%s: adapter pins %d bytes, %s is %d", probe, size, name, want)
		checked++
	}
	assert.GreaterOrEqual(t, checked, 8, "the adapter should be firing at least eight probes")
}
