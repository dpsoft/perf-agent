package gpuprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/internal/bpfstack"
	"github.com/dpsoft/perf-agent/internal/gpuabi"
	pp "github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/symbolize"
)

// frameNames renders a frame slice as its symbol names, so a test can pin
// both the frames and their order in one assertion.
func frameNames(frames []pp.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Name)
	}
	return out
}

func putU32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func putU64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

// newTestConsumer builds a Consumer with no BPF objects attached: every test
// below exercises decode/accounting only, so nothing here needs privileges.
// It goes through newConsumer, the same constructor Attach uses, so the side
// tables a Consumer needs can never be initialized in only one of the two
// paths.
func newTestConsumer(sink gpu.EventSink) *Consumer {
	return newConsumer(Config{Backend: gpu.BackendCUPTI, Sink: sink})
}

// recordingSink captures what the consumer normalized, and can be told to
// reject so the SinkRejected accounting is exercised. It is mutex-guarded
// because the lifecycle tests below drive it from Run's goroutine.
type recordingSink struct {
	mu       sync.Mutex
	launches []gpu.GPUKernelLaunch
	execs    []gpu.GPUKernelExec
	err      error
	// onEmit, if set, is called after each accepted event. The lifecycle
	// tests use it to know Run has completed a loop iteration.
	onEmit func()
}

func (s *recordingSink) note() {
	if s.onEmit != nil {
		s.onEmit()
	}
}

func (s *recordingSink) EmitLaunch(l gpu.GPUKernelLaunch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.launches = append(s.launches, l)
	s.note()
	return nil
}

func (s *recordingSink) EmitExec(e gpu.GPUKernelExec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.execs = append(s.execs, e)
	s.note()
	return nil
}

func (s *recordingSink) EmitPCSample(gpu.GPUPCSample) error   { return s.errOnly() }
func (s *recordingSink) EmitModule(gpu.GPUModule) error       { return s.errOnly() }
func (s *recordingSink) EmitEvent(gpu.GPUTimelineEvent) error { return s.errOnly() }

func (s *recordingSink) errOnly() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *recordingSink) launchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.launches)
}

// scriptedReader is a batchReader that hands out queued samples and then
// blocks in Read, exactly like a real ringbuf with nothing to deliver.
//
// It models ringbuf.(*Reader)'s *locking*, not just its errors, because the
// locking is where the cancellation bug lived. Read holds mu for the whole
// blocking wait (ringbuf/reader.go ReadInto locks r.mu and defers the
// unlock around the epoll wait); SetDeadline wants that same mu, so against
// a blocked reader it parks forever and wakes nothing; Close signals first
// and only then takes mu, mirroring the real Close closing its poller before
// acquiring the lock — which is exactly why Close, and only Close, can
// interrupt a read in progress. A fake whose SetDeadline returned
// immediately would have passed with the broken implementation too.
type scriptedReader struct {
	mu   sync.Mutex
	recs chan []byte
	done chan struct{}
	once sync.Once
	err  atomic.Value
}

func newScriptedReader(n int) *scriptedReader {
	return &scriptedReader{recs: make(chan []byte, n), done: make(chan struct{})}
}

func (r *scriptedReader) stop(err error) {
	r.once.Do(func() {
		r.err.Store(err)
		close(r.done)
	})
}

func (r *scriptedReader) Read() (ringbuf.Record, error) {
	// Held across the block, like the real ReadInto.
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case b := <-r.recs:
		return ringbuf.Record{RawSample: b}, nil
	case <-r.done:
		err, _ := r.err.Load().(error)
		return ringbuf.Record{}, err
	}
}

// SetDeadline blocks behind a read in progress, as the real one does. Calling
// it to cancel a blocked Run is the deadlock this models.
func (r *scriptedReader) SetDeadline(time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stop(os.ErrDeadlineExceeded)
}

// Close signals before taking mu, so it can interrupt a read in progress. It
// is idempotent and safe to call concurrently with Read.
func (r *scriptedReader) Close() error {
	r.stop(ringbuf.ErrClosed)
	r.mu.Lock()
	defer r.mu.Unlock()
	return nil
}

// oneLaunchBatch builds a wire-format batch carrying a single launch record.
func oneLaunchBatch(pid uint32, seq uint64) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunch)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 1)
	putU64(buf[8:], seq)
	putU32(buf[16:], pid)
	putU64(buf[24:], gpuabi.SizeLaunch)
	return buf
}

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
	buf := make([]byte, batchHdrSize+40)
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
	buf := make([]byte, batchHdrSize+96)
	putU32(buf[0:], 1)
	putU32(buf[4:], 2)
	putU64(buf[8:], 7)
	putU32(buf[16:], 11)
	putU32(buf[20:], 12)
	putU64(buf[24:], 96)
	putU64(buf[batchHdrSize:], 100) // first launch correlation
	putU64(buf[batchHdrSize+48:], 101)

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	assert.Equal(t, uint32(1), b.Kind)
	assert.Equal(t, uint64(7), b.Seq)
	require.Len(t, b.Launches, 2)
	assert.Equal(t, uint64(100), b.Launches[0].Correlation)
	assert.Equal(t, uint64(101), b.Launches[1].Correlation)
}

func TestDecodeBatchRejectsTruncatedPayload(t *testing.T) {
	buf := make([]byte, batchHdrSize+10)
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
	buf := make([]byte, batchHdrSize+48)
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
	c.noteSeq(1, 100, 0)
	c.noteSeq(1, 100, 1)
	assert.Zero(t, c.stats.SequenceGaps)
	c.noteSeq(1, 100, 5) // 2,3,4 never arrived
	assert.Equal(t, uint64(3), c.stats.SequenceGaps)
}

