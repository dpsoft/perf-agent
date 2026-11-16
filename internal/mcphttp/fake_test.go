package mcphttp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// fake is a Handler: a real mcpstdio server whose tools a test controls,
// counting the FlushWarnings calls.
//   - block waits for its release or the end of its context; each call is
//     sent on calls as it starts;
//   - slow sleeps {"ms": N} milliseconds, or until its context ends;
//   - big answers {"bytes": N} bytes of text;
//   - received sends what mcpstdio.Received says, and when it ran, on
//     receipts;
//   - capture_cancel is a Control tool and page_state a Snapshot tool:
//     each answers at once, or with {"block": true} behaves as block;
//   - hold takes the engine, a one-slot semaphore, then behaves as block;
//     queued waits for the engine, or the end of its context, counts
//     itself in started and answers. Together they stand in for a tool
//     holding perfmcp's engine and the calls queued behind it.
//
// The resource pageStateURI is a Snapshot resource and otherURI a plain
// one.
type fake struct {
	*mcpstdio.Server
	flushed  atomic.Int32
	calls    chan *call
	receipts chan receipt
	engine   chan struct{}
	started  atomic.Int32
}

const (
	pageStateURI = "perf-agent://page/state"
	otherURI     = "perf-agent://other"
)

// call is one running block call.
type call struct {
	ctx     context.Context
	release chan struct{}
	// ended receives ctx.Err() as the call returns.
	ended chan error
}

type receipt struct {
	at, ran time.Time
	ok      bool
}

func (f *fake) FlushWarnings() {
	f.flushed.Add(1)
	f.Server.FlushWarnings()
}

func newFake() *fake {
	f := &fake{calls: make(chan *call, 64), receipts: make(chan receipt, 16), engine: make(chan struct{}, 1)}
	f.Server = mcpstdio.New(mcpstdio.Options{
		Info: mcpstdio.Implementation{Name: "fake"},
		Tools: []mcpstdio.Tool{
			{Name: "block", Handler: func(ctx context.Context, _ json.RawMessage) (mcpstdio.Result, error) {
				return f.block(ctx)
			}},
			{Name: "capture_cancel", Control: true, Handler: f.maybeBlock("cancelling")},
			{Name: "page_state", Snapshot: true, Handler: f.maybeBlock("state")},
			{Name: "hold", Handler: func(ctx context.Context, _ json.RawMessage) (mcpstdio.Result, error) {
				f.engine <- struct{}{}
				defer func() { <-f.engine }()
				return f.block(ctx)
			}},
			{Name: "queued", Handler: func(ctx context.Context, _ json.RawMessage) (mcpstdio.Result, error) {
				select {
				case f.engine <- struct{}{}:
				case <-ctx.Done():
					return mcpstdio.Result{}, fmt.Errorf("cancelled while queued: %w", ctx.Err())
				}
				f.started.Add(1)
				<-f.engine
				return mcpstdio.Result{Text: "ran"}, nil
			}},
			{Name: "slow", Handler: func(ctx context.Context, args json.RawMessage) (mcpstdio.Result, error) {
				var a struct{ MS int }
				_ = json.Unmarshal(args, &a)
				select {
				case <-time.After(time.Duration(a.MS) * time.Millisecond):
					return mcpstdio.Result{Text: "slept"}, nil
				case <-ctx.Done():
					return mcpstdio.Result{}, ctx.Err()
				}
			}},
			{Name: "big", Handler: func(_ context.Context, args json.RawMessage) (mcpstdio.Result, error) {
				var a struct{ Bytes int }
				_ = json.Unmarshal(args, &a)
				return mcpstdio.Result{Text: strings.Repeat("a", a.Bytes)}, nil
			}},
			{Name: "received", Handler: func(ctx context.Context, _ json.RawMessage) (mcpstdio.Result, error) {
				at, ok := mcpstdio.Received(ctx)
				f.receipts <- receipt{at: at, ran: time.Now(), ok: ok}
				return mcpstdio.Result{Text: "ok"}, nil
			}},
		},
		Resources: []mcpstdio.Resource{
			{URI: pageStateURI, Name: "page state", Snapshot: true, Read: func(context.Context) (string, error) { return "{}", nil }},
			{URI: otherURI, Name: "other", Text: "other"},
		},
	})
	return f
}

