package stream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpu/codec"
)

type Backend struct {
	r io.Reader

	mu      sync.Mutex
	started bool
	done    chan struct{}
	cancel  context.CancelCauseFunc
	runErr  error
}

func New(r io.Reader) *Backend {
	return &Backend{r: r}
}

func (b *Backend) ID() gpu.GPUBackendID {
	return "stream"
}

func (b *Backend) Capabilities() []gpu.GPUCapability {
	return []gpu.GPUCapability{
		gpu.CapabilityLaunchTrace,
		gpu.CapabilityExecTimeline,
		gpu.CapabilityDeviceCounters,
		gpu.CapabilityPCSampling,
		gpu.CapabilityStallReasons,
		gpu.CapabilitySourceMap,
	}
}

func (b *Backend) Start(ctx context.Context, sink gpu.EventSink) error {
	if sink == nil {
		return fmt.Errorf("event sink is required")
	}
	if b.r == nil {
		return fmt.Errorf("stream reader is required")
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return fmt.Errorf("stream backend already started")
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	b.cancel = cancel
	b.done = make(chan struct{})
	b.started = true
	go b.run(runCtx, sink)
	return nil
}

func (b *Backend) run(ctx context.Context, sink gpu.EventSink) {
	defer close(b.done)

	scanner := bufio.NewScanner(b.r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if err := emitDecodedLine(scanner.Bytes(), sink); err != nil {
			b.setRunErr(err)
			return
		}
		if err := context.Cause(ctx); err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			b.setRunErr(err)
			return
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		b.setRunErr(fmt.Errorf("scan stream: %w", err))
		return
	}
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		b.setRunErr(err)
	}
}

func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	done := b.done
	b.mu.Unlock()

	if done == nil {
		return nil
	}

	select {
	case <-done:
		return b.err()
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (b *Backend) Close() error {
	if closer, ok := b.r.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func (b *Backend) setRunErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.runErr = err
}

func (b *Backend) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.runErr
}

func emitDecodedLine(line []byte, sink gpu.EventSink) error {
	event, err := codec.DecodeLine(line)
	if err != nil {
		return fmt.Errorf("decode line: %w", err)
	}

	switch event.Kind {
	case codec.KindLaunch:
		sink.EmitLaunch(event.Launch)
	case codec.KindExec:
		sink.EmitExec(event.Exec)
	case codec.KindCounter:
		sink.EmitCounter(event.Counter)
	case codec.KindSample:
		sink.EmitSample(event.Sample)
	case codec.KindEvent:
		sink.EmitEvent(event.Event)
	default:
		return fmt.Errorf("unsupported decoded event kind %q", event.Kind)
	}
	return nil
}
