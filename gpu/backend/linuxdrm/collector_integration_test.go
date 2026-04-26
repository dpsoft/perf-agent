package linuxdrm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/gpu"
	"golang.org/x/sys/unix"
	"kernel.org/pub/linux/libs/security/libcap/cap"
)

type liveEventSink struct {
	events chan gpu.GPUTimelineEvent
}

func (s *liveEventSink) EmitLaunch(gpu.GPUKernelLaunch)   {}
func (s *liveEventSink) EmitExec(gpu.GPUKernelExec)       {}
func (s *liveEventSink) EmitCounter(gpu.GPUCounterSample) {}
func (s *liveEventSink) EmitSample(gpu.GPUSample)         {}
func (s *liveEventSink) EmitEvent(event gpu.GPUTimelineEvent) {
	select {
	case s.events <- event:
	default:
	}
}

func TestLinuxDRMLiveSmoke(t *testing.T) {
	requireBPFCaps(t)

	renderNode, err := firstRenderNode()
	if err != nil {
		t.Skipf("no DRM render node: %v", err)
	}

	f, err := os.OpenFile(renderNode, os.O_RDWR, 0)
	if err != nil {
		t.Skipf("open render node: %v", err)
	}
	defer f.Close()

	b, err := New(Config{PID: os.Getpid()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()

	sink := &liveEventSink{events: make(chan gpu.GPUTimelineEvent, 16)}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := b.Start(ctx, sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = b.Stop(context.Background()) }()

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), uintptr(0), uintptr(0))
	if errno != 0 && errno != unix.ENOTTY && errno != unix.EINVAL {
		t.Skipf("render-node ioctl returned unexpected errno: %v", errno)
	}

	fd := int32(f.Fd())
	for {
		select {
		case event := <-sink.events:
			if event.Kind != gpu.TimelineEventIOCtl {
				continue
			}
			if event.PID != uint32(os.Getpid()) {
				continue
			}
			if event.FD != fd {
				continue
			}
			if event.Source != "ebpf" {
				t.Fatalf("source=%q", event.Source)
			}
			if got := event.Attributes["command"]; got != "0" {
				t.Fatalf("command=%q", got)
			}
			return
		case <-ctx.Done():
			t.Fatal("timed out waiting for linuxdrm ioctl event")
		}
	}
}

func requireBPFCaps(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	caps := cap.GetProc()
	have, err := caps.GetFlag(cap.Permitted, cap.BPF)
	if err != nil {
		t.Skipf("check caps: %v", err)
	}
	if !have {
		t.Skip("CAP_BPF not in permitted set")
	}
	for _, c := range []cap.Value{cap.SYS_ADMIN, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE} {
		have, err := caps.GetFlag(cap.Permitted, c)
		if err != nil {
			t.Skipf("check caps: %v", err)
		}
		if !have {
			t.Skipf("%v not in permitted set", c)
		}
	}
}

func firstRenderNode() (string, error) {
	matches, err := filepath.Glob("/dev/dri/renderD*")
	if err != nil {
		return "", err
	}
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeDevice == 0 {
			continue
		}
		return match, nil
	}
	return "", errors.New("no render nodes found")
}
