package hip

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	pp "github.com/dpsoft/perf-agent/pprof"
)

var errFakeLiveStop = errors.New("fake live stop")

type fakeLiveObjects struct {
	closed bool
}

func (o *fakeLiveObjects) LaunchProgram() any { return struct{}{} }

func (o *fakeLiveObjects) EventsHandle() any { return struct{}{} }

func (o *fakeLiveObjects) StacksHandle() stackBytesLookup { return nil }

func (o *fakeLiveObjects) Close() error {
	o.closed = true
	return nil
}

type fakeExecutable struct {
	path   string
	symbol string
	pid    int
	closed bool
}

func (e *fakeExecutable) Uprobe(symbol string, _ any, pid int) (io.Closer, error) {
	e.symbol = symbol
	e.pid = pid
	return fakeCloser{closeFn: func() error {
		e.closed = true
		return nil
	}}, nil
}

type fakeCloser struct {
	closeFn func() error
}

func (c fakeCloser) Close() error {
	if c.closeFn != nil {
		return c.closeFn()
	}
	return nil
}

type fakeReader struct {
	samples   [][]byte
	errs      []error
	readIndex int
	closed    bool
}

func (r *fakeReader) Read() ([]byte, error) {
	if r.readIndex < len(r.samples) {
		sample := r.samples[r.readIndex]
		r.readIndex++
		return sample, nil
	}
	if idx := r.readIndex - len(r.samples); idx < len(r.errs) {
		r.readIndex++
		return nil, r.errs[idx]
	}
	return nil, io.EOF
}

func (r *fakeReader) SetDeadline(time.Time) {}

func (r *fakeReader) Close() error {
	r.closed = true
	return nil
}

func TestRunLiveAttachesConfiguredSymbolAndEmitsLaunchRecord(t *testing.T) {
	src, err := New(Config{
		PID:         123,
		LibraryPath: "/opt/rocm/lib/libamdhip64.so",
		Symbol:      "hipLaunchKernel",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	src.decoder = recordDecoder{
		resolveKernel: func(pid uint32, addr uint64) (string, bool) {
			if pid != 123 || addr != 0x1234 {
				t.Fatalf("resolveKernel(%d, %#x)", pid, addr)
			}
			return "hip_kernel", true
		},
		resolveStack: func(pid uint32, stackID int32) []pp.Frame {
			if pid != 123 || stackID != 7 {
				t.Fatalf("resolveStack(%d, %d)", pid, stackID)
			}
			return []pp.Frame{pp.FrameFromName("train_step")}
		},
	}

	objs := &fakeLiveObjects{}
	exec := &fakeExecutable{}
	reader := &fakeReader{
		samples: [][]byte{mustEncodeRawRecord(t, rawRecord{
			PID:          123,
			TID:          456,
			TimeNs:       789,
			FunctionAddr: 0x1234,
			UserStackID:  7,
			Stream:       0xbeef,
		})},
		errs: []error{errFakeLiveStop},
	}
	src.live = liveDeps{
		load: func(pid uint32) (liveObjects, error) {
			if pid != 123 {
				t.Fatalf("load(%d)", pid)
			}
			return objs, nil
		},
		openExecutable: func(path string) (uprobeExecutable, error) {
			exec.path = path
			return exec, nil
		},
		newReader: func(got liveObjects) (liveReader, error) {
			if gotObjs, ok := got.(*fakeLiveObjects); !ok || gotObjs != objs {
				t.Fatal("unexpected objects passed to reader")
			}
			return reader, nil
		},
	}

	var sink captureSink
	err = src.runLive(context.Background(), &sink)
	if !errors.Is(err, errFakeLiveStop) {
		t.Fatalf("runLive err=%v", err)
	}
	if exec.path != "/opt/rocm/lib/libamdhip64.so" {
		t.Fatalf("path=%q", exec.path)
	}
	if exec.symbol != "hipLaunchKernel" {
		t.Fatalf("symbol=%q", exec.symbol)
	}
	if exec.pid != 123 {
		t.Fatalf("pid=%d", exec.pid)
	}
	if got := len(sink.records); got != 1 {
		t.Fatalf("records=%d", got)
	}
	if sink.records[0].KernelName != "hip_kernel" {
		t.Fatalf("kernel=%q", sink.records[0].KernelName)
	}
	if !objs.closed {
		t.Fatal("expected objects close")
	}
	if !exec.closed {
		t.Fatal("expected uprobe close")
	}
	if !reader.closed {
		t.Fatal("expected reader close")
	}
}

func mustEncodeRawRecord(t *testing.T, record rawRecord) []byte {
	t.Helper()

	var payload bytes.Buffer
	if err := binary.Write(&payload, binary.LittleEndian, record); err != nil {
		t.Fatalf("write raw record: %v", err)
	}
	return payload.Bytes()
}
