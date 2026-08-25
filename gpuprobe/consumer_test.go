package gpuprobe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
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
	mu        sync.Mutex
	launches  []gpu.GPUKernelLaunch
	execs     []gpu.GPUKernelExec
	pcSamples []gpu.GPUPCSample
	modules   []gpu.GPUModule
	err       error
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

func (s *recordingSink) EmitPCSample(p gpu.GPUPCSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.pcSamples = append(s.pcSamples, p)
	s.note()
	return nil
}

func (s *recordingSink) EmitModule(m gpu.GPUModule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.modules = append(s.modules, m)
	s.note()
	return nil
}

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

// Stats.Undecoded is the "carried but not interpreted" counter. Every kind
// the ABI defines has an applyBatch arm except kindDropped, which is decoded
// and carried while its normalization waits for the consumer task that
// follows this one. The kind used below is neither - it is a kind neither
// side of the wire knows, which is the ABI having drifted, and which must
// still be counted rather than dropped quietly (§6.1 admits no silent loss
// anywhere).
func TestUndecodedKindsAreCountedNotDropped(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	buf := make([]byte, batchHdrSize+40)
	putU32(buf[0:], 15) // below kindMax, above every kind either side defines
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
	// The correlation carries the batch header's pid: a vendor value alone
	// collides across processes (gpu.CorrelationID, issue #36).
	assert.Equal(t, gpu.CorrelationID{Backend: gpu.BackendCUPTI, PID: 4242, Value: "77"}, got.Correlation)
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
	putU32(buf[16:], 4242) // pid comes from the batch header
	putU64(buf[24:], 48)
	putU64(buf[batchHdrSize+0:], 88)   // correlation
	putU64(buf[batchHdrSize+32:], 10)  // start_ns
	putU64(buf[batchHdrSize+40:], 200) // end_ns

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)

	require.Len(t, sink.execs, 1)
	assert.Equal(t, "88", sink.execs[0].Correlation.Value)
	// GPUKernelExec has no PID field of its own; the correlation is where an
	// execution's process identity lives, and the join depends on it being
	// there (issue #36).
	assert.Equal(t, uint32(4242), sink.execs[0].Correlation.PID,
		"an execution's correlation must name the process that produced it")
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
	st := c.Stats()
	assert.Equal(t, uint64(1), st.ZeroCorrelation)
	assert.Equal(t, uint64(1), st.ZeroCorrelationExecs,
		"an execution's zero correlation is a spec §6 contract violation, and must be countable "+
			"apart from the PC samples that make ZeroCorrelation large on a healthy run (issue #52)")
}

// TestZeroCorrelationExecsCountsOnlyExecutions is the separation itself.
// ZeroCorrelation is an aggregate over every record kind, and the kinds mean
// different things when they carry no correlation: for a PC sample in
// continuous collection it is the documented normal case (spec §6.3 finding
// 3) and for a launch it is at worst a lost anchor, while for an EXECUTION it
// is the shim breaking spec §6 outright. The whole value of the narrow
// counter is that it can be asserted zero while the aggregate cannot, so it
// must not move for anything but an execution.
func TestZeroCorrelationExecsCountsOnlyExecutions(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	buf := make([]byte, batchHdrSize+48)
	putU32(buf[0:], kindLaunch)
	putU32(buf[4:], 1)
	putU64(buf[24:], 48)
	putU64(buf[batchHdrSize+0:], 0) // no correlation

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	c.applyBatch(b)
	c.Flush()

	require.Len(t, sink.launches, 1)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.ZeroCorrelation, "the aggregate still sees it")
	assert.Zero(t, st.ZeroCorrelationExecs,
		"no execution arrived, so the contract-violation counter must stay clean - "+
			"otherwise it would move for records whose zero correlation is not a violation at all")
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

	// The record is interned rather than counted as undecoded: it is a
	// normalized record now, and the name it carries reaches the launches
	// and executions for kernel 0xAAAA.
	c := newTestConsumer(&recordingSink{})
	c.applyBatch(b)
	st := c.Stats()
	assert.Zero(t, st.Undecoded, "kernel names are decoded, not carried undecoded")
	assert.Equal(t, uint64(1), st.Records)
	assert.Equal(t, uint64(1), st.KernelNamesLearned)
	assert.Equal(t, 1, st.KnownKernelNames)
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

	require.Contains(t, spec.Maps, "gpu_stacks")
	assert.Equal(t, ebpf.Hash, spec.Maps["gpu_stacks"].Type,
		"a hand-rolled walk cannot populate a kernel BPF_MAP_TYPE_STACK_TRACE")
	assert.Equal(t, uint32(4), spec.Maps["gpu_stacks"].KeySize, "a u32 handle")
	assert.Equal(t, uint32(gpuStackSize), spec.Maps["gpu_stacks"].ValueSize,
		"n_pcs, walker_flags, then MAX_FRAMES u64 PCs")
	assert.Equal(t, uint32(gpuStackCapacity), spec.Maps["gpu_stacks"].MaxEntries)

	assert.NotContains(t, spec.Maps, "stackmap",
		"the kernel stackmap is gone: bpf_get_stackid stopped at the first frame it could not follow")

	require.Contains(t, spec.Maps, "walk_errors")
	assert.Equal(t, ebpf.Array, spec.Maps["walk_errors"].Type)
	assert.Equal(t, uint32(walkErrMax), spec.Maps["walk_errors"].MaxEntries,
		"one slot per way the capture can fail; none of them is silent")

	// The walker's own tables must have come along with the header, or the
	// walk degrades to the frame-pointer path this phase exists to replace.
	for _, name := range []string{"walker_scratch", "gpu_stack_scratch", "stack_id_seq",
		"pids", "pid_mappings", "cfi_rules", "cfi_classification", "cfi_miss_events"} {
		assert.Contains(t, spec.Maps, name)
	}

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

// fakeStackStore is a stackStore: the seam that stands in for the BPF
// gpu_stacks map, which cannot be created without CAP_BPF. It records
// deletions because deleting a consumed entry is load-bearing, not
// housekeeping - see Consumer.freeStackLocked.
//
// entries holds PCs rather than bytes so a test reads as a list of program
// counters; encodeGPUStack lays them out on the wire exactly as
// bpf/gpu_usdt.bpf.c writes a struct gpu_stack.
type fakeStackStore struct {
	entries map[uint32][]uint64
	flags   map[uint32]uint32
	// raw overrides the encoding for one id, so a test can hand the decoder
	// a value the encoder would never produce.
	raw       map[uint32][]byte
	deleted   []uint32
	lookups   int
	lookupErr error
	deleteErr error
}

func newFakeStackStore() *fakeStackStore {
	return &fakeStackStore{entries: map[uint32][]uint64{}, flags: map[uint32]uint32{}}
}

// encodeGPUStack lays out a struct gpu_stack the way the BPF program does:
// a u32 count, a u32 walker_flags, then MAX_FRAMES u64 PCs, leaf first, in a
// fixed-size buffer. Slots past n_pcs are whatever the per-CPU scratch last
// held, so this deliberately fills them with a poison value: a decoder that
// scans for a zero terminator instead of honouring n_pcs must fail loudly
// here rather than in production.
func encodeGPUStack(pcs []uint64, flags uint32) []byte {
	buf := make([]byte, gpuStackSize)
	putU32(buf[0:], uint32(len(pcs)))
	putU32(buf[4:], flags)
	for i := 0; i < maxWalkFrames; i++ {
		if i < len(pcs) {
			putU64(buf[gpuStackHdrSize+i*8:], pcs[i])
			continue
		}
		putU64(buf[gpuStackHdrSize+i*8:], 0xdeadbeefdeadbeef)
	}
	return buf
}

// put stores a walk result under the given id.
func (f *fakeStackStore) put(id uint32, pcs ...uint64) {
	f.entries[id] = pcs
}

func (f *fakeStackStore) LookupBytes(key any) ([]byte, error) {
	f.lookups++
	if f.lookupErr != nil {
		return nil, f.lookupErr
	}
	id := key.(uint32)
	if b, ok := f.raw[id]; ok {
		return b, nil
	}
	pcs, ok := f.entries[id]
	if !ok {
		return nil, ebpf.ErrKeyNotExist
	}
	return encodeGPUStack(pcs, f.flags[id]), nil
}

func (f *fakeStackStore) Delete(key any) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	id := key.(uint32)
	_, inEntries := f.entries[id]
	_, inRaw := f.raw[id]
	if !inEntries && !inRaw {
		return ebpf.ErrKeyNotExist
	}
	delete(f.entries, id)
	delete(f.raw, id)
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeStackStore) wasDeleted(id uint32) bool {
	return slices.Contains(f.deleted, id)
}

// resolveStackForTest is the locking wrapper the resolve-path tests use;
// resolveStackLocked itself requires mu.
func (c *Consumer) resolveStackForTest(stackID int32, pid uint32) ([]pp.Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resolveStackLocked(pid, stackID)
}

// fakeSymbolizer names each IP "fn_<hex>" so a test can assert both the
// frames and their order without a real symbolizer or a real process.
type fakeSymbolizer struct {
	err  error
	pids []uint32
	ips  [][]uint64
	// unresolved, when non-nil, decides per IP whether the frame comes back
	// address-only: a Frame with no error, a name that is just the hex
	// address, and Reason set. That is exactly what
	// symbolize.LocalSymbolizer produces when blazesym cannot open the
	// target process - the shape that made the Phase 4a gate look like a
	// success while resolving nothing.
	unresolved func(ip uint64) bool
}

func (s *fakeSymbolizer) SymbolizeProcess(pid uint32, ips []uint64) ([]symbolize.Frame, error) {
	s.pids = append(s.pids, pid)
	s.ips = append(s.ips, slices.Clone(ips))
	if s.err != nil {
		return nil, s.err
	}
	out := make([]symbolize.Frame, 0, len(ips))
	for _, ip := range ips {
		if s.unresolved != nil && s.unresolved(ip) {
			out = append(out, symbolize.Frame{
				Address: ip,
				Name:    fmt.Sprintf("0x%x", ip),
				Reason:  symbolize.FailureMissingSymbols,
			})
			continue
		}
		out = append(out, symbolize.Frame{Address: ip, Name: fmt.Sprintf("fn_%x", ip)})
	}
	return out, nil
}

func (s *fakeSymbolizer) Close() error { return nil }

