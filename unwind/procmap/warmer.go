package procmap

import (
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultWarmInterval is how often a Warmer sweeps /proc.
//
// The interval is the resolution of the guarantee, and nothing more: a process
// that lives longer than one sweep is resolvable after it exits, one that
// lives less than a sweep may not be. There is no interval that catches
// everything — a process can start and exit between any two instants — so this
// buys a bound rather than a certainty, and the bound is what gets reported.
//
// 500ms because the cost is not the sweep, it is the first read of each new
// PID: readdir on /proc is cheap and the per-PID sync.Once means an already
// warmed process costs a map lookup. Halving the interval therefore does not
// double the work; it only shortens the window in which a process can be born
// and die unseen.
const DefaultWarmInterval = 500 * time.Millisecond

// Warmer keeps a Resolver's per-PID cache populated while the processes are
// still alive.
//
// Without it, a system-wide capture resolves nothing for any process that
// exited during the capture window — the profilers resolve at collect time,
// which is after the window closes, and /proc/<pid>/maps is gone by then.
// Issue #56.
type Warmer struct {
	r        *Resolver
	interval time.Duration

	mu   sync.Mutex
	stop chan struct{}
	done chan struct{}

	sweeps atomic.Uint64
	warmed atomic.Uint64 // PIDs read for the first time
	seen   atomic.Uint64 // PIDs offered to the cache across all sweeps
}

// NewWarmer returns a Warmer for r. interval <= 0 means DefaultWarmInterval.
func NewWarmer(r *Resolver, interval time.Duration) *Warmer {
	if interval <= 0 {
		interval = DefaultWarmInterval
	}
	return &Warmer{r: r, interval: interval}
}

// Sweep walks /proc once and warms every process it finds. Returns how many
// PIDs it saw and how many of those it read for the first time.
//
// Exported because it is the whole of the Warmer's behaviour: the goroutine
// below only calls this on a timer, so a test that drives Sweep directly is
// testing the real thing rather than a timing-dependent shadow of it.
func (w *Warmer) Sweep() (seen, warmed int) {
	entries, err := os.ReadDir(w.r.procRoot)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue // not a PID: /proc/self, /proc/sys, ...
		}
		pid := uint32(n)
		seen++
		if w.r.isCached(pid) {
			continue
		}
		if w.r.Warm(pid) > 0 {
			warmed++
		}
	}
	w.sweeps.Add(1)
	w.seen.Add(uint64(seen))
	w.warmed.Add(uint64(warmed))
	return seen, warmed
}

// Start begins sweeping in the background. Idempotent.
func (w *Warmer) Start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	w.stop, w.done = stop, done
	// A sweep BEFORE the first tick, not after it: the interval is how often
	// the picture is refreshed, not how long to wait before taking one. A
	// capture shorter than one interval would otherwise warm nothing at all.
	w.Sweep()
	go func() {
		defer close(done)
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				w.Sweep()
			}
		}
	}()
}

// Stop ends the background sweeps and waits for the goroutine to exit. Safe to
// call more than once, and safe to call without a preceding Start.
func (w *Warmer) Stop() {
	w.mu.Lock()
	stop, done := w.stop, w.done
	w.stop, w.done = nil, nil
	w.mu.Unlock()
	if stop == nil {
		return
	}
	close(stop)
	<-done
}

// Stats reports what the warmer did. Every one of these is assertable: a
// warmer that stopped sweeping must not look like a machine that stopped
// starting processes.
func (w *Warmer) Stats() (sweeps, pidsSeen, pidsWarmed uint64) {
	return w.sweeps.Load(), w.seen.Load(), w.warmed.Load()
}
