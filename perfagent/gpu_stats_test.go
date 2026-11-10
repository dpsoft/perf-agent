package perfagent

import (
	"testing"

	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/stretchr/testify/require"
)

func TestGPUStatsDoNotAddRecordsBatchesAndStacks(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{
		KernelDropped:       100,
		KernelDroppedByKind: gpuprobe.RecordKindCounts{1: 40, 2: 60},
		SequenceGaps:        3,
		StacksEvicted:       7,
		StacksMissing:       11,
		SinkRejected:        2,
	})
	s := buf.String()
	require.Contains(t, s, "kernel_dropped=100 record(s) (launch=40 exec=60)")
	require.Contains(t, s, "increase --gpu-ring-bytes for bursts")
	require.Contains(t, s, "sequence_gaps=3 batch(es) never arrived")
	require.Contains(t, s, "record count unknown")
	require.Contains(t, s, "may overlap BPF transport losses")
	require.Contains(t, s, "sink_rejected=2 event(s) after transport")
	require.Contains(t, s, "CPU attribution incomplete: stacks_evicted=7 stacks_missing=11")
	require.NotContains(t, s, "LOST 123 record(s)")
}

func TestDroppedSampledRecordsDoNotClaimExecutionLoss(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{KernelDropped: 4, KernelDroppedByKind: gpuprobe.RecordKindCounts{5: 4}})
	require.Contains(t, buf.String(), "kernel_dropped=4 record(s) (sampled_launch=4)")
	require.NotContains(t, buf.String(), "execution records were lost")
}

func TestMissingGPUAttributionDoesNotClaimLostExecutions(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{StacksMissing: 2, StacksEvicted: 3, StackLookupFailed: 4, SymbolizeFailed: 5})
	s := buf.String()
	require.Contains(t, s, "CPU attribution incomplete")
	require.Contains(t, s, "these counters do not count lost GPU executions")
	require.NotContains(t, s, "LOST")
	require.NotContains(t, s, "under-reports GPU work")
}

func TestUnreadableGPUDropCountersDoNotLookHealthy(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{KernelDropReadErrors: 2})
	require.Contains(t, buf.String(), "drop accounting incomplete: 2 BPF counter slot(s) unreadable")
}

func TestGPUStopUnreadBytesAreReportedInBytes(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{StopUnreadBytes: 4096, StopDrainedBatches: 12})
	require.Contains(t, buf.String(), "final drain delivered 12 batch(es) the probes wrote before the stop")
	require.Contains(t, buf.String(), "stop_unread_bytes=4096 ring byte(s)")
	require.Contains(t, buf.String(), "in no drop counter")
	require.NotContains(t, buf.String(), "kernel_dropped=")
}

// stacks_missing alone does not say whether the walk failed or the consumer
// fell behind; the reasons line does.
func TestGPUMissingStacksAreExplained(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{
		StacksMissing: 10, StackMapFull: 7, StackWalkEmpty: 2, StackMapUpdateFailed: 1,
		KernelStacksMissing: 10, StackMemoHits: 90, StackMemoMisses: 10,
	})
	s := buf.String()
	require.Contains(t, s, "why stacks were missing: stack_map_full=7 stack_walk_empty=2 "+
		"stack_map_update_failed=1 stack_walk_scratch_failed=0 (bpf side counted 10)")
	require.Contains(t, s, "stack memo: hits=90 misses=10 expired=0 clears=0")
}

func TestGPUSequenceGapDoesNotClaimKnownRecordCount(t *testing.T) {
	buf := captureLog(t)
	logGPUIngestionStats(gpuprobe.Stats{SequenceGaps: 1, SequenceReordered: 4, SequenceStale: 2})
	require.Contains(t, buf.String(), "sequence_gaps=1 batch(es) never arrived (reordered=4 stale=2)")
	require.Contains(t, buf.String(), "record count unknown")
	require.NotContains(t, buf.String(), "LOST")
	require.NotContains(t, buf.String(), "kernel_dropped=")
}
