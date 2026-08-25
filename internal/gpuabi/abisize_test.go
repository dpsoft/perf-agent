package gpuabi

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wire sizes live in three places in two languages: the C structs in
// shim/core/usdt_abi.h (what the producer writes), the REC_* defines in
// bpf/gpu_usdt.bpf.c (what the kernel copies and how many fit a batch), and
// the Size* constants here (what the decoders slice at). All three are
// hand-maintained, and every existing size test in this package pins the Go
// constant against a *literal* — which proves only that the constant and the
// test agree, not that either matches the record the producer emits.
//
// A record that grew in C while Go kept the old size does not error anywhere:
// the decoder reads the right bytes for field one and progressively wronger
// ones after it, and every record in the batch after the first is offset by
// the difference. That is the exact silent-corruption shape the version
// suffix exists to prevent, so the three sources are pinned to each other
// here rather than each to its own literal.

const (
	abiHeader = "../../shim/core/usdt_abi.h"
	bpfSource = "../../bpf/gpu_usdt.bpf.c"
)

// cSizeAssert reads `GPU_STATIC_ASSERT(sizeof(struct <name>) == <n>, ...)`.
func cSizeAssert(t *testing.T, src []byte, structName string) int {
	t.Helper()
	re := regexp.MustCompile(`GPU_STATIC_ASSERT\(sizeof\(struct ` +
		regexp.QuoteMeta(structName) + `\) == (\d+)`)
	m := re.FindSubmatch(src)
	require.NotNilf(t, m, "no size assertion for struct %s in %s", structName, abiHeader)
	n, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	return n
}

func cDefine(t *testing.T, src []byte, name string) int {
	t.Helper()
	re := regexp.MustCompile(`(?m)^#define\s+` + regexp.QuoteMeta(name) + `\s+(\d+)`)
	m := re.FindSubmatch(src)
	require.NotNilf(t, m, "no #define %s", name)
	n, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	return n
}

func TestGoSizesMatchTheCStructs(t *testing.T) {
	src, err := os.ReadFile(abiHeader)
	require.NoError(t, err)

	for _, tc := range []struct {
		structName string
		goSize     int
	}{
		{"gpu_launch_v1", SizeLaunch},
		{"gpu_exec_v1", SizeExec},
		{"gpu_module_load_v1", SizeModuleLoad},
		{"gpu_pc_sample_batch_v1", SizePCSample},
		{"gpu_config_v1", SizeConfig},
		{"gpu_dropped_v1", SizeDropped},
		{"gpu_launch_sampled_v1", SizeLaunchSampled},
		{"gpu_kernel_name_v1", SizeKernelName},
		{"gpu_stall_reason_map_v1", SizeStallReason},
		{"gpu_sampling_window_v1", SizeSamplingWindow},
	} {
		assert.Equalf(t, cSizeAssert(t, src, tc.structName), tc.goSize,
			"struct %s: the C size and the Go Size* constant disagree. A decoder "+
				"slicing at the wrong stride corrupts every record after the first "+
				"in a batch and errors nowhere", tc.structName)
	}
}

func TestGoNameBoundsMatchTheCDefines(t *testing.T) {
	src, err := os.ReadFile(abiHeader)
	require.NoError(t, err)

	assert.Equal(t, cDefine(t, src, "GPU_KERNEL_NAME_MAX"), GPUKernelNameMax)
	assert.Equal(t, cDefine(t, src, "GPU_STALL_NAME_MAX"), GPUStallNameMax,
		"the range check in DecodeStallReason is only sound against the C buffer size")
	assert.Equal(t, int(SamplingModeContinuous), cDefine(t, src, "GPU_SAMPLING_MODE_CONTINUOUS"))
	assert.Equal(t, int(SamplingModeKernelSerialized), cDefine(t, src, "GPU_SAMPLING_MODE_KERNEL_SERIALIZED"))
}