// Sequence numbers are per-probe, so a gap on one kind must not be inferred
// from another kind's numbering.
func TestSequenceGapsArePerKind(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	c.noteSeq(kindLaunch, 100, 10)
	c.noteSeq(kindExec, 100, 0)
	c.noteSeq(kindLaunch, 100, 11)
	c.noteSeq(kindExec, 100, 1)
	assert.Zero(t, c.stats.SequenceGaps)
}

// The shim's seq_ counter is per-process, so a system-wide attach
// (Config.PID == 0) sees one independent stream per profiled process.
// Merging them into one counter would manufacture phantom gaps — the exact
// opposite of what SequenceGaps exists to report.
func TestSequenceGapsArePerProcess(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	// Two processes, each perfectly monotonic, interleaved on the same kind.
	for seq := uint64(0); seq < 4; seq++ {
		c.noteSeq(kindLaunch, 111, seq)
		c.noteSeq(kindLaunch, 222, seq)
	}
	assert.Zero(t, c.stats.SequenceGaps, "independent per-PID streams are not gaps")

	// A real gap inside one process is still counted, and only once.
	c.noteSeq(kindLaunch, 111, 7) // 4,5,6 never arrived
	assert.Equal(t, uint64(3), c.stats.SequenceGaps)
	c.noteSeq(kindLaunch, 222, 4) // 222 continues cleanly
	assert.Equal(t, uint64(3), c.stats.SequenceGaps)
}

// applyBatch must take the PID from the batch header, not from Config.
func TestApplyBatchKeepsPerProcessSequencesApart(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	mk := func(pid uint32, seq uint64) batch {
		buf := make([]byte, batchHdrSize+48)
		putU32(buf[0:], kindLaunch)
		putU32(buf[4:], 1)
		putU64(buf[8:], seq)
		putU32(buf[16:], pid)
		putU64(buf[24:], 48)
		b, err := decodeBatch(buf)
		require.NoError(t, err)
		return b
	}
	for seq := uint64(0); seq < 3; seq++ {
		c.applyBatch(mk(111, seq))
		c.applyBatch(mk(222, seq))
	}
	st := c.Stats()
	assert.Zero(t, st.SequenceGaps)
	assert.Equal(t, uint64(6), st.Batches)
	assert.Equal(t, uint64(6), st.Records)
}

func TestApplyBatchNormalizesLaunches(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	buf := make([]byte, batchHdrSize+48)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 1)
	putU64(buf[8:], 3)
	putU32(buf[16:], 4242) // pid comes from the batch header
	putU64(buf[24:], 48)
	putU64(buf[batchHdrSize+0:], 77)   // correlation
	putU64(buf[batchHdrSize+32:], 900) // time_ns
	putU32(buf[batchHdrSize+40:], 55)  // tid comes from the record

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	// A correlated launch is held briefly in case its sampled twin is the
	// next ringbuf sample (see sampledstacks.go). Flush ends that wait; it
	// is what Run does on the way out and what Close does, so this asserts
	// the same launch any caller sees.
	c.Flush()

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

	buf := make([]byte, batchHdrSize+48)
	putU32(buf[0:], kindExec)
	putU32(buf[4:], 1)
	putU64(buf[24:], 48)
	putU64(buf[batchHdrSize+0:], 88)   // correlation
	putU64(buf[batchHdrSize+32:], 10)  // start_ns
	putU64(buf[batchHdrSize+40:], 200) // end_ns

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

	buf := make([]byte, batchHdrSize+96)
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

// Close is documented to be callable from another goroutine while Run is
// blocked in Read. Consumer.Close must therefore not clear c.reader, which
// Run dereferences without holding mu: doing so both races the read and, on
// the next iteration, dereferences nil.
func TestCloseWhileRunIsBlockedReturnsWithoutPanic(t *testing.T) {
	reader := newScriptedReader(4)
	applied := make(chan struct{}, 4)
	sink := &recordingSink{onEmit: func() { applied <- struct{}{} }}

	c := newTestConsumer(sink)
	c.reader = reader

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()

	// Get Run past its first iteration so a stale reader pointer would be
	// dereferenced on the next one.
	reader.recs <- oneLaunchBatch(7, 0)
	select {
	case <-applied:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never delivered the first batch")
	}

	require.NoError(t, c.Close())

	select {
	case err := <-done:
		assert.NoError(t, err, "Run returns cleanly when the reader is closed")
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Close")
	}
	assert.Equal(t, 1, sink.launchCount())
	// Close is idempotent; a second one must not panic either.
	require.NoError(t, c.Close())
}

// Run must honour its context while the read is *blocked* — the only state
// that matters, since a ringbuf with nothing to deliver is the normal one.
// Cancellation therefore closes the reader; a deadline would park behind the
// blocked read forever (see Run, and scriptedReader's doc comment).
//
// Regression: with the AfterFunc calling SetDeadline this test hangs until
// its 5s deadline and fails, because scriptedReader models the real reader's
// lock. Do not "fix" a future failure here by giving the fake a lock-free
// SetDeadline.
func TestRunStopsOnContextCancel(t *testing.T) {
	reader := newScriptedReader(1)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Let Run reach the blocking Read before cancelling: cancelling first
	// would leave the reader unlocked, which is the one case the broken
	// implementation also survived.
	waitForBlockedRead(t, reader)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	// Cancellation closed the reader; Consumer.Close must still be safe and
	// must not report the already-closed reader as an error.
	assert.NoError(t, c.Close())
}

