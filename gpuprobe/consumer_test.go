package gpuprobe

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
)

func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

// newTestConsumer builds a Consumer with no BPF objects attached: every test
// below exercises decode/accounting only, so nothing here needs privileges.
func newTestConsumer(sink gpu.EventSink) *Consumer {
	return &Consumer{
		cfg:       Config{Backend: gpu.BackendCUPTI, Sink: sink},
		seqByKind: map[uint32]uint64{},
	}
}

// recordingSink captures what the consumer normalized, and can be told to
// reject so the SinkRejected accounting is exercised.
type recordingSink struct {
	launches []gpu.GPUKernelLaunch
	execs    []gpu.GPUKernelExec
	err      error
}

func (s *recordingSink) EmitLaunch(l gpu.GPUKernelLaunch) error {
	if s.err != nil {
		return s.err
	}
	s.launches = append(s.launches, l)
	return nil
}

func (s *recordingSink) EmitExec(e gpu.GPUKernelExec) error {
	if s.err != nil {
		return s.err
	}
	s.execs = append(s.execs, e)
	return nil
}

func (s *recordingSink) EmitPCSample(gpu.GPUPCSample) error   { return s.err }
func (s *recordingSink) EmitModule(gpu.GPUModule) error       { return s.err }
func (s *recordingSink) EmitEvent(gpu.GPUTimelineEvent) error { return s.err }

// Cookie values are part of the contract between the Go attach code and the
// BPF program's record_size switch. A mismatch decodes garbage silently.
func TestProbeKindCookiesMatchTheBPFProgram(t *testing.T) {
	assert.Equal(t, uint64(1), cookieFor("gpu_launch_v1"))
	assert.Equal(t, uint64(2), cookieFor("gpu_exec_v1"))
	assert.Equal(t, uint64(3), cookieFor("gpu_module_load_v1"))
	assert.Equal(t, uint64(4), cookieFor("gpu_pc_sample_batch_v1"))
	assert.Equal(t, uint64(0), cookieFor("gpu_unknown_v9"), "unknown probes are not attached")
}

// Module and PC-sample records are on the wire in Phase 3 but are not turned
// into canonical events until Phases 4 and 6. They must be counted, not
// silently discarded — §6.1 admits no silent loss anywhere.
func TestUndecodedKindsAreCountedNotDropped(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	buf := make([]byte, 32+40)
	putU32(buf[0:], kindModule)
	putU32(buf[4:], 1)
	putU64(buf[24:], 40)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	assert.Equal(t, uint64(1), c.Stats().Undecoded)
	assert.Zero(t, c.Stats().Records)
}

func TestDecodeBatchSplitsRecordsByKind(t *testing.T) {
	// header: kind=1 count=2 seq=7 pid=11 tid=12 bytes=96
	buf := make([]byte, 32+96)
	putU32(buf[0:], 1)
	putU32(buf[4:], 2)
	putU64(buf[8:], 7)
	putU32(buf[16:], 11)
	putU32(buf[20:], 12)
	putU64(buf[24:], 96)
	putU64(buf[32:], 100) // first launch correlation
	putU64(buf[32+48:], 101)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), b.Kind)
	assert.Equal(t, uint64(7), b.Seq)
	require.Len(t, b.Launches, 2)
	assert.Equal(t, uint64(100), b.Launches[0].Correlation)
	assert.Equal(t, uint64(101), b.Launches[1].Correlation)
}

func TestDecodeBatchRejectsTruncatedPayload(t *testing.T) {
	buf := make([]byte, 32+10)
	putU32(buf[0:], 1)
	putU32(buf[4:], 2)
	putU64(buf[24:], 96) // claims 96 bytes it does not have
	_, err := decodeBatch(buf)
	require.Error(t, err)
}

func TestDecodeBatchRejectsShortHeader(t *testing.T) {
	_, err := decodeBatch(make([]byte, batchHdrSize-1))
	require.Error(t, err)
}

// A count larger than the declared payload must fail rather than read past
// the end of the sample.
func TestDecodeBatchRejectsCountBeyondPayload(t *testing.T) {
	buf := make([]byte, 32+48)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 2) // claims two launches
	putU64(buf[24:], 48)
	_, err := decodeBatch(buf)
	require.Error(t, err)
}

// Gaps in a probe's sequence numbers are losses the consumer did not observe
// and must not hide (spec §6.1).
func TestSequenceGapsAreCounted(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	c.noteSeq(1, 0)
	c.noteSeq(1, 1)
	assert.Zero(t, c.stats.SequenceGaps)
	c.noteSeq(1, 5) // 2,3,4 never arrived
	assert.Equal(t, uint64(3), c.stats.SequenceGaps)
}