// stackConsumer wires a consumer to a fake stackmap and symbolizer.
func stackConsumer(t *testing.T, sink gpu.EventSink, cfg Config) (*Consumer, *fakeStackStore, *fakeSymbolizer) {
	t.Helper()
	sm, sym := newFakeStackStore(), &fakeSymbolizer{}
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

// --- Phase 4b: stack_id is a handle into the walker's own map ---

// The read path's shape is unchanged from the stackmap days, and so is the
// discipline that keeps it working: an entry is deleted the moment it is
// read. Nothing else reclaims a gpu_stacks slot, so a resolve that leaked
// one would fill the map and turn capture off for the rest of the run.
func TestResolveReadsTheWalkerMapAndDeletesTheEntry(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(7, 0x401000, 0x401100, 0x7f0000001000)

	frames, ok := c.resolveStackForTest(7, 4242)
	require.True(t, ok)
	require.Len(t, frames, 3)
	assert.Zero(t, c.Stats().StackLookupFailed)
	assert.NotContains(t, stacks.entries, uint32(7),
		"a resolved entry must be deleted so the id can be reused")
	assert.True(t, stacks.wasDeleted(7))
}

// A walk that produced nothing is not an attribution. The BPF side does not
// normally insert one - it counts the empty walk and hands the consumer -1 -
// so this is the defensive half: if an empty entry ever does arrive, it is
// counted rather than attached to a launch as a zero-frame call path.
func TestAnEmptyWalkIsCountedNotAttached(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(9)

	_, ok := c.resolveStackForTest(9, 4242)
	assert.False(t, ok, "a walk that produced no frames is not an attribution")
	assert.Equal(t, uint64(1), c.Stats().StackWalkEmpty)
	assert.Equal(t, uint64(1), c.Stats().StackLookupFailed,
		"StackWalkEmpty says why; the launch still lands in exactly one bucket")
	assert.True(t, stacks.wasDeleted(9), "an empty entry is still ours to release")
}

// MAX_FRAMES worth of PCs means the walk hit its bound; the frames we have
// are real and worth attributing, but the truncation is visible.
func TestATruncatedWalkIsCountedButStillUsed(t *testing.T) {
	full := make([]uint64, maxWalkFrames)
	for i := range full {
		full[i] = uint64(0x401000 + i*16)
	}
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(11, full...)

	frames, ok := c.resolveStackForTest(11, 4242)
	require.True(t, ok)
	assert.Len(t, frames, maxWalkFrames)
	assert.Equal(t, uint64(1), c.Stats().StackWalkTruncated)
	assert.Zero(t, c.Stats().StackLookupFailed, "a truncated walk is not a failed one")
}

// A walk shorter than MAX_FRAMES is not truncated, and nothing past n_pcs is
// read. The scratch record is per-CPU and reused, so the slots past the walk
// hold the previous sample's PCs - a decoder that scanned for a zero
// terminator instead of honouring n_pcs would splice two unrelated stacks
// together and present the result as one call path.
func TestResolveHonoursNPcsRatherThanScanningForAZero(t *testing.T) {
	c, stacks, sym := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(13, 0x401000, 0x401100)

	frames, ok := c.resolveStackForTest(13, 4242)
	require.True(t, ok)
	require.Len(t, frames, 2)
	require.Len(t, sym.ips, 1)
	assert.Equal(t, []uint64{0x401000, 0x401100}, sym.ips[0],
		"only the PCs the walk actually produced reach the symbolizer")
	assert.Zero(t, c.Stats().StackWalkTruncated)
}

// walker_flags rides along with the PCs because the walk is the only place
// that knows how it went: whether DWARF fired, whether the FP chain reached
// a natural terminator, whether a CFI lookup cut it short. Nothing reads it
// yet - Task 2 does - and re-deriving it after the fact is impossible, which
// is why it is in the map value from the start rather than added later.
func TestDecodeGPUStackCarriesTheWalkersFlags(t *testing.T) {
	pcs, flags, ok := decodeGPUStack(encodeGPUStack([]uint64{0x401000, 0x401100}, 0x06))
	require.True(t, ok)
	assert.Equal(t, []uint64{0x401000, 0x401100}, pcs)
	assert.Equal(t, uint32(0x06), flags, "WALKER_FLAG_DWARF_USED | WALKER_FLAG_CFI_MISS")
}

// Identical PCs; only the flags the walker reported differ. An FP-only walk
// through vendor libraries is exactly the case that produced a
// profiler-only stack on real hardware, so the two must be countable apart -
// a single "we got a stack" counter cannot express it.
func TestFPOnlyAndDWARFWalksAreCountedSeparately(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	pcs := []uint64{0x401000, 0x401100}
	stacks.put(1, pcs...)
	stacks.flags[1] = walkerFlagDWARFUsed
	stacks.put(2, pcs...)
	stacks.flags[2] = walkerFlagFPTerminated

	_, ok := c.resolveStackForTest(1, 4242)
	require.True(t, ok)
	_, ok = c.resolveStackForTest(2, 4242)
	require.True(t, ok)

	assert.Equal(t, uint64(1), c.Stats().StacksWalkedDWARF)
	assert.Equal(t, uint64(1), c.Stats().StacksWalkedFPOnly)
}

// n_pcs == maxWalkFrames used to be the only truncation signal, and it
// misses exactly the failure this phase exists to fix: a walk that dies at
// the first vendor-library frame produces two or three PCs, not 127, and
// used to read as a complete, untruncated stack - silently, on the workload
// this phase is about. WALKER_FLAG_FP_TERMINATED clear at a short length is
// what makes that failure visible instead.
func TestAWalkThatDiesEarlyIsCountedAbandonedNotSilent(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	// Two frames: the probe's own PC, then one vendor frame the FP chain
	// could not continue past. flags left at zero - no
	// WALKER_FLAG_FP_TERMINATED, no WALKER_FLAG_DWARF_USED - which is
	// exactly what a bpf_probe_read_user fault on that frame produces.
	stacks.put(21, 0x401000, 0x401100)

	frames, ok := c.resolveStackForTest(21, 4242)
	require.True(t, ok, "the frames captured before the walk died are still used")
	assert.Len(t, frames, 2)
	assert.Equal(t, uint64(1), c.Stats().StackWalkAbandoned)
	assert.Zero(t, c.Stats().StackWalkTruncated,
		"cut short of a natural end is a different failure than hitting MAX_FRAMES")
}

// A genuinely complete short walk - the FP chain reached its natural end -
// is neither truncated nor abandoned. Only a walk that stopped WITHOUT
// reaching that terminator counts against either bucket.
func TestACompleteShortWalkIsNeitherTruncatedNorAbandoned(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(23, 0x401000, 0x401100)
	stacks.flags[23] = walkerFlagFPTerminated

	_, ok := c.resolveStackForTest(23, 4242)
	require.True(t, ok)
	assert.Zero(t, c.Stats().StackWalkTruncated)
	assert.Zero(t, c.Stats().StackWalkAbandoned)
}

// The walkerFlag* constants are a hand-copied mirror of the WALKER_FLAG_*
// macros in bpf/unwind_common.h, and nothing in the build makes them agree.
// A wrong bit here does not fail to compile - it silently reclassifies every
// walk, which is exactly the class of defect this file is full of tests
// about. So read the header and check.
func TestWalkerFlagsMirrorTheBPFHeader(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_common.h")
	require.NoError(t, err)

	re := regexp.MustCompile(`(?m)^#define\s+(WALKER_FLAG_\w+)\s+(0x[0-9a-fA-F]+)`)
	got := map[string]uint32{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		v, err := strconv.ParseUint(m[2], 0, 32)
		require.NoError(t, err)
		got[m[1]] = uint32(v)
	}
	assert.Equal(t, map[string]uint32{
		"WALKER_FLAG_FP_TERMINATED":     walkerFlagFPTerminated,
		"WALKER_FLAG_DWARF_USED":        walkerFlagDWARFUsed,
		"WALKER_FLAG_CFI_MISS":          walkerFlagCFIMiss,
		"WALKER_FLAG_RA_UNDEFINED":      walkerFlagRAUndefined,
		"WALKER_FLAG_FP_EXHAUSTED":      walkerFlagFPExhausted,
		"WALKER_FLAG_FP_NONMONOTONIC":   walkerFlagFPNonMonotonic,
		"WALKER_FLAG_ROOT_DISAGREEMENT": walkerFlagRootDisagreement,
	}, got, "bpf/unwind_common.h and consumer.go disagree about the walker's flag bits")
}

// The defect this test exists for, measured on the Phase 4b gate:
// abandoned == 62 alongside dwarf == 62. EVERY successful DWARF walk was
// counted abandoned.
//
// A hybrid walk that crosses a frame-pointer-less frame cannot end via
// walkerFlagFPTerminated: the DWARF step that carried it across set the
// frame pointer to zero (fp_type UNDEFINED), so the walk ends at the next
// FP_SAFE frame with nothing to follow. walk_step says so with
// walkerFlagRAUndefined when the CFI itself marks the frame outermost, and
// that has to count as an end of chain - otherwise the counter fires on
// success and carries no information.
func TestASuccessfulDWARFWalkIsNotCountedAbandoned(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	// The gate's shape: probe frame, two frame-pointer-less bridges, main.
	stacks.put(31, 0x401000, 0x401100, 0x401200, 0x401300)
	stacks.flags[31] = walkerFlagDWARFUsed | walkerFlagRAUndefined

	frames, ok := c.resolveStackForTest(31, 4242)
	require.True(t, ok)
	assert.Len(t, frames, 4)
	assert.Equal(t, uint64(1), c.Stats().StacksWalkedDWARF)
	assert.Zero(t, c.Stats().StackWalkAbandoned,
		"the unwind information said the chain ends here; the walk succeeded")
	assert.Zero(t, c.Stats().StackWalkTruncated)
	assert.Equal(t, uint64(1), c.Stats().StackWalkReachedRoot,
		"the good outcome needs a counter of its own, not just the absence of a bad one")
	assert.Zero(t, c.Stats().StackWalkFPExhausted)
}

// The other half of the same fact: narrowing StackWalkAbandoned must not
// hollow it out. A DWARF walk that stopped WITHOUT either terminator - a
// read fault at a live address, a non-monotonic frame pointer, a return
// address in an untracked register - is still a walk that could not
// proceed, and must still be counted.
func TestADWARFWalkThatCouldNotProceedIsStillCountedAbandoned(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(33, 0x401000, 0x401100)
	stacks.flags[33] = walkerFlagDWARFUsed

	_, ok := c.resolveStackForTest(33, 4242)
	require.True(t, ok)
	assert.Equal(t, uint64(1), c.Stats().StacksWalkedDWARF)
	assert.Equal(t, uint64(1), c.Stats().StackWalkAbandoned,
		"neither terminator bit set and short of MAX_FRAMES is the failure case")
}

// walkerFlagRAUndefined has to suppress truncation for the same reason
// walkerFlagFPTerminated does: a stack that is exactly maxWalkFrames deep
// and ended at the chain's end is complete, not cut off at the budget.
func TestAFullLengthWalkTerminatedByUnwindInfoIsNotCountedTruncated(t *testing.T) {
	full := make([]uint64, maxWalkFrames)
	for i := range full {
		full[i] = uint64(0x401000 + i*16)
	}
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(35, full...)
	stacks.flags[35] = walkerFlagDWARFUsed | walkerFlagRAUndefined

	_, ok := c.resolveStackForTest(35, 4242)
	require.True(t, ok)
	assert.Zero(t, c.Stats().StackWalkTruncated)
	assert.Zero(t, c.Stats().StackWalkAbandoned)
}

// Issue #44. WALKER_FLAG_UNWIND_TERMINATED (0x08) used to be set on two
// unrelated conditions: a frame whose CFI marks it outermost, and a walk that
// arrived at an FP_SAFE frame with the frame pointer already zeroed by the
// DWARF step below it. The first is the walk succeeding; the second is the
// walk being cut off mid-stack with the caller of that frame still real and
// still on the stack. Sharing a bit made the second read as the first, so a
// truncation nothing else could see moved NO counter at all: not
// StackWalkAbandoned, not StackWalkTruncated, not StacksWalkedCFIMiss.
//
// walkerFlagFPExhausted (0x10) is that second condition, and it belongs with
// the failures. Against the pre-#44 walker this capture's flags would have
// been 0x08 and every assertion below would read the opposite way.
func TestAWalkThatRanOutOfFramePointerIsCountedAbandoned(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	// The measured shape: probe frame, two FP-less bridges, then main -
	// where the walk stops because unwinding the last bridge produced no
	// %rbp. __libc_start_call_main and _start are missing and nothing in
	// the capture says so except this flag.
	stacks.put(41, 0x401000, 0x401100, 0x401200, 0x401300)
	stacks.flags[41] = walkerFlagDWARFUsed | walkerFlagFPExhausted

	frames, ok := c.resolveStackForTest(41, 4242)
	require.True(t, ok)
	assert.Len(t, frames, 4, "the frames it did get are real and are still attributed")
	assert.Equal(t, uint64(1), c.Stats().StackWalkFPExhausted,
		"the lost frame pointer is the named cause and needs to be visible")
	assert.Equal(t, uint64(1), c.Stats().StackWalkAbandoned,
		"a walk stopped by a lost frame pointer did not reach the root; it is a failure")
	assert.Zero(t, c.Stats().StackWalkReachedRoot,
		"nothing in the unwind information said this frame was outermost")
	assert.Zero(t, c.Stats().StackWalkTruncated,
		"it did not run out of budget; it ran out of frame pointer")
}

// The two outcomes must stay apart in BOTH directions. A walk that ended
// because the CFI marked the frame outermost is not an FP exhaustion, and
// must not inflate the counter that measures issue #45.
func TestReachedRootAndFPExhaustedAreDisjoint(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(43, 0x401000, 0x401100)
	stacks.flags[43] = walkerFlagDWARFUsed | walkerFlagRAUndefined
	_, ok := c.resolveStackForTest(43, 4242)
	require.True(t, ok)

	stacks.put(44, 0x402000, 0x402100)
	stacks.flags[44] = walkerFlagDWARFUsed | walkerFlagFPExhausted
	_, ok = c.resolveStackForTest(44, 4242)
	require.True(t, ok)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StackWalkReachedRoot)
	assert.Equal(t, uint64(1), st.StackWalkFPExhausted)
	assert.Equal(t, uint64(1), st.StackWalkAbandoned,
		"exactly the FP-exhausted one; the one that reached the root is not a failure")
	assert.Zero(t, st.StackWalkTruncated)
}

// The shape issue #45 produces on a real main-thread stack: the walk runs
// off the end of the frame-pointer chain (walkerFlagFPTerminated) AND the
// unwind tables of the frame it steps onto declare it outermost
// (walkerFlagRAUndefined). Both bits, one walk, one root.
//
// Before #45 walk_step stopped at the zero saved frame pointer and the second
// bit could not be set on such a walk at all, so the pair was impossible and
// the classification switch documented the two as mutually exclusive. They
// are not, and the walk must still be counted exactly once - as a success.
func TestAWalkMayReachTheRootByBothTheFPChainAndTheCFI(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	// probe frame, two FP-less bridges, main, two libc frames, _start.
	stacks.put(47, 0x401000, 0x401100, 0x401200, 0x401300, 0x7f0000, 0x7f0100, 0x400875)
	stacks.flags[47] = walkerFlagDWARFUsed | walkerFlagFPTerminated | walkerFlagRAUndefined

	frames, ok := c.resolveStackForTest(47, 4242)
	require.True(t, ok)
	assert.Len(t, frames, 7)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StackWalkReachedRoot,
		"the CFI said this frame was outermost; that is the good outcome and it must be counted")
	assert.Zero(t, st.StackWalkAbandoned,
		"a walk that reached the root by BOTH routes is not abandoned")
	assert.Zero(t, st.StackWalkFPExhausted,
		"the frame pointer ran out at the root, which is not an exhaustion")
	assert.Zero(t, st.StackWalkTruncated)
}

