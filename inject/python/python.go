package python

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Manager.
type Options struct {
	// StrictPerPID makes ActivateAll fail-fast on the first error. Used for
	// --pid N --inject-python; lenient (false) for -a --inject-python.
	StrictPerPID bool

	// Logger receives structured log lines for every detect/activate/deactivate
	// event. nil → slog.Default().
	Logger *slog.Logger

	// Injector overrides the default ptraceop-based injector. Tests inject
	// stubs; production passes nil and gets the real ptraceop.Injector.
	// In production this is wired by perfagent.Agent (Task 8) since this
	// package does not import inject/ptraceop directly.
	Injector LowLevelInjector

	// Detector overrides the default /proc-based detector. Tests inject stubs;
	// production passes nil.
	Detector Detector

	// DeactivateDeadline caps the total time spent in DeactivateAll. Default
	// 5 seconds.
	DeactivateDeadline time.Duration
}

// LowLevelInjector is the contract Manager uses for the ptrace dance. The
// production implementation wraps inject/ptraceop.Injector; tests can stub it.
type LowLevelInjector interface {
	RemoteActivate(pid uint32, addrs SymbolAddrsForTarget) error
	RemoteDeactivate(pid uint32, addrs SymbolAddrsForTarget) error
}

// SymbolAddrsForTarget is the data the LowLevelInjector needs to perform one
// remote-call sequence — independent of inject/ptraceop's exact struct layout
// to keep the test boundary clean.
type SymbolAddrsForTarget struct {
	PyGILEnsure  uint64
	PyGILRelease uint64
	PyRunString  uint64
}

// Stats holds counters that operators inspect after a run. All counters are
// safe for concurrent use.
type Stats struct {
	Activated        atomic.Uint64
	Deactivated      atomic.Uint64
	SkippedNotPython atomic.Uint64
	SkippedTooOld    atomic.Uint64
	SkippedNoTramp   atomic.Uint64
	SkippedNoSymbols atomic.Uint64
	ActivateFailed   atomic.Uint64
	DeactivateFailed atomic.Uint64
}

// Manager orchestrates Python perf-trampoline injection across a profile run:
// detection ladder, strict/lenient policy, in-memory tracked-PID set, bounded
// shutdown deactivation, and idempotent late-arrival activation.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	tracked map[uint32]*trackedTarget
	pending sync.Map // pid uint32 → struct{} (in-flight late activation)

	stats Stats
}

type trackedTarget struct {
	target      *Target
	activatedAt time.Time
}

// NewManager constructs a Manager.
//
// Defaults:
//   - opts.Logger             → slog.Default() if nil
//   - opts.DeactivateDeadline → 5 * time.Second if zero
//   - opts.Detector           → NewDetector("/proc", logger) if nil
//
// opts.Injector has no in-package default — inject/python deliberately does
// not import inject/ptraceop, so the low-level injector must be supplied by
// the caller. perfagent.Agent wires it via the ptraceopBridge; tests inject
// stubs. NewManager panics with a clear message if Injector is nil; surfacing
// that wiring mistake at construction is friendlier than the deep NPE the
// first ActivateAll call would otherwise raise.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.DeactivateDeadline == 0 {
		opts.DeactivateDeadline = 5 * time.Second
	}
	if opts.Detector == nil {
		opts.Detector = NewDetector("/proc", opts.Logger)
	}
	if opts.Injector == nil {
		panic("python.NewManager: opts.Injector is required (perfagent.Agent wires this; tests inject stubs)")
	}
	return &Manager{
		opts:    opts,
		log:     opts.Logger,
		tracked: make(map[uint32]*trackedTarget),
	}
}

// Stats returns a pointer to the in-place atomic counters. Callers should
// treat it as read-only; mutate via the atomic methods if needed (currently
// only the package itself mutates them).
func (m *Manager) Stats() *Stats { return &m.stats }

// ActivateAll runs detection and activation for the given PIDs. Returns nil
// in lenient mode (errors are logged + counted); returns the first error in
// strict mode. Caller blocks on this; it is fine to call once at profile
// start before BPF attach.
func (m *Manager) ActivateAll(pids []uint32) error {
	for _, pid := range pids {
		if err := m.activateOne(pid); err != nil {
			if m.opts.StrictPerPID {
				return err
			}
		}
	}
	m.logSummary()
	return nil
}

// ActivateLate is the mmap-watcher hook for new exec events during -a mode.
// Always lenient (logs + counts on error). Idempotent: dedupes by tracked
// and pending sets.
func (m *Manager) ActivateLate(pid uint32) {
	if _, loaded := m.pending.LoadOrStore(pid, struct{}{}); loaded {
		return // in-flight activation for this pid
	}
	defer m.pending.Delete(pid)

	m.mu.Lock()
	_, already := m.tracked[pid]
	m.mu.Unlock()
	if already {
		return
	}
	_ = m.activateOne(pid) // lenient: errors logged inside
}

// DeactivateAll runs the bounded shutdown deactivation pass. Tolerates ESRCH
// (process gone). Honors ctx cancellation AND the configured deactivation
// deadline (5s default).
func (m *Manager) DeactivateAll(ctx context.Context) {
	deadline, cancel := context.WithTimeout(ctx, m.opts.DeactivateDeadline)
	defer cancel()

	snapshot := m.snapshotTracked()
	for pid, tt := range snapshot {
		select {
		case <-deadline.Done():
			m.log.Warn("python deactivate cancelled (deadline or ctx)",
				"abandoned", len(snapshot)-int(m.stats.Deactivated.Load()))
			return
		default:
		}
		addrs := SymbolAddrsForTarget{
			PyGILEnsure:  tt.target.PyGILEnsureAddr,
			PyGILRelease: tt.target.PyGILReleaseAddr,
			PyRunString:  tt.target.PyRunStringAddr,
		}
		if err := m.opts.Injector.RemoteDeactivate(pid, addrs); err != nil {
			if isProcessGone(err) {
				continue
			}
			m.log.Warn("python deactivate failed", "pid", pid, "err", err)
			m.stats.DeactivateFailed.Add(1)
			continue
		}
		m.stats.Deactivated.Add(1)
	}
}
