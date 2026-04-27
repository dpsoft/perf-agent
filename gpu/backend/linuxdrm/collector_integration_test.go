package linuxdrm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
	if event.Kind != gpu.TimelineEventIOCtl {
		return
	}
	select {
	case s.events <- event:
	default:
	}
}

func TestLiveEventSinkQueuesOnlyIOCtlEvents(t *testing.T) {
	sink := &liveEventSink{events: make(chan gpu.GPUTimelineEvent, 1)}
	sink.EmitEvent(gpu.GPUTimelineEvent{Kind: gpu.TimelineEventWait})
	select {
	case event := <-sink.events:
		t.Fatalf("unexpected event queued: %#v", event)
	default:
	}

	want := gpu.GPUTimelineEvent{Kind: gpu.TimelineEventIOCtl, Name: "drm-syncobj-wait"}
	sink.EmitEvent(want)
	select {
	case got := <-sink.events:
		if got.Kind != want.Kind || got.Name != want.Name {
			t.Fatalf("event=%#v want kind=%q name=%q", got, want.Kind, want.Name)
		}
	default:
		t.Fatal("expected ioctl event")
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
	defer func() {
		if err := b.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}()

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
		case <-b.done:
			t.Fatalf("linuxdrm backend exited early: %v", b.err())
		case <-ctx.Done():
			if err := b.Stop(context.Background()); err != nil {
				t.Fatalf("timed out waiting for linuxdrm ioctl event: backend error: %v", err)
			}
			t.Fatal("timed out waiting for linuxdrm ioctl event")
		}
	}
}

func TestLinuxDRMAMDGPUObservation(t *testing.T) {
	requireBPFCaps(t)

	renderNode, err := firstRenderNode()
	if err != nil {
		t.Skipf("no DRM render node: %v", err)
	}
	info, ok := lookupDRMDeviceInfo(drmMajor, renderNodeMinor(renderNode))
	if !ok || info.Driver != "amdgpu" {
		t.Skip("render node is not backed by amdgpu")
	}

	workload, args, err := firstAMDGPUWorkload()
	if err != nil {
		t.Skipf("no amdgpu workload tool: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmdArgs := append([]string{"-lc", `sleep 1; exec "$0" "$@"`, workload}, args...)
	cmd := exec.CommandContext(ctx, "/bin/sh", cmdArgs...)
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devNull.Close()
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", workload, err)
	}
	defer func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	b, err := New(Config{PID: cmd.Process.Pid})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = b.Close() }()

	sink := &liveEventSink{events: make(chan gpu.GPUTimelineEvent, 64)}
	if err := b.Start(ctx, sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		if err := b.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}()

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
	}()

	for {
		select {
		case event := <-sink.events:
			if event.PID != uint32(cmd.Process.Pid) {
				continue
			}
			if !strings.HasPrefix(event.Name, "amdgpu-") {
				continue
			}
			if got := event.Attributes["command_family"]; got != "amdgpu" {
				t.Fatalf("command_family=%q", got)
			}
			return
		case err := <-waitErr:
			if err != nil {
				t.Fatalf("amdgpu workload failed: %v", err)
			}
			t.Fatal("amdgpu workload exited before any amdgpu ioctl event was observed")
		case <-b.done:
			t.Fatalf("linuxdrm backend exited early: %v", b.err())
		case <-ctx.Done():
			if err := b.Stop(context.Background()); err != nil {
				t.Fatalf("timed out waiting for amdgpu ioctl event: backend error: %v", err)
			}
			t.Fatal("timed out waiting for amdgpu ioctl event")
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

func renderNodeMinor(path string) uint32 {
	base := filepath.Base(path)
	value := strings.TrimPrefix(base, "renderD")
	minor, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(minor)
}

func firstAMDGPUWorkload() (string, []string, error) {
	for _, candidate := range []struct {
		bin  string
		args []string
	}{
		{bin: "rocminfo"},
		{bin: "amdgpu-arch"},
	} {
		path, err := exec.LookPath(candidate.bin)
		if err == nil {
			return path, candidate.args, nil
		}
	}
	return "", nil, fmt.Errorf("no supported amdgpu workload tool found")
}
