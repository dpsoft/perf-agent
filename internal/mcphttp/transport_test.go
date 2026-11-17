package mcphttp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// noSession is what every message but initialize gets without a session.
const noSession = "no session: send initialize first"

func TestInitializeMintsASession(t *testing.T) {
	h := startHub(t, nil, nil)
	seen := map[string]bool{}
	for range 4 {
		sid := h.initialize()
		raw, err := base64.RawURLEncoding.DecodeString(sid)
		if len(sid) != 43 || err != nil || len(raw) != 32 {
			t.Fatalf("session id %q: %d chars, %v", sid, len(sid), err)
		}
		if seen[sid] {
			t.Fatalf("session id %q minted twice", sid)
		}
		seen[sid] = true
	}
	// A session header on initialize, even an unknown one, is ignored.
	old := strings.Repeat("A", 43)
	sid := h.initialize(HeaderSession, old)
	if sid == old || seen[sid] {
		t.Fatalf("initialize with an old header got %q", sid)
	}
	if got := h.tr.Stats().Sessions; got != 5 {
		t.Fatalf("%d sessions, want 5", got)
	}
}

func TestSessionRules(t *testing.T) {
	h := startHub(t, nil, nil)
	ping := `{"jsonrpc":"2.0","id":9,"method":"ping"}`
	r := h.post("", ping)
	if code, msg, id := r.rpcErr(); r.status != http.StatusBadRequest || code != -32600 || msg != noSession || string(id) != "9" {
		t.Fatalf("missing session: %d %s", r.status, r.body)
	}
	r = h.post(strings.Repeat("x", 43), ping)
	if r.status != http.StatusNotFound {
		t.Fatalf("unknown session: %d %s", r.status, r.body)
	}
	sid := h.initialize()
	if r = h.post(sid, ping); r.status != http.StatusOK || !strings.Contains(string(r.body), `"result":{}`) {
		t.Fatalf("ping: %d %s", r.status, r.body)
	}
	if r = h.do(http.MethodDelete, sid, ""); r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Fatalf("DELETE: %d %s", r.status, r.body)
	}
	if r = h.do(http.MethodDelete, sid, ""); r.status != http.StatusNotFound {
		t.Fatalf("DELETE again: %d %s", r.status, r.body)
	}
	if r = h.post(sid, ping); r.status != http.StatusNotFound {
		t.Fatalf("ping on a deleted session: %d %s", r.status, r.body)
	}
	if r = h.do(http.MethodDelete, "", ""); r.status != http.StatusBadRequest {
		t.Fatalf("DELETE without a session: %d %s", r.status, r.body)
	}
	// A notification without a session gets the same answer, with no id.
	r = h.post("", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code, msg, id := r.rpcErr(); r.status != http.StatusBadRequest || code != -32600 || msg != noSession || string(id) != "null" {
		t.Fatalf("notification without a session: %d %s", r.status, r.body)
	}
	if got := h.tr.Stats().Sessions; got != 0 {
		t.Fatalf("%d sessions, want 0", got)
	}
}

func TestProtocolVersionHeader(t *testing.T) {
	h := startHub(t, nil, nil)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	sid := h.initialize()
	r := h.post(sid, ping, HeaderVersion, "1999-01-01")
	if code, _, id := r.rpcErr(); r.status != http.StatusBadRequest || code != -32600 || string(id) != "1" {
		t.Fatalf("unsupported version: %d %s", r.status, r.body)
	}
	if r = h.post(sid, ping); r.status != http.StatusOK {
		t.Fatalf("no version header: %d %s", r.status, r.body)
	}
	if r = h.post(sid, ping, HeaderVersion, "2025-11-25"); r.status != http.StatusOK {
		t.Fatalf("the session's version: %d %s", r.status, r.body)
	}
	old := h.post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	oldSID := old.header.Get(HeaderSession)
	if old.status != http.StatusOK || oldSID == "" || !strings.Contains(string(old.body), `"protocolVersion":"2025-06-18"`) {
		t.Fatalf("2025-06-18 initialize: %d %s", old.status, old.body)
	}
	if r = h.post(oldSID, ping, HeaderVersion, "2025-06-18"); r.status != http.StatusOK {
		t.Fatalf("2025-06-18 session: %d %s", r.status, r.body)
	}
	if r = h.do(http.MethodDelete, sid, "", HeaderVersion, "1999-01-01"); r.status != http.StatusBadRequest {
		t.Fatalf("DELETE with an unsupported version: %d %s", r.status, r.body)
	}
	// Without a session, the session rule answers first, whatever the
	// version; an unknown session too.
	for _, s := range []string{"", strings.Repeat("x", 43)} {
		for _, v := range []string{"2026-07-28", "1999-01-01"} {
			r = h.post(s, ping, HeaderVersion, v)
			want := http.StatusBadRequest
			if s != "" {
				want = http.StatusNotFound
			}
			if _, msg, _ := r.rpcErr(); r.status != want || (s == "" && msg != noSession) {
				t.Fatalf("session %q version %s: %d %s", s, v, r.status, r.body)
			}
		}
	}
}

