package perfmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// engine is the one slot that grants the engine state: the store, the
// diffs, the attempts, the pins and the live page's publications. Its
// holder alone reads and changes them, so they take no locks of their own.
// Every tool holds it while it runs, and a capture job takes it for its
// own steps; a tool that runs on the reader (a Control tool) never does.
type engine struct{ slot chan struct{} }

func newEngine() *engine { return &engine{slot: make(chan struct{}, 1)} }

// acquire takes the engine. A free engine is taken at once, whatever ctx
// says, so a call that does not have to wait is never refused: calls
// pipelined before EOF, which run with a cancelled context, are still
// answered. Only a wait for a held engine ends with ctx.
func (e *engine) acquire(ctx context.Context) error {
	select {
	case e.slot <- struct{}{}:
		return nil
	default:
	}
	select {
	case e.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release gives the engine back. It must be held.
func (e *engine) release() { <-e.slot }

// engineHandler runs h holding the engine. A call whose context ends while
// it waits is answered with that.
func (s *Server) engineHandler(h mcpstdio.Handler) mcpstdio.Handler {
	return func(ctx context.Context, args json.RawMessage) (mcpstdio.Result, error) {
		if err := s.engine.acquire(ctx); err != nil {
			return mcpstdio.Result{}, fmt.Errorf("the call ended while it waited for the server's engine: %w", err)
		}
		defer s.engine.release()
		return h(ctx, args)
	}
}
