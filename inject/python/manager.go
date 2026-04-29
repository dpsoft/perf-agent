package python

import (
	"errors"
	"fmt"
	"syscall"
	"time"
)

// activateOne runs detection + activation for one PID. Always returns nil in
// lenient mode after logging; returns wrapped error in strict mode.
func (m *Manager) activateOne(pid uint32) error {
	target, err := m.opts.Detector.Detect(pid)
	if err != nil {
		m.recordSkipReason(err)
		m.log.Warn("python inject skipped",
			"pid", pid, "reason", reasonString(err))
		if m.opts.StrictPerPID {
			return fmt.Errorf("inject pid=%d: %w", pid, err)
		}
		return nil
	}

	addrs := SymbolAddrsForTarget{
		PyGILEnsure:  target.PyGILEnsureAddr,
		PyGILRelease: target.PyGILReleaseAddr,
		PyRunString:  target.PyRunStringAddr,
	}
	if err := m.opts.Injector.RemoteActivate(pid, addrs); err != nil {
		m.stats.ActivateFailed.Add(1)
		m.log.Warn("python inject failed", "pid", pid, "err", err)
		if m.opts.StrictPerPID {
			return fmt.Errorf("activate pid=%d: %w", pid, err)
		}
		return nil
	}
	m.mu.Lock()
	m.tracked[pid] = &trackedTarget{target: target, activatedAt: time.Now()}
	m.mu.Unlock()
	m.stats.Activated.Add(1)
	m.log.Info("python inject activated",
		"pid", pid, "libpython", target.LibPythonPath,
		"version", fmt.Sprintf("%d.%d", target.Major, target.Minor))
	return nil
}

func (m *Manager) recordSkipReason(err error) {
	switch {
	case errors.Is(err, ErrPythonTooOld):
		m.stats.SkippedTooOld.Add(1)
	case errors.Is(err, ErrNoPerfTrampoline):
		m.stats.SkippedNoTramp.Add(1)
	case errors.Is(err, ErrStaticallyLinkedNoSymbols):
		m.stats.SkippedNoSymbols.Add(1)
	case errors.Is(err, ErrNotPython):
		m.stats.SkippedNotPython.Add(1)
	default:
		m.stats.SkippedNotPython.Add(1) // unknown errors classified as "not python"
	}
}

func reasonString(err error) string {
	switch {
	case errors.Is(err, ErrPythonTooOld):
		return "python_too_old"
	case errors.Is(err, ErrNoPerfTrampoline):
		return "no_perf_trampoline"
	case errors.Is(err, ErrStaticallyLinkedNoSymbols):
		return "no_libpython_symbols"
	case errors.Is(err, ErrNotPython):
		return "not_python"
	default:
		return "other"
	}
}

func (m *Manager) snapshotTracked() map[uint32]*trackedTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[uint32]*trackedTarget, len(m.tracked))
	for k, v := range m.tracked {
		out[k] = v
	}
	return out
}

func (m *Manager) logSummary() {
	m.log.Info("python inject summary",
		"activated", m.stats.Activated.Load(),
		"skipped",
		m.stats.SkippedNotPython.Load()+
			m.stats.SkippedTooOld.Load()+
			m.stats.SkippedNoTramp.Load()+
			m.stats.SkippedNoSymbols.Load(),
		"failed", m.stats.ActivateFailed.Load(),
	)
}

// isProcessGone returns true if the error is a "no such process" (ESRCH),
// indicating the target exited between our snapshot and the deactivate call.
func isProcessGone(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, syscall.ESRCH)
}
