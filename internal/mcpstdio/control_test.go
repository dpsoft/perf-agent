package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// blocker is a tool that runs until it is released or its context ends.
type blocker struct {
	started chan context.Context // the handler's context, once it runs
	release chan struct{}
	ended   chan struct{} // one value each time the handler returns
}

func newBlocker() *blocker {
	return &blocker{started: make(chan context.Context, 4), release: make(chan struct{}), ended: make(chan struct{}, 4)}
}

func (b *blocker) tool() Tool {
	return Tool{Name: "block", Description: "blocks", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		defer func() { b.ended <- struct{}{} }()
		b.started <- ctx
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-b.release:
			return Result{Text: "released"}, nil
		}
	}}
}

func controlTool() Tool {
	return Tool{Name: "ctl", Description: "answers at once", Control: true,
		Handler: func(context.Context, json.RawMessage) (Result, error) { return Result{Text: "ctl"}, nil }}
}

// closeLog records every OnClose call.
type closeLog struct {
	mu  sync.Mutex
	got []CloseReason
	ch  chan CloseReason
}

func newCloseLog(o *Options) *closeLog {
	l := &closeLog{ch: make(chan CloseReason, 8)}
	o.OnClose = func(r CloseReason) {
		l.mu.Lock()
		l.got = append(l.got, r)
		l.mu.Unlock()
		l.ch <- r
	}
	return l
}

func (l *closeLog) reasons() []CloseReason {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.got)
}

// blockedWriter blocks every Write until the test ends. entered gets a
// value as each Write starts.
type blockedWriter struct {
	entered chan struct{}
	gate    chan struct{}
}

func newBlockedWriter(t *testing.T) *blockedWriter {
	w := &blockedWriter{entered: make(chan struct{}, 64), gate: make(chan struct{})}
	t.Cleanup(func() { close(w.gate) })
	return w
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.entered <- struct{}{}
	<-w.gate
	return 0, errors.New("closed")
}

// blockedReader never returns until the test ends.
type blockedReader chan struct{}

func (r blockedReader) Read([]byte) (int, error) {
	<-r
	return 0, io.EOF
}

func newBlockedReader(t *testing.T) blockedReader {
	r := make(blockedReader)
	t.Cleanup(func() { close(r) })
	return r
}

func callLine(id any, tool string, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%v,"method":"tools/call","params":{"name":"%s","arguments":%s}}`, id, tool, args)
}

func cancelLine(id string) string {
	return `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `,"reason":"test"}}`
}

func wantID(t *testing.T, r response, id string) {
	t.Helper()
	if string(r.ID) != id {
		t.Fatalf("response id = %s, want %s (error %+v result %s)", r.ID, id, r.Error, r.Result)
	}
}

// toolText is a tools/call result's text and isError.
func toolText(t *testing.T, r response) (string, bool) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("id %s: protocol error %+v", r.ID, r.Error)
	}
	var res struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		t.Fatalf("id %s: result %s (%v)", r.ID, r.Result, err)
	}
	return res.Content[0].Text, res.IsError
}

func withTools(tools ...Tool) Options {
	o := testOptions()
	o.Tools = append(o.Tools, tools...)
	return o
}

