package gpuprobe

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeLink records when it was closed relative to the reader's flush.
type fakeLink struct {
	link.Link
	mu     sync.Mutex
	closed int
	onDone func()
}

func (l *fakeLink) Close() error {
	l.mu.Lock()
	l.closed++
	l.mu.Unlock()
	if l.onDone != nil {
		l.onDone()
	}
	return nil
}

func (l *fakeLink) closes() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// stallFirstEmit holds the sink on its first event until release is called,
// so the reader is backlogged with everything after it when the capture
// stops.
func stallFirstEmit(extra func()) (onEmit func(), release func()) {
	gate := make(chan struct{})
	var once sync.Once
	first := true
	return func() {
			if first {
				first = false
				<-gate
			}
			if extra != nil {
				extra()
			}
		}, func() {
			once.Do(func() { close(gate) })
		}
}

// stopWhileBacklogged cancels Run while its first event is held in the sink,
// and lets it go only once the capture boundary is marked.
func stopWhileBacklogged(t *testing.T, c *Consumer, reader *scriptedReader, release func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := runConsumer(t, c, ctx)
	require.Eventually(t, func() bool { return reader.queuedBytes.Load() < int64(reader.cap0) },
		5*time.Second, time.Millisecond, "Run never took the first record")
	cancel()
	require.Eventually(t, func() bool { return reader.flushes.Load() == 1 },
		5*time.Second, time.Millisecond, "the stop never reached Flush")
	release()
	waitRun(t, done)
}

func runConsumer(t *testing.T, c *Consumer, ctx context.Context) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	return done
}

func waitRun(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// Records the probes wrote before the stop are part of the capture. Closing
// the reader on cancellation discarded whatever a backlogged consumer had not
// read yet, and BPF counted none of it as dropped.
func TestStopDrainsRecordsAcceptedBeforeTheBoundary(t *testing.T) {
	const n = 200
	reader := newScriptedReader(n)
	// The consumer is backlogged: everything is still queued when the
	// capture stops, and Run has not read any of it.
	for seq := range uint64(n) {
		reader.push(oneLaunchBatch(7, seq))
	}
	reader.cap0 = int(reader.queuedBytes.Load())
	onEmit, release := stallFirstEmit(nil)
	sink := &recordingSink{onEmit: onEmit}
	c := newTestConsumer(sink)
	c.reader = reader

	stopWhileBacklogged(t, c, reader, release)

	st := c.Stats()
	assert.Equal(t, n, sink.launchCount(), "every accepted record reaches the sink")
	assert.Equal(t, uint64(n), st.Batches)
	assert.Equal(t, uint64(n-1), st.StopDrainedBatches, "all but the one in hand were read after the boundary")
	assert.Zero(t, st.StopUnreadBytes)
	assert.Zero(t, reader.deadlines.Load(), "SetDeadline on a blocked reader deadlocks")
}

// The probes come down before the drain starts. Otherwise a target that keeps
// running refills the ring while it is drained, the drain has no end, and the
// drop counters read later include activity after the capture.
func TestStopDetachesProbesBeforeDraining(t *testing.T) {
	reader := newScriptedReader(1)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader
	var order []string
	var mu sync.Mutex
	note := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	l1 := &fakeLink{onDone: func() { note("link") }}
	l2 := &fakeLink{onDone: func() { note("link") }}
	c.links = []link.Link{l1, l2}
	reader.onFlush = func() { note("flush") }

	ctx, cancel := context.WithCancel(t.Context())
	done := runConsumer(t, c, ctx)
	waitForBlockedRead(t, reader)
	cancel()
	waitRun(t, done)

	mu.Lock()
	assert.Equal(t, []string{"link", "link", "flush"}, order)
	mu.Unlock()

	// Close afterwards must not close the links a second time.
	require.NoError(t, c.Close())
	assert.Equal(t, 1, l1.closes())
	assert.Equal(t, 1, l2.closes())
}

// An idle consumer parked in Read must still stop promptly: Flush wakes it
// the way Close used to, without the reader's mutex.
func TestStopWakesAnIdleBlockedReader(t *testing.T) {
	reader := newScriptedReader(1)
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	ctx, cancel := context.WithCancel(t.Context())
	done := runConsumer(t, c, ctx)
	waitForBlockedRead(t, reader)
	cancel()
	waitRun(t, done)

	assert.Equal(t, int32(1), reader.flushes.Load())
	assert.Zero(t, reader.deadlines.Load())
	st := c.Stats()
	assert.Zero(t, st.StopDrainedBatches)
	assert.Zero(t, st.StopUnreadBytes)
}

// The drain is bounded. A consumer too slow to finish within the timeout gives
// up, and what it leaves behind is counted in bytes -- accepted by BPF, so no
// drop counter has it -- rather than silently discarded.
func TestStopDrainIsBoundedAndCountsWhatItAbandons(t *testing.T) {
	const n = 50
	reader := newScriptedReader(n)
	for seq := range uint64(n) {
		reader.push(oneLaunchBatch(7, seq))
	}
	reader.cap0 = int(reader.queuedBytes.Load())
	recSize := uint64(ringRecordBytes(len(oneLaunchBatch(7, 0))))
	// A slow sink stands in for symbolization pressure.
	onEmit, release := stallFirstEmit(func() { time.Sleep(5 * time.Millisecond) })
	sink := &recordingSink{onEmit: onEmit}
	c := newConsumer(Config{Sink: sink, StopDrainTimeout: 20 * time.Millisecond})
	c.reader = reader

	start := time.Now()
	stopWhileBacklogged(t, c, reader, release)
	assert.Less(t, time.Since(start), 2*time.Second)

	st := c.Stats()
	require.Positive(t, st.StopUnreadBytes, "the timeout must have cut the drain short")
	assert.Less(t, st.Batches, uint64(n))
	assert.Equal(t, uint64(n)*recSize, st.Batches*recSize+st.StopUnreadBytes,
		"every accepted record is either delivered or counted as unread")
	assert.Equal(t, st.Batches-1, st.StopDrainedBatches)
}

// Once stopped, the counters are fixed: nothing a slow caller does before
// reading Stats (symbolization, writing the profile) can add to them.
func TestStatsAfterStopDoNotMove(t *testing.T) {
	reader := newScriptedReader(4)
	reader.push(oneLaunchBatch(7, 0))
	c := newTestConsumer(&recordingSink{})
	c.reader = reader

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	waitRun(t, runConsumer(t, c, ctx))
	first := c.Stats()

	// A record that would only appear if something were still producing.
	reader.push(oneLaunchBatch(7, 1))
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, first, c.Stats())
}
