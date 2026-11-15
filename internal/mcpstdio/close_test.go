package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// Stdin's EOF means the client is gone: a pipelined call whose handler
// honours its context is answered as cancelled, in order, and the next is
// still answered.
func TestEOFMeansTheClientIsGone(t *testing.T) {
	work := Tool{Name: "work", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return Result{Text: "finished"}, nil
		}
	}}
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		callLine(2, "work", "{}") + "\n" +
		callLine(3, "echo", `{"n":3}`) + "\n"
	var out bytes.Buffer
	if err := New(withTools(work)).Serve(t.Context(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte("\n")), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("%d answers:\n%s", len(lines), out.Bytes())
	}
	var rs [3]response
	for i, l := range lines {
		if err := json.Unmarshal(l, &rs[i]); err != nil {
			t.Fatal(err)
		}
	}
	wantID(t, rs[0], "1")
	wantID(t, rs[1], "2")
	if text, isErr := toolText(t, rs[1]); !isErr || text != context.Canceled.Error() {
		t.Fatalf("work = %q isError=%v, want the cancellation", text, isErr)
	}
	wantID(t, rs[2], "3")
	if text, isErr := toolText(t, rs[2]); isErr || text != `{"n":3}` {
		t.Fatalf("echo = %q isError=%v", text, isErr)
	}
}

func TestOnCloseRunsBeforeTheHandlersSeeTheCancel(t *testing.T) {
	for _, path := range []string{"context", "eof"} {
		t.Run(path, func(t *testing.T) {
			b := newBlocker()
			o := withTools(b.tool())
			o.DrainTimeout = 100 * time.Millisecond
			handler := make(chan context.Context, 1)
			seen := make(chan error, 1)
			o.OnClose = func(CloseReason) { seen <- (<-handler).Err() }
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sr, cw := io.Pipe()
			defer func() { _ = cw.Close() }()
			done := make(chan error, 1)
			go func() { done <- New(o).Serve(ctx, sr, io.Discard) }()
			in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" + callLine(2, "block", "{}") + "\n"
			if _, err := io.WriteString(cw, in); err != nil {
				t.Fatal(err)
			}
			handler <- <-b.started
			if path == "context" {
				cancel()
			} else {
				_ = cw.Close()
			}
			if err := <-seen; err != nil {
				t.Fatalf("the handler's context was already %v when OnClose ran", err)
			}
			<-done
		})
	}
}

// errReader returns its data, then err instead of EOF.
type errReader struct {
	r   io.Reader
	err error
}

func (e errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, e.err
	}
	return n, err
}

func TestAReadErrorIsKeptWhenTheDrainFails(t *testing.T) {
	o := testOptions()
	o.DrainTimeout = 100 * time.Millisecond
	boom := errors.New("boom")
	in := errReader{strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n"), boom}
	w := newBlockedWriter(t)
	err := New(o).Serve(t.Context(), in, w)
	if !errors.Is(err, ErrClientGone) || !errors.Is(err, boom) {
		t.Fatalf("Serve = %v, want ErrClientGone and the read error", err)
	}
}