// The drop classes are wire constants shared with the shim by nothing but
// agreement. A renumber on one side silently refiles every loss under the
// wrong heading — the reader sees a plausible number attached to the wrong
// cause, which is worse than seeing nothing.
func TestGoDropClassesMatchTheCDefines(t *testing.T) {
	src, err := os.ReadFile(abiHeader)
	require.NoError(t, err)

	for _, tc := range []struct {
		def string
		got uint8
	}{
		{"GPU_DROP_CLASS_INVALID", DropClassInvalid},
		{"GPU_DROP_CLASS_PC_DROPPED_HW", DropClassPCDroppedHW},
		{"GPU_DROP_CLASS_PC_BUFFER_FULL", DropClassPCBufferFull},
		{"GPU_DROP_CLASS_PC_NON_USER_KERNEL", DropClassPCNonUserKernel},
		{"GPU_DROP_CLASS_GRAPH_EXEC", DropClassGraphExec},
	} {
		assert.Equalf(t, cDefine(t, src, tc.def), int(tc.got), "drop class %s", tc.def)
	}
	assert.Equal(t, cDefine(t, src, "GPU_DROP_CLASS_MAX"), int(DropClassGraphExec),
		"GPU_DROP_CLASS_MAX must name the highest class Go knows, or a class "+
			"added on the C side has no Go constant and renders as unknown")
	assert.Equal(t, cDefine(t, src, "GPU_VENDOR_NVIDIA"), 1)
}

// The BPF program copies `count * REC_<kind>` bytes out of user memory and
// writes that byte count into the batch header. If REC_* disagrees with the
// Go Size*, the consumer's `count > len(payload)/Size*` guard is computed
// against a stride the kernel never used: too small a Go size passes the
// guard and decodes overlapping garbage, too large a one rejects valid
// batches as short. Neither is loud.
func TestBPFRecordSizesMatchTheGoSizes(t *testing.T) {
	src, err := os.ReadFile(bpfSource)
	require.NoError(t, err)

	for _, tc := range []struct {
		def    string
		goSize int
	}{
		{"REC_LAUNCH", SizeLaunch},
		{"REC_EXEC", SizeExec},
		{"REC_MODULE", SizeModuleLoad},
		{"REC_PC", SizePCSample},
		{"REC_LAUNCH_SAMPLED", SizeLaunchSampled},
		{"REC_KERNEL_NAME", SizeKernelName},
		{"REC_STALL_MAP", SizeStallReason},
		{"REC_SAMPLING_WINDOW", SizeSamplingWindow},
		{"REC_CONFIG", SizeConfig},
	} {
		assert.Equalf(t, cDefine(t, src, tc.def), tc.goSize,
			"%s in %s disagrees with the Go decoder's stride", tc.def, bpfSource)
	}
}

// MAX_RECORD_BYTES sizes nothing on its own, but the BPF program's
// _Static_assert(MAX_RECORD_BYTES <= PAYLOAD_BYTES) is what guarantees every
// kind can be delivered at all — and it is stated in terms of a constant a
// new record can silently outgrow. gpu_stall_reason_map_v1 at 136 does not,
// and this asserts that rather than assuming it.
func TestNoRecordExceedsTheBPFWorstCase(t *testing.T) {
	src, err := os.ReadFile(bpfSource)
	require.NoError(t, err)

	largest := cDefine(t, src, "MAX_RECORD_BYTES")
	payload := cDefine(t, src, "MAX_RECORDS_PER_BATCH") * cDefine(t, src, "MAX_BATCHED_RECORD_BYTES")
	require.Equal(t, 3072, payload, "the payload budget is unchanged by the PC-sampling probes")
	require.LessOrEqual(t, largest, payload)

	for name, size := range map[string]int{
		"SizeLaunch":         SizeLaunch,
		"SizeExec":           SizeExec,
		"SizeModuleLoad":     SizeModuleLoad,
		"SizePCSample":       SizePCSample,
		"SizeConfig":         SizeConfig,
		"SizeLaunchSampled":  SizeLaunchSampled,
		"SizeKernelName":     SizeKernelName,
		"SizeStallReason":    SizeStallReason,
		"SizeSamplingWindow": SizeSamplingWindow,
	} {
		assert.LessOrEqualf(t, size, largest,
			"%s (%d) exceeds MAX_RECORD_BYTES (%d): raise it in %s, or the kind can never be delivered",
			name, size, largest, bpfSource)
	}

	// The stall map must NOT have raised the batched-record reservation.
	// Sizing 136 in would grow every launch batch's reservation from 3072 to
	// 8704 bytes to serve a probe that fires a few dozen times per process.
	assert.Equal(t, SizeExec, cDefine(t, src, "MAX_BATCHED_RECORD_BYTES"),
		"MAX_BATCHED_RECORD_BYTES must stay at the largest kind that truly batches")
}
