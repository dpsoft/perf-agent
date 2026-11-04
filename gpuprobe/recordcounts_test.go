package gpuprobe

import (
	"testing"

	"github.com/cilium/ebpf/asm"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	"github.com/stretchr/testify/require"
)

func TestRecordKindCountsPreserveUnknownKinds(t *testing.T) {
	counts := RecordKindCounts{0: 2, kindLaunch: 10, kindExec: 20, kindLaunchSampled: 3, kindDropped: 4, kindMax - 1: 5}
	require.Equal(t, uint64(44), counts.Total())
	require.Equal(t, "unknown=2 launch=10 exec=20 sampled_launch=3 drop_report=4 kind_15=5", counts.String())
}

func TestRecordKindCountsWithoutBPFObjects(t *testing.T) {
	counts, failures := readKindCounts(nil)
	require.Zero(t, counts.Total())
	require.Empty(t, counts.String())
	require.Zero(t, failures)
}

func TestCompactSampledBatchPreservesDecodedRecord(t *testing.T) {
	compact := sampledBatch(123, 8)
	require.Len(t, compact, batchHdrSize+gpuabi.SizeLaunchSampled)
	a, err := decodeBatch(compact)
	require.NoError(t, err)
	// The old transport padded every message to the full batch reservation.
	padded := append(append([]byte(nil), compact...), make([]byte, 3072-gpuabi.SizeLaunchSampled)...)
	b, err := decodeBatch(padded)
	require.NoError(t, err)
	require.Equal(t, b, a)
}

func TestEmbeddedGPUBPFReservesCompactSampledMessages(t *testing.T) {
	spec, err := loadGpuusdt()
	require.NoError(t, err)
	// Check the object that ships, rather than only the C source: a stale
	// object would silently retain the 30x padded sampled-launch reservation.
	prog := spec.Programs["gpu_usdt_batch"]
	require.NotNil(t, prog)
	var reserveSizes []int64
	var r2 int64
	for _, ins := range prog.Instructions {
		if ins.OpCode == asm.Mov.Imm(asm.R2, 0).OpCode && ins.Dst == asm.R2 {
			r2 = ins.Constant
		}
		if ins.IsBuiltinCall() && ins.Constant == int64(asm.FnRingbufReserve) {
			reserveSizes = append(reserveSizes, r2)
		}
	}
	require.Contains(t, reserveSizes, int64(batchHdrSize+gpuabi.SizeLaunchSampled))
}
