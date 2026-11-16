package perfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// engine is the one slot that grants the engine state: the store, the
// diffs, the attempts, the pins and the live page's publications. Its
// holder alone reads and changes them, so they take no locks of their own.
// Every tool holds it while it runs, and a capture job takes it for its
// own steps; a tool that runs on the reader (a Control tool) never does.
//
// The engine also records who holds it, so a call that waits too long is
// told. The record sits in fields under mu, set on acquire and cleared on
// release, so taking a free engine allocates nothing.
type engine struct {
	slot chan struct{}

	mu   sync.Mutex
	held bool
	who  holder
}

// holder is who holds the engine: a tool and the label of the session
// that called it, a capture job ("capture <id>") or maintenance ("capture
// retention"), with by empty; since is when it took the engine.
type holder struct {
	tool, by string
	since    time.Time
}

func newEngine() *engine { return &engine{slot: make(chan struct{}, 1)} }

// errEngineBusy is acquireWithin's answer when its wait ran out.
var errEngineBusy = errors.New("the engine is busy")

// acquire takes the engine for h. A free engine is taken at once, whatever
// ctx says, so a call that does not have to wait is never refused: calls
// pipelined before EOF, which run with a cancelled context, are still
// answered. Only a wait for a held engine ends with ctx.
func (e *engine) acquire(ctx context.Context, h holder) error {
	return e.acquireWithin(ctx, h, 0)
}

// acquireWithin is acquire with the wait for a held engine bounded by d (0:
// unbounded); a wait that runs out is errEngineBusy. A timer is made only
// once the engine was found held.
func (e *engine) acquireWithin(ctx context.Context, h holder, d time.Duration) error {
	select {
	case e.slot <- struct{}{}:
		e.record(h)
		return nil
	default:
	}
	var expired <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		expired = t.C
	}
	select {
	case e.slot <- struct{}{}:
		e.record(h)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-expired:
		return errEngineBusy
	}
}

// record notes h as the holder, from now.
func (e *engine) record(h holder) {
	h.since = time.Now()
	e.mu.Lock()
	e.held, e.who = true, h
	e.mu.Unlock()
}

// holding is the engine's holder, and whether it is held. Any goroutine
// may call it.
func (e *engine) holding() (holder, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.who, e.held
}

// release gives the engine back. It must be held. The record is cleared
// first, so it never names a holder that has let go after the next one
// took the engine.
func (e *engine) release() {
	e.mu.Lock()
	e.held, e.who = false, holder{}
	e.mu.Unlock()
	<-e.slot
}

// engineHandler runs h, the handler of tool, holding the engine. A call
// waits for it at most MaxEngineWait, and is then answered with who holds
// it; a call whose context ends while it waits is answered with that.
// Either way the handler never runs.
func (s *Server) engineHandler(tool string, h mcpstdio.Handler) mcpstdio.Handler {
	return func(ctx context.Context, args json.RawMessage) (mcpstdio.Result, error) {
		err := s.engine.acquireWithin(ctx, holder{tool: tool, by: mcpstdio.ClientLabel(ctx)}, s.lim.MaxEngineWait)
		switch {
		case errors.Is(err, errEngineBusy):
			return mcpstdio.Result{}, errors.New(s.busyText())
		case err != nil:
			return mcpstdio.Result{}, fmt.Errorf("the call ended while it waited for the server's engine: %w", err)
		}
		defer s.engine.release()
		if s.holdEngine != nil {
			s.holdEngine(tool)
		}
		return h(ctx, args)
	}
}

// busyText is the answer to a call that waited MaxEngineWait for the
// engine: who holds it and for how long, in whole seconds. A holder with
// no session label (a capture job, maintenance) is "another session".
func (s *Server) busyText() string {
	h, ok := s.engine.holding()
	if !ok {
		// The holder let go as the wait ran out.
		return s.command() + " is busy; call again"
	}
	// The label is the client's own text, shown to another session: it is
	// quoted.
	from := "another session"
	if h.by != "" {
		from = strconv.Quote(h.by)
	}
	return fmt.Sprintf("%s is busy with %s from %s (running %ds); call again",
		s.command(), h.tool, from, int64(time.Since(h.since)/time.Second))
}
