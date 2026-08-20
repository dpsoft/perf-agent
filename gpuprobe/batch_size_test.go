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

// TestPerKindBatchCapsFitOneReservation pins the Phase 4a payload arithmetic.
//
// The reservation is sized for the largest record that ever *batches*
// (MAX_BATCHED_RECORD_BYTES, 48) times MAX_RECORDS_PER_BATCH, not for the
// largest record on the wire (MAX_RECORD_BYTES, 272 —
// gpu_kernel_name_v1). Sizing it for 272 would cost 40 + 64*272 = 17448
// bytes per batch and leave ~240 batches in a 4MB ring. Instead
// gpu_usdt_batch caps each kind at BATCH_CAP(size) records.
//
// The soundness conditions are: every kind fits at least one record in a
// reservation, and no kind's cap can ask for more bytes than the payload
// holds. Both are compile-time in the C (there is a _Static_assert for the
// first) but neither is visible from Go, and getting them wrong is silent
// corruption rather than a build error — so re-derive them from the source.
func TestPerKindBatchCapsFitOneReservation(t *testing.T) {
	const src = "../bpf/gpu_usdt.bpf.c"

	maxRecords := readDefine(t, src, "MAX_RECORDS_PER_BATCH")
	batched := readDefine(t, src, "MAX_BATCHED_RECORD_BYTES")
	largest := readDefine(t, src, "MAX_RECORD_BYTES")
	payload := maxRecords * batched

	require.Equal(t, 3072, payload, "the payload budget is unchanged by Phase 4a")
	require.LessOrEqual(t, largest, payload,
		"a record kind larger than one reservation could never be delivered at all")

	for _, name := range []string{
		"REC_LAUNCH", "REC_EXEC", "REC_MODULE", "REC_PC",
		"REC_LAUNCH_SAMPLED", "REC_KERNEL_NAME",
	} {
		size := readDefine(t, src, name)
		require.Positive(t, size)
		capRecs := payload / size
		if capRecs > maxRecords {
			capRecs = maxRecords
		}
		require.Positive(t, capRecs, "%s must fit at least one record per batch", name)
		require.LessOrEqualf(t, capRecs*size, payload,
			"%s: %d records of %d bytes overruns the %d-byte payload", name, capRecs, size, payload)
	}
}

// The two Phase 4a probes must stay unbatched. gpu_launch_sampled_v1 carries
// one captured stack, so batching it would attach that stack to N unrelated
// launches; gpu_kernel_name_v1 is 272 bytes, and only count == 1 keeps it
// comfortably inside the payload budget. Either is silent corruption, not a
// build error, so pin it against the stub.
func TestSampledProbesAreNotBatchedInTheStub(t *testing.T) {
	b, err := os.ReadFile("../shim/stub/stub.cc")
	require.NoError(t, err)
	for _, probe := range []string{"gpu_launch_sampled_v1", "gpu_kernel_name_v1"} {
		re := regexp.MustCompile(`Batch<` + regexp.QuoteMeta(probe) + `\s*,`)
		require.Nilf(t, re.Find(b),
			"%s must be emitted one record at a time, never through perfagent::Batch", probe)
		require.Containsf(t, string(b), probe+"_emit(",
			"%s should still be emitted directly", probe)
	}
}