// waitForBlockedRead blocks until r is parked inside Read holding its mutex.
// TryLock failing is the observable form of "a read is in progress".
func waitForBlockedRead(t *testing.T, r *scriptedReader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !r.mu.TryLock() {
			return
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Run never entered a blocking Read")
}

// Close and cancellation arriving together is the shape that made the nil
// assignment unsafe: the AfterFunc closure touches c.reader too. Under -race
// this fails if Close writes the field.
func TestConcurrentCloseAndCancelAreSafe(t *testing.T) {
	reader := newScriptedReader(1)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = c.Close() }()
	go func() { defer wg.Done(); cancel() }()
	wg.Wait()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// A sample that does not decode is loss and gets counted, not skipped.
func TestRunCountsMalformedSamples(t *testing.T) {
	reader := newScriptedReader(2)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	reader.recs <- make([]byte, batchHdrSize-1) // too short for a header
	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()

	require.Eventually(t, func() bool { return c.Stats().Malformed == 1 },
		5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Close())
	<-done
}

// A wire correlation of zero is the ABI's "no correlation" (usdt_abi.h says
// so for PC samples in continuous collection, the mode this project ships).
// It must produce the *zero* gpu.CorrelationID, because that is what
// gpu/timeline.go tests to route a record to the heuristic join. Formatting
// it as "0" would hand every uncorrelated record the same valid-looking ID
// and collapse them all into one exact-join bucket.
func TestZeroWireCorrelationBecomesTheZeroCorrelationID(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	buf := make([]byte, batchHdrSize+96)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 2)
	putU64(buf[24:], 96)
	putU64(buf[batchHdrSize+0:], 0)  // no correlation
	putU64(buf[batchHdrSize+48:], 9) // a real one

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	// The uncorrelated launch goes out immediately - it can never be paired
	// with a sampled stack - while the correlated one waits for a possible
	// twin until Flush. Arrival order survives that split, which is what the
	// index-based assertions below rely on.
	c.Flush()

	require.Len(t, sink.launches, 2)
	assert.Equal(t, gpu.CorrelationID{}, sink.launches[0].Correlation,
		"wire zero means no correlation; the timeline's heuristic path keys off the zero value")
	assert.Equal(t, gpu.CorrelationID{Backend: gpu.BackendCUPTI, Value: "9"}, sink.launches[1].Correlation,
		"a non-zero correlation is unaffected")

	st := c.Stats()
	assert.Equal(t, uint64(1), st.ZeroCorrelation, "the demotion to the heuristic join is counted, not silent")
	assert.Equal(t, uint64(2), st.Records, "an uncorrelated record is normalized, not dropped")
	assert.Zero(t, st.SinkRejected)
}

// Execs take the same path: a zero correlation there also demotes to the
// heuristic join rather than joining every uncorrelated exec to one bucket.
func TestZeroWireCorrelationOnExecs(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	buf := make([]byte, batchHdrSize+48)
	putU32(buf[0:], kindExec)
	putU32(buf[4:], 1)
	putU64(buf[24:], 48)
	putU64(buf[batchHdrSize+0:], 0) // no correlation

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)

	require.Len(t, sink.execs, 1)
	assert.Equal(t, gpu.CorrelationID{}, sink.execs[0].Correlation)
	assert.Equal(t, uint64(1), c.Stats().ZeroCorrelation)
}

// --- Phase 4a: the batch header carries a stack id -------------------------

// sampledBatch builds a wire-format batch carrying one gpu_launch_sampled_v1
// record with the given header stack_id. The shim always emits this kind
// unbatched, which is what makes a per-batch stack id sound.
func sampledBatch(stackID int32, samplePeriod uint32) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunchSampled)
	putU32(buf[0:], kindLaunchSampled)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeLaunchSampled))
	putU32(buf[32:], uint32(stackID))
	putU64(buf[batchHdrSize:], 7)               // correlation
	putU32(buf[batchHdrSize+44:], samplePeriod) // sample_period
	return buf
}

// batchHdrSize is a hard-coded offset table shared with struct batch_hdr in
// bpf/gpu_usdt.bpf.c. Nothing errors when the two disagree — every field
// simply decodes from the wrong place — so pin the number itself.
func TestBatchHeaderIsFortyBytesAndAppendOnly(t *testing.T) {
	require.Equal(t, 40, batchHdrSize,
		"struct batch_hdr grew to 40 bytes in Phase 4a; the C _Static_assert says the same")

	// Every pre-4a field must still decode from its original offset: the new
	// stack_id was appended at 32, not spliced in.
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunch)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 1)
	putU64(buf[8:], 99)
	putU32(buf[16:], 111)
	putU32(buf[20:], 222)
	putU64(buf[24:], uint64(gpuabi.SizeLaunch))
	putU32(buf[32:], ^uint32(0)) // stack_id = -1

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	assert.Equal(t, uint32(kindLaunch), b.Kind)
	assert.Equal(t, uint64(99), b.Seq)
	assert.Equal(t, uint32(111), b.PID)
	assert.Equal(t, uint32(222), b.TID)
	assert.Equal(t, uint32(1), b.RawCount)
	assert.Equal(t, int32(-1), b.StackID)
	require.Len(t, b.Launches, 1)
}

func TestProbeKindCookiesCoverTheSampledProbes(t *testing.T) {
	assert.Equal(t, uint64(5), cookieFor("gpu_launch_sampled_v1"))
	assert.Equal(t, uint64(6), cookieFor("gpu_kernel_name_v1"))
}