// probe is the first request Claude Code 2.1.325 sends, as captured: its
// headers, and its body completed where the capture cut it at 200 bytes.
func probe(t *testing.T) (http.Header, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "livehttp", "testdata", "headers-claude-code-2.1.325.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	var rec struct {
		H    map[string]string `json:"h"`
		Body string            `json:"body"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.H["mcp-method"] != "server/discover" {
		t.Fatalf("the first captured request is %q, not the probe", rec.H["mcp-method"])
	}
	body, ok := strings.CutSuffix(rec.Body, `{"name":"claud`)
	if !ok {
		t.Fatalf("captured probe body changed: %s", rec.Body)
	}
	body += `{"name":"claude-code","version":"2.1.325"}}}}`
	hdr := http.Header{}
	for k, v := range rec.H {
		hdr.Set(k, v)
	}
	return hdr, body
}

func TestAModernProbeGets400AndInitializeThenWorks(t *testing.T) {
	h := startHub(t, nil, nil)
	hdr, body := probe(t)
	req, err := http.NewRequest(http.MethodPost, h.url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		switch k {
		case "Host", "Content-Length", "Connection":
		case "Authorization":
			req.Header.Set(k, "Bearer "+string(h.token))
		default:
			req.Header[k] = v
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err = json.NewDecoder(resp.Body).Decode(&got)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusBadRequest || got.JSONRPC != "2.0" || got.Error.Code != -32600 ||
		got.Error.Message != noSession || string(got.ID) != `"server-discover-probe-1"` {
		t.Fatalf("probe: %d %+v %v", resp.StatusCode, got, err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("probe answer Content-Type %q", ct)
	}
	r := h.post("", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"claude-code"}}}`)
	sid := r.header.Get(HeaderSession)
	if r.status != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d %s", r.status, r.body)
	}
	if r = h.post(sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); r.status != http.StatusAccepted {
		t.Fatalf("initialized: %d %s", r.status, r.body)
	}
	r = h.post(sid, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, HeaderVersion, "2025-11-25")
	if r.status != http.StatusOK || !strings.Contains(string(r.body), `"name":"block"`) {
		t.Fatalf("tools/list: %d %s", r.status, r.body)
	}
}

func TestNotificationsAndResponsesGet202(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	for _, body := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":77}}`,
		`{"jsonrpc":"2.0","method":"notifications/whatever"}`,
		`{"jsonrpc":"2.0","id":5,"result":{}}`,
		`{"jsonrpc":"2.0","id":6,"error":{"code":-1,"message":"no"}}`,
	} {
		r := h.post(sid, body)
		if r.status != http.StatusAccepted || len(r.body) != 0 || r.header.Get("Content-Type") != "" {
			t.Errorf("%s: %d %q %s", body, r.status, r.header.Get("Content-Type"), r.body)
		}
	}
}

func TestABatchIsRefused(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	for _, s := range []string{"", sid} {
		r := h.post(s, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
		if code, msg, _ := r.rpcErr(); r.status != http.StatusBadRequest || code != -32600 || !strings.Contains(msg, "batch") {
			t.Errorf("batch, session %q: %d %s", s, r.status, r.body)
		}
	}
	r := h.post(sid, `{"jsonrpc":"2.0","id":1,`)
	if code, _, _ := r.rpcErr(); r.status != http.StatusBadRequest || code != -32700 {
		t.Errorf("parse error: %d %s", r.status, r.body)
	}
}

// TestOnlyJSONIsEverAnswered drives every kind of answer; the harness
// checks each one the transport wrote (§D2.5, JSON only).
func TestOnlyJSONIsEverAnswered(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	bodies := []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"slow","arguments":{"ms":1},"_meta":{"progressToken":"p1"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"server/discover"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":6,"result":{}}`,
		`[1]`,
		`nope`,
	}
	for _, b := range bodies {
		r := h.post(sid, b)
		if ct := r.header.Get("Content-Type"); len(r.body) > 0 && ct != "application/json" {
			t.Errorf("%s: Content-Type %q", b, ct)
		}
		if strings.Contains(string(r.body), `"method"`) {
			t.Errorf("%s: answer carries a method: %s", b, r.body)
		}
	}
	h.post("", `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	h.post(strings.Repeat("x", 43), `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	h.post(sid, `{"jsonrpc":"2.0","id":9,"method":"ping"}`, HeaderVersion, "1999-01-01")
	h.do(http.MethodDelete, sid, "")
	h.do(http.MethodDelete, sid, "")
	h.tr.Close()
	h.post("", initBody)
	h.mu.Lock()
	n := len(h.answers)
	h.mu.Unlock()
	if n < len(bodies)+7 {
		t.Fatalf("the transport wrote %d answers", n)
	}
}

func TestALongCallOutlivesTheServersWriteTimeout(t *testing.T) {
	h := startHub(t, nil, func(s *http.Server) { s.WriteTimeout = 300 * time.Millisecond })
	sid := h.initialize()
	r := h.post(sid, callBody(1, "slow", map[string]int{"ms": 600}))
	if text, isErr := r.toolText(); r.status != http.StatusOK || isErr || text != "slept" {
		t.Fatalf("slow call: %d %s", r.status, r.body)
	}
}

func TestTheBodyDeadlineIsClearedBeforeTheWait(t *testing.T) {
	h := startHub(t, func(o *Options) { o.IngressBodyTimeout = 100 * time.Millisecond }, nil)
	sid := h.initialize()
	c := h.dial()
	br := bufio.NewReader(c)
	for i := range 2 {
		if _, err := c.Write(h.rawRequest(sid, callBody(i, "slow", map[string]int{"ms": 500}))); err != nil {
			t.Fatalf("request %d on one connection: %v", i, err)
		}
		r := readReply(t, br)
		if text, isErr := r.toolText(); r.status != http.StatusOK || isErr || text != "slept" {
			t.Fatalf("request %d on one connection: %d %s", i, r.status, r.body)
		}
		if r.close {
			t.Fatalf("request %d: the server closes the connection", i)
		}
	}
	// A body deadline left on the connection would fail net/http's
	// background read during the wait and end the request's context.
	// net/http clears the deadline itself when a body is read to its end,
	// as here, so this checks the outcome, not which of the two cleared it.
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.answers[len(h.answers)-2:] {
		if a.reqErr != nil {
			t.Fatalf("the connection's read failed during the wait: %v", a.reqErr)
		}
	}
}

// TestAClientThatStopsReadingIsDroppedAfterTheWriteBound: an answer far
// larger than the socket buffers, to a client that reads nothing, holds
// its handler for WriteBound from the start of the write, not longer. The
// time to make the answer is not counted: under the race detector a
// 16 MiB answer takes about a second to build.
func TestAClientThatStopsReadingIsDroppedAfterTheWriteBound(t *testing.T) {
	const bound = 200 * time.Millisecond
	h := startHub(t, func(o *Options) { o.WriteBound = bound }, nil)
	sid := h.initialize()
	// initialize's handler counts as returned only after its answer left.
	eventually(t, time.Second, "initialize's handler returns", func() bool { return h.served.Load() == 1 })
	base := h.tr.Stats()
	c := h.dial()
	if _, err := c.Write(h.rawRequest(sid, callBody(1, "big", map[string]int{"bytes": 16 << 20}))); err != nil {
		t.Fatal(err)
	}
	// Without the bound the handler never returns; 10 s only keeps a
	// failing run short.
	eventually(t, 10*time.Second, "the handler returns", func() bool { return h.served.Load() == 2 })
	h.mu.Lock()
	a := h.answers[len(h.answers)-1]
	h.mu.Unlock()
	if a.wrote.IsZero() {
		t.Fatal("the transport wrote nothing")
	}
	if d := a.returned.Sub(a.wrote); d > bound+500*time.Millisecond {
		t.Fatalf("the handler returned %v after it started writing, WriteBound %v", d, bound)
	}
	if got := h.tr.Stats(); got != base {
		t.Fatalf("Stats %+v, baseline %+v", got, base)
	}
}

func TestAClosedConnectionDoesNotCancel(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	c := h.dial()
	if _, err := c.Write(h.rawRequest(sid, callBody(1, "block", nil))); err != nil {
		t.Fatal(err)
	}
	call := h.f.nextCall(t)
	_ = c.Close()
	// net/http notices the closed connection and cancels the request's
	// own context; the call's context is not that one.
	time.Sleep(100 * time.Millisecond)
	close(call.release)
	if err := call.end(t); err != nil {
		t.Fatalf("the call's context ended with the connection: %v", err)
	}
	eventually(t, 2*time.Second, "the call is unregistered", func() bool { return h.tr.Stats().InFlight == 0 })
}

func TestCancelledIsScopedToItsSession(t *testing.T) {
	h := startHub(t, nil, nil)
	a, b := h.initialize(), h.initialize()
	ans := h.callAsync(a, 7, "block", nil)
	call := h.f.nextCall(t)
	if r := h.post(b, cancelBody(7)); r.status != http.StatusAccepted {
		t.Fatalf("B's cancel: %d %s", r.status, r.body)
	}
	if r := h.post(a, cancelBody("7")); r.status != http.StatusAccepted {
		t.Fatalf("A's cancel of a string id: %d %s", r.status, r.body)
	}
	if err := call.ctx.Err(); err != nil {
		t.Fatalf("another session's cancel ended A's call: %v", err)
	}
	if r := h.post(a, cancelBody(7)); r.status != http.StatusAccepted {
		t.Fatalf("A's cancel: %d %s", r.status, r.body)
	}
	if err := call.end(t); err == nil {
		t.Fatal("A's own cancel left its call running")
	}
	r := await(t, ans, 2*time.Second)
	if text, isErr := r.toolText(); r.status != http.StatusOK || !isErr || !strings.Contains(text, "cancel") {
		t.Fatalf("A's cancelled call: %d %s", r.status, r.body)
	}
}

func TestADuplicateInFlightIDIsRefused(t *testing.T) {
	h := startHub(t, nil, nil)
	a, b := h.initialize(), h.initialize()
	first := h.callAsync(a, "x", "block", nil)
	call := h.f.nextCall(t)
	r := h.post(a, callBody("x", "slow", map[string]int{"ms": 1}))
	if code, _, id := r.rpcErr(); code != -32600 || string(id) != `"x"` {
		t.Fatalf("duplicate id: %d %s", r.status, r.body)
	}
	// The same id in another session, and another id in this one, run.
	if r = h.post(b, callBody("x", "slow", map[string]int{"ms": 1})); r.status != http.StatusOK {
		t.Fatalf("same id in another session: %d %s", r.status, r.body)
	}
	if r = h.post(a, callBody(1, "slow", map[string]int{"ms": 1})); r.status != http.StatusOK {
		t.Fatalf("another id: %d %s", r.status, r.body)
	}
	if err := call.ctx.Err(); err != nil {
		t.Fatalf("the refusal cancelled the first call: %v", err)
	}
	close(call.release)
	if r = await(t, first, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("first: %d %s", r.status, r.body)
	}
	if r = h.post(a, callBody("x", "slow", map[string]int{"ms": 1})); r.status != http.StatusOK {
		t.Fatalf("the id once answered: %d %s", r.status, r.body)
	}
}

func TestDeleteCancelsItsSessionsRequests(t *testing.T) {
	h := startHub(t, nil, nil)
	a, b := h.initialize(), h.initialize()
	ansA := h.callAsync(a, 1, "block", nil)
	callA := h.f.nextCall(t)
	ansB := h.callAsync(b, 1, "block", nil)
	callB := h.f.nextCall(t)
	if r := h.do(http.MethodDelete, a, ""); r.status != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", r.status, r.body)
	}
	if err := callA.end(t); err == nil {
		t.Fatal("DELETE left its session's call running")
	}
	if r := await(t, ansA, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("A's call: %d %s", r.status, r.body)
	}
	if err := callB.ctx.Err(); err != nil {
		t.Fatalf("DELETE of A ended B's call: %v", err)
	}
	close(callB.release)
	if r := await(t, ansB, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("B's call: %d %s", r.status, r.body)
	}
	if got := h.tr.Stats(); got.Sessions != 1 || got.InFlight != 0 {
		t.Fatalf("Stats %+v", got)
	}
}

func TestEvictionSkipsSessionsWithInFlightCalls(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxSessions = 2 }, nil)
	a := h.initialize()
	b := h.initialize()
	ansA := h.callAsync(a, 1, "block", nil)
	callA := h.f.nextCall(t)
	c := h.initialize()
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	if r := h.post(b, ping); r.status != http.StatusNotFound {
		t.Fatalf("evicted B: %d %s", r.status, r.body)
	}
	if r := h.post(a, ping); r.status != http.StatusOK {
		t.Fatalf("A, in flight: %d %s", r.status, r.body)
	}
	ansC := h.callAsync(c, 1, "block", nil)
	callC := h.f.nextCall(t)
	r := h.post("", initBody)
	if code, msg, _ := r.rpcErr(); r.status != http.StatusServiceUnavailable || code != -32000 || !strings.Contains(msg, "MaxSessions") ||
		r.header.Get(HeaderSession) != "" {
		t.Fatalf("initialize with every session pinned: %d %s", r.status, r.body)
	}
	close(callA.release)
	close(callC.release)
	await(t, ansA, 2*time.Second)
	await(t, ansC, 2*time.Second)
	// Unpinned now: A is the least recently used.
	h.initialize()
	if r := h.post(a, ping); r.status != http.StatusNotFound {
		t.Fatalf("A once idle: %d %s", r.status, r.body)
	}
	if r := h.post(c, ping); r.status != http.StatusOK {
		t.Fatalf("C: %d %s", r.status, r.body)
	}
}