// The step past the frame-pointer root that issue #45 added can land on a
// frame whose CFI says a caller EXISTS. The two sources then disagree about
// where the stack ends, and walkerFlagFPTerminated is ALREADY set from the
// frame below - so without a bit of its own that walk is indistinguishable
// from a clean termination and no counter moves.
//
// That is the exact defect issue #44 exists to remove, recreated by #45's own
// fix, which is why the classification switch tests this bit BEFORE
// walkerFlagsTerminated.
func TestAWalkWhoseRootTheCFIContradictsIsNotCountedASuccess(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(50, 0x401000, 0x401100, 0x401200)
	// Exactly the shape walk_step produces: FP_TERMINATED from the frame
	// below, then the disagreement on the step past it.
	stacks.flags[50] = walkerFlagDWARFUsed | walkerFlagFPTerminated | walkerFlagRootDisagreement

	_, ok := c.resolveStackForTest(50, 4242)
	require.True(t, ok)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StackWalkRootDisagreement)
	assert.Equal(t, uint64(1), st.StackWalkAbandoned,
		"an ending the frame-pointer chain and the unwind tables disagree about must not be filed as a success just because FP_TERMINATED is set")
	assert.Zero(t, st.StackWalkReachedRoot,
		"nothing declared this frame outermost; the CFI said the opposite")
	assert.Zero(t, st.StackWalkTruncated)
}

// A frame pointer that does not increase used to be an unflagged bare
// `return 1` in walk_step - indistinguishable, from the outside, from a
// bpf_probe_read_user fault. It now has a bit, and that bit is a failure:
// counted in StackWalkAbandoned like walkerFlagFPExhausted, with its own
// named-cause counter beside it.
func TestAWalkStoppedByANonMonotonicFramePointerIsCountedAbandoned(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(48, 0x401000, 0x401100, 0x401200)
	stacks.flags[48] = walkerFlagDWARFUsed | walkerFlagFPNonMonotonic

	frames, ok := c.resolveStackForTest(48, 4242)
	require.True(t, ok)
	assert.Len(t, frames, 3,
		"the last frame is the return address walk_step now records before stopping")
	st := c.Stats()
	assert.Equal(t, uint64(1), st.StackWalkFPNonMonotonic)
	assert.Equal(t, uint64(1), st.StackWalkAbandoned,
		"a corrupt frame link is a failure to continue, not an end of chain")
	assert.Zero(t, st.StackWalkFPExhausted,
		"the two failures have different causes and must not share a counter")
	assert.Zero(t, st.StackWalkReachedRoot)
	assert.Zero(t, st.StackWalkTruncated)
}

// Same reasoning as the FP-exhausted case: a named failure on the last
// permitted frame is abandonment, not "ran out of room".
func TestAFullLengthNonMonotonicWalkIsAbandonedNotTruncated(t *testing.T) {
	full := make([]uint64, maxWalkFrames)
	for i := range full {
		full[i] = uint64(0x401000 + i*16)
	}
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(49, full...)
	stacks.flags[49] = walkerFlagDWARFUsed | walkerFlagFPNonMonotonic

	_, ok := c.resolveStackForTest(49, 4242)
	require.True(t, ok)
	assert.Equal(t, uint64(1), c.Stats().StackWalkAbandoned)
	assert.Equal(t, uint64(1), c.Stats().StackWalkFPNonMonotonic)
	assert.Zero(t, c.Stats().StackWalkTruncated)
}

// A walk whose frame pointer ran out on its LAST permitted frame stopped for
// a reason it named, not because bpf_loop ran out of iterations. Filing it
// under StackWalkTruncated would hide a known failure inside "ran out of
// room", which is the same kind of mislabelling issue #44 is about.
func TestAFullLengthWalkThatRanOutOfFramePointerIsAbandonedNotTruncated(t *testing.T) {
	full := make([]uint64, maxWalkFrames)
	for i := range full {
		full[i] = uint64(0x401000 + i*16)
	}
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(45, full...)
	stacks.flags[45] = walkerFlagDWARFUsed | walkerFlagFPExhausted

	_, ok := c.resolveStackForTest(45, 4242)
	require.True(t, ok)
	assert.Equal(t, uint64(1), c.Stats().StackWalkAbandoned)
	assert.Equal(t, uint64(1), c.Stats().StackWalkFPExhausted)
	assert.Zero(t, c.Stats().StackWalkTruncated,
		"the budget was incidental; the walk had already lost the frame pointer")
}

// The two arms of the old shared bit must be set by DIFFERENT bits in the
// walker source itself, not merely be different constants in this file. Both
// arms live in walk_step in bpf/unwind_common.h; if a future edit points them
// at the same macro again the Go side would keep classifying happily and the
// counter would go quiet exactly as it did before #44.
func TestTheTwoTerminationArmsSetDifferentFlagsInWalkStep(t *testing.T) {
	src, err := os.ReadFile("../bpf/unwind_common.h")
	require.NoError(t, err)
	body := string(src)
	step := body[strings.Index(body, "static long walk_step("):]
	require.NotEmpty(t, step, "walk_step not found in the shared header")

	// The RA-undefined arm: inside the DWARF path, guarded by the CFI's own
	// ra_type. The FP-exhausted arm: inside the FP path, guarded by ctx->fp.
	raArm := strings.Index(step, "e.ra_type == RA_TYPE_UNDEFINED")
	fpArm := strings.Index(step, "if (ctx->fp == 0)")
	require.Positive(t, raArm, "the ra_type == UNDEFINED arm is gone")
	require.Positive(t, fpArm, "the ctx->fp == 0 arm is gone")
	require.Less(t, raArm, fpArm, "arms found out of order; the slices below would be wrong")

	flagIn := func(region string) string {
		i := strings.Index(region, "walker_flags |= WALKER_FLAG_")
		require.Positive(t, i, "no walker_flags assignment in this arm")
		rest := region[i+len("walker_flags |= "):]
		return rest[:strings.IndexAny(rest, ";")]
	}
	raFlag := flagIn(step[raArm:fpArm])
	fpFlag := flagIn(step[fpArm:])

	assert.Equal(t, "WALKER_FLAG_RA_UNDEFINED", raFlag,
		"the CFI-says-outermost arm must set the success flag")
	assert.Equal(t, "WALKER_FLAG_FP_EXHAUSTED", fpFlag,
		"the lost-frame-pointer arm must set the failure flag, not the success one")
	assert.NotEqual(t, raFlag, fpFlag,
		"one bit for two unrelated outcomes is the defect in issue #44")
}

// A stack that is genuinely maxWalkFrames deep AND reached a natural
// terminator on its last frame is a complete stack, not a truncated one:
// n_pcs == maxWalkFrames alone cannot tell the two apart, and
// walkerFlagFPTerminated is what does.
func TestAFullLengthWalkThatTerminatedNaturallyIsNotCountedTruncated(t *testing.T) {
	full := make([]uint64, maxWalkFrames)
	for i := range full {
		full[i] = uint64(0x401000 + i*16)
	}
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.put(25, full...)
	stacks.flags[25] = walkerFlagFPTerminated

	frames, ok := c.resolveStackForTest(25, 4242)
	require.True(t, ok)
	assert.Len(t, frames, maxWalkFrames)
	assert.Zero(t, c.Stats().StackWalkTruncated,
		"maxWalkFrames deep and naturally terminated is complete, not truncated")
	assert.Zero(t, c.Stats().StackWalkAbandoned)
}

// A value shorter than one struct gpu_stack cannot be decoded into a call
// path, and guessing at a partial one would be a fabrication.
func TestAShortStackValueIsRefused(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	stacks.raw = map[uint32][]byte{15: make([]byte, gpuStackHdrSize+8*4)}

	_, ok := c.resolveStackForTest(15, 4242)
	assert.False(t, ok)
	assert.Equal(t, uint64(1), c.Stats().StackLookupFailed)
}

// n_pcs is read off the wire, so a value claiming more frames than the map
// can hold must be clamped rather than trusted into an out-of-range slice.
func TestAnOverlongNPcsIsClampedNotTrusted(t *testing.T) {
	c, stacks, _ := stackConsumer(t, &recordingSink{}, Config{})
	raw := encodeGPUStack([]uint64{0x401000}, 0)
	putU32(raw[0:], maxWalkFrames+9)
	stacks.raw = map[uint32][]byte{17: raw}

	frames, ok := c.resolveStackForTest(17, 4242)
	require.True(t, ok)
	assert.Len(t, frames, maxWalkFrames, "clamped to what the value can hold")
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

// The other arrival order. No shim in this repository produces it any more -
// they fire the sampled probe before the batched add(), see issue #67 and
// sampledstacks.go - but the ABI is public and a foreign or older producer
// may still emit batched-first, so the consumer must join it. The launch
// waits for the twin, then goes out once, with its stack.
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

// Correlation ids are per-process: CUPTI restarts them from a low value in
// every process it is loaded into, and the probes fire in EVERY process that
// maps the shim (uprobe_multi attaches to the file, and Config.PID == 0 is a
// supported mode). So two profiled processes collide on correlation almost
// immediately, and a side table keyed on correlation alone would hand
// process A's stack - symbolized against /proc/A/maps - to process B's
// launch. That is not a lost stack, it is a fabricated call path under
// measured GPU time, which is the exact failure this whole phase exists to
// prevent.
//
// This is the pendingStacks half: both stacks arrive first and park.
func TestSameCorrelationInTwoProcessesKeepsItsOwnStack(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000) // the stack captured in pid 4242
	sm.put(2, 0x2000) // the stack captured in pid 5353

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, sampledBatchWith(5353, 7, 2, 8))
	require.Equal(t, 2, c.Stats().PendingStacks,
		"two processes, two stacks: one must not overwrite the other")

	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, launchBatchWith(5353, 7))
	c.Flush()

	require.Len(t, sink.launches, 2)
	byPID := map[uint32][]string{}
	for _, l := range sink.launches {
		byPID[l.Launch.PID] = frameNames(l.Launch.CPUStack)
	}
	assert.Equal(t, []string{"fn_1000"}, byPID[4242],
		"pid 4242 must get the stack captured in pid 4242")
	assert.Equal(t, []string{"fn_2000"}, byPID[5353],
		"pid 5353 must get the stack captured in pid 5353")
	assert.Equal(t, uint64(2), c.Stats().StacksAttached)
	assert.Zero(t, c.Stats().StacksEvicted,
		"neither capture may be treated as a replacement of the other")
}

// The deferredLaunches half of the same collision: a launch held for pid
// 4242 must not be released by a stack captured in pid 5353 that happens to
// carry the same correlation.
func TestHeldLaunchDoesNotTakeAnotherProcessesStack(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000)
	sm.put(2, 0x2000)

	apply(t, c, launchBatchWith(4242, 7))
	require.Equal(t, 1, c.Stats().PendingLaunches)

	// Pre-fix this took the held pid-4242 launch and attached pid 5353's
	// stack to it.
	apply(t, c, sampledBatchWith(5353, 7, 2, 8))
	assert.Equal(t, 1, c.Stats().PendingLaunches,
		"another process's stack must not release this process's launch")
	assert.Equal(t, 1, c.Stats().PendingStacks, "it parks for its own launch instead")

	// pid 5353's launch releases the held pid-4242 one (any non-sampled
	// batch does) and then collects its own parked stack.
	apply(t, c, launchBatchWith(5353, 7))
	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	c.Flush()

	require.Len(t, sink.launches, 2)
	byPID := map[uint32][]string{}
	for _, l := range sink.launches {
		byPID[l.Launch.PID] = frameNames(l.Launch.CPUStack)
	}
	assert.Equal(t, []string{"fn_2000"}, byPID[5353])
	assert.Empty(t, byPID[4242],
		"the pid-4242 launch was released before its own stack arrived: no stack is correct, a borrowed one is not")
}