func TestDecodeBatchCarriesTheStackID(t *testing.T) {
	b, err := decodeBatch(sampledBatch(4242, 8))
	require.NoError(t, err)
	assert.Equal(t, int32(4242), b.StackID)
	require.Len(t, b.SampledLaunches, 1)
	assert.Equal(t, uint64(7), b.SampledLaunches[0].Correlation)
	assert.Equal(t, uint32(8), b.SampledLaunches[0].SamplePeriod)
}

// A failed bpf_get_stackid is a real outcome — a stack too deep, a full
// stackmap, a frame-pointer-less binary. It is counted, and the launch still
// arrives: losing a launch because its stack was lost would be worse than
// losing the stack.
func TestMissingStackIsCountedNotDropped(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	b, err := decodeBatch(sampledBatch(-1, 8))
	require.NoError(t, err)
	c.applyBatch(b)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksMissing,
		"a launch whose stack capture failed is still a launch")
	assert.Equal(t, uint64(1), st.Records)
	assert.Equal(t, uint64(1), st.SampledLaunches)
	assert.Zero(t, st.KernelDropped, "a missing stack is not a dropped record")
	assert.Zero(t, st.Undecoded)
}

// bpf_get_stackid returns the raw negative errno, not always -1; anything
// negative means "no stack".
func TestAnyNegativeStackIDCountsAsMissing(t *testing.T) {
	for _, id := range []int32{-1, -14 /* -EFAULT */, -7 /* -E2BIG */} {
		c := newTestConsumer(&recordingSink{})
		b, err := decodeBatch(sampledBatch(id, 8))
		require.NoError(t, err)
		c.applyBatch(b)
		assert.Equalf(t, uint64(1), c.Stats().StacksMissing, "stack_id %d", id)
	}
}

func TestPresentStackIsNotCountedAsMissing(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	b, err := decodeBatch(sampledBatch(0, 8)) // zero is a legal stackmap key
	require.NoError(t, err)
	c.applyBatch(b)
	assert.Zero(t, c.Stats().StacksMissing, "stack id 0 is a real stack, not a failure")
	assert.Equal(t, uint64(1), c.Stats().Records)
}

// The BPF program writes stack_id = -1 on every kind it does not capture for.
// StacksMissing must not turn that into phantom loss.
func TestNonSampledKindsDoNotCountStacksMissing(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunch)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeLaunch))
	putU32(buf[32:], ^uint32(0)) // stack_id = -1

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	assert.Zero(t, c.Stats().StacksMissing, "only the sampled kind carries a stack")
	assert.Equal(t, uint64(1), c.Stats().Records)
}

// A zero sample_period would make the scale factor a division by zero, so the
// ABI decoder rejects it — and a rejected record must surface as a malformed
// batch rather than a silently empty one.
func TestSampledBatchWithZeroSamplePeriodIsRejected(t *testing.T) {
	_, err := decodeBatch(sampledBatch(1, 0))
	require.ErrorIs(t, err, gpuabi.ErrInvalidSamplePeriod)
}

func TestDecodeKernelNameBatch(t *testing.T) {
	buf := make([]byte, batchHdrSize+gpuabi.SizeKernelName)
	putU32(buf[0:], kindKernelName)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeKernelName))
	putU64(buf[batchHdrSize:], 0xAAAA)
	binary.LittleEndian.PutUint16(buf[batchHdrSize+8:], 5)
	copy(buf[batchHdrSize+16:], "kAddPfi")

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	require.Len(t, b.KernelNames, 1)
	assert.Equal(t, uint64(0xAAAA), b.KernelNames[0].KernelID)
	assert.Equal(t, "kAddP", b.KernelNames[0].Name)

	// Interning is Task 5; until then the record is carried and counted, not
	// silently dropped.
	c := newTestConsumer(&recordingSink{})
	c.applyBatch(b)
	assert.Equal(t, uint64(1), c.Stats().Undecoded)
}

