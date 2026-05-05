package hip

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/dpsoft/perf-agent/gpu/host"
)

var errLiveNotImplemented = errors.New("hip live uprobes are not implemented yet")
var errSourceStarted = errors.New("hip host source already started")

type Source struct {
	cfg       Config
	decoder   recordDecoder
	startLive func(context.Context, host.HostSink) error
	live      liveDeps

	mu      sync.Mutex
	started bool
	done    chan struct{}
	cancel  context.CancelCauseFunc
	runErr  error
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errSourceStarted
	}

	if len(s.cfg.testRecords) > 0 {
		for _, record := range s.cfg.testRecords {
			launch, err := s.decode(record)
			if err != nil {
				return err
			}
			if err := sink.EmitLaunchRecord(launch.toHostRecord()); err != nil {
				return err
			}
		}
		s.started = true
		return nil
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	s.runErr = nil
	s.started = true
	go s.run(runCtx, sink)
	return nil
}

func (s *Source) run(ctx context.Context, sink host.HostSink) {
	defer close(s.done)
	if err := s.startLive(ctx, sink); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.mu.Lock()
		s.runErr = err
		s.mu.Unlock()
	}
}

func (s *Source) Stop(ctx context.Context) error {
	s.mu.Lock()
	done := s.done
	cancel := s.cancel
	s.mu.Unlock()

	if done == nil {
		return nil
	}
	if cancel != nil {
		cancel(context.Canceled)
	}

	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.runErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

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
