package mcpstdio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// failingWriter blocks its first Write until fail is closed, then fails it
// and every later one.
type failingWriter struct {
	entered chan struct{}
	fail    chan struct{}
}

func (w *failingWriter) Write([]byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.fail
	return 0, errors.New("EPIPE")
}

func TestNoHandlerStartsOnceTheClientIsDead(t *testing.T) {
	b := newBlocker()
	traced := make(chan struct{}, 1)
	trace := Tool{Name: "trace", Handler: func(context.Context, json.RawMessage) (Result, error) {
		traced <- struct{}{}
		return Result{Text: "t"}, nil
	}}
	o := withTools(b.tool(), trace)
	gone := make(chan struct{})
	hold := make(chan struct{})
	o.OnClose = func(r CloseReason) {
		if r == CloseClientGone {
			close(gone)
			<-hold // Serve has not stopped the dispatcher yet
		}
	}
	sr, cw := io.Pipe()
	defer func() { _ = cw.Close() }()
	w := &failingWriter{entered: make(chan struct{}, 1), fail: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- New(o).Serve(t.Context(), sr, w) }()
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		callLine(2, "block", "{}") + "\n" + callLine(3, "trace", "{}") + "\n"
	if _, err := io.WriteString(cw, in); err != nil {
		t.Fatal(err)
	}
	<-w.entered
	<-b.started
	close(w.fail) // the answer to initialize fails: the client is dead
	<-gone
	close(b.release)
	<-b.ended
	select {
	case <-traced:
		t.Error("a queued handler started after the client was dead")
	case <-time.After(200 * time.Millisecond):
	}
	close(hold)
	if err := <-done; !errors.Is(err, ErrClientGone) {
		t.Fatalf("Serve = %v", err)
	}
}

func TestMaxResultBytesKeepsAResultUnderHalfTheOutput(t *testing.T) {
	for _, o := range []Options{
		{},
		{MaxToolErrorBytes: 1 << 20},
		{MaxToolErrorBytes: 1024},
		{MaxOutBytes: 64 << 10},
	} {
		s := New(o)
		n := s.MaxResultBytes()
		id := json.RawMessage(`"` + strings.Repeat("i", maxResultIDBytes-2) + `"`)
		line := func(n int) int {
			// Quotes double, the escaping MaxResultBytes allows for; a
			// line that escapes worse is replaced (toolarge_test.go).
			raw, err := marshal(toolResult(Result{Text: strings.Repeat(`"`, n)}))
			if err != nil {
				t.Fatal(err)
			}
			b, err := encodeLine(&outgoing{JSONRPC: "2.0", ID: id, Result: raw})
			if err != nil {
				t.Fatal(err)
			}
			return len(b)
		}
		if got := line(n); 2*got > s.o.MaxOutBytes {
			t.Errorf("%+v: a %d-byte result is a %d-byte line, over half of MaxOutBytes %d", o, n, got, s.o.MaxOutBytes)
		}
		if got := line(n + resultEnvelopeBytes); 2*got <= s.o.MaxOutBytes {
			t.Errorf("%+v: MaxResultBytes %d is not tight: %d more still fit", o, n, resultEnvelopeBytes)
		}
		if o.MaxToolErrorBytes > 0 && n < o.MaxToolErrorBytes {
			t.Errorf("%+v: the default MaxOutBytes holds results of %d bytes, under MaxToolErrorBytes", o, n)
		}
	}
}

func TestTheReadErrorIsKeptOnEveryEnd(t *testing.T) {
	boom := errors.New("boom")
	for _, path := range []string{"dead", "context"} {
		t.Run(path, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			// It ignores its context, so it outlives EOF.
			stuck := Tool{Name: "stuck", Handler: func(context.Context, json.RawMessage) (Result, error) {
				<-release
				return Result{Text: "late"}, nil
			}}
			o := withTools(stuck)
			o.DrainTimeout = 100 * time.Millisecond
			o.StallTimeout = 100 * time.Millisecond
			closes := newCloseLog(&o)
			in := errReader{strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
				callLine(2, "stuck", "{}") + "\n" +
				callLine(3, "stuck", "{}") + "\n"), boom}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			w := newBlockedWriter(t)
			done := make(chan error, 1)
			go func() { done <- New(o).Serve(ctx, in, w) }()
			if r := <-closes.ch; r != CloseEOF {
				t.Fatalf("OnClose(%v)", r)
			}
			// The initialize answer must be stuck in the blocked write
			// before the session ends: with nothing written yet, the drain
			// has nothing to wait for and ends at once, rightly without
			// ErrClientGone.
			<-w.entered
			if path == "dead" {
				// Its answer waits behind the blocked one and stalls, while
				// the second call holds the dispatcher.
				release <- struct{}{}
			} else {
				cancel()
			}
			err := <-done
			if !errors.Is(err, ErrClientGone) || !errors.Is(err, boom) {
				t.Fatalf("Serve = %v, want ErrClientGone and the read error", err)
			}
		})
	}
}