// TestAnOldSessionIs404AndAnOldTokenIs401AfterARestart: a client that kept
// its session id and token across a server restart re-initializes (404,
// never 400) and fetches the new token (401 with no WWW-Authenticate).
func TestAnOldSessionIs404AndAnOldTokenIs401AfterARestart(t *testing.T) {
	first := startHub(t, nil, nil)
	sid := first.initialize()
	second := startHub(t, nil, nil)
	r := second.post(sid, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if r.status != http.StatusNotFound {
		t.Fatalf("old session: %d %s", r.status, r.body)
	}
	newToken := second.token
	second.token = first.token
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		r = second.do(m, sid, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if r.status != http.StatusUnauthorized || r.header.Values("WWW-Authenticate") != nil {
			t.Fatalf("%s with the old token: %d %v %s", m, r.status, r.header, r.body)
		}
	}
	second.token = newToken
	if fresh := second.initialize(); fresh == sid {
		t.Fatal("the new server reissued the old session id")
	}
}

func TestReceiptIsStampedAtParse(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	before := time.Now()
	if r := h.post(sid, callBody(1, "received", nil)); r.status != http.StatusOK {
		t.Fatalf("%d %s", r.status, r.body)
	}
	var got receipt
	select {
	case got = <-h.f.receipts:
	case <-time.After(2 * time.Second):
		t.Fatal("the received tool did not run")
	}
	if !got.ok || got.at.Before(before) || got.at.After(got.ran) {
		t.Fatalf("received %v ok %v; sent at %v, ran at %v", got.at, got.ok, before, got.ran)
	}
}

func TestCloseCancelsEverything(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	ans := h.callAsync(sid, 1, "block", nil)
	call := h.f.nextCall(t)
	start := time.Now()
	h.tr.Close()
	r := await(t, ans, time.Second)
	if text, isErr := r.toolText(); r.status != http.StatusOK || !isErr || !strings.Contains(text, "cancel") {
		t.Fatalf("in-flight call at Close: %d %s", r.status, r.body)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("answered %v after Close", d)
	}
	if call.ctx.Err() == nil {
		t.Fatal("the call's context outlived Close")
	}
	for _, s := range []string{"", sid} {
		r = h.post(s, initBody)
		if code, _, _ := r.rpcErr(); r.status != http.StatusServiceUnavailable || code != -32000 {
			t.Fatalf("after Close, session %q: %d %s", s, r.status, r.body)
		}
	}
	if r = h.do(http.MethodDelete, sid, ""); r.status != http.StatusServiceUnavailable {
		t.Fatalf("DELETE after Close: %d %s", r.status, r.body)
	}
	if n := h.f.flushed.Load(); n != 1 {
		t.Fatalf("FlushWarnings called %d times", n)
	}
	h.tr.Close()
	if n := h.f.flushed.Load(); n != 1 {
		t.Fatalf("a second Close flushed again: %d", n)
	}
	if got := h.tr.Stats(); got != (Stats{}) {
		t.Fatalf("Stats after Close %+v", got)
	}
}

// TestConcurrentCallsOnOneSession runs many calls of one session at once:
// each gets its own answer, and the session's label never changes.
func TestConcurrentCallsOnOneSession(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxCalls, o.MaxCallsPerSession, o.MaxIngressReaders = 16, 16, 16 }, nil)
	sid := h.initialize()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			r := await(t, h.callAsync(sid, i, "slow", map[string]int{"ms": 20}), 5*time.Second)
			var b struct {
				ID int `json:"id"`
			}
			_ = json.Unmarshal(r.body, &b)
			if text, _ := r.toolText(); r.status != http.StatusOK || b.ID != i || text != "slept" {
				t.Errorf("call %d: %d %s", i, r.status, r.body)
			}
		})
	}
	wg.Wait()
	if got := h.tr.Stats(); got.InFlight != 0 || got.Sessions != 1 {
		t.Fatalf("Stats %+v", got)
	}
}