// Sequence numbers are per-probe, so a gap on one kind must not be inferred
// from another kind's numbering.
func TestSequenceGapsArePerKind(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	c.noteSeq(kindLaunch, 10)
	c.noteSeq(kindExec, 0)
	c.noteSeq(kindLaunch, 11)
	c.noteSeq(kindExec, 1)
	assert.Zero(t, c.stats.SequenceGaps)
}

func TestApplyBatchNormalizesLaunches(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	buf := make([]byte, 32+48)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 1)
	putU64(buf[8:], 3)
	putU32(buf[16:], 4242) // pid comes from the batch header
	putU64(buf[24:], 48)
	putU64(buf[32+0:], 77)   // correlation
	putU64(buf[32+32:], 900) // time_ns
	putU32(buf[32+40:], 55)  // tid comes from the record

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)

	require.Len(t, sink.launches, 1)
	got := sink.launches[0]
	assert.Equal(t, gpu.CorrelationID{Backend: gpu.BackendCUPTI, Value: "77"}, got.Correlation)
	assert.Equal(t, uint64(900), got.TimeNs)
	assert.Equal(t, uint32(4242), got.Launch.PID)
	assert.Equal(t, uint32(55), got.Launch.TID)
	assert.Equal(t, gpu.ClockDomainCPUMonotonic, got.ClockDomain)
	assert.Empty(t, got.Launch.CPUStack, "CPU stack capture is a later phase")
	assert.Equal(t, uint64(1), c.Stats().Records)
	assert.Zero(t, c.Stats().SinkRejected)
}

func TestApplyBatchNormalizesExecs(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	buf := make([]byte, 32+48)
	putU32(buf[0:], kindExec)
	putU32(buf[4:], 1)
	putU64(buf[24:], 48)
	putU64(buf[32+0:], 88)   // correlation
	putU64(buf[32+32:], 10)  // start_ns
	putU64(buf[32+40:], 200) // end_ns

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)

	require.Len(t, sink.execs, 1)
	assert.Equal(t, "88", sink.execs[0].Correlation.Value)
	assert.Equal(t, uint64(10), sink.execs[0].StartNs)
	assert.Equal(t, uint64(200), sink.execs[0].EndNs)
}

// A sink that refuses an event is loss too, and gets its own counter.
func TestSinkRejectionsAreCounted(t *testing.T) {
	c := newTestConsumer(&recordingSink{err: gpu.ErrSinkFull})

	buf := make([]byte, 32+96)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 2)
	putU64(buf[24:], 96)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)

	assert.Equal(t, uint64(2), c.Stats().Records)
	assert.Equal(t, uint64(2), c.Stats().SinkRejected)
}

// A Consumer with no BPF objects loaded must report zero rather than panic,
// which is what every test above relies on.
func TestStatsWithoutBPFObjectsReportsZeroKernelDrops(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	assert.Zero(t, c.Stats().KernelDropped)
}

// The KernelVersion the loader is given must be a plausible LINUX_VERSION_CODE
// for the running kernel: uprobe_multi needs 6.6+, and a zero here would send
// cilium/ebpf back to the vDSO probe that a setcap'd binary cannot do.
func TestKernelVersionCodeIsPlausible(t *testing.T) {
	kv := kernelVersionCode()
	require.NotZero(t, kv)
	major := kv >> 16
	assert.GreaterOrEqual(t, major, uint32(4), "major version parsed from uname")
	assert.Less(t, major, uint32(100))
}

// The uprobe_multi BPF link is the whole point of this attach path: the
// perf_uprobe PMU needs CAP_SYS_ADMIN and the BPF link does not. The kernel
// refuses LINK_CREATE(BPF_TRACE_UPROBE_MULTI) unless the program was loaded
// with that expected_attach_type, which comes from the ELF section name — so
// assert it on the embedded object rather than discovering it at attach time
// on a machine with privileges.
func TestEmbeddedProgramIsUprobeMulti(t *testing.T) {
	spec, err := loadGpuusdt()
	require.NoError(t, err)

	prog := spec.Programs["gpu_usdt_batch"]
	require.NotNil(t, prog, "program name must match the generated GpuUsdtBatch field")
	assert.Equal(t, ebpf.Kprobe, prog.Type)
	assert.Equal(t, ebpf.AttachTraceUprobeMulti, prog.AttachType,
		"section must be uprobe.multi, not uprobe; see Attach's doc comment")
	assert.Equal(t, "GPL", prog.License)

	require.Contains(t, spec.Maps, "events")
	assert.Equal(t, ebpf.RingBuf, spec.Maps["events"].Type)
	require.Contains(t, spec.Maps, "dropped")
	assert.Equal(t, ebpf.Array, spec.Maps["dropped"].Type)
	assert.Equal(t, uint32(kindMax), spec.Maps["dropped"].MaxEntries,
		"Go-side kindMax must match KIND_MAX in the BPF program")
}
