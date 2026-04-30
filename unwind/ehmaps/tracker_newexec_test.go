package ehmaps

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewExec_NoSubscribersZeroDispatch constructs a PIDTracker without
// registering any OnNewExec hook, then drives a synthetic ForkEvent through
// Run. Asserts that onNewExec is still nil after the event is processed —
// guarding the lazy-subscription invariant: no hook means no dispatch, zero
// overhead for the producer.
func TestNewExec_NoSubscribersZeroDispatch(t *testing.T) {
	w := &fakeMmapWatcher{ch: make(chan MmapEventRecord, 4)}
	tracker := NewPIDTracker(nil, nil, nil)

	// Confirm no hook is registered (the zero value must be nil).
	if tracker.onNewExec != nil {
		t.Fatal("onNewExec should be nil when SetOnNewExec was never called")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		tracker.Run(ctx, w)
		close(done)
	}()

	// Send a group-leader ForkEvent (pid == tid).
	w.ch <- MmapEventRecord{Kind: ForkEvent, PID: 99, TID: 99}
	// Give Run time to process the event.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// The tracker still has no hook — the event was handled by the single
	// nil check in Run without any further dispatch.
	if tracker.onNewExec != nil {
		t.Fatal("onNewExec must remain nil; something unexpectedly set it")
	}
}

// TestNewExec_HookFires registers an OnNewExec hook via SetOnNewExec, drives
// a synthetic group-leader ForkEvent for pid=12345, and asserts the hook
// received pid=12345.
func TestNewExec_HookFires(t *testing.T) {
	w := &fakeMmapWatcher{ch: make(chan MmapEventRecord, 4)}
	tracker := NewPIDTracker(nil, nil, nil)

	var got atomic.Uint32
	tracker.SetOnNewExec(func(pid uint32) {
		got.Store(pid)
	})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		tracker.Run(ctx, w)
		close(done)
	}()

	// Send a group-leader ForkEvent for pid=12345.
	const wantPID uint32 = 12345
	w.ch <- MmapEventRecord{Kind: ForkEvent, PID: wantPID, TID: wantPID}

	// Wait for the hook to fire (Run dispatches synchronously; 200ms is generous).
	deadline := time.After(200 * time.Millisecond)
	for got.Load() != wantPID {
		select {
		case <-deadline:
			t.Fatalf("OnNewExec hook did not fire within deadline; got pid=%d, want %d",
				got.Load(), wantPID)
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	<-done
}