// What a batched-first producer costs, and the reason no shim here is one.
//
// Three records land in this order: launch batch, exec batch, sampled
// record. The launch is held for its twin; the exec batch releases it
// stackless (which is correct - the timeline wants launches promptly); the
// twin then arrives with nowhere to go and parks forever.
//
// This WAS the shipped stub. It added to the launch batch, then to the exec
// batch, then fired the unbatched sampled probe, so a launch that both
// FILLED the batch and was sampled produced exactly this sequence: 58
// sampled, 57 attached, 1 parked, on the privileged gate (issue #67). Issue
// #50's jittered stride is what made a sampled ordinal able to land on a
// batch boundary at all - at the old fixed stride of 8 the collision was
// arithmetically impossible against 32-record batches.
//
// The fix is in the producer: both shims now fire the sampled probe BEFORE
// the batched add(), which makes sampled-first unconditional, and
// shim/stub/probe_order_test.cc pins that order without any privilege. This
// test therefore no longer describes our producers. It stays because the
// ABI is public and a foreign or older producer may still emit in this
// order, and because what the consumer does then must be a documented,
// counted outcome rather than a surprise: the launch ships, the execution
// ships, the GPU time is measured and projects as unattributed, and the
// stack is counted in PendingStacks rather than vanishing.
//
// It is not fixable on this side without giving something up. Holding the
// launch past the exec batch delays every launch systematically to buy back
// a rare attribution, and attaching the stack after the fact means
// re-emitting a launch the sink has already been given.
func TestStackParksUnattachedWhenAnotherBatchSplitsTheTwins(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000)

	// Launch 7 is the record that filled the batch, so the batch is queued
	// before its own sampled record.
	apply(t, c, launchBatchWith(4242, 5, 6, 7))
	require.Equal(t, 3, c.Stats().PendingLaunches)

	// The exec batch the stub queues in the same iteration lands next.
	execs := make([]byte, batchHdrSize+gpuabi.SizeExec)
	putU32(execs[0:], kindExec)
	putU32(execs[4:], 1)
	putU32(execs[16:], 4242)
	putU64(execs[24:], gpuabi.SizeExec)
	putU64(execs[batchHdrSize:], 7)
	apply(t, c, execs)
	require.Equal(t, 3, sink.launchCount(), "the exec batch releases the held launches, as it must")

	// The twin arrives to find its launch already gone.
	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	c.Flush()

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksResolved)
	assert.Zero(t, st.StacksAttached)
	assert.Equal(t, 1, st.PendingStacks,
		"the orphaned stack is counted, not silently dropped - this is what PendingStacks:24 was")
	require.Len(t, sink.launches, 3)
	assert.Empty(t, sink.launches[2].Launch.CPUStack,
		"launch 7 still ships; it just ships without a call path")
	assert.Zero(t, st.StacksEvicted, "nothing was pushed out; it is still parked")
}

// The same batch boundary, in the order the fixed producers actually emit
// (issue #67): the sampled record for the launch that fills the batch goes
// out BEFORE the batched add(), so it reaches the ringbuf ahead of the batch
// that carries its twin.
//
// Correlation 7 is the record that fills the launch batch here, and the exec
// batch of that same producer loop iteration follows immediately - the batch
// that released the launch stackless in the test above. The stack is parked
// rather than held, so it is not the deferred queue's to release, and the
// join survives the exec batch untouched. That is the whole difference the
// reorder buys, stated in the consumer's own vocabulary rather than the
// producer's; shim/stub/probe_order_test.cc is what pins the producer to
// this order.
func TestStackSurvivesASplittingBatchWhenTheSampledRecordLeads(t *testing.T) {
	sink := &recordingSink{}
	c, sm, _ := stackConsumer(t, sink, Config{})
	sm.put(1, 0x1000)

	// Sampled first: the producer fires this probe before the add() that
	// flushes the batch below.
	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	require.Equal(t, 1, c.Stats().PendingStacks, "the stack waits for its twin")

	// The batch the twin fills, then the exec batch of the same iteration.
	apply(t, c, launchBatchWith(4242, 5, 6, 7))
	execs := make([]byte, batchHdrSize+gpuabi.SizeExec)
	putU32(execs[0:], kindExec)
	putU32(execs[4:], 1)
	putU32(execs[16:], 4242)
	putU64(execs[24:], gpuabi.SizeExec)
	putU64(execs[batchHdrSize:], 7)
	apply(t, c, execs)
	c.Flush()

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksResolved)
	assert.Equal(t, uint64(1), st.StacksAttached,
		"the launch that filled the batch must still carry its stack")
	assert.Zero(t, st.PendingStacks,
		"nothing may be left parked: the exec batch cannot take a stack, only a held launch")
	assert.Zero(t, st.StacksEvicted)
	require.Len(t, sink.launches, 3)
	// By correlation, not by position: a launch that collects a parked stack
	// is emitted on the spot while its batch-mates are still held, so
	// correlation 7 overtakes 5 and 6 here. admitLaunchLocked documents that
	// reordering and bounds it to one batch.
	byCorr := map[string][]string{}
	for _, l := range sink.launches {
		byCorr[l.Correlation.Value] = frameNames(l.Launch.CPUStack)
	}
	assert.Equal(t, []string{"fn_1000"}, byCorr["7"],
		"correlation 7 is the sampled one and must carry its own stack")
	assert.Empty(t, byCorr["5"], "an unsampled batch-mate must stay stackless")
	assert.Empty(t, byCorr["6"], "an unsampled batch-mate must stay stackless")
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
	// A lookup that fails for any reason OTHER than key-not-exist leaves an
	// entry sitting in the map that nothing else will ever remove. The
	// program calls bpf_get_stackid without BPF_F_REUSE_STACKID, so that
	// bucket then answers -EEXIST for every later capture hashing to it,
	// and capture degrades into a permanent StacksMissing stream that reads
	// like a broken probe rather than a leaked slot.
	t.Run("lookup failed but the entry is still there", func(t *testing.T) {
		c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
		sm.put(4, 0x1000)
		sm.lookupErr = errors.New("bad file descriptor")
		apply(t, c, sampledBatchWith(4242, 7, 4, 8))
		assert.Equal(t, uint64(1), c.Stats().StackLookupFailed)
		assert.True(t, sm.wasDeleted(4), "a slot we cannot read is still a slot we must free")
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
	sm := newFakeStackStore()
	sm.put(1, 0x1000)
	c.stacks = sm

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	require.Len(t, sink.launches, 1)
	assert.Empty(t, sink.launches[0].Launch.CPUStack)
	assert.Equal(t, uint64(1), c.Stats().SymbolizeFailed)
}

// Symbolization that returns a frame per IP and resolves not one name is
// not a success. It is not a SymbolizeFailed either - the stack is
// delivered - so it needs a counter of its own, or it reads as a healthy
// run. This is the exact failure the Phase 4a gate hit: 63 stacks, 0
// SymbolizeFailed, and every frame named after its own address.
func TestSymbolizationThatResolvesNoNameIsCountedNotSilent(t *testing.T) {
	sink := &recordingSink{}
	sym := &fakeSymbolizer{unresolved: func(uint64) bool { return true }}
	c, sm, _ := stackConsumer(t, sink, Config{Symbolizer: sym})
	sm.put(1, 0x1000, 0x2000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StacksUnresolved,
		"a capture in which no frame resolved must be counted, not reported as a clean run")
	assert.Equal(t, uint64(2), st.StackFramesUnresolved, "both frames came back address-only")
	assert.Zero(t, st.SymbolizeFailed,
		"the symbolizer returned frames and no error; this is a resolution failure, not a symbolizer failure")
	// The launch and its (unreadable) stack are still delivered: an
	// address-only stack is worse than a named one and better than none,
	// and operators can still decode it with addr2line.
	assert.Equal(t, uint64(1), st.StacksResolved)
	require.Len(t, sink.launches, 1)
	assert.Equal(t, []string{"0x2000", "0x1000"}, frameNames(sink.launches[0].Launch.CPUStack))
}

// One unresolvable frame in an otherwise readable stack is a normal event -
// a stripped vendor library, a vDSO frame - and must not be reported as an
// unresolved stack, or the counter cries wolf on every real profile.
func TestPartiallyResolvedStackIsNotAnUnresolvedStack(t *testing.T) {
	sink := &recordingSink{}
	sym := &fakeSymbolizer{unresolved: func(ip uint64) bool { return ip == 0x2000 }}
	c, sm, _ := stackConsumer(t, sink, Config{Symbolizer: sym})
	sm.put(1, 0x1000, 0x2000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	st := c.Stats()
	assert.Zero(t, st.StacksUnresolved, "one frame resolved, so the stack is readable")
	assert.Equal(t, uint64(1), st.StackFramesUnresolved, "the one address-only frame is still counted")
}

// A fully resolved stack must leave both counters alone, or "zero" stops
// meaning anything.
func TestFullyResolvedStackCountsNoUnresolvedFrames(t *testing.T) {
	c, sm, _ := stackConsumer(t, &recordingSink{}, Config{})
	sm.put(1, 0x1000, 0x2000)

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, launchBatchWith(4242, 7))
	c.Flush()

	st := c.Stats()
	assert.Zero(t, st.StacksUnresolved)
	assert.Zero(t, st.StackFramesUnresolved)
	assert.Equal(t, uint64(1), st.StacksResolved)
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
	// A real shim path, and a symbolizer that places frames in modules, so
	// the StacksProfilerOnly term is actually exercised: with an empty
	// ShimPath the guard is off and that term is zero by construction, which
	// would make the identity below pass without ever having been tested
	// against a refusal.
	shim := mappedLibrary(t)
	sink := &recordingSink{}
	sym := &moduleSymbolizer{modules: map[uint64]string{
		0x1000: "/app/train", 0x2000: "/app/train", 0x3000: "/app/train",
		0x4000: shim, // never left the profiler: refused
	}}
	c, sm, _ := stackConsumer(t, sink, Config{SampledStackCapacity: 1, ShimPath: shim, Symbolizer: sym})

	sm.put(1, 0x1000) // resolves, then parks
	sm.put(2, 0x2000) // resolves, parks, evicting the first
	sm.put(3, 0x3000) // resolves and attaches to a held launch
	sm.put(4, 0x4000) // resolves, then is refused as profiler-only
	apply(t, c, sampledBatchWith(4242, 1, 1, 8))
	apply(t, c, sampledBatchWith(4242, 2, 2, 8))
	apply(t, c, launchBatchWith(4242, 3))
	apply(t, c, sampledBatchWith(4242, 3, 3, 8))
	apply(t, c, sampledBatchWith(4242, 8, 4, 8))
	apply(t, c, sampledBatchWith(4242, 4, -1, 8))  // capture failed
	apply(t, c, sampledBatchWith(4242, 0, 5, 8))   // no correlation
	apply(t, c, sampledBatchWith(4242, 6, 404, 8)) // no such entry
	c.Flush()

	st := c.Stats()
	assert.Equal(t, uint64(7), st.SampledLaunches)
	// Every term of the second identity is non-zero here, which is the point
	// of the arrangement above: one attached, one evicted, one refused, one
	// still parked.
	assert.Equal(t, uint64(1), st.StacksAttached)
	assert.Equal(t, uint64(1), st.StacksEvicted)
	assert.Equal(t, uint64(1), st.StacksProfilerOnly)
	assert.Equal(t, 1, st.PendingStacks)
	assert.Equal(t, st.SampledLaunches,
		st.StacksMissing+st.StacksUncorrelated+st.StackLookupFailed+st.SymbolizeFailed+st.StacksResolved,
		"every sampled launch must land in exactly one bucket")
	assert.Equal(t, st.StacksResolved,
		st.StacksAttached+st.StacksEvicted+st.StacksProfilerOnly+uint64(st.PendingStacks),
		"every resolved stack is attached, evicted, refused as profiler-only, or still waiting - nothing else")
	assert.LessOrEqual(t, st.StacksProfilerOnlyUncertain, st.StacksProfilerOnly,
		"the uncertain count is a subset of the refusals, never a bucket of its own")
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

// --- Phase 4a: interned kernel names -------------------------------------

// kernelNameBatch builds a gpu_kernel_name_v1 batch for one kernel.
func kernelNameBatch(id uint64, name string, truncated bool) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeKernelName)
	putU32(buf[0:], kindKernelName)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeKernelName))
	putU32(buf[32:], ^uint32(0)) // stack_id = -1 on every non-sampled kind
	rec := buf[batchHdrSize:]
	putU64(rec[0:], id)
	binary.LittleEndian.PutUint16(rec[8:], uint16(len(name)))
	if truncated {
		rec[10] = 1
	}
	copy(rec[16:], name)
	return buf
}

// launchBatchKernels is launchBatchWith with a kernel id on every record.
func launchBatchKernels(pid uint32, kernelID uint64, corrs ...uint64) []byte {
	buf := launchBatchWith(pid, corrs...)
	for i := range corrs {
		putU64(buf[batchHdrSize+i*gpuabi.SizeLaunch+8:], kernelID)
	}
	return buf
}

// execBatchKernels builds an exec batch carrying one kernel id.
func execBatchKernels(kernelID uint64, corrs ...uint64) []byte {
	buf := make([]byte, batchHdrSize+len(corrs)*gpuabi.SizeExec)
	putU32(buf[0:], kindExec)
	putU32(buf[4:], uint32(len(corrs)))
	putU64(buf[24:], uint64(len(corrs)*gpuabi.SizeExec))
	putU32(buf[32:], ^uint32(0))
	for i, corr := range corrs {
		rec := buf[batchHdrSize+i*gpuabi.SizeExec:]
		putU64(rec[0:], corr)
		putU64(rec[8:], kernelID)
		putU64(rec[32:], uint64(10+i))
		putU64(rec[40:], uint64(60+i))
	}
	return buf
}

