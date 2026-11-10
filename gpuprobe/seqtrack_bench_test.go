package gpuprobe

import (
	"context"
	"testing"
	"time"

	"github.com/cilium/ebpf/ringbuf"

	"github.com/dpsoft/perf-agent/gpu"
)

type benchSink struct{}

func (benchSink) EmitLaunch(gpu.GPUKernelLaunch) error             { return nil }
func (benchSink) EmitExec(gpu.GPUKernelExec) error                 { return nil }
func (benchSink) EmitPCSample(gpu.GPUPCSample) error               { return nil }
func (benchSink) EmitModule(gpu.GPUModule) error                   { return nil }
func (benchSink) EmitEvent(gpu.GPUTimelineEvent) error             { return nil }
func (benchSink) EmitSamplingWindow(gpu.GPUSamplingWindow) error   { return nil }
func (benchSink) EmitGraphExecutions(gpu.GPUGraphExecutions) error { return nil }

func BenchmarkNoteSeqInOrder(b *testing.B) {
	c := newTestConsumer(&recordingSink{})
	var seq uint64
	for b.Loop() {
		c.noteSeq(kindLaunch, 1, seq)
		seq++
	}
}

// Adjacent pairs swapped: 1,0,3,2,... -- what two producer threads deliver.
func BenchmarkNoteSeqReordered(b *testing.B) {
	c := newTestConsumer(&recordingSink{})
	var i uint64
	for b.Loop() {
		c.noteSeq(kindLaunchSampled, 1, i^1)
		i++
	}
}

// 9 kinds x 64 pids, round robin: a system-wide attach.
func BenchmarkNoteSeqManyStreams(b *testing.B) {
	c := newTestConsumer(&recordingSink{})
	var i uint64
	for b.Loop() {
		s := i % (9 * 64)
		c.noteSeq(uint32(s%9), uint32(s/9), i/(9*64))
		i++
	}
}

// One launch batch through applyBatch: the per-batch cost noteSeq is part of.
func BenchmarkApplyBatchLaunch(b *testing.B) {
	c := newTestConsumer(benchSink{})
	bt, err := decodeBatch(oneLaunchBatch(7, 0))
	if err != nil {
		b.Fatal(err)
	}
	var seq uint64
	for b.Loop() {
		bt.Seq = seq
		c.applyBatch(bt)
		seq++
	}
}

// countingReader hands out b.N launch batches with increasing seq, then
// reports the reader closed.
type countingReader struct {
	buf  []byte
	n, i uint64
}

func (r *countingReader) Read() (ringbuf.Record, error) {
	if r.i == r.n {
		return ringbuf.Record{}, ringbuf.ErrClosed
	}
	putU64(r.buf[8:], r.i)
	r.i++
	return ringbuf.Record{RawSample: r.buf}, nil
}
func (r *countingReader) SetDeadline(time.Time) {}
func (r *countingReader) Close() error          { return nil }
func (r *countingReader) Flush() error          { return nil }
func (r *countingReader) AvailableBytes() int   { return 0 }

// The Run loop end to end on a busy ring, through a reader that never blocks.
func BenchmarkRunLoop(b *testing.B) {
	c := newTestConsumer(benchSink{})
	c.reader = &countingReader{buf: oneLaunchBatch(7, 0), n: uint64(b.N)}
	b.ResetTimer()
	if err := c.Run(context.Background()); err != nil {
		b.Fatal(err)
	}
}