func TestCancelledEndsTheRunningHandlerWithNoResponse(t *testing.T) {
	b := newBlocker()
	c := newClient(t, withTools(b.tool()))
	c.init()
	c.send(callLine(2, "block", "{}"))
	ctx := <-b.started
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{ "requestId" : 2 }}`)
	<-ctx.Done()
	<-b.ended
	// Had id 2 been answered, its answer would come first: the dispatcher
	// takes one request at a time.
	wantID(t, c.call(`{"jsonrpc":"2.0","id":3,"method":"ping"}`), "3")
}

func TestCancelForAnotherIDDoesNothing(t *testing.T) {
	b := newBlocker()
	c := newClient(t, withTools(b.tool(), controlTool()))
	c.init()
	c.send(callLine(2, "block", "{}"))
	ctx := <-b.started
	c.send(cancelLine("3"))
	c.send(cancelLine(`"2"`))
	c.send(cancelLine("20"))
	// The reader takes lines in order, so once the control call is
	// answered the cancels before it have been seen.
	wantID(t, c.call(callLine(4, "ctl", "{}")), "4")
	if ctx.Err() != nil {
		t.Fatalf("a cancel for another id ended the handler: %v", ctx.Err())
	}
	close(b.release)
	r := c.recv()
	wantID(t, r, "2")
	if text, isErr := toolText(t, r); text != "released" || isErr {
		t.Fatalf("block = %q isError=%v", text, isErr)
	}
}

func TestCancelOfAQueuedRequestDropsIt(t *testing.T) {
	b := newBlocker()
	c := newClient(t, withTools(b.tool(), controlTool()))
	c.init()
	c.send(callLine(2, "block", "{}"))
	<-b.started
	c.send(callLine(3, "echo", `{"n":3}`))
	c.send(callLine(4, "echo", `{"n":4}`))
	c.send(cancelLine("3"))
	wantID(t, c.call(callLine(5, "ctl", "{}")), "5")
	close(b.release)
	wantID(t, c.recv(), "2")
	wantID(t, c.recv(), "4")
	wantID(t, c.call(`{"jsonrpc":"2.0","id":6,"method":"ping"}`), "6")
}

func TestEOFCancelsTheHandlerAndAnswersTheQueue(t *testing.T) {
	b := newBlocker()
	o := withTools(b.tool())
	closes := newCloseLog(&o)
	c := newClient(t, o)
	if r := c.call(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`); r.Error != nil {
		t.Fatalf("initialize: %+v", r.Error)
	}
	c.send(callLine(1, "block", "{}"))
	ctx := <-b.started
	for id := 2; id <= 4; id++ {
		c.send(callLine(id, "echo", fmt.Sprintf(`{"n":%d}`, id)))
	}
	_ = c.w.Close()
	<-ctx.Done()
	r := c.recv()
	wantID(t, r, "1")
	if _, isErr := toolText(t, r); !isErr {
		t.Fatalf("the handler ended by EOF answered without isError: %s", r.Result)
	}
	for id := 2; id <= 4; id++ {
		r := c.recv()
		wantID(t, r, fmt.Sprint(id))
		if text, isErr := toolText(t, r); isErr || text != fmt.Sprintf(`{"n":%d}`, id) {
			t.Fatalf("id %d = %q isError=%v", id, text, isErr)
		}
	}
	if err := <-c.done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	c.done <- nil
	if got := closes.reasons(); !slices.Equal(got, []CloseReason{CloseEOF}) {
		t.Fatalf("OnClose = %v, want [CloseEOF]", got)
	}
}

func TestPipelinedRequestsBeforeEOFAreAllAnswered(t *testing.T) {
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		callLine(2, "echo", `{"n":2}`),
		callLine(3, "echo", `{"n":3}`),
		callLine(4, "echo", `{"n":4}`),
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := New(testOptions()).Serve(t.Context(), strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	lines := bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte("\n")), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("%d responses:\n%s", len(lines), out.Bytes())
	}
	for i, l := range lines {
		var r response
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		wantID(t, r, fmt.Sprint(i+1))
		if i == 0 {
			if r.Error != nil {
				t.Fatalf("initialize: %+v", r.Error)
			}
			continue
		}
		if _, isErr := toolText(t, r); isErr {
			t.Fatalf("id %d is an error: %s", i+1, r.Result)
		}
	}
}

