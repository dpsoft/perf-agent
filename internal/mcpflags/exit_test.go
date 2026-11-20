package mcpflags

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

type blockedHandler struct{ release chan struct{} }

func (h blockedHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h blockedHandler) Handle(context.Context, slog.Record) error {
	<-h.release
	return nil
}
func (h blockedHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h blockedHandler) WithGroup(string) slog.Handler      { return h }

func TestBoundedNeverWaitsPastItsBound(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	const bound = 100 * time.Millisecond
	const slack = 50 * time.Millisecond

	start := time.Now()
	Bounded(bound, func() { <-release }, func() {})
	if d := time.Since(start); d < bound || d > bound+slack {
		t.Errorf("Bounded with a blocked step took %v, want about %v", d, bound)
	}

	start = time.Now()
	LogBounded(slog.New(blockedHandler{release}), "msg", "k", "v")
	if d := time.Since(start); d < BlockedWait || d > BlockedWait+slack {
		t.Errorf("LogBounded with a blocked handler took %v, want about %v", d, BlockedWait)
	}

	start = time.Now()
	Bounded(time.Minute, func() {}, func() {})
	if d := time.Since(start); d > slack {
		t.Errorf("Bounded with finished steps took %v, want an immediate return", d)
	}
}
