package ehmaps

import (
	"fmt"
	"sync"
)

// MultiCPUMmapWatcher owns one MmapWatcher per online CPU and fans
// their events into one channel. Used by dwarfagent in system-wide
// mode: per-PID watchers can't do -a because they only see mmaps from
// the specific TID they attach to, and inherit=1 is forbidden on
// fds we need to mmap (EINVAL from perf_mmap). Per-CPU (pid=-1, cpu=N)
// is the standard workaround.
type MultiCPUMmapWatcher struct {
	watchers []*MmapWatcher
	events   chan MmapEventRecord
	fanWG    sync.WaitGroup
	done     chan struct{}
	closeMu  sync.Mutex
	closed   bool
}

// NewMultiCPUMmapWatcher opens one SystemWideMmapWatcher per element
// of cpus. On any error, every watcher opened so far is closed and
// (nil, err) is returned.
func NewMultiCPUMmapWatcher(cpus []int) (*MultiCPUMmapWatcher, error) {
	m := &MultiCPUMmapWatcher{
		events: make(chan MmapEventRecord, 512),
		done:   make(chan struct{}),
	}
	for _, cpu := range cpus {
		w, err := NewSystemWideMmapWatcher(cpu)
		if err != nil {
			// Best-effort cleanup of anything opened so far.
			for _, opened := range m.watchers {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("mmap watcher cpu=%d: %w", cpu, err)
		}
		m.watchers = append(m.watchers, w)
	}
	for _, w := range m.watchers {
		m.fanWG.Add(1)
		go m.fanIn(w)
	}
	return m, nil
}

// fanIn forwards events from one child watcher into the shared channel
// until the child's events channel closes or done fires.
func (m *MultiCPUMmapWatcher) fanIn(w *MmapWatcher) {
	defer m.fanWG.Done()
	for {
		select {
		case ev, ok := <-w.Events():
			if !ok {
				return
			}
			select {
			case m.events <- ev:
			case <-m.done:
				return
			}
		case <-m.done:
			return
		}
	}
}

// Events returns the merged channel. Matches the MmapWatcher.Events()
// signature so PIDTracker.Run can consume either shape.
func (m *MultiCPUMmapWatcher) Events() <-chan MmapEventRecord {
	return m.events
}

// Close stops fan-in goroutines, closes every child watcher, and
// releases the merged channel. Idempotent.
func (m *MultiCPUMmapWatcher) Close() error {
	m.closeMu.Lock()
	if m.closed {
		m.closeMu.Unlock()
		return nil
	}
	m.closed = true
	close(m.done)
	m.closeMu.Unlock()

	var firstErr error
	for _, w := range m.watchers {
		if err := w.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.fanWG.Wait()
	close(m.events)
	return firstErr
}