// TestALargeAnswerIsReplaced bounds an answer body by MaxAnswerBytes, as
// the stdio output queue bounds an answer line.
func TestALargeAnswerIsReplaced(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxAnswerBytes = 64 << 10 }, nil)
	sid := h.initialize()
	r := h.post(sid, callBody(1, "big", map[string]int{"bytes": 63 << 10}))
	if text, isErr := r.toolText(); r.status != http.StatusOK || isErr || len(text) != 63<<10 {
		t.Fatalf("an answer under the bound: %d, %d bytes", r.status, len(r.body))
	}
	r = h.post(sid, callBody(2, "big", map[string]int{"bytes": 64 << 10}))
	if text, isErr := r.toolText(); r.status != http.StatusOK || !isErr || !strings.Contains(text, "too large") {
		t.Fatalf("an answer over the bound: %d %.200s", r.status, r.body)
	}
	if !strings.Contains(string(r.body), `"id":2`) {
		t.Fatalf("the replacement lost the id: %s", r.body)
	}
}

func TestStatsCountsWhatIsInFlight(t *testing.T) {
	h := startHub(t, nil, nil)
	sid := h.initialize()
	var answers []<-chan reply
	var calls []*call
	for i := range 3 {
		answers = append(answers, h.callAsync(sid, i, "block", nil))
		calls = append(calls, h.f.nextCall(t))
	}
	if got := h.tr.Stats(); got.InFlight != 3 || got.Sessions != 1 || got.Readers != 0 || got.ReservedBytes != 0 {
		t.Fatalf("Stats %+v", got)
	}
	for i, c := range calls {
		close(c.release)
		if r := await(t, answers[i], 2*time.Second); r.status != http.StatusOK {
			t.Fatalf("call %d: %d", i, r.status)
		}
	}
	if got := h.tr.Stats(); got.InFlight != 0 {
		t.Fatalf("Stats %+v", got)
	}
}

