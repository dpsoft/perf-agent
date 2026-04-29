package linuxdrm

import (
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/dpsoft/perf-agent/gpu"
)

var (
	errEventSinkRequired = errors.New("event sink is required")
	errBackendStarted    = errors.New("linuxdrm backend already started")
	baseCapabilities     = []gpu.GPUCapability{
		gpu.CapabilityLifecycleTimeline,
	}
)

func debugGPULivef(format string, args ...any) {
	if os.Getenv("PERF_AGENT_DEBUG_GPU_LIVE") == "" {
		return
	}
	log.Printf("gpu-live-debug: "+format, args...)
}

type Backend struct {
	cfg Config

	cgroups *cgroupPathCache

	mu      sync.Mutex
	started bool
	done    chan struct{}
	cancel  context.CancelCauseFunc
	runErr  error
	objs    *Objects
	reader  *ringbuf.Reader
	enterTP link.Link
	exitTP  link.Link
	wakeup  link.Link
	wakeupN link.Link
	switchT link.Link
}

func New(cfg Config) (*Backend, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &Backend{
		cfg:     cfg,
		cgroups: newCgroupPathCache(lookupCgroupPath),
	}, nil
}

func (b *Backend) ID() gpu.GPUBackendID {
	return gpu.BackendLinuxDRM
}

func (b *Backend) Capabilities() []gpu.GPUCapability {
	return slices.Clone(baseCapabilities)
}

func (b *Backend) Start(ctx context.Context, sink gpu.EventSink) error {
	if sink == nil {
		return errEventSinkRequired
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.started {
		return errBackendStarted
	}

	runCtx, cancel := context.WithCancelCause(ctx)
	if b.cfg.testRun == nil && len(b.cfg.testRecords) == 0 {
		if err := b.openLive(); err != nil {
			cancel(err)
			return err
		}
	}
	b.cancel = cancel
	b.done = make(chan struct{})
	b.runErr = nil
	b.started = true

	go b.run(runCtx, sink)
	return nil
}

func (b *Backend) run(ctx context.Context, sink gpu.EventSink) {
	defer close(b.done)

	if b.cfg.testRun != nil {
		if err := b.cfg.testRun(ctx, sink); err != nil {
			b.setRunErr(err)
		}
		return
	}
	if err := b.emitTestRecords(sink); err != nil {
		b.setRunErr(err)
		return
	}
	if len(b.cfg.testRecords) > 0 {
		<-ctx.Done()
		return
	}

	if err := b.runLive(ctx, sink); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ringbuf.ErrClosed) {
			return
		}
		b.setRunErr(err)
	}
}

func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	done := b.done
	cancel := b.cancel
	b.mu.Unlock()

	if done == nil {
		debugGPULivef("linuxdrm stop: backend not started")
		return nil
	}
	if cancel != nil {
		debugGPULivef("linuxdrm stop: canceling run context")
		cancel(context.Canceled)
	}
	debugGPULivef("linuxdrm stop: closing ringbuf reader")
	b.closeReader()

	select {
	case <-done:
		debugGPULivef("linuxdrm stop: run goroutine finished")
		return b.err()
	case <-ctx.Done():
		debugGPULivef("linuxdrm stop: stop context done: %v", context.Cause(ctx))
		return context.Cause(ctx)
	}
}

func (b *Backend) Close() error {
	b.closeReader()
	return errors.Join(
		closeLink(b.enterTP),
		closeLink(b.exitTP),
		closeLink(b.wakeup),
		closeLink(b.wakeupN),
		closeLink(b.switchT),
		closeObjects(b.objs),
	)
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

func (b *Backend) emitTestRecords(sink gpu.EventSink) error {
	for _, record := range b.cfg.testRecords {
		if err := b.emitRecord(record, sink); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) openLive() error {
	objs, err := Load(uint32(b.cfg.PID))
	if err != nil {
		return err
	}

	enterTP, err := link.Tracepoint("syscalls", "sys_enter_ioctl", objs.EnterProgram(), nil)
	if err != nil {
		_ = objs.Close()
		return err
	}
	exitTP, err := link.Tracepoint("syscalls", "sys_exit_ioctl", objs.ExitProgram(), nil)
	if err != nil {
		_ = enterTP.Close()
		_ = objs.Close()
		return err
	}
	wakeup, err := link.AttachTracing(link.TracingOptions{Program: objs.WakeupProgram()})
	if err != nil {
		_ = exitTP.Close()
		_ = enterTP.Close()
		_ = objs.Close()
		return err
	}
	wakeupN, err := link.AttachTracing(link.TracingOptions{Program: objs.WakeupNewProgram()})
	if err != nil {
		_ = wakeup.Close()
		_ = exitTP.Close()
		_ = enterTP.Close()
		_ = objs.Close()
		return err
	}
	switchT, err := link.AttachTracing(link.TracingOptions{Program: objs.SwitchProgram()})
	if err != nil {
		_ = wakeupN.Close()
		_ = wakeup.Close()
		_ = exitTP.Close()
		_ = enterTP.Close()
		_ = objs.Close()
		return err
	}
	reader, err := ringbuf.NewReader(objs.EventsMap())
	if err != nil {
		_ = switchT.Close()
		_ = wakeupN.Close()
		_ = wakeup.Close()
		_ = exitTP.Close()
		_ = enterTP.Close()
		_ = objs.Close()
		return err
	}

	b.objs = objs
	b.enterTP = enterTP
	b.exitTP = exitTP
	b.wakeup = wakeup
	b.wakeupN = wakeupN
	b.switchT = switchT
	b.reader = reader
	return nil
}

func (b *Backend) runLive(ctx context.Context, sink gpu.EventSink) error {
	for {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}

		b.mu.Lock()
		reader := b.reader
		b.mu.Unlock()
		if reader == nil {
			return nil
		}
		reader.SetDeadline(time.Now().Add(200 * time.Millisecond))

		record, err := reader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return err
		}

		decoded, err := decodeRecord(record.RawSample)
		if err != nil {
			return err
		}
		if err := b.emitRecord(decoded, sink); err != nil {
			return err
		}
	}
}

func (b *Backend) emitRecord(record rawRecord, sink gpu.EventSink) error {
	event, err := normalizeRecordWithResolvers(record, lookupDRMDeviceInfo, b.cgroups.Lookup)
	if err != nil {
		return err
	}
	sink.EmitEvent(event)
	return nil
}

func (b *Backend) closeReader() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reader != nil {
		_ = b.reader.Close()
		b.reader = nil
	}
}

func closeLink(tp link.Link) error {
	if tp == nil {
		return nil
	}
	return tp.Close()
}

func closeObjects(objs *Objects) error {
	if objs == nil {
		return nil
	}
	return objs.Close()
}
