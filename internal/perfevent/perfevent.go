// Package perfevent opens per-CPU software perf_event_open events and
// attaches a BPF program to each. It exists to deduplicate the boilerplate
// that profile.NewProfiler and dwarfagent.NewProfilerWithMode each used to
// spell out independently — same PerfEventAttr, same per-CPU loop, same
// "tolerate ESRCH on offline CPU" rule, same cleanup-on-error.
package perfevent

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

// Set is a bundle of per-CPU perf events with their attached BPF links.
// Close releases everything in the right order; safe to call once.
type Set struct {
	fds   []int
	links []link.Link
}

// FDs returns the underlying perf_event file descriptors. Callers should
// treat the returned slice as read-only — the Set still owns these fds and
// will close them via Close. Used by collectors that need to read counter
// values directly (currently none, but PMU-side collectors might).
func (s *Set) FDs() []int { return s.fds }

// Close closes every attached link and then every fd. Errors are
// best-effort — the first non-nil error is returned, but cleanup proceeds
// regardless.
func (s *Set) Close() error {
	if s == nil {
		return nil
	}
	var firstErr error
	for _, l := range s.links {
		if err := l.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, fd := range s.fds {
		if err := unix.Close(fd); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.links = nil
	s.fds = nil
	return firstErr
}

// Option configures OpenAll.
type Option func(*config)

type config struct {
	// deferEnable causes OpenAll to set PerfBitDisabled at perf_event_open
	// time and call PERF_EVENT_IOC_ENABLE after the BPF link is attached.
	// Eliminates the (tiny) race window where the event fires between
	// open and attach. dwarfagent uses this; profile historically did not.
	deferEnable bool
}

// WithDeferredEnable opens each event disabled and enables it after the
// BPF program is attached. Recommended for new call sites.
func WithDeferredEnable() Option { return func(c *config) { c.deferEnable = true } }

// OpenAll opens one perf event per CPU configured per spec (Type/Config
// determine the event source; SamplePeriod is interpreted as a frequency
// in Hz when spec.Frequency is true, otherwise as a fixed sample period),
// attaches prog to each via link.AttachRawLink, and returns the resulting Set.
//
// Offline CPUs (ESRCH from perf_event_open) are skipped silently — they
// come back online or stay offline, neither is an error here. If every CPU
// is offline / no event was attached, OpenAll returns an error.
//
// On any failure mid-loop, every fd and link opened so far is released
// before returning. Caller never has to clean up after a non-nil error.
func OpenAll(prog *ebpf.Program, cpus []uint, spec EventSpec, opts ...Option) (*Set, error) {
	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}

	var bits uint64
	if spec.Frequency {
		bits |= unix.PerfBitFreq
	}
	if cfg.deferEnable {
		bits |= unix.PerfBitDisabled
	}
	attr := &unix.PerfEventAttr{
		Type:   spec.Type,
		Config: spec.Config,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: spec.SamplePeriod,
		Bits:   bits,
	}

	s := &Set{}
	for _, cpu := range cpus {
		fd, err := unix.PerfEventOpen(attr, -1, int(cpu), -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			_ = s.Close()
			return nil, fmt.Errorf("perf_event_open cpu=%d: %w", cpu, err)
		}
		s.fds = append(s.fds, fd)

		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target:  fd,
			Program: prog,
			Attach:  ebpf.AttachPerfEvent,
		})
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("attach perf event cpu=%d: %w", cpu, err)
		}
		s.links = append(s.links, rl)

		if cfg.deferEnable {
			if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
				_ = s.Close()
				return nil, fmt.Errorf("enable perf event cpu=%d: %w", cpu, err)
			}
		}
	}

	if len(s.fds) == 0 {
		return nil, fmt.Errorf("no perf events attached (cpus=%d, all offline?)", len(cpus))
	}
	return s, nil
}
