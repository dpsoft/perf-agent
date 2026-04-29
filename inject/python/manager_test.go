package python

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stubDetector implements Detector by mapping PID → result.
type stubDetector struct {
	results map[uint32]stubResult
}

type stubResult struct {
	target *Target
	err    error
}

func (s *stubDetector) Detect(pid uint32) (*Target, error) {
	r, ok := s.results[pid]
	if !ok {
		return nil, ErrNotPython
	}
	return r.target, r.err
}

// stubInjector counts and optionally errors.
type stubInjector struct {
	mu              sync.Mutex
	activated       []uint32
	deactivated     []uint32
	activateErr     error
	deactivateErr   error
	deactivateDelay time.Duration
}

func (s *stubInjector) RemoteActivate(pid uint32, _ SymbolAddrsForTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activateErr != nil {
		return s.activateErr
	}
	s.activated = append(s.activated, pid)
	return nil
}

func (s *stubInjector) RemoteDeactivate(pid uint32, _ SymbolAddrsForTarget) error {
	if s.deactivateDelay > 0 {
		time.Sleep(s.deactivateDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deactivateErr != nil {
		return s.deactivateErr
	}
	s.deactivated = append(s.deactivated, pid)
	return nil
}

func newTestManager(t *testing.T, det Detector, inj LowLevelInjector, strict bool) *Manager {
	t.Helper()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(Options{
		StrictPerPID: strict,
		Logger:       silent,
		Detector:     det,
		Injector:     inj,
	})
}

func makeTarget(pid uint32) *Target {
	return &Target{
		PID:              pid,
		LibPythonPath:    "/fake/libpython3.12.so",
		LoadBase:         0x400000,
		PyGILEnsureAddr:  0x401000,
		PyGILReleaseAddr: 0x402000,
		PyRunStringAddr:  0x403000,
		Major:            3,
		Minor:            12,
	}
}

func TestActivateAll_StrictExitsOnFirstError(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
		200: {err: ErrPythonTooOld},
		300: {target: makeTarget(300)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, true)

	err := m.ActivateAll([]uint32{100, 200, 300})
	if err == nil {
		t.Fatal("expected strict error; got nil")
	}
	if !errors.Is(err, ErrPythonTooOld) {
		t.Fatalf("expected ErrPythonTooOld; got %v", err)
	}
	if got := m.stats.Activated.Load(); got != 1 {
		t.Errorf("Activated = %d, want 1", got)
	}
	for _, p := range inj.activated {
		if p == 300 {
			t.Errorf("strict mode should not have activated pid 300")
		}
	}
}

func TestActivateAll_LenientContinuesOnError(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
		200: {err: ErrPythonTooOld},
		300: {target: makeTarget(300)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, false)

	if err := m.ActivateAll([]uint32{100, 200, 300}); err != nil {
		t.Fatalf("lenient ActivateAll returned error: %v", err)
	}
	if got := m.stats.Activated.Load(); got != 2 {
		t.Errorf("Activated = %d, want 2", got)
	}
	if got := m.stats.SkippedTooOld.Load(); got != 1 {
		t.Errorf("SkippedTooOld = %d, want 1", got)
	}
}

func TestDeactivateAll_HonorsDeadline(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
		200: {target: makeTarget(200)},
		300: {target: makeTarget(300)},
	}}
	// Inject delay so the second-or-later Deactivate hits the deadline.
	inj := &stubInjector{deactivateDelay: 100 * time.Millisecond}
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(Options{
		Logger:             silent,
		Detector:           det,
		Injector:           inj,
		DeactivateDeadline: 50 * time.Millisecond,
	})
	if err := m.ActivateAll([]uint32{100, 200, 300}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	start := time.Now()
	m.DeactivateAll(t.Context())
	elapsed := time.Since(start)
	if elapsed > 250*time.Millisecond {
		t.Errorf("DeactivateAll took %v; deadline should have capped near 50-150ms", elapsed)
	}
}

func TestDeactivateAll_ToleratesESRCH(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{deactivateErr: syscall.ESRCH}
	m := newTestManager(t, det, inj, false)
	if err := m.ActivateAll([]uint32{100}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	m.DeactivateAll(t.Context())
	if got := m.stats.DeactivateFailed.Load(); got != 0 {
		t.Errorf("DeactivateFailed = %d on ESRCH; want 0", got)
	}
}

func TestActivateLate_DedupesViaTracked(t *testing.T) {
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, false)

	if err := m.ActivateAll([]uint32{100}); err != nil {
		t.Fatalf("ActivateAll: %v", err)
	}
	// Now late-arrival event for the same PID — must not trigger second activation.
	m.ActivateLate(100)
	if len(inj.activated) != 1 {
		t.Errorf("activate count = %d, want 1", len(inj.activated))
	}
}

func TestActivateLate_DedupesViaPending(t *testing.T) {
	// Multiple concurrent ActivateLate calls for the same PID — only one must
	// reach the injector.
	det := &stubDetector{results: map[uint32]stubResult{
		100: {target: makeTarget(100)},
	}}
	inj := &stubInjector{}
	m := newTestManager(t, det, inj, false)

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { m.ActivateLate(100) })
	}
	wg.Wait()
	if len(inj.activated) > 1 {
		t.Errorf("ActivateLate not deduped under concurrency: %v", inj.activated)
	}
}

// Compile-time check: ensure context is imported correctly.
var _ = context.Background