// block sends a call on calls and waits for its release or the end of
// ctx.
func (f *fake) block(ctx context.Context) (mcpstdio.Result, error) {
	c := &call{ctx: ctx, release: make(chan struct{}), ended: make(chan error, 1)}
	f.calls <- c
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	err := ctx.Err()
	c.ended <- err
	if err != nil {
		return mcpstdio.Result{}, fmt.Errorf("cancelled: %w", err)
	}
	return mcpstdio.Result{Text: "released"}, nil
}

// maybeBlock answers text at once, or behaves as block when the arguments
// are {"block": true}.
func (f *fake) maybeBlock(text string) mcpstdio.Handler {
	return func(ctx context.Context, args json.RawMessage) (mcpstdio.Result, error) {
		var a struct{ Block bool }
		_ = json.Unmarshal(args, &a)
		if a.Block {
			return f.block(ctx)
		}
		return mcpstdio.Result{Text: text}, nil
	}
}

// nextCall is the next block call to start.
func (f *fake) nextCall(t *testing.T) *call {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no block call started")
		return nil
	}
}

// end is the error the call's context had as it returned, waiting at
// most 2 s for it to return.
func (c *call) end(t *testing.T) error {
	t.Helper()
	select {
	case err := <-c.ended:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("the block call did not return")
		return nil
	}
}

// hub is Front, MCPGuard and a Transport on a loopback listener, with a
// recorder between the guard and the transport that checks every answer
// the transport writes.
type hub struct {
	t      *testing.T
	url    string
	addr   string
	token  livehttp.Token
	f      *fake
	tr     *Transport
	client *http.Client
	// served counts the transport's ServeHTTP calls that returned.
	served atomic.Int32
	mu     sync.Mutex
	// answers are what the transport wrote.
	answers []answer
}

type answer struct {
	status int
	header http.Header
	body   []byte
	// reqErr is the request context's error as the transport returned:
	// net/http cancels it when its background read of the connection
	// fails, a read deadline's expiry included.
	reqErr error
}

const testMaxBody = 1 << 20

func startHub(t *testing.T, opts func(*Options), srvOpts func(*http.Server)) *hub {
	t.Helper()
	baseline := runtime.NumGoroutine()
	h := &hub{t: t, f: newFake()}
	o := Options{Lifetime: t.Context(), MaxAnswerBytes: 32 << 20}
	if opts != nil {
		opts(&o)
	}
	h.tr = New(h.f, o)
	h.token = mustToken(t)
	page := mustToken(t)
	front := livehttp.Front(page, livehttp.MCPGuard(h.token, testMaxBody, h.record(h.tr)), http.NotFoundHandler(), http.NotFoundHandler())
	srv := livehttp.NewServer(front, nil)
	if srvOpts != nil {
		srvOpts(srv)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ln)
	}()
	h.addr = ln.Addr().String()
	h.url = "http://" + h.addr + "/mcp"
	h.client = &http.Client{Transport: &http.Transport{}}
	t.Cleanup(func() {
		h.tr.Close()
		_ = srv.Close()
		<-served
		h.client.CloseIdleConnections()
		waitGoroutines(t, baseline)
		h.checkAnswers()
	})
	return h
}

func mustToken(t *testing.T) livehttp.Token {
	t.Helper()
	tok, err := livehttp.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// waitGoroutines waits up to 5 s for the goroutine count to fall back to
// baseline.
func waitGoroutines(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Errorf("%d goroutines, baseline %d:\n%s", runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// record wraps the transport so every answer it writes is kept.
func (h *hub) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer h.served.Add(1)
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		h.mu.Lock()
		h.answers = append(h.answers, answer{status: cmpStatus(rec.status), header: w.Header().Clone(), body: rec.body.Bytes(), reqErr: r.Context().Err()})
		h.mu.Unlock()
	})
}