// A count larger than the declared payload must fail for the new, larger
// record kinds too — 272 bytes is the one record big enough that an
// off-by-one here would read well past the sample.
func TestDecodeRejectsCountBeyondPayloadForTheLargeKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind uint32
		size int
		want error
	}{
		// kindLaunchSampled never reaches the length check: a count of 2 is
		// refused outright, because one header carries one stack id.
		{"sampled", kindLaunchSampled, gpuabi.SizeLaunchSampled, errSampledBatchNotSingular},
		{"kernelname", kindKernelName, gpuabi.SizeKernelName, gpuabi.ErrShortRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, batchHdrSize+tc.size)
			putU32(buf[0:], tc.kind)
			putU32(buf[4:], 2) // claims two, carries one
			putU64(buf[24:], uint64(tc.size))
			_, err := decodeBatch(buf)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// The stack map and the kernel-side stack-failure counter are part of the
// contract this task adds; assert them on the embedded object rather than
// discovering their absence on a machine with capabilities.
func TestEmbeddedProgramCarriesTheStackMap(t *testing.T) {
	spec, err := loadGpuusdt()
	require.NoError(t, err)

	require.Contains(t, spec.Maps, "stackmap")
	assert.Equal(t, ebpf.StackTrace, spec.Maps["stackmap"].Type)
	assert.Equal(t, uint32(127*8), spec.Maps["stackmap"].ValueSize,
		"PERF_MAX_STACK_DEPTH frames of u64")

	require.Contains(t, spec.Maps, "stacks_missing")
	assert.Equal(t, ebpf.Array, spec.Maps["stacks_missing"].Type)
	assert.Equal(t, uint32(kindMax), spec.Maps["stacks_missing"].MaxEntries)
	assert.NotSame(t, spec.Maps["dropped"], spec.Maps["stacks_missing"],
		"a failed capture is not a dropped record and must not inflate KernelDropped")
}

// A sampled-launch batch carrying more than one record would attribute the
// batch's single captured stack to every launch in it — silently, because
// every record still decodes. Both ends refuse it: the BPF program caps this
// kind at one record (max_records in bpf/gpu_usdt.bpf.c, asserted below
// against the source) and decodeBatch rejects any other count.
func TestSampledBatchWithMoreThanOneRecordIsRejected(t *testing.T) {
	for _, count := range []uint32{0, 2, 54} {
		buf := make([]byte, batchHdrSize+2*gpuabi.SizeLaunchSampled)
		putU32(buf[0:], kindLaunchSampled)
		putU32(buf[4:], count)
		putU64(buf[24:], uint64(2*gpuabi.SizeLaunchSampled))
		putU32(buf[32:], 5) // a perfectly good stack id
		putU32(buf[batchHdrSize+44:], 8)
		putU32(buf[batchHdrSize+gpuabi.SizeLaunchSampled+44:], 8)

		_, err := decodeBatch(buf)
		require.ErrorIsf(t, err, errSampledBatchNotSingular,
			"count=%d: one header carries one stack id, so it may carry only one launch", count)
	}
}

// ...and the rejection is counted, never skipped: Run's decode failure path
// is the only thing standing between a mis-batched producer and silence.
func TestRunCountsAMisBatchedSampledBatchAsMalformed(t *testing.T) {
	reader := newScriptedReader(2)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	buf := make([]byte, batchHdrSize+2*gpuabi.SizeLaunchSampled)
	putU32(buf[0:], kindLaunchSampled)
	putU32(buf[4:], 2)
	putU64(buf[24:], uint64(2*gpuabi.SizeLaunchSampled))
	putU32(buf[batchHdrSize+44:], 8)
	putU32(buf[batchHdrSize+gpuabi.SizeLaunchSampled+44:], 8)
	reader.recs <- buf

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	require.Eventually(t, func() bool { return c.Stats().Malformed == 1 },
		5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Close())
	<-done

	st := c.Stats()
	assert.Zero(t, st.Records, "not one of the two records is normalized")
	assert.Zero(t, st.SampledLaunches)
	assert.Zero(t, st.StacksMissing, "a rejected batch is malformed, not a missing stack")
}

// A single capture failure must be counted once. It is hoisted out of the
// record loop in applyBatch so that stays true even if the count invariant
// above is ever loosened.
func TestAStackFailureIsCountedOncePerBatch(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	b, err := decodeBatch(sampledBatch(-1, 8))
	require.NoError(t, err)
	// Force the shape the decoder refuses, to prove applyBatch would not
	// multiply one capture failure by the record count.
	b.SampledLaunches = append(b.SampledLaunches, b.SampledLaunches[0], b.SampledLaunches[0])
	c.applyBatch(b)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksMissing, "one batch, one capture, one failure")
	assert.Equal(t, uint64(3), st.SampledLaunches)
}

// --- Phase 4a: resolving a capture and getting it onto the right launch ---

// fakeStackmap is a stackStore: the seam that stands in for the BPF
// stackmap, which cannot be created without CAP_BPF. It records deletions
// because deleting a consumed entry is load-bearing, not housekeeping - see
// Consumer.freeStackLocked.
type fakeStackmap struct {
	entries   map[uint32][]byte
	deleted   []uint32
	lookups   int
	lookupErr error
	deleteErr error
}

func newFakeStackmap() *fakeStackmap {
	return &fakeStackmap{entries: map[uint32][]byte{}}
}

// put stores a stack as the kernel would: little-endian u64 instruction
// pointers, leaf first, in a fixed-size buffer that the first zero slot
// terminates.
func (f *fakeStackmap) put(id uint32, ips ...uint64) {
	buf := make([]byte, bpfstack.MaxFrames*8)
	for i, ip := range ips {
		putU64(buf[i*8:], ip)
	}
	f.entries[id] = buf
}

func (f *fakeStackmap) LookupBytes(key any) ([]byte, error) {
	f.lookups++
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	raw, ok := f.entries[key.(uint32)]
	if !ok {
		return nil, ebpf.ErrKeyNotExist
	}
	return raw, nil
}

func (f *fakeStackmap) Delete(key any) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	id := key.(uint32)
	if _, ok := f.entries[id]; !ok {
		return ebpf.ErrKeyNotExist
	}
	delete(f.entries, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeStackmap) wasDeleted(id uint32) bool {
	return slices.Contains(f.deleted, id)
}

// fakeSymbolizer names each IP "fn_<hex>" so a test can assert both the
// frames and their order without a real symbolizer or a real process.
type fakeSymbolizer struct {
	err  error
	pids []uint32
	ips  [][]uint64
}

func (s *fakeSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	s.pids = append(s.pids, pid)
	s.ips = append(s.ips, slices.Clone(ips))
	if s.err != nil {
		return nil, s.err
	}
	out := make([]symbolize.Frame, 0, len(ips))
	for _, ip := range ips {
		out = append(out, symbolize.Frame{Address: ip, Name: fmt.Sprintf("fn_%x", ip)})
	}
	return out, nil
}

func (s *fakeSymbolizer) Close() error { return nil }

// stackConsumer wires a consumer to a fake stackmap and symbolizer.
func stackConsumer(t *testing.T, sink gpu.EventSink, cfg Config) (*Consumer, *fakeStackmap, *fakeSymbolizer) {
	t.Helper()
	sm, sym := newFakeStackmap(), &fakeSymbolizer{}
	cfg.Backend, cfg.Sink = gpu.BackendCUPTI, sink
	if cfg.Symbolizer == nil {
		cfg.Symbolizer = sym
	}
	c := newConsumer(cfg)
	c.stacks = sm
	return c, sm, sym
}