// The ordinary order: the producer interns a name the first time it sees a
// kernel, before the launch that used it is batched out. Both the launch
// and the execution must carry it - the projection's [gpu:kernel:<name>]
// frame is built from the *execution's* name, and the timeline's heuristic
// join matches launches on theirs.
func TestKernelNamesReachLaunchesAndExecutions(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})

	apply(t, c, kernelNameBatch(0xAAAA, "kAdd", false))
	apply(t, c, launchBatchKernels(4242, 0xAAAA, 7))
	apply(t, c, execBatchKernels(0xAAAA, 7))
	c.Flush()

	require.Len(t, sink.launches, 1)
	require.Len(t, sink.execs, 1)
	assert.Equal(t, "kAdd", sink.launches[0].KernelName)
	assert.Equal(t, "kAdd", sink.execs[0].KernelName)
	st := c.Stats()
	assert.Equal(t, uint64(1), st.KernelNamesLearned)
	assert.Zero(t, st.KernelNamesUnresolved)
	assert.Zero(t, st.Undecoded, "a kernel name is a normalized record now")
}

// Names are not guaranteed to precede their use: the producer only emits a
// name at intern time if the name probe was armed then, and otherwise
// replays its table on the next drain tick - up to a full interval after the
// launches that referenced it. Those events wait, exactly as a launch waits
// for a late sampled stack, and are named when the record lands.
func TestKernelNamesArrivingLateStillNameTheirEvents(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})

	// One name for an unrelated kernel: this producer demonstrably names
	// its kernels, so an event for an unknown one is worth waiting for.
	apply(t, c, kernelNameBatch(0x1111, "kOther", false))

	// The exec batch is also what releases the launch from the sampled-stack
	// wait, so both events are past that stage and into the name wait. Flush
	// is deliberately not used here: Flush ends every wait, which is the
	// case TestFlushAndCloseReleaseEventsWaitingForAName covers.
	apply(t, c, launchBatchKernels(4242, 0xBBBB, 7))
	apply(t, c, execBatchKernels(0xBBBB, 7))
	assert.Zero(t, sink.launchCount(), "an event whose kernel is unnamed waits for the name")
	assert.Equal(t, 2, c.Stats().PendingNamedEvents)

	apply(t, c, kernelNameBatch(0xBBBB, "kLate", false))

	require.Len(t, sink.launches, 1)
	require.Len(t, sink.execs, 1)
	assert.Equal(t, "kLate", sink.launches[0].KernelName)
	assert.Equal(t, "kLate", sink.execs[0].KernelName)
	assert.Zero(t, c.Stats().KernelNamesUnresolved, "nothing was released unnamed")
	assert.Zero(t, c.Stats().PendingNamedEvents)
}

// A producer that never emits names must not have every one of its events
// held to the queue's bound waiting for something that is not coming.
func TestEventsAreNotHeldWhenTheProducerNeverNamesAnything(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})

	apply(t, c, execBatchKernels(0xCCCC, 7))
	assert.Equal(t, 1, len(sink.execs), "with no names in play, nothing waits")
	assert.Zero(t, c.Stats().PendingNamedEvents)
	assert.Equal(t, uint64(1), c.Stats().KernelNamesUnresolved,
		"an unnamed execution must be countable, not a mystery")
}

// A kernel id of zero is the ABI's "no kernel": it can never resolve, so it
// must never wait either.
func TestKernelIDZeroNeverWaitsForAName(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})
	apply(t, c, kernelNameBatch(0x1111, "kOther", false))

	apply(t, c, execBatchKernels(0, 7))
	assert.Equal(t, 1, len(sink.execs))
	assert.Zero(t, c.Stats().PendingNamedEvents)
	assert.Equal(t, uint64(1), c.Stats().KernelNamesUnresolved)
}

// Mangled C++ names routinely exceed the ABI's 256-byte inline limit, and
// two distinct kernels can share their first 256 bytes. A truncated name
// presented as complete is therefore a claim about which kernel ran that
// the data does not support: the marker travels with the name into the
// frame, and the count makes the aggregate visible.
func TestTruncatedKernelNameIsMarkedNotPresentedAsComplete(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})

	apply(t, c, kernelNameBatch(0xAAAA, "_Z4kAddPfiLongMangled", true))
	apply(t, c, execBatchKernels(0xAAAA, 7))

	require.Len(t, sink.execs, 1)
	assert.Equal(t, "_Z4kAddPfiLongMangled"+truncatedNameSuffix, sink.execs[0].KernelName)
	assert.Equal(t, uint64(1), c.Stats().KernelNamesTruncated)
}

// "One entry per distinct kernel" is an assumption about the workload, not
// a guarantee, so the table is bounded - and what it drops is counted,
// because every later event for that kernel goes out unnamed.
func TestKernelNameTableIsBoundedAndEvictionsCounted(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{KernelNameCapacity: 2})

	apply(t, c, kernelNameBatch(1, "kOne", false))
	apply(t, c, kernelNameBatch(2, "kTwo", false))
	apply(t, c, kernelNameBatch(3, "kThree", false))

	st := c.Stats()
	assert.Equal(t, 2, st.KnownKernelNames)
	assert.Equal(t, uint64(1), st.KernelNamesEvicted, "an evicted name is counted, never silent")

	// Kernel 1 was the oldest: its executions still flow, unnamed.
	apply(t, c, execBatchKernels(1, 7))
	c.Flush()
	require.Len(t, sink.execs, 1)
	assert.Empty(t, sink.execs[0].KernelName)
	assert.Equal(t, uint64(1), c.Stats().KernelNamesUnresolved)
}

// The producer replays its whole name table on late attach, so the same
// kernel is interned more than once. A replay must not evict anything or
// duplicate the entry.
func TestReplayedKernelNameReplacesInPlace(t *testing.T) {
	c, _, _ := stackConsumer(t, &recordingSink{}, Config{KernelNameCapacity: 2})
	for range 50 {
		apply(t, c, kernelNameBatch(1, "kOne", false))
	}
	st := c.Stats()
	assert.Equal(t, 1, st.KnownKernelNames)
	assert.Equal(t, uint64(50), st.KernelNamesLearned)
	assert.Zero(t, st.KernelNamesEvicted, "re-interning the same kernel must not push anything out")
}

// Holding events for a name is bounded, and as with held launches the bound
// must never become loss: overflow releases the oldest event unnamed rather
// than dropping it.
func TestHeldUnnamedEventsAreBoundedAndNeverDropped(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{PendingNamedEventCapacity: 2})
	apply(t, c, kernelNameBatch(0x1111, "kOther", false))

	apply(t, c, execBatchKernels(0xBBBB, 1, 2, 3, 4))
	assert.Equal(t, 2, len(sink.execs), "the two oldest are released to make room, not discarded")
	assert.Equal(t, uint64(2), c.Stats().KernelNamesUnresolved)

	apply(t, c, kernelNameBatch(0xBBBB, "kLate", false))
	require.Len(t, sink.execs, 4, "every execution reaches the sink exactly once")
	assert.Empty(t, sink.execs[0].KernelName, "the ones pushed out went unnamed")
	assert.Equal(t, "kLate", sink.execs[3].KernelName)
	for i, e := range sink.execs {
		assert.Equal(t, strconv.Itoa(i+1), e.Correlation.Value, "arrival order is preserved")
	}
}

// Flush and Close are the end of the wait: an event still holding out for a
// name goes on unnamed rather than being dropped at teardown.
func TestFlushAndCloseReleaseEventsWaitingForAName(t *testing.T) {
	sink := &recordingSink{}
	c, _, _ := stackConsumer(t, sink, Config{})
	c.reader = newScriptedReader(0)
	apply(t, c, kernelNameBatch(0x1111, "kOther", false))

	apply(t, c, launchBatchKernels(4242, 0xBBBB, 7))
	apply(t, c, execBatchKernels(0xBBBB, 7))
	require.Zero(t, sink.launchCount())

	c.Flush()
	assert.Equal(t, 1, sink.launchCount(), "a launch held for a name is released by Flush, not lost")
	assert.Equal(t, 1, len(sink.execs))
	assert.Empty(t, sink.launches[0].KernelName)
	assert.Equal(t, uint64(2), c.Stats().KernelNamesUnresolved)

	apply(t, c, execBatchKernels(0xBBBB, 8))
	require.NoError(t, c.Close())
	assert.Equal(t, 2, len(sink.execs), "an event held at teardown would be silent record loss")
}

// --- Why a sample was malformed --------------------------------------------

// Malformed on its own is a symptom nobody can act on. The reason table is
// what turns "63 samples were malformed" into "63 samples carried
// sample_period 0", which is the difference between a debuggable profiler and
// an opaque one.
func TestDecodeFailureReasonsAreRecorded(t *testing.T) {
	reader := newScriptedReader(4)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	// A sampled batch whose record carries sample_period == 0: the exact wire
	// shape a producer whose field stores were optimized away produces.
	reader.recs <- sampledBatch(1, 0)
	reader.recs <- sampledBatch(1, 0)
	// ...and one that fails for a different reason, so the table has to keep
	// the two apart rather than collapsing them into a count.
	reader.recs <- make([]byte, batchHdrSize-1)

	done := make(chan error, 1)
	go func() { done <- c.Run(context.Background()) }()
	require.Eventually(t, func() bool { return c.Stats().Malformed == 3 },
		5*time.Second, 5*time.Millisecond)
	require.NoError(t, c.Close())
	<-done

	st := c.Stats()
	require.Len(t, st.DecodeFailures, 2, "two distinct reasons, three failures")
	assert.Equal(t, gpuabi.ErrInvalidSamplePeriod.Error(), st.DecodeFailures[0].Reason,
		"the first reason seen leads, so the table reads as a history")
	assert.Equal(t, uint64(2), st.DecodeFailures[0].Count,
		"both zero-period samples fold into one reason")
	assert.Equal(t, gpuabi.ErrShortRecord.Error(), st.DecodeFailures[1].Reason)
	assert.Equal(t, uint64(1), st.DecodeFailures[1].Count)
	assert.Zero(t, st.DecodeReasonsUnrecorded, "two reasons fit in a table of four")

	var total uint64
	for _, f := range st.DecodeFailures {
		total += f.Count
	}
	assert.Equal(t, st.Malformed, total+st.DecodeReasonsUnrecorded,
		"the reasons must account for every malformed sample")
}

// A healthy consumer pays nothing for the feature, and says nothing either:
// an empty slice would read as "there were failures" to a %v.
func TestDecodeFailuresAreNilWhenNothingFailed(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	assert.Nil(t, c.Stats().DecodeFailures)
	assert.Zero(t, c.Stats().DecodeReasonsUnrecorded)
}

// The table is a fixed-size array, so a producer inventing endless distinct
// reasons cannot grow it. What it must not do is lose the fact that those
// failures happened.
func TestDecodeFailureTableIsBoundedAndCountsWhatItDropped(t *testing.T) {
	var tbl decodeFailureTable
	for i := 0; i < maxDecodeFailureReasons+3; i++ {
		tbl.note(fmt.Errorf("reason %d", i))
	}
	// ...and one repeat of a reason that did fit, which must still land in
	// its own slot rather than in the unrecorded bucket.
	tbl.note(errors.New("reason 0"))

	got := tbl.snapshot()
	require.Len(t, got, maxDecodeFailureReasons)
	assert.Equal(t, "reason 0", got[0].Reason)
	assert.Equal(t, uint64(2), got[0].Count)
	assert.Equal(t, uint64(3), tbl.unrecorded,
		"the three reasons that did not fit are counted, not dropped silently")
}

// Distinct detail on the same sentinel is a distinct reason: a batch of two
// sampled launches and a batch of nine are different producer bugs, and
// folding them together would hide one of them.
func TestDecodeFailureWrappedDetailIsNotFoldedIntoItsSentinel(t *testing.T) {
	var tbl decodeFailureTable
	tbl.note(errSampledBatchNotSingular)
	tbl.note(fmt.Errorf("%w: count=2", errSampledBatchNotSingular))
	tbl.note(fmt.Errorf("%w: count=2", errSampledBatchNotSingular))
	tbl.note(fmt.Errorf("%w: count=9", errSampledBatchNotSingular))

	got := tbl.snapshot()
	require.Len(t, got, 3)
	assert.Equal(t, uint64(1), got[0].Count)
	assert.Equal(t, errSampledBatchNotSingular.Error()+": count=2", got[1].Reason)
	assert.Equal(t, uint64(2), got[1].Count)
	assert.Equal(t, errSampledBatchNotSingular.Error()+": count=9", got[2].Reason)
}

