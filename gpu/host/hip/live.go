package hip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/gpu/host"
	pp "github.com/dpsoft/perf-agent/pprof"
)

type liveObjects interface {
	LaunchProgram() any
	EventsHandle() any
	StacksHandle() stackBytesLookup
	Close() error
}

type uprobeExecutable interface {
	Uprobe(symbol string, program any, pid int) (io.Closer, error)
}

type liveReader interface {
	Read() ([]byte, error)
	SetDeadline(time.Time)
	Close() error
}

type liveDeps struct {
	load           func(pid uint32) (liveObjects, error)
	openExecutable func(path string) (uprobeExecutable, error)
	newReader      func(liveObjects) (liveReader, error)
}

func defaultLiveDeps() liveDeps {
	return liveDeps{
		load: func(pid uint32) (liveObjects, error) {
			return Load(pid)
		},
		openExecutable: func(path string) (uprobeExecutable, error) {
			executable, err := link.OpenExecutable(path)
			if err != nil {
				return nil, err
			}
			return executableWrapper{Executable: executable}, nil
		},
		newReader: func(objs liveObjects) (liveReader, error) {
			rawMap, ok := objs.EventsHandle().(*ebpf.Map)
			if !ok || rawMap == nil {
				return nil, fmt.Errorf("hip live events map is unavailable")
			}
			reader, err := ringbuf.NewReader(rawMap)
			if err != nil {
				return nil, err
			}
			return ringbufReader{Reader: reader}, nil
		},
	}
}

func (s *Source) runLive(ctx context.Context, sink host.HostSink) error {
	objs, err := s.live.load(uint32(s.cfg.PID))
	if err != nil {
		return err
	}
	defer func() { _ = objs.Close() }()

	executable, err := s.live.openExecutable(s.cfg.LibraryPath)
	if err != nil {
		return err
	}
	uprobe, err := executable.Uprobe(s.cfg.Symbol, objs.LaunchProgram(), s.cfg.PID)
	if err != nil {
		return err
	}
	defer func() { _ = uprobe.Close() }()

	reader, err := s.live.newReader(objs)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	decoder := s.decoder
	var symbolizer processSymbolizer
	if decoder.resolveKernel == nil || decoder.resolveStack == nil {
		var err error
		symbolizer, err = blazesym.NewSymbolizer(
			blazesym.SymbolizerWithCodeInfo(true),
			blazesym.SymbolizerWithInlinedFns(true),
		)
		if err != nil {
			return fmt.Errorf("create hip live symbolizer: %w", err)
		}
		defer symbolizer.Close()
	}
	if decoder.resolveKernel == nil {
		decoder.resolveKernel = func(pid uint32, addr uint64) (string, bool) {
			return resolveKernelName(symbolizer, pid, addr), true
		}
	}
	if decoder.resolveStack == nil {
		decoder.resolveStack = func(pid uint32, stackID int32) []pp.Frame {
			return resolveStackFrames(symbolizer, objs.StacksHandle(), pid, stackID)
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}

		reader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		raw, err := reader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return err
		}

		record, err := decodeRecord(raw)
		if err != nil {
			return err
		}
		launch, err := decoder.decode(record)
		if err != nil {
			return err
		}
		if err := sink.EmitLaunchRecord(launch.toHostRecord()); err != nil {
			return err
		}
	}
}

type executableWrapper struct {
	*link.Executable
}

func (e executableWrapper) Uprobe(symbol string, program any, pid int) (io.Closer, error) {
	prog, ok := program.(*ebpf.Program)
	if !ok || prog == nil {
		return nil, fmt.Errorf("hip launch program is unavailable")
	}
	return e.Executable.Uprobe(symbol, prog, &link.UprobeOptions{PID: pid})
}

type ringbufReader struct {
	*ringbuf.Reader
}

func (r ringbufReader) Read() ([]byte, error) {
	record, err := r.Reader.Read()
	if err != nil {
		return nil, err
	}
	return record.RawSample, nil
}