func TestNewAppliesTheDefaults(t *testing.T) {
	tr := New(newFake(), Options{})
	defer tr.Close()
	want := Options{MaxSessions: 32, MaxMessageBytes: 4 << 20, IngressBodyTimeout: 2 * time.Second,
		WriteBound: 10 * time.Second, MaxAnswerBytes: 4 << 20,
		MaxIngressReaders: 16, MaxIngressBytes: 8 << 20, MaxControlInFlight: 32, MaxPageStateInFlight: 4,
		MaxCalls: 16, MaxCallsPerSession: 4, MaxRequestBytes: 16 << 20}
	got := tr.o
	got.Lifetime, got.Log = nil, nil
	if got != want {
		t.Fatalf("options %+v, want %+v", got, want)
	}
}

// TestABodyThatStallsClosesItsConnection: a body that stops arriving short
// of its Content-Length is answered, and its connection closed, within
// IngressBodyTimeout, while the server still runs; net/http's drain of the
// unread rest must not wait on the client.
func TestABodyThatStallsClosesItsConnection(t *testing.T) {
	const timeout = 100 * time.Millisecond
	h := startHub(t, func(o *Options) { o.IngressBodyTimeout = timeout }, nil)
	sid := h.initialize()
	req := h.rawRequest(sid, callBody(1, "slow", map[string]int{"ms": 1}))
	c := h.dial()
	start := time.Now()
	if _, err := c.Write(req[:len(req)-10]); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(timeout + time.Second))
	_, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("the connection stayed open %v after the body stalled: %v", time.Since(start), err)
	}
	if d := time.Since(start); d > timeout+500*time.Millisecond {
		t.Fatalf("the connection closed %v after the body stalled", d)
	}
	eventually(t, time.Second, "the reader is released", func() bool { return h.tr.Stats().Readers == 0 })
}