// launchBatchWith builds a launch batch carrying the given correlations, in
// order, all from one pid.
func launchBatchWith(pid uint32, corrs ...uint64) []byte {
	buf := make([]byte, batchHdrSize+len(corrs)*gpuabi.SizeLaunch)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], uint32(len(corrs)))
	putU32(buf[16:], pid)
	putU64(buf[24:], uint64(len(corrs)*gpuabi.SizeLaunch))
	putU32(buf[32:], ^uint32(0)) // stack_id = -1 on every non-sampled kind
	for i, corr := range corrs {
		rec := buf[batchHdrSize+i*gpuabi.SizeLaunch:]
		putU64(rec[0:], corr)
		putU64(rec[32:], uint64(100+i)) // time_ns
	}
	return buf
}

// sampledBatchWith is sampledBatch with the correlation and pid under the
// test's control.
func sampledBatchWith(pid uint32, corr uint64, stackID int32, period uint32) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeLaunchSampled)
	putU32(buf[0:], kindLaunchSampled)
	putU32(buf[4:], 1)
	putU32(buf[16:], pid)
	putU64(buf[24:], uint64(gpuabi.SizeLaunchSampled))
	putU32(buf[32:], uint32(stackID))
	putU64(buf[batchHdrSize:], corr)
	putU32(buf[batchHdrSize+44:], period)
	return buf
}

func apply(t *testing.T, c *Consumer, wire []byte) {
	t.Helper()
	b, err := decodeBatch(wire)
	require.NoError(t, err)
	c.applyBatch(b)
}

// The common arrival order: the shim's launch batch only flushes when it
// fills, so the unbatched sampled record for a launch usually reaches the
// ringbuf well before the batched record for that same launch. The resolved
// stack waits for its twin and rides out on it.
//
// The frames must come out root-first (main, then the launch site), because
// the gpu projection nests [gpu:launch] and the kernel frame underneath
// them. ToProfFrames returns leaf-first, so a missing reverse would invert
// every GPU flame graph - visible here as fn_2000 leading.
func TestSampledStackArrivingFirstAttachesToTheBatchedLaunch(t *testing.T) {
	sink := &recordingSink{}
	c, sm, sym := stackConsumer(t, sink, Config{})
	sm.put(5, 0x2000, 0x1000) // leaf first, as the kernel stores it

	apply(t, c, sampledBatchWith(4242, 7, 5, 8))
	assert.Zero(t, sink.launchCount(), "a sampled record is not a launch of its own")

	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1, "one launch in, one launch out - the sampled twin is not a second launch")
	got := sink.launches[0]
	assert.Equal(t, []string{"fn_1000", "fn_2000"}, frameNames(got.Launch.CPUStack),
		"frames must be root-first; leaf-first would invert every GPU flame graph")
	assert.Equal(t, uint32(8), got.Launch.SamplePeriod,
		"the period travels with the stack, so a consumer never has to reconstruct it")
	assert.Equal(t, []uint32{4242}, sym.pids, "symbolization is against the launching process")

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksResolved)
	assert.Equal(t, uint64(1), st.StacksAttached)
	assert.Zero(t, st.PendingStacks, "the parked stack must be taken, not left behind")
	assert.True(t, sm.wasDeleted(5), "a consumed stackmap entry must be freed")
}

// The other arrival order, which happens whenever the launch that fills the
// shim's batch is also the one the sampler picked: the flush is queued
// inside the add() that precedes the sampler check, so the batched record
// reaches the ringbuf first. The launch waits for the twin already on its
// way, then goes out once - with its stack.
func TestBatchedLaunchArrivingFirstWaitsForItsStack(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(9, 0x3000)

	apply(t, c, launchBatchWith(4242, 7))
	assert.Zero(t, sink.launchCount(), "a correlated launch waits briefly for a possible sampled twin")
	assert.Equal(t, 1, c.Stats().PendingLaunches, "held launches are visible, not invisible")

	apply(t, c, sampledBatchWith(4242, 7, 9, 4))
	require.Len(t, sink.launches, 1, "the launch goes out exactly once, whichever half arrived first")
	assert.Equal(t, []string{"fn_3000"}, frameNames(sink.launches[0].Launch.CPUStack))
	assert.Equal(t, uint32(4), sink.launches[0].Launch.SamplePeriod)
	assert.Zero(t, c.Stats().PendingLaunches, "a paired launch is released, not left held")

	c.Flush()
	assert.Len(t, sink.launches, 1, "flushing must not emit the paired launch a second time")
}

// The sampled record shares its correlation with the batched one, so
// emitting it as a launch would give the timeline two launches for one -
// and timeline.go has no dedup that would catch it. Nothing but the stack
// may cross over.
func TestSampledRecordIsNeverEmittedAsItsOwnLaunch(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	c.Flush()

	assert.Zero(t, sink.launchCount(), "the sampled record must never reach the sink as a launch")
	st := c.Stats()
	assert.Equal(t, uint64(1), st.SampledLaunches)
	assert.Equal(t, 1, st.PendingStacks, "its stack waits for the batched twin instead")
}

// The rule the whole design exists to protect: a launch the sampler did not
// pick has no CPU call path, and must not be handed one. Here two launches
// arrive in the same batch and only the first was sampled.
func TestUnsampledLaunchNeverBorrowsASampledSiblingsStack(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(3, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 3, 8))
	apply(t, c, launchBatchWith(4242, 7, 8))
	c.Flush()

	require.Len(t, sink.launches, 2)
	assert.Equal(t, []string{"fn_1000"}, frameNames(sink.launches[0].Launch.CPUStack))
	assert.Empty(t, sink.launches[1].Launch.CPUStack,
		"the unsampled sibling must project as unattributed, not under a borrowed call path")
	assert.Zero(t, sink.launches[1].Launch.SamplePeriod,
		"a period with no stack would advertise attribution that does not exist")
}