func TestOverloadIsImmediateAndOrdered(t *testing.T) {
	b := newBlocker()
	c := newClient(t, withTools(b.tool()))
	c.init()
	c.send(callLine(2, "block", "{}"))
	ctx := <-b.started
	var batch strings.Builder
	for id := 3; id <= 22; id++ {
		batch.WriteString(callLine(id, "echo", fmt.Sprintf(`{"n":%d}`, id)) + "\n")
	}
	c.send(strings.TrimSuffix(batch.String(), "\n"))
	// The handler still runs: these are answered without waiting on it.
	for id := 11; id <= 22; id++ {
		r := c.recv()
		wantID(t, r, fmt.Sprint(id))
		wantErr(t, r, -32000)
		if r.Error.Message != "server busy: 8 requests queued; retry" {
			t.Fatalf("message = %q", r.Error.Message)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("the handler ended before the overload answers")
	}
	c.send(cancelLine("2"))
	for id := 3; id <= 10; id++ {
		r := c.recv()
		wantID(t, r, fmt.Sprint(id))
		if text, isErr := toolText(t, r); isErr || text != fmt.Sprintf(`{"n":%d}`, id) {
			t.Fatalf("id %d = %q isError=%v", id, text, isErr)
		}
	}
	for _, l := range c.lines {
		if !bytes.HasSuffix(l, []byte("\n")) || bytes.Count(l, []byte("\n")) != 1 {
			t.Errorf("bad framing: %q", l)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(l, &m); err != nil || string(m["jsonrpc"]) != `"2.0"` {
			t.Errorf("not one JSON-RPC message: %q (%v)", l, err)
		}
	}
}

func TestControlToolsAnswerAheadOfTheQueue(t *testing.T) {
	b := newBlocker()
	c := newClient(t, withTools(b.tool(), controlTool()))
	c.init()
	c.send(callLine(2, "block", "{}"))
	<-b.started
	c.send(callLine(3, "echo", "{}"))
	c.send(callLine(4, "echo", "{}"))
	start := time.Now()
	r := c.call(callLine(5, "ctl", "{}"))
	took := time.Since(start)
	wantID(t, r, "5")
	if text, isErr := toolText(t, r); text != "ctl" || isErr {
		t.Fatalf("ctl = %q isError=%v", text, isErr)
	}
	if took >= 100*time.Millisecond {
		t.Errorf("the control call took %v", took)
	}
	close(b.release)
	for _, id := range []string{"2", "3", "4"} {
		wantID(t, c.recv(), id)
	}
}

func TestAControlToolBeforeInitializeQueues(t *testing.T) {
	c := newClient(t, withTools(controlTool()))
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" + callLine(2, "ctl", "{}"))
	r := c.recv()
	wantID(t, r, "1")
	if r.Error != nil {
		t.Fatalf("initialize: %+v", r.Error)
	}
	r = c.recv()
	wantID(t, r, "2")
	if text, isErr := toolText(t, r); text != "ctl" || isErr {
		t.Fatalf("ctl = %q isError=%v", text, isErr)
	}
}

func TestABigAnswerIntoAnEmptyQueueFits(t *testing.T) {
	o := testOptions()
	o.MaxOutBytes = 1 << 10
	closes := newCloseLog(&o)
	c := newClient(t, o)
	c.init()
	big := strings.Repeat("b", 64<<10)
	r := c.call(callLine(2, "echo", `{"s":"`+big+`"}`))
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, big) {
		t.Fatalf("the big answer was not delivered (%d bytes, isError=%v)", len(text), isErr)
	}
	if got := closes.reasons(); len(got) != 0 {
		t.Fatalf("OnClose = %v before EOF", got)
	}
	_ = c.w.Close()
	if err := <-c.done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	c.done <- nil
	if got := closes.reasons(); !slices.Equal(got, []CloseReason{CloseEOF}) {
		t.Fatalf("OnClose = %v, want [CloseEOF]", got)
	}
}

func TestABlockedStdoutMeansADeadClient(t *testing.T) {
	o := testOptions()
	o.MaxOutMessages = 4
	closes := newCloseLog(&o)
	var in strings.Builder
	for id := 1; id <= 20; id++ {
		fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":"ping"}`+"\n", id)
	}
	r := io.MultiReader(strings.NewReader(in.String()), newBlockedReader(t))
	w := newBlockedWriter(t)
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- New(o).Serve(t.Context(), r, w) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Serve = %v, want ErrClientGone", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("Serve took %v", took)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve still runs 1s after its output blocked")
	}
	if got := closes.reasons(); !slices.Equal(got, []CloseReason{CloseClientGone}) {
		t.Fatalf("OnClose = %v, want [CloseClientGone]", got)
	}
}

func TestEOFAndCancelAreSeenWhileStdoutIsBlocked(t *testing.T) {
	b := newBlocker()
	o := withTools(b.tool())
	o.DrainTimeout = 200 * time.Millisecond
	closes := newCloseLog(&o)
	sr, cw := io.Pipe()
	w := newBlockedWriter(t)
	done := make(chan error, 1)
	go func() { done <- New(o).Serve(t.Context(), sr, w) }()
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(cw, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	<-w.entered // the answer to initialize is stuck in a write
	send(callLine(2, "block", "{}"))
	ctx := <-b.started
	send(cancelLine("2"))
	<-ctx.Done()
	<-b.ended
	_ = cw.Close()
	if r := <-closes.ch; r != CloseEOF {
		t.Fatalf("OnClose(%v), want CloseEOF", r)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrClientGone) {
			t.Fatalf("Serve = %v, want ErrClientGone", err)
		}
	case <-time.After(o.DrainTimeout + time.Second):
		t.Fatal("Serve did not return after its drain")
	}
}

