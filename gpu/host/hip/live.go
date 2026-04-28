package hip

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/dpsoft/perf-agent/gpu/host"
)

type liveObjects interface {
	LaunchProgram() any
	EventsHandle() any
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
		load: func(uint32) (liveObjects, error) {
			return nil, errLiveNotImplemented
		},
		openExecutable: func(string) (uprobeExecutable, error) {
			return nil, errLiveNotImplemented
		},
		newReader: func(liveObjects) (liveReader, error) {
			return nil, errLiveNotImplemented
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
		launch, err := s.decode(record)
		if err != nil {
			return err
		}
		if err := sink.EmitLaunchRecord(launch.toHostRecord()); err != nil {
			return err
		}
	}
}