// The state this has to survive is thousands of identical failures - an ABI
// drift fails every sample the same way. Formatting the reason each time
// would make a broken producer expensive on top of broken, so the repeat path
// must not allocate at all.
func TestRepeatedDecodeFailuresDoNotAllocate(t *testing.T) {
	var tbl decodeFailureTable
	tbl.note(gpuabi.ErrInvalidSamplePeriod) // the one allocation: a new reason

	allocs := testing.AllocsPerRun(1000, func() {
		tbl.note(gpuabi.ErrInvalidSamplePeriod)
	})
	assert.Zero(t, allocs, "a repeat of a known reason must be identity-matched, not formatted")
	// AllocsPerRun warms up before it measures, so the exact call count is
	// its business; what matters is that every one of them was counted.
	assert.Greater(t, tbl.snapshot()[0].Count, uint64(1000),
		"every repeat must still be counted, allocation-free or not")
}

// execBatchWith builds an exec batch carrying the given correlations, in
// order, all from one pid.
func execBatchWith(pid uint32, startNs uint64, corrs ...uint64) []byte {
	buf := make([]byte, batchHdrSize+len(corrs)*gpuabi.SizeExec)
	putU32(buf[0:], kindExec)
	putU32(buf[4:], uint32(len(corrs)))
	putU32(buf[16:], pid)
	putU64(buf[24:], uint64(len(corrs)*gpuabi.SizeExec))
	putU32(buf[32:], ^uint32(0)) // stack_id = -1 on every non-sampled kind
	for i, corr := range corrs {
		rec := buf[batchHdrSize+i*gpuabi.SizeExec:]
		putU64(rec[0:], corr)
		putU64(rec[32:], startNs+uint64(i)) // start_ns
		putU64(rec[40:], startNs+uint64(i)+10)
	}
	return buf
}

// TestSameCorrelationInTwoProcessesJoinsInTheTimeline is issue #36 end to
// end: the consumer's own side tables were already (pid, correlation)-keyed,
// but the gpu.Timeline underneath them was not, so a launch that kept its own
// stack all the way through the consumer could still be handed to the other
// process's execution one layer down.
//
// Both processes use wire correlation 7 - the collision is not contrived,
// vendor correlation counters restart from a low value in every process and
// the probes fire for every process that maps the shim.
func TestSameCorrelationInTwoProcessesJoinsInTheTimeline(t *testing.T) {
	tl := gpu.NewTimeline(gpu.TimelineConfig{})
	c, sm, _ := stackConsumer(t, tl, Config{})
	sm.put(1, 0x1000) // the stack captured in pid 4242
	sm.put(2, 0x2000) // the stack captured in pid 5353

	apply(t, c, sampledBatchWith(4242, 7, 1, 8))
	apply(t, c, sampledBatchWith(5353, 7, 2, 8))
	apply(t, c, launchBatchWith(4242, 7))
	apply(t, c, launchBatchWith(5353, 7))
	c.Flush()
	apply(t, c, execBatchWith(4242, 1000, 7))
	apply(t, c, execBatchWith(5353, 2000, 7))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 2)

	wantStack := map[uint64]string{1000: "fn_1000", 2000: "fn_2000"}
	wantPID := map[uint64]uint32{1000: 4242, 2000: 5353}
	for _, view := range snap.Executions {
		pid := wantPID[view.Exec.StartNs]
		require.NotNilf(t, view.Launch, "pid %d's execution lost its launch", pid)
		assert.Equalf(t, gpu.JoinExact, view.Join,
			"pid %d supplied a correlation live in its own process", pid)
		assert.Equalf(t, pid, view.Launch.Launch.PID,
			"pid %d's execution joined a launch from pid %d", pid, view.Launch.Launch.PID)
		assert.Equalf(t, []string{wantStack[view.Exec.StartNs]}, frameNames(view.Launch.Launch.CPUStack),
			"pid %d's GPU time must carry the call path captured in pid %d, not the other process's", pid, pid)
	}
	assert.Equal(t, uint64(2), snap.JoinStats.ExactExecutionJoinCount)
	assert.Equal(t, uint64(2), snap.JoinStats.MatchedLaunchCount,
		"two distinct launches were matched, not one matched twice")
	assert.Zero(t, snap.JoinStats.UnmatchedExecutionCount)
	assert.Zero(t, snap.JoinStats.UnmatchedLaunchCount)
	assert.Equal(t, uint64(2), c.Stats().StacksAttached)
}

// ----- The two PC-sampling probes, plus gpu_config_v1's first decoder.
//
// Task 2 puts these on the wire and decodes them; normalizing them into
// events is a later task. Until then they still land in applyBatch's
// `default:` arm and are counted as Stats.Undecoded, which is the contract:
// a kind the transport carries but the consumer does not yet interpret is
// counted, never dropped quietly. These tests assert BOTH halves — that
// decodeBatch reads the records correctly, and that the count they produce is
// visible — so the later change that makes Undecoded go to zero for them has
// something concrete to flip.

func TestDecodeStallReasonMapBatch(t *testing.T) {
	const n = 3
	buf := make([]byte, batchHdrSize+n*gpuabi.SizeStallReason)
	putU32(buf[0:], kindStallMap)
	putU32(buf[4:], n)
	putU64(buf[24:], uint64(n*gpuabi.SizeStallReason))
	names := []string{"selected", "long_scoreboard", "mio_throttle"}
	for i, name := range names {
		off := batchHdrSize + i*gpuabi.SizeStallReason
		putU32(buf[off:], uint32(10+i))
		binary.LittleEndian.PutUint16(buf[off+4:], uint16(len(name)))
		copy(buf[off+8:], name)
	}

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	require.Len(t, b.StallReasons, n)
	for i, name := range names {
		assert.Equal(t, uint32(10+i), b.StallReasons[i].Index)
		assert.Equal(t, name, b.StallReasons[i].Name,
			"record %d decoded at the wrong stride: 136 bytes per entry", i)
	}
}

// name_len is producer-supplied and indexes a fixed 128-byte array. The
// decode runs on the ringbuf drain goroutine, so a slice-out-of-range here
// takes the consumer down and loses everything still in the ring — much worse
// than refusing one batch. It must be an error at the batch boundary.
func TestDecodeStallReasonMapRejectsAnOverlongNameWithoutPanicking(t *testing.T) {
	buf := make([]byte, batchHdrSize+gpuabi.SizeStallReason)
	putU32(buf[0:], kindStallMap)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeStallReason))
	binary.LittleEndian.PutUint16(buf[batchHdrSize+4:], uint16(gpuabi.GPUStallNameMax)+1)

	require.NotPanics(t, func() {
		_, err := decodeBatch(buf)
		require.Error(t, err, "a name_len past the fixed array must be refused, not sliced")
	})
}

func TestDecodeSamplingWindowBatch(t *testing.T) {
	buf := make([]byte, batchHdrSize+2*gpuabi.SizeSamplingWindow)
	putU32(buf[0:], kindSamplingWindow)
	putU32(buf[4:], 2)
	putU64(buf[24:], uint64(2*gpuabi.SizeSamplingWindow))
	// A closed burst, then one still open at the producer's last report.
	putU64(buf[batchHdrSize:], 1_000)
	putU64(buf[batchHdrSize+8:], 51_000)
	buf[batchHdrSize+16] = gpuabi.SamplingModeKernelSerialized
	off := batchHdrSize + gpuabi.SizeSamplingWindow
	putU64(buf[off:], 100_000)
	putU64(buf[off+8:], 0)
	buf[off+16] = gpuabi.SamplingModeKernelSerialized

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	require.Len(t, b.SamplingWindows, 2)
	assert.Equal(t, uint64(1_000), b.SamplingWindows[0].StartNs)
	assert.Equal(t, uint64(51_000), b.SamplingWindows[0].EndNs)
	assert.False(t, b.SamplingWindows[0].Open())
	assert.True(t, b.SamplingWindows[1].Open(),
		"end_ns == 0 means the producer stopped mid-burst; it is not a zero-length window")
}

func TestDecodeConfigBatch(t *testing.T) {
	buf := make([]byte, batchHdrSize+gpuabi.SizeConfig)
	putU32(buf[0:], kindConfig)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeConfig))
	putU64(buf[batchHdrSize:], 1_695_000_000)
	putU32(buf[batchHdrSize+8:], 5)
	putU32(buf[batchHdrSize+12:], 82)
	buf[batchHdrSize+16] = 1

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	require.Len(t, b.Configs, 1)
	assert.Equal(t, uint64(1_695_000_000), b.Configs[0].ClockHz)
	assert.Equal(t, uint32(5), b.Configs[0].SamplingFactor)
	assert.Equal(t, uint32(82), b.Configs[0].SMCount)
}

// A count that overruns the declared payload must be refused for each new
// kind exactly as it is for the frozen ones.
func TestDecodeRejectsCountBeyondPayloadForTheNewKinds(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind uint32
		size int
	}{
		{"stallmap", kindStallMap, gpuabi.SizeStallReason},
		{"samplingwindow", kindSamplingWindow, gpuabi.SizeSamplingWindow},
		{"config", kindConfig, gpuabi.SizeConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, batchHdrSize+tc.size)
			putU32(buf[0:], tc.kind)
			putU32(buf[4:], 2) // claims two, carries one
			putU64(buf[24:], uint64(tc.size))
			_, err := decodeBatch(buf)
			require.ErrorIs(t, err, gpuabi.ErrShortRecord)
		})
	}
}

// ----- Task 7: the five PC-sampling kinds are normalized, not carried.
//
// This is the assertion Task 2 left to be flipped. Undecoded was the
// contract while the kinds were on the wire and uninterpreted; now every one
// of them has an applyBatch arm, so the counter must read zero for all five
// and the records must be counted in Records instead.

func TestTheFivePCSamplingKindsAreDecodedNotCountedUndecoded(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind uint32
		size int
	}{
		{"module", kindModule, gpuabi.SizeModuleLoad},
		{"pc", kindPC, gpuabi.SizePCSample},
		{"stallmap", kindStallMap, gpuabi.SizeStallReason},
		{"samplingwindow", kindSamplingWindow, gpuabi.SizeSamplingWindow},
		{"config", kindConfig, gpuabi.SizeConfig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := make([]byte, batchHdrSize+tc.size)
			putU32(buf[0:], tc.kind)
			putU32(buf[4:], 1)
			putU64(buf[24:], uint64(tc.size))
			// A sampling window with end_ns == 0 is the "open" case, which
			// is legal; give this one a real end so the healthy-run
			// assertions below hold for every kind alike.
			if tc.kind == kindSamplingWindow {
				putU64(buf[batchHdrSize:], 1_000)
				putU64(buf[batchHdrSize+8:], 2_000)
				buf[batchHdrSize+16] = gpuabi.SamplingModeContinuous
			}

			b, err := decodeBatch(buf)
			require.NoError(t, err)

			c := newTestConsumer(&recordingSink{})
			c.applyBatch(b)
			c.Flush()
			st := c.Stats()
			assert.Zero(t, st.Undecoded,
				"applyBatch has an arm for this kind now; Undecoded reading non-zero means it fell through to default:")
			assert.Equal(t, uint64(1), st.Records,
				"a decoded record is counted in Records, which is what Undecoded stopped counting")
			assert.Zero(t, st.SinkRejected)
			assert.Zero(t, st.Malformed)
			assert.Zero(t, st.SamplingWindowsOpen)
			assert.Zero(t, st.ConfigsDisagreed)
			assert.Zero(t, st.StallNamesEvicted)
			assert.Zero(t, st.StallNamesTruncated)
		})
	}
}

// moduleBatch builds one gpu_module_load_v1 batch.
func moduleBatch(pid uint32, crc, moduleID, size, loadNs, bytesPtr uint64) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeModuleLoad)
	putU32(buf[0:], kindModule)
	putU32(buf[4:], 1)
	putU32(buf[16:], pid)
	putU64(buf[24:], uint64(gpuabi.SizeModuleLoad))
	putU32(buf[32:], ^uint32(0)) // stack_id = -1 on every non-sampled kind
	rec := buf[batchHdrSize:]
	putU64(rec[0:], crc)
	putU64(rec[8:], moduleID)
	putU64(rec[16:], size)
	putU64(rec[24:], loadNs)
	putU64(rec[32:], bytesPtr)
	return buf
}

// pcSample is one wire gpu_pc_sample_batch_v1 record's worth of fields.
type pcSample struct {
	crc         uint64
	correlation uint64
	pcOffset    uint64
	fnIndex     uint32
	stallIndex  uint32
	count       uint32
}