func cmpStatus(s int) int {
	if s == 0 {
		return http.StatusOK
	}
	return s
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// checkAnswers holds every answer the transport wrote to the JSON-only
// rule: a body is one application/json object that is no request or
// notification of the server's; 202 and 204 have no body; nothing is an
// event stream.
func (h *hub) checkAnswers() {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, a := range h.answers {
		ct := a.header.Get("Content-Type")
		if strings.Contains(ct, "event-stream") {
			h.t.Errorf("answer %d: Content-Type %q", i, ct)
		}
		if a.status == http.StatusAccepted || a.status == http.StatusNoContent {
			if len(a.body) > 0 {
				h.t.Errorf("answer %d: status %d with a body %q", i, a.status, a.body)
			}
			continue
		}
		if len(a.body) == 0 {
			continue
		}
		if mt, _, err := mime.ParseMediaType(ct); err != nil || mt != "application/json" {
			h.t.Errorf("answer %d: status %d, Content-Type %q", i, a.status, ct)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(a.body, &obj); err != nil {
			h.t.Errorf("answer %d: not one JSON object: %v", i, err)
			continue
		}
		if _, ok := obj["method"]; ok {
			h.t.Errorf("answer %d: the server sent a request or notification: %.200s", i, a.body)
		}
	}
}

// reply is one HTTP answer as the client read it. close is whether the
// server said it closes the connection: net/http takes "Connection: close"
// out of the header and reports it here.
type reply struct {
	status int
	header http.Header
	body   []byte
	close  bool
}

// rpcErr is the error of a JSON-RPC answer body, zero when there is none.
func (r reply) rpcErr() (code int, msg string, id json.RawMessage) {
	var b struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Error.Code, b.Error.Message, b.ID
}

// toolText is a tools/call answer's text and isError.
func (r reply) toolText() (string, bool) {
	var b struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if json.Unmarshal(r.body, &b) != nil || len(b.Result.Content) == 0 {
		return "", false
	}
	return b.Result.Content[0].Text, b.Result.IsError
}

// do sends one request to /mcp with the client's headers, the bearer token
// and the session sid when it is not "", then hdr's name and value pairs.
func (h *hub) do(method, sid, body string, hdr ...string) reply {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url, strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if method == http.MethodPost {
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Body, req.ContentLength = nil, 0
	}
	req.Header.Set("Authorization", "Bearer "+string(h.token))
	if sid != "" {
		req.Header.Set(HeaderSession, sid)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("%s: reading the answer: %v", method, err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: b, close: resp.Close}
}

func (h *hub) post(sid, body string, hdr ...string) reply {
	h.t.Helper()
	return h.do(http.MethodPost, sid, body, hdr...)
}

const initBody = `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test"}}}`

// initialize opens a session and returns its id.
func (h *hub) initialize(hdr ...string) string {
	h.t.Helper()
	r := h.post("", initBody, hdr...)
	sid := r.header.Get(HeaderSession)
	if r.status != http.StatusOK || sid == "" {
		h.t.Fatalf("initialize: status %d, session %q, body %s", r.status, sid, r.body)
	}
	return sid
}

func callBody(id any, tool string, args any) string {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func cancelBody(id any) string {
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": id, "reason": "test"}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// callAsync sends a tools/call on its own goroutine; the reply arrives on
// the channel.
func (h *hub) callAsync(sid string, id any, tool string, args any) <-chan reply {
	ch := make(chan reply, 1)
	h.callInto(ch, sid, id, tool, args)
	return ch
}

// callInto sends a tools/call on its own goroutine; the reply arrives on
// ch, which several calls may share.
func (h *hub) callInto(ch chan<- reply, sid string, id any, tool string, args any) {
	body := callBody(id, tool, args)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, h.url, strings.NewReader(body))
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+string(h.token))
		req.Header.Set(HeaderSession, sid)
		resp, err := h.client.Do(req)
		if err != nil {
			ch <- reply{status: -1, body: []byte(err.Error())}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		ch <- reply{status: resp.StatusCode, header: resp.Header, body: b}
	}()
}

func await(t *testing.T, ch <-chan reply, within time.Duration) reply {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("no answer within %v", within)
		return reply{}
	}
}

// rawRequest is one HTTP/1.1 POST to /mcp as bytes, with the client's
// headers, the bearer token and the session sid when it is not "".
func (h *hub) rawRequest(sid, body string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "POST /mcp HTTP/1.1\r\nHost: %s\r\nAccept: application/json, text/event-stream\r\nContent-Type: application/json\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n", h.addr, h.token, len(body))
	if sid != "" {
		fmt.Fprintf(&b, "%s: %s\r\n", HeaderSession, sid)
	}
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.Bytes()
}

// rawHead is rawRequest's headers announcing a body of n bytes, without
// the body.
func (h *hub) rawHead(sid string, n int) []byte {
	full := h.rawRequest(sid, strings.Repeat(" ", n))
	return full[:len(full)-n]
}

// dial opens a raw connection to the hub, closed with the test.
func (h *hub) dial() net.Conn {
	h.t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

// readReply reads one answer from a raw connection.
func readReply(t *testing.T, br *bufio.Reader) reply {
	t.Helper()
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading an answer: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading an answer body: %v", err)
	}
	return reply{status: resp.StatusCode, header: resp.Header, body: b, close: resp.Close}
}

// eventually waits up to within for cond.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, within)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
