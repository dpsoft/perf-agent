package hip

import (
	"context"
	"errors"
	"fmt"

	"github.com/dpsoft/perf-agent/gpu/host"
)

var errLiveNotImplemented = errors.New("hip live uprobes are not implemented yet")

type Source struct {
	cfg       Config
	decoder   recordDecoder
	startLive func(context.Context, host.HostSink) error
	live      liveDeps
}

func New(cfg Config) (*Source, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	src := &Source{cfg: cfg}
	src.startLive = src.runLive
	src.live = defaultLiveDeps()
	return src, nil
}

func (s *Source) ID() string { return "hip-uprobes" }

func (s *Source) Start(ctx context.Context, sink host.HostSink) error {
	if len(s.cfg.testRecords) == 0 {
		return s.startLive(ctx, sink)
	}
	for _, record := range s.cfg.testRecords {
		launch, err := s.decode(record)
		if err != nil {
			return err
		}
		if err := sink.EmitLaunchRecord(launch.toHostRecord()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) Stop(context.Context) error { return nil }

func (s *Source) Close() error { return nil }

func (s *Source) decode(record rawRecord) (launchRecord, error) {
	if s.cfg.testDecode != nil {
		launch, err := s.cfg.testDecode(record)
		if err != nil {
			return launchRecord{}, err
		}
		return launch, nil
	}
	if s.decoder.resolveKernel == nil {
		return launchRecord{}, fmt.Errorf("hip source live decode not configured")
	}
	return s.decoder.decode(record)
}