// pcBatch builds one gpu_pc_sample_batch_v1 batch of n records.
func pcBatch(pid uint32, seq uint64, samples ...pcSample) []byte {
	n := len(samples)
	buf := make([]byte, batchHdrSize+n*gpuabi.SizePCSample)
	putU32(buf[0:], kindPC)
	putU32(buf[4:], uint32(n))
	putU64(buf[8:], seq)
	putU32(buf[16:], pid)
	putU64(buf[24:], uint64(n*gpuabi.SizePCSample))
	putU32(buf[32:], ^uint32(0))
	for i, s := range samples {
		rec := buf[batchHdrSize+i*gpuabi.SizePCSample:]
		putU64(rec[0:], s.crc)
		putU64(rec[8:], s.correlation)
		putU64(rec[16:], s.pcOffset)
		putU32(rec[24:], s.fnIndex)
		putU32(rec[28:], s.stallIndex)
		putU32(rec[32:], s.count)
	}
	return buf
}

// stallMapBatch builds one gpu_stall_reason_map_v1 batch from index -> name
// pairs, in the given order.
func stallMapBatch(seq uint64, indices []uint32, names []string, truncated bool) []byte {
	n := len(indices)
	buf := make([]byte, batchHdrSize+n*gpuabi.SizeStallReason)
	putU32(buf[0:], kindStallMap)
	putU32(buf[4:], uint32(n))
	putU64(buf[8:], seq)
	putU64(buf[24:], uint64(n*gpuabi.SizeStallReason))
	putU32(buf[32:], ^uint32(0))
	for i := range indices {
		rec := buf[batchHdrSize+i*gpuabi.SizeStallReason:]
		putU32(rec[0:], indices[i])
		binary.LittleEndian.PutUint16(rec[4:], uint16(len(names[i])))
		if truncated {
			rec[6] = 1
		}
		copy(rec[8:], names[i])
	}
	return buf
}

func samplingWindowBatch(startNs, endNs uint64, mode uint8) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeSamplingWindow)
	putU32(buf[0:], kindSamplingWindow)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeSamplingWindow))
	putU32(buf[32:], ^uint32(0))
	putU64(buf[batchHdrSize:], startNs)
	putU64(buf[batchHdrSize+8:], endNs)
	buf[batchHdrSize+16] = mode
	return buf
}

func configBatch(clockHz uint64, factor, smCount uint32) []byte {
	buf := make([]byte, batchHdrSize+gpuabi.SizeConfig)
	putU32(buf[0:], kindConfig)
	putU32(buf[4:], 1)
	putU64(buf[24:], uint64(gpuabi.SizeConfig))
	putU32(buf[32:], ^uint32(0))
	putU64(buf[batchHdrSize:], clockHz)
	putU32(buf[batchHdrSize+8:], factor)
	putU32(buf[batchHdrSize+12:], smCount)
	buf[batchHdrSize+16] = 1 // vendor
	return buf
}

// A module load says THAT a module loaded, with its content hash and size.
// bytes_ptr is decoded and deliberately dropped: it points into the
// producer's address space and following it would need CAP_SYS_PTRACE.
func TestModuleLoadIsNormalizedAndBytesPtrIsNotFollowed(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)
	apply(t, c, moduleBatch(4242, 0xC0FFEE, 9, 8192, 1_700_000, 0x7f1234560000))

	require.Len(t, sink.modules, 1)
	assert.Equal(t, gpu.ModuleRef{Backend: gpu.BackendCUPTI, CRC: 0xC0FFEE}, sink.modules[0].Ref,
		"a module is identified by content hash, never by the producer's module id")
	assert.Equal(t, uint64(8192), sink.modules[0].SizeBytes)
	assert.Equal(t, uint64(1_700_000), sink.modules[0].LoadedNs)

	st := c.Stats()
	assert.Equal(t, uint64(1), st.ModulesDecoded)
	assert.Zero(t, st.Undecoded)
	assert.Zero(t, st.SinkRejected)
}

// The ordinary Tier B order: the stall map first, then the PC samples that
// refer to it. Every sample resolves, nothing waits, nothing is missing.
func TestPCSamplesResolveStallNamesFromTheMap(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)
	apply(t, c, stallMapBatch(1,
		[]uint32{0, 17, 23},
		[]string{"selected", "long_scoreboard", "mio_throttle"}, false))
	apply(t, c, pcBatch(4242, 1,
		pcSample{crc: 0xAA, pcOffset: 0x40, fnIndex: 3, stallIndex: 17, count: 5},
		pcSample{crc: 0xAA, pcOffset: 0x50, fnIndex: 3, stallIndex: 0, count: 2},
	))

	require.Len(t, sink.pcSamples, 2)
	assert.Equal(t, "long_scoreboard", sink.pcSamples[0].StallReason)
	assert.Equal(t, "selected", sink.pcSamples[1].StallReason,
		"stall index 0 is an ordinary vendor index, not a sentinel meaning 'no stall reason'")
	assert.Equal(t, gpu.ModuleRef{Backend: gpu.BackendCUPTI, CRC: 0xAA}, sink.pcSamples[0].Module)
	assert.Equal(t, uint64(0x40), sink.pcSamples[0].PCOffset)
	assert.Equal(t, uint64(5), sink.pcSamples[0].Count)
	assert.Equal(t, gpu.ClockDomainCPUMonotonic, sink.pcSamples[0].ClockDomain)

	st := c.Stats()
	assert.Zero(t, st.Undecoded)
	assert.Zero(t, st.StallNamesMissing, "every index was in the map")
	assert.Zero(t, st.PendingStallSamples)
	assert.Equal(t, uint64(2), st.PCSamplesDecoded)
	assert.Equal(t, uint64(3), st.StallNamesLearned)
	assert.Equal(t, 3, st.KnownStallNames)
}

// The order the ABI actually produces on a fresh attach: the stall map is
// one-shot plus late-attach replay, so the FIRST PC batch of a run routinely
// precedes it. Those samples must be held and resolved when the map lands,
// not sent out permanently blank.
func TestPCBatchBeforeItsStallMapStillResolves(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	apply(t, c, pcBatch(4242, 1,
		pcSample{crc: 0xAA, pcOffset: 0x40, stallIndex: 17, count: 5},
		pcSample{crc: 0xAA, pcOffset: 0x50, stallIndex: 17, count: 1},
	))
	assert.Empty(t, sink.pcSamples, "a sample with an unmapped index waits rather than going out blank")
	assert.Equal(t, 2, c.Stats().PendingStallSamples)

	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreboard"}, false))

	require.Len(t, sink.pcSamples, 2)
	assert.Equal(t, "long_scoreboard", sink.pcSamples[0].StallReason)
	assert.Equal(t, "long_scoreboard", sink.pcSamples[1].StallReason)
	assert.Equal(t, uint64(0x40), sink.pcSamples[0].PCOffset,
		"held samples are released oldest first")

	st := c.Stats()
	assert.Zero(t, st.StallNamesMissing, "a sample that resolved late is not a missing name")
	assert.Zero(t, st.PendingStallSamples)
	assert.Zero(t, st.Undecoded)
}

// FunctionIndex is half of the key Timeline.pendingModule groups Tier B samples
// by, so dropping it on the decode path would put every sample of a process into
// one group per cubin - a partial re-run of the collapse Task 8a exists to fix.
// It reads as healthy from the counters alone, which only see samples arriving
// and being grouped, never which group. Hence a test on the value, not a count.
func TestDecodedPCSamplesCarryTheirFunctionIndex(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	apply(t, c, stallMapBatch(1, []uint32{3}, []string{"mio_throttle"}, false))
	apply(t, c, pcBatch(4242, 2,
		pcSample{crc: 0xAA, pcOffset: 0x40, fnIndex: 7, stallIndex: 3, count: 5},
		pcSample{crc: 0xAA, pcOffset: 0x80, fnIndex: 11, stallIndex: 3, count: 2},
	))

	require.Len(t, sink.pcSamples, 2)
	assert.Equal(t, uint32(7), sink.pcSamples[0].FunctionIndex)
	assert.Equal(t, uint32(11), sink.pcSamples[1].FunctionIndex,
		"two samples in one cubin must keep distinct function indices, or they group as one")
}

// An index the map never carried has no name and never will. It becomes the
// empty string - never "stall#17", which would put an unstable vendor number
// into a label value - and it is counted, because a blank stall reason with
// no counter behind it is a mystery rather than a measurement.
func TestUnmappedStallIndexYieldsEmptyStringAndIsCounted(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreboard"}, false))
	apply(t, c, pcBatch(4242, 1, pcSample{crc: 0xAA, pcOffset: 0x40, stallIndex: 99, count: 3}))
	// The map for 99 is never coming; the sample is held until Flush, then
	// released unresolved rather than dropped.
	c.Flush()

	require.Len(t, sink.pcSamples, 1)
	assert.Equal(t, "", sink.pcSamples[0].StallReason,
		"an unresolved stall index must be empty, never a rendered index")
	assert.NotContains(t, sink.pcSamples[0].StallReason, "99")
	assert.Equal(t, uint64(3), sink.pcSamples[0].Count,
		"the sample itself is delivered intact; only its stall reason is missing")

	st := c.Stats()
	assert.Equal(t, uint64(1), st.StallNamesMissing)
	assert.Equal(t, uint64(1), st.PCSamplesDecoded)
	assert.Zero(t, st.Undecoded)
	assert.Zero(t, st.SinkRejected)
}

// A producer that never emits a stall map at all must not cost a record. The
// pending queue releases its oldest on every push, so the samples flow with
// a bounded lag and every one of them is counted as missing.
func TestPCSamplesFlowWhenNoStallMapEverArrives(t *testing.T) {
	sink := &recordingSink{}
	c := newConsumer(Config{
		Backend:                    gpu.BackendCUPTI,
		Sink:                       sink,
		PendingStallSampleCapacity: 4,
	})
	for i := 0; i < 20; i++ {
		apply(t, c, pcBatch(4242, uint64(i+1),
			pcSample{crc: 0xAA, pcOffset: uint64(i), stallIndex: 7, count: 1}))
	}
	assert.Equal(t, 4, c.Stats().PendingStallSamples, "the queue is bounded")
	c.Flush()

	require.Len(t, sink.pcSamples, 20, "a missing stall name must never cost a record")
	for i, s := range sink.pcSamples {
		assert.Equalf(t, uint64(i), s.PCOffset, "released in arrival order")
		assert.Equal(t, "", s.StallReason)
	}
	st := c.Stats()
	assert.Equal(t, uint64(20), st.StallNamesMissing)
	assert.Equal(t, uint64(20), st.PCSamplesDecoded)
	assert.Zero(t, st.KnownStallNames)
	assert.Zero(t, st.PendingStallSamples)
}

// A truncated stall name still resolves, marked, so it is never presented as
// complete. The ABI's buffer is exactly CUPTI's own maximum, so this counter
// reading non-zero says the producer is not the producer we think it is.
func TestTruncatedStallNameIsMarkedAndCounted(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)
	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreb"}, true))
	apply(t, c, pcBatch(4242, 1, pcSample{crc: 0xAA, stallIndex: 17, count: 1}))

	require.Len(t, sink.pcSamples, 1)
	assert.Equal(t, "long_scoreb"+truncatedStallSuffix, sink.pcSamples[0].StallReason)
	assert.Equal(t, uint64(1), c.Stats().StallNamesTruncated)
	assert.Zero(t, c.Stats().StallNamesMissing, "a truncated name resolved; it is not a missing one")
}

// The bounded table is a ceiling, not a dial. When it bites the PC samples
// still flow - unresolved, and counted twice over: once as an eviction and
// once as a missing name.
func TestStallNameTableEvictionIsCountedAndCostsNoRecord(t *testing.T) {
	sink := &recordingSink{}
	c := newConsumer(Config{Backend: gpu.BackendCUPTI, Sink: sink, StallNameCapacity: 2})
	apply(t, c, stallMapBatch(1, []uint32{1, 2, 3}, []string{"a", "b", "c"}, false))
	assert.Equal(t, uint64(1), c.Stats().StallNamesEvicted)
	assert.Equal(t, 2, c.Stats().KnownStallNames)

	apply(t, c, pcBatch(4242, 1, pcSample{crc: 0xAA, stallIndex: 1, count: 1}))
	c.Flush()
	require.Len(t, sink.pcSamples, 1)
	assert.Equal(t, "", sink.pcSamples[0].StallReason, "index 1 was evicted")
	assert.Equal(t, uint64(1), c.Stats().StallNamesMissing)
}

// A re-interned index (the late-attach replay case) replaces the value in
// place without taking a second FIFO position, exactly as kernelNameTable
// does - otherwise a replayed table would evict itself.
func TestStallMapReplayDoesNotEvictItself(t *testing.T) {
	c := newConsumer(Config{Backend: gpu.BackendCUPTI, Sink: &recordingSink{}, StallNameCapacity: 3})
	for range 5 {
		apply(t, c, stallMapBatch(1, []uint32{1, 2, 3}, []string{"a", "b", "c"}, false))
	}
	st := c.Stats()
	assert.Zero(t, st.StallNamesEvicted, "a replayed table must not push itself out")
	assert.Equal(t, 3, st.KnownStallNames)
	assert.Equal(t, uint64(15), st.StallNamesLearned, "records, not distinct indices")
}