func TestContextDoneDrainsThenReturns(t *testing.T) {
	progressed := make(chan struct{})
	slow := Tool{Name: "slow", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		if !Progress(ctx, 1, 2, "half") {
			return Result{}, errors.New("progress was not enqueued")
		}
		close(progressed)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}

	t.Run("drained", func(t *testing.T) {
		o := withTools(slow)
		closes := newCloseLog(&o)
		ctx, cancel := context.WithCancel(t.Context())
		sr, cw := io.Pipe()
		defer func() { _ = cw.Close() }()
		or, ow := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := New(o).Serve(ctx, sr, ow)
			_ = ow.Close()
			done <- err
		}()
		in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow","arguments":{},"_meta":{"progressToken":"p"}}}` + "\n"
		if _, err := io.WriteString(cw, in); err != nil {
			t.Fatal(err)
		}
		// Nothing has been read: the answer to initialize is stuck in a
		// write and the progress notification waits in the queue.
		<-progressed
		cancel()
		out, err := io.ReadAll(or)
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("Serve = %v", err)
		}
		if !bytes.Contains(out, []byte(`"method":"notifications/progress"`)) || !bytes.Contains(out, []byte(`"progressToken":"p"`)) {
			t.Fatalf("the enqueued progress was not written:\n%s", out)
		}
		if got := closes.reasons(); !slices.Equal(got, []CloseReason{CloseContext}) {
			t.Fatalf("OnClose = %v, want [CloseContext]", got)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		o := testOptions()
		o.DrainTimeout = 200 * time.Millisecond
		ctx, cancel := context.WithCancel(t.Context())
		sr, cw := io.Pipe()
		defer func() { _ = cw.Close() }()
		w := newBlockedWriter(t)
		done := make(chan error, 1)
		go func() { done <- New(o).Serve(ctx, sr, w) }()
		if _, err := io.WriteString(cw, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"); err != nil {
			t.Fatal(err)
		}
		<-w.entered
		cancel()
		start := time.Now()
		select {
		case err := <-done:
			// It is context.Canceled too, so a caller that treats a
			// cancellation as a normal end still does.
			if !errors.Is(err, ErrClientGone) || !errors.Is(err, context.Canceled) {
				t.Fatalf("Serve = %v, want ErrClientGone and context.Canceled", err)
			}
			if took := time.Since(start); took > o.DrainTimeout+100*time.Millisecond {
				t.Errorf("Serve took %v after the cancel", took)
			}
		case <-time.After(o.DrainTimeout + time.Second):
			t.Fatal("Serve did not return after its drain")
		}
	})
}

func TestProgressGoesThroughTheWriter(t *testing.T) {
	prog := Tool{Name: "prog", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		a := Progress(ctx, 1, 2, "half")
		b := Progress(ctx, 2, 0, "")
		return Result{Text: fmt.Sprint(a, b)}, nil
	}}
	c := newClient(t, withTools(prog))
	c.init()
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"prog","arguments":{},"_meta":{"progressToken":"tok"}}}`)
	type note struct {
		JSONRPC string                     `json:"jsonrpc"`
		ID      json.RawMessage            `json:"id"`
		Method  string                     `json:"method"`
		Params  map[string]json.RawMessage `json:"params"`
	}
	var notes []note
	for range 2 {
		var n note
		if err := json.Unmarshal(c.recvRaw(), &n); err != nil {
			t.Fatal(err)
		}
		notes = append(notes, n)
	}
	for i, want := range []map[string]string{
		{"progressToken": `"tok"`, "progress": "1", "total": "2", "message": `"half"`},
		{"progressToken": `"tok"`, "progress": "2"},
	} {
		n := notes[i]
		if n.JSONRPC != "2.0" || n.Method != "notifications/progress" || len(n.ID) != 0 || len(n.Params) != len(want) {
			t.Fatalf("notification %d = %+v", i, n)
		}
		for k, v := range want {
			if string(n.Params[k]) != v {
				t.Errorf("notification %d %s = %s, want %s", i, k, n.Params[k], v)
			}
		}
	}
	r := c.recv()
	wantID(t, r, "2")
	if text, _ := toolText(t, r); text != "true true" {
		t.Fatalf("Progress returned %q", text)
	}

	// Without a progress token nothing is sent.
	r = c.call(callLine(3, "prog", "{}"))
	wantID(t, r, "3")
	if text, _ := toolText(t, r); text != "false false" {
		t.Fatalf("Progress without a token returned %q", text)
	}
	if Progress(t.Context(), 1, 1, "") {
		t.Fatal("Progress outside a request returned true")
	}
}
