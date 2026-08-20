package gpuprobe_test

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestShimBatchSizeFitsKernelCap pins the relationship between the shim's
// producer-side batch size and the BPF program's consumer-side cap.
//
// bpf/gpu_usdt.bpf.c defines MAX_RECORDS_PER_BATCH: a batch larger than that
// is truncated and the excess counted in the `dropped` map
// (gpu_usdt_batch's `if (count > MAX_RECORDS_PER_BATCH)` branch). The stub
// in shim/stub/stub.cc batches at a fixed size via perfagent::Batch<Kind,
// N>. Today N (32) is comfortably under MAX_RECORDS_PER_BATCH (64), so
// nothing drops. Nothing currently ties the two together: raising the
// shim's batch size past the kernel cap would silently start dropping
// records, defeating the "no loss is ever silent" contract this consumer
// otherwise upholds (see Consumer.Stats' KernelDropped).
//
// This test reads both constants out of the C sources — no privileges
// required, no BPF load, no attach — so a future change to either constant
// that breaks the relationship fails a plain `go test ./gpuprobe/` instead
// of surfacing as a mysterious KernelDropped count on whoever next runs the
// gate under capabilities.
func TestShimBatchSizeFitsKernelCap(t *testing.T) {
	bpfCap := readDefine(t, "../bpf/gpu_usdt.bpf.c", "MAX_RECORDS_PER_BATCH")

	for _, probe := range []string{"gpu_launch_v1", "gpu_exec_v1"} {
		shimBatch := readBatchTemplateArg(t, "../shim/stub/stub.cc", probe)
		require.LessOrEqualf(t, shimBatch, bpfCap,
			"shim batches %s at %d records, but bpf/gpu_usdt.bpf.c caps MAX_RECORDS_PER_BATCH at %d; "+
				"raising the shim's batch size above the kernel cap makes gpu_usdt_batch silently "+
				"truncate and count the excess in the dropped map instead of failing loudly",
			probe, shimBatch, bpfCap)
	}
}

func readDefine(t *testing.T, path, name string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^#define\s+` + regexp.QuoteMeta(name) + `\s+(\d+)`)
	m := re.FindSubmatch(b)
	require.NotNilf(t, m, "did not find #define %s in %s", name, path)
	v, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	return v
}

func readBatchTemplateArg(t *testing.T, path, probe string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	re := regexp.MustCompile(`Batch<` + regexp.QuoteMeta(probe) + `,\s*(\d+)>`)
	m := re.FindSubmatch(b)
	require.NotNilf(t, m, "did not find perfagent::Batch<%s, N> in %s", probe, path)
	v, err := strconv.Atoi(string(m[1]))
	require.NoError(t, err)
	return v
}