// A held launch is waiting for a record that would be the next ringbuf
// sample. Anything else arriving ends the wait: launches must not sit
// behind an exec batch, because the timeline joins executions against
// launches it has already been given.
func TestAnyOtherBatchReleasesHeldLaunches(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})

	apply(t, c, launchBatchWith(4242, 7))
	require.Zero(t, sink.launchCount())

	buf := make([]byte, batchHdrSize+gpuabi.SizeExec)
	putU32(buf[0:], kindExec)
	putU32(buf[4:], 1)
	putU64(buf[24:], gpuabi.SizeExec)
	putU64(buf[batchHdrSize:], 7)
	apply(t, c, buf)

	assert.Equal(t, 1, sink.launchCount(), "an exec batch means the sampled twin is not coming")
	assert.Zero(t, c.Stats().PendingLaunches)
}

// The side table is fed by a profiled application, so it must be bounded -
// and what it pushes out must be counted, because an evicted stack is
// attribution that silently never arrives otherwise. The launch itself is
// untouched: it still ships, without a stack.
func TestParkedStacksAreBoundedAndEvictionsCounted(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{SampledStackCapacity: 2})
	for id := uint32(1); id <= 3; id++ {
		sm.put(id, 0x1000+uint64(id))
		apply(t, c, sampledBatchWith(4242, uint64(id), int32(id), 8))
	}

	st := c.Stats()
	assert.Equal(t, 2, st.PendingStacks, "the table must not grow past its bound")
	assert.Equal(t, uint64(1), st.StacksEvicted, "an evicted stack is counted, never silent")
	assert.Equal(t, uint64(3), st.StacksResolved)

	// Correlation 1 is the one that was pushed out: its launch still
	// arrives, and arrives without a stack rather than with someone else's.
	apply(t, c, launchBatchWith(4242, 1))
	c.Flush()
	require.Len(t, sink.launches, 1)
	assert.Empty(t, sink.launches[0].Launch.CPUStack)
}

// Holding launches is bounded too, but the bound must never become loss:
// pushing past it releases the oldest held launch to the sink rather than
// dropping it.
func TestHeldLaunchesAreBoundedAndNeverDropped(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{DeferredLaunchCapacity: 2})

	apply(t, c, launchBatchWith(4242, 1, 2, 3, 4, 5))
	assert.Equal(t, 3, sink.launchCount(), "the three oldest are released to make room, not discarded")
	assert.Equal(t, 2, c.Stats().PendingLaunches)

	c.Flush()
	require.Len(t, sink.launches, 5, "every launch reaches the sink exactly once")
	for i, l := range sink.launches {
		assert.Equal(t, strconv.Itoa(i+1), l.Correlation.Value, "arrival order is preserved")
	}
}

// Deletion is what keeps capture working, not tidiness: bpf_get_stackid is
// called without BPF_F_REUSE_STACKID, so a bucket holding a live entry
// answers -EEXIST. Nothing else ever removes an entry, so without this the
// map fills and every later capture fails - and reads as a broken capture
// path rather than a full map. The entry must be freed on every outcome,
// including the ones where its contents are never used.
func TestStackmapEntriesAreFreedOnEveryOutcome(t *testing.T) {
	t.Run("resolved", func(t *testing.T) {
		c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
		sm.put(1, 0x1000)
		apply(t, c, sampledBatchWith(4242, 7, 1, 8))
		assert.True(t, sm.wasDeleted(1))
	})
	t.Run("uncorrelated", func(t *testing.T) {
		c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
		sm.put(2, 0x1000)
		apply(t, c, sampledBatchWith(4242, 0, 2, 8))
		assert.True(t, sm.wasDeleted(2), "an unpairable capture still owns its slot")
		assert.Equal(t, uint64(1), c.Stats().StacksUncorrelated)
	})
	t.Run("symbolization failed", func(t *testing.T) {
		sym := &fakeSymbolizer{err: errors.New("no symbols")}
		c, sm, _ := stackConsumer(t, &recordingSink{}, Config{Symbolizer: sym})
		sm.put(3, 0x1000)
		apply(t, c, sampledBatchWith(4242, 7, 3, 8))
		assert.True(t, sm.wasDeleted(3), "a slot whose contents could not be symbolized is still consumed")
	})
	t.Run("no entry to free", func(t *testing.T) {
		c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
		apply(t, c, sampledBatchWith(4242, 7, 99, 8))
		assert.Empty(t, sm.deleted)
		assert.Equal(t, uint64(1), c.Stats().StackLookupFailed)
	})
}

// A delete that fails is the leading indicator of the map filling up, so it
// is counted rather than ignored.
func TestStackDeleteFailureIsCounted(t *testing.T) {
	c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
	sm.put(1, 0x1000)
	sm.deleteErr = errors.New("permission denied")

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	assert.Equal(t, uint64(1), c.Stats().StackDeleteFailed)
	assert.Equal(t, uint64(1), c.Stats().StacksResolved, "the capture still resolved; only the free failed")
}