// Tier B populates no correlation at all, so every PC record carries zero by
// design. That is exactly why it needs a counter of its own: ZeroCorrelation
// is enormous and healthy here, and a Tier A contract violation - where
// CUPTI populates every record - would be invisible inside it (issue #52's
// defect, in the PC-sample population).
func TestTierBZeroCorrelationIsCountedApartFromTheAggregate(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)
	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreboard"}, false))
	apply(t, c, pcBatch(4242, 1,
		pcSample{crc: 0xAA, correlation: 0, stallIndex: 17, count: 1},
		pcSample{crc: 0xAA, correlation: 0, stallIndex: 17, count: 1},
		pcSample{crc: 0xAA, correlation: 0, stallIndex: 17, count: 1},
	))

	st := c.Stats()
	assert.Equal(t, uint64(3), st.PCSamplesWithoutCorrelation,
		"in Tier B this equals PCSamplesDecoded on a perfectly healthy run")
	assert.Equal(t, st.PCSamplesDecoded, st.PCSamplesWithoutCorrelation)
	assert.Equal(t, uint64(3), st.ZeroCorrelation, "the aggregate still counts them")
	assert.Zero(t, st.ZeroCorrelationExecs,
		"an execution's zero is a different fact and must not be moved by a PC sample")

	require.Len(t, sink.pcSamples, 3)
	for _, s := range sink.pcSamples {
		assert.False(t, s.Correlation.Present(),
			"a wire zero must yield the zero CorrelationID, not one carrying only a pid")
		assert.Equal(t, gpu.CorrelationID{}, s.Correlation)
	}
}

// Tier A is the opposite condition: CUPTI populates correlationId on every
// record, so PCSamplesWithoutCorrelation must read zero and the correlation
// must carry the batch header's pid, exactly as launches and executions do.
func TestTierAPCSampleCarriesTheProcessQualifiedCorrelation(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)
	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreboard"}, false))
	apply(t, c, pcBatch(4242, 1, pcSample{crc: 0xAA, correlation: 99, stallIndex: 17, count: 1}))

	require.Len(t, sink.pcSamples, 1)
	assert.Equal(t, gpu.CorrelationID{Backend: gpu.BackendCUPTI, PID: 4242, Value: "99"},
		sink.pcSamples[0].Correlation,
		"vendor correlation counters restart in every process, so the pid is part of the id")

	st := c.Stats()
	assert.Zero(t, st.PCSamplesWithoutCorrelation,
		"a single non-zero here breaks Tier A's whole claim of exact launch attribution")
	assert.Zero(t, st.ZeroCorrelation)
}

// A sampling window is decoded and counted; end_ns == 0 is the ABI's "open
// when the producer stopped reporting" and is counted apart, because an open
// window means the executions after its start are serialized="unknown"
// rather than "false".
func TestSamplingWindowsAreCountedAndTheOpenOneIsSeparate(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	apply(t, c, samplingWindowBatch(1_000, 51_000, gpuabi.SamplingModeKernelSerialized))
	assert.Zero(t, c.Stats().SamplingWindowsOpen)

	apply(t, c, samplingWindowBatch(100_000, 0, gpuabi.SamplingModeKernelSerialized))
	st := c.Stats()
	assert.Equal(t, uint64(2), st.SamplingWindowsDecoded)
	assert.Equal(t, uint64(1), st.SamplingWindowsOpen,
		"end_ns == 0 is a hard exit mid-burst, not a zero-length window")
	assert.Zero(t, st.Undecoded)
}

// An inverted window is a producer contract violation, not a short buffer.
// It is refused at the batch boundary so a negative duration can never reach
// the serialization disclosure.
func TestInvertedSamplingWindowIsRefusedAtTheBoundary(t *testing.T) {
	_, err := decodeBatch(samplingWindowBatch(51_000, 1_000, gpuabi.SamplingModeKernelSerialized))
	require.ErrorIs(t, err, gpuabi.ErrWindowInverted)
}

// gpu_config_v1 has been on the wire since Phase 3 with nothing reading it.
// Its three fields are reachable now, and a second producer disagreeing with
// the first is counted rather than silently overwriting the answer.
func TestConfigIsDecodedAndDisagreementIsCounted(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	apply(t, c, configBatch(1_695_000_000, 5, 82))
	st := c.Stats()
	assert.Equal(t, uint64(1), st.ConfigsDecoded)
	assert.Equal(t, uint32(5), st.ConfigSamplingFactor)
	assert.Equal(t, uint32(82), st.ConfigSMCount)
	assert.Equal(t, uint64(1_695_000_000), st.ConfigClockHz)
	assert.Zero(t, st.ConfigsDisagreed)
	assert.Zero(t, st.Undecoded)

	// The late-attach replay: the same record again is not a disagreement.
	apply(t, c, configBatch(1_695_000_000, 5, 82))
	assert.Zero(t, c.Stats().ConfigsDisagreed)

	// A second producer with a different configuration is.
	apply(t, c, configBatch(1_695_000_000, 9, 82))
	st = c.Stats()
	assert.Equal(t, uint64(1), st.ConfigsDisagreed,
		"last-writer-wins gauges must say when they describe an arbitrary one of several producers")
	assert.Equal(t, uint32(9), st.ConfigSamplingFactor)
}

// Sequence-gap accounting is per (kind, pid) and must work on the new kinds
// exactly as it does on launches: a jump in a probe's monotonic sequence is
// a whole batch the consumer never saw.
func TestSequenceGapsAreCountedOnTheNewKinds(t *testing.T) {
	c := newTestConsumer(&recordingSink{})
	apply(t, c, pcBatch(4242, 1, pcSample{crc: 0xAA, stallIndex: 17, count: 1}))
	apply(t, c, pcBatch(4242, 4, pcSample{crc: 0xAA, stallIndex: 17, count: 1}))
	assert.Equal(t, uint64(2), c.Stats().SequenceGaps,
		"seq 2 and 3 never arrived; each is a whole batch of PC records")

	// A different pid is an independent stream, not a gap.
	apply(t, c, pcBatch(7, 1, pcSample{crc: 0xAA, stallIndex: 17, count: 1}))
	assert.Equal(t, uint64(2), c.Stats().SequenceGaps)

	// And so is a different kind from the same pid.
	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreboard"}, false))
	assert.Equal(t, uint64(2), c.Stats().SequenceGaps)
}

// A sink at capacity refuses PC samples and modules the same way it refuses
// launches: the record is not retried and not silently dropped, it is
// counted in SinkRejected.
func TestRejectedPCSamplesAndModulesAreCounted(t *testing.T) {
	sink := &recordingSink{err: errors.New("full")}
	c := newTestConsumer(sink)
	apply(t, c, stallMapBatch(1, []uint32{17}, []string{"long_scoreboard"}, false))
	apply(t, c, pcBatch(4242, 1, pcSample{crc: 0xAA, stallIndex: 17, count: 1}))
	apply(t, c, moduleBatch(4242, 0xC0FFEE, 9, 8192, 1_700_000, 0))

	st := c.Stats()
	assert.Equal(t, uint64(2), st.SinkRejected)
	assert.Zero(t, st.Undecoded)
}

// A whole Tier B run's worth of the five kinds, in the order a late-attaching
// consumer sees them: PC samples first, then the replayed map, config and
// module. Every loss counter must read zero at the end - which is the
// "assertable at zero on a healthy run" rule for all of them at once.
func TestHealthyTierBRunLeavesEveryNewLossCounterAtZero(t *testing.T) {
	sink := &recordingSink{}
	c := newTestConsumer(sink)

	apply(t, c, pcBatch(4242, 1,
		pcSample{crc: 0xAA, pcOffset: 0x40, fnIndex: 3, stallIndex: 17, count: 5},
		pcSample{crc: 0xAA, pcOffset: 0x50, fnIndex: 3, stallIndex: 23, count: 2},
	))
	apply(t, c, stallMapBatch(1,
		[]uint32{17, 23}, []string{"long_scoreboard", "mio_throttle"}, false))
	apply(t, c, configBatch(1_695_000_000, 5, 82))
	apply(t, c, moduleBatch(4242, 0xAA, 9, 8192, 1_700_000, 0x7f0000000000))
	apply(t, c, samplingWindowBatch(1_000, 51_000, gpuabi.SamplingModeContinuous))
	c.Flush()

	require.Len(t, sink.pcSamples, 2)
	require.Len(t, sink.modules, 1)

	st := c.Stats()
	assert.Zero(t, st.Undecoded)
	assert.Zero(t, st.Malformed)
	assert.Zero(t, st.SequenceGaps)
	assert.Zero(t, st.SinkRejected)
	assert.Zero(t, st.StallNamesMissing)
	assert.Zero(t, st.StallNamesEvicted)
	assert.Zero(t, st.StallNamesTruncated)
	assert.Zero(t, st.SamplingWindowsOpen)
	assert.Zero(t, st.ConfigsDisagreed)
	assert.Zero(t, st.PendingStallSamples)
	assert.Zero(t, st.ZeroCorrelationExecs)
	// The one new counter that is NOT zero on a healthy Tier B run, and the
	// reason it is its own counter rather than a share of ZeroCorrelation.
	assert.Equal(t, uint64(2), st.PCSamplesWithoutCorrelation)
	assert.Equal(t, uint64(7), st.Records,
		"2 PC samples + 2 stall names + 1 config + 1 module + 1 window")
}

func TestCookieForCoversTheNewProbes(t *testing.T) {
	assert.Equal(t, uint64(7), cookieFor("gpu_stall_reason_map_v1"))
	assert.Equal(t, uint64(8), cookieFor("gpu_sampling_window_v1"))
	assert.Equal(t, uint64(9), cookieFor("gpu_config_v1"))
}

// record_size and max_records must have grown arms for the new kinds. A kind
// cookieFor installs but record_size cannot size is not merely undelivered:
// gpu_usdt_batch charges the whole batch to KIND_UNKNOWN and returns, so
// every record of that kind is lost and attributed to slot 0.
func TestBPFSizesEveryKindCookieForInstalls(t *testing.T) {
	src, err := os.ReadFile("../bpf/gpu_usdt.bpf.c")
	require.NoError(t, err)
	text := string(src)

	for _, kind := range []string{"KIND_STALL_MAP", "KIND_SAMPLING_WINDOW", "KIND_CONFIG"} {
		assert.Containsf(t, text, "if (kind == "+kind+")\n        return REC_",
			"record_size has no arm for %s; its batches would be charged to KIND_UNKNOWN and lost", kind)
		assert.Containsf(t, text, "if (kind == "+kind+")\n        return BATCH_CAP(REC_",
			"max_records has no arm for %s", kind)
	}
}

// gpu_dropped_v1 gets its first kind, cookie and decode path in this phase.
//
// Without them the shim's drop classes are unreachable: the probe never
// attaches, its semaphore never arms, the shim never emits, and every class
// reads zero exactly when loss is worst. That is the shape of twelve past
// defects on this project, so the wire path is asserted here rather than
// assumed from the shim's side.
func TestDroppedProbeHasAKindAndDecodes(t *testing.T) {
	require.Equal(t, uint64(kindDropped), cookieFor("gpu_dropped_v1"),
		"an unattached probe is a drop class that can never go non-zero")

	const n = 2
	buf := make([]byte, batchHdrSize+n*gpuabi.SizeDropped)
	putU32(buf[0:], kindDropped)
	putU32(buf[4:], n)
	putU64(buf[24:], uint64(n*gpuabi.SizeDropped))
	putU64(buf[batchHdrSize:], 17)
	buf[batchHdrSize+8] = gpuabi.DropClassPCNonUserKernel
	putU64(buf[batchHdrSize+gpuabi.SizeDropped:], 3)
	buf[batchHdrSize+gpuabi.SizeDropped+8] = gpuabi.DropClassPCDroppedHW

	b, err := decodeBatch(buf)
	require.NoError(t, err)
	require.Len(t, b.Drops, 2)
	assert.Equal(t, uint64(17), b.Drops[0].Count)
	assert.Equal(t, gpuabi.DropClassPCNonUserKernel, b.Drops[0].Class)
	assert.Equal(t, uint64(3), b.Drops[1].Count)
	assert.Equal(t, gpuabi.DropClassPCDroppedHW, b.Drops[1].Class)
}

// A count that overruns the payload must error rather than slice past the end
// inside the ringbuf drain goroutine.
func TestDecodeBatchRejectsAnOverlongDroppedBatch(t *testing.T) {
	buf := make([]byte, batchHdrSize+gpuabi.SizeDropped)
	putU32(buf[0:], kindDropped)
	putU32(buf[4:], 4)
	putU64(buf[24:], uint64(gpuabi.SizeDropped))

	_, err := decodeBatch(buf)
	assert.ErrorIs(t, err, gpuabi.ErrShortRecord)
}