// bpf_get_stackid is content-addressed: two launches from the same call
// site in flight at once carry the same stack id, and the first resolution
// deletes the entry. The second finds nothing. That is real attribution
// loss and it is counted - the alternative, serving cached frames for an id
// that a different stack may since have taken, would attribute GPU time to
// a call path that did not produce it.
func TestTwoCapturesSharingAStackIDLoseTheSecondVisibly(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, sampledBatchWith(4242, 8, 1, 8))
	apply(t, c, launchBatchWith(4242, 7, 8))
	c.Flush()

	require.Len(t, sink.launches, 2)
	assert.Equal(t, []string{"fn_1000"}, frameNames(sink.launches[0].Launch.CPUStack))
	assert.Empty(t, sink.launches[1].Launch.CPUStack)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StackLookupFailed, "the second capture's loss is counted")
	assert.Equal(t, uint64(2), st.SampledLaunches)
}

// A symbolization failure must cost the stack, never the launch: the launch
// still reaches the sink, where it projects as unattributed GPU time
// instead of vanishing from the profile.
func TestSymbolizationFailureDegradesToNoStackNotALostLaunch(t *testing.T) {
	sink := &recordingSink{}
	sym := &fakeSymbolizer{err: errors.New("blazesym: no such process")}
	c, sm, _ := stackConsumer(t, sink, Config{Symbolizer: sym})
	sm.put(1, 0x1000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1, "the launch survives its stack")
	assert.Empty(t, sink.launches[0].Launch.CPUStack)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.SymbolizeFailed)
	assert.Zero(t, st.StacksResolved)
	assert.Zero(t, st.StacksAttached)
}

// A consumer configured without a Symbolizer still delivers every launch;
// what it cannot do is attribute them, and that is counted rather than
// looking like a producer that never sampled anything.
func TestMissingSymbolizerIsCountedNotSilent(t *testing.T) {
	sink := &recordingSink{}
	c := newConsumer(Config{Backend: gpu.BackendCUPTI, Sink: sink})
	sm := newFakeStackmap()
	sm.put(1, 0x1000)
	c.stacks = sm

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1)
	assert.Empty(t, sink.launches[0].Launch.CPUStack)
	assert.Equal(t, uint64(1), c.Stats().SymbolizeFailed)
}

// An entry that reads back with no instruction pointers yields no stack,
// and says so.
func TestEmptyStackEntryIsCountedAsALookupFailure(t *testing.T) {
	c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
	sm.put(1) // present, but zero-terminated at the first slot

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	assert.Equal(t, uint64(1), c.Stats().StackLookupFailed)
	assert.Zero(t, c.Stats().StacksResolved)
}

// Every sampled launch ends in exactly one bucket, and the buckets add up.
// A reader who cannot reconcile the counters cannot tell a quiet producer
// from a consumer quietly losing captures, which is the whole point of §6.1
// accounting.
func TestSampledStackAccountingReconciles(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{SampledStackCapacity: 1})

	sm.put(1, 0x1000) // resolves, then parks
	sm.put(2, 0x2000) // resolves, parks, evicting the first
	sm.put(3, 0x3000) // resolves and attaches to a held launch
	apply(t, c, sampledBatchWith(4242, 1, 1, 8))
	apply(t, c, sampledBatchWith(4242, 2, 2, 8))
	apply(t, c, launchBatchWith(4242, 3))
	apply(t, c, sampledBatchWith(4242, 3, 3, 8))
	apply(t, c, sampledBatchWith(4242, 4, -1, 8))  // capture failed
	apply(t, c, sampledBatchWith(4242, 0, 5, 8))   // no correlation
	apply(t, c, sampledBatchWith(4242, 6, 404, 8)) // no such entry
	c.Flush()

	st := c.Stats()
	assert.Equal(t, uint64(6), st.SampledLaunches)
	assert.Equal(t, st.SampledLaunches,
		st.StacksMissing+st.StacksUncorrelated+st.StackLookupFailed+st.SymbolizeFailed+st.StacksResolved,
		"every sampled launch must land in exactly one bucket")
	assert.Equal(t, st.StacksResolved,
		st.StacksAttached+st.StacksEvicted+uint64(st.PendingStacks),
		"every resolved stack is attached, evicted, or still waiting - nothing else")
}

// Run must release what it is holding on the way out, so a caller can
// cancel, wait for Run, and take a complete Snapshot without closing the
// consumer.
func TestRunReleasesHeldLaunchesWhenItReturns(t *testing.T) {
	reader := newScriptedReader(1)
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})
	c.reader = reader

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	reader.recs <- launchBatchWith(4242, 7)
	// Wait for the batch to be applied: it is held, not emitted, so the
	// sink cannot signal it. The pending gauge can.
	require.Eventually(t, func() bool { return c.Stats().PendingLaunches == 1 },
		5*time.Second, time.Millisecond, "Run never applied the launch batch")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	assert.Equal(t, 1, sink.launchCount(), "a held launch must not wait for Close to be delivered")
	assert.Zero(t, c.Stats().PendingLaunches)
}

// Close is the last chance: whatever is still held goes to the sink rather
// than being dropped at teardown.
func TestCloseReleasesHeldLaunches(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})
	c.reader = newScriptedReader(0)

	apply(t, c, launchBatchWith(4242, 7))
	require.Zero(t, sink.launchCount())

	require.NoError(t, c.Close())
	assert.Equal(t, 1, sink.launchCount(), "a launch held at teardown would be silent record loss")
}

// A sink that rejects a held launch is still a rejection that must be
// counted when the launch is finally released.
func TestSinkRejectionOfAReleasedLaunchIsCounted(t *testing.T) {
	sink := &recordingSink{err: errors.New("full")}
	c, _, _ := stackConsumer(t, sink, Config{})

	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()
	assert.Equal(t, uint64(1), c.Stats().SinkRejected)
}
