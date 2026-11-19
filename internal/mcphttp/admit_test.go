package mcphttp

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

const (
	pingBody     = `{"jsonrpc":"2.0","id":"ping","method":"ping"}`
	toolsList    = `{"jsonrpc":"2.0","id":"list","method":"tools/list"}`
	initialized  = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	controlBound = 100 * time.Millisecond
)

// wantBusy fails unless r is the 503 refusal of limit at n for id, with
// Retry-After.
func wantBusy(t *testing.T, what string, r reply, limit string, n int64, id string) {
	t.Helper()
	wantBusyText(t, what, r, fmt.Sprintf("server busy: %s=%d reached; retry", limit, n), id)
}

// wantBusyKept is wantBusy for a large body refused by an ingress cap at
// n of which kept is held for small messages.
func wantBusyKept(t *testing.T, what string, r reply, limit string, n, kept int64, id string) {
	t.Helper()
	wantBusyText(t, what, r, fmt.Sprintf("server busy: %s=%d reached (%d kept for small messages); retry", limit, n, kept), id)
}

// wantBusyText fails unless r is the 503 refusal want for id, with
// Retry-After.
func wantBusyText(t *testing.T, what string, r reply, want, id string) {
	t.Helper()
	code, msg, gotID := r.rpcErr()
	if r.status != http.StatusServiceUnavailable || code != -32000 || msg != want || string(gotID) != id ||
		r.header.Get("Retry-After") != "1" {
		t.Fatalf("%s: want 503 %q for id %s; got %d Retry-After %q %s", what, want, id, r.status, r.header.Get("Retry-After"), r.body)
	}
}

// timed is f's reply, failing when it took longer than bound.
func timed(t *testing.T, what string, bound time.Duration, f func() reply) reply {
	t.Helper()
	start := time.Now()
	r := f()
	if d := time.Since(start); d > bound {
		t.Fatalf("%s answered after %v, over %v", what, d, bound)
	}
	return r
}

func readsBody(uri string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":"r","method":"resources/read","params":{"uri":%q}}`, uri)
}

func TestClassify(t *testing.T) {
	f := newFake()
	cases := []struct {
		msg  string
		want Class
	}{
		{initialized, ClassControl},
		{cancelBody(1), ClassControl},
		{pingBody, ClassControl},
		{initBody, ClassControl},
		{toolsList, ClassControl},
		{`{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`, ClassControl},
		{`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, ClassControl},
		{`{"jsonrpc":"2.0","id":1,"method":"resources/templates/list"}`, ClassControl},
		{callBody(1, "capture_cancel", nil), ClassControl},
		{callBody(1, "page_state", nil), ClassPageState},
		{readsBody(pageStateURI), ClassPageState},
		{callBody(1, "block", nil), ClassCalls},
		{callBody(1, "nope", nil), ClassCalls},
		{readsBody(otherURI), ClassCalls},
		{readsBody("perf-agent://nope"), ClassCalls},
		{`{"jsonrpc":"2.0","id":1,"method":"prompts/get","params":{"name":"x"}}`, ClassCalls},
		{`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, ClassCalls},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, ClassCalls},
	}
	for _, c := range cases {
		req, bad := f.Parse([]byte(c.msg))
		if bad != nil || req == nil {
			t.Fatalf("%s: does not parse: %s", c.msg, bad)
		}
		if got := classify(f, req); got != c.want {
			t.Errorf("%s: class %v, want %v", c.msg, got, c.want)
		}
	}
	if got := classify(f, &mcpstdio.Request{Method: "tools/call", ID: []byte("1"), Tool: "page_state"}); got != ClassPageState {
		t.Errorf("a parsed Snapshot call: %v", got)
	}
}

// TestSameSessionSaturation: one session far beyond MaxCallsPerSession,
// then the server-wide cap from several sessions. The excess is refused
// at once, goroutines and request bytes stay under the caps, controls from
// the saturating session and from others are answered, and the admitted
// calls complete (§D2.4).
func TestSameSessionSaturation(t *testing.T) {
	// Forty bodies may be read at the same instant; the reader cap is not
	// what this test is about.
	h := startHub(t, func(o *Options) { o.MaxIngressReaders = 128 }, nil)
	a := h.initialize()
	var others []string
	for range 5 {
		others = append(others, h.initialize())
	}
	idle := h.initialize()
	baseline := runtime.NumGoroutine()

	fromA := make(chan reply, 40)
	for i := range 40 {
		h.callInto(fromA, a, i, "block", nil)
	}
	var held []*call
	for range 4 {
		held = append(held, h.f.nextCall(t))
	}
	seen := map[string]bool{}
	for range 36 {
		r := await(t, fromA, 2*time.Second)
		_, _, id := r.rpcErr()
		wantBusy(t, "A's call "+string(id), r, "MaxCallsPerSession", 4, string(id))
		if seen[string(id)] {
			t.Fatalf("two answers for id %s", id)
		}
		seen[string(id)] = true
	}

	fromOthers := make(chan reply, 20)
	for i, sid := range others {
		for j := range 4 {
			h.callInto(fromOthers, sid, 100+10*i+j, "block", nil)
		}
	}
	for range 12 {
		held = append(held, h.f.nextCall(t))
	}
	for range 8 {
		r := await(t, fromOthers, 2*time.Second)
		_, _, id := r.rpcErr()
		wantBusy(t, "call "+string(id), r, "MaxCalls", 16, string(id))
	}
	select {
	case c := <-h.f.calls:
		t.Fatalf("a 17th call started: %v", c)
	default:
	}
	st := h.tr.Stats()
	if st.Calls != 16 || st.InFlight != 16 || st.Readers != 0 || st.ReservedBytes != 0 || st.Control != 0 ||
		st.RequestBytes <= 0 || st.RequestBytes > 16*64 {
		t.Fatalf("Stats %+v", st)
	}
	// Each held call is a server connection, its background read, the
	// client's two connection loops and the sending goroutine; the 44
	// refused requests hold nothing.
	eventually(t, 2*time.Second, "goroutines under the caps", func() bool {
		return runtime.NumGoroutine() <= baseline+16*6
	})

	for _, sid := range []string{a, others[0]} {
		for _, body := range []string{pingBody, toolsList} {
			if r := timed(t, body, controlBound, func() reply { return h.post(sid, body) }); r.status != http.StatusOK {
				t.Fatalf("%s: %d %s", body, r.status, r.body)
			}
		}
		if r := timed(t, "cancelled", controlBound, func() reply { return h.post(sid, cancelBody(999)) }); r.status != http.StatusAccepted {
			t.Fatalf("cancelled: %d %s", r.status, r.body)
		}
	}
	if r := timed(t, "DELETE", controlBound, func() reply { return h.do(http.MethodDelete, idle, "") }); r.status != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", r.status, r.body)
	}

	for _, c := range held {
		close(c.release)
	}
	for ch, n := range map[chan reply]int{fromA: 4, fromOthers: 12} {
		for range n {
			r := await(t, ch, 2*time.Second)
			if text, isErr := r.toolText(); r.status != http.StatusOK || isErr || text != "released" {
				t.Fatalf("an admitted call: %d %s", r.status, r.body)
			}
		}
	}
	if st := h.tr.Stats(); st.Calls != 0 || st.InFlight != 0 || st.RequestBytes != 0 {
		t.Fatalf("Stats after the calls %+v", st)
	}
}

// TestControlIsServedBehindAFullCallClass: the call class is full of
// small fast POSTs queued behind a tool holding the engine; a cancel and a
// capture_cancel sent behind them are read, classed as control and
// answered before any queued call starts (§D2.4).
func TestControlIsServedBehindAFullCallClass(t *testing.T) {
	h := startHub(t, nil, nil)
	var sids []string
	for range 4 {
		sids = append(sids, h.initialize())
	}
	holder := h.callAsync(sids[0], "hold", "hold", nil)
	held := h.f.nextCall(t)
	queued := make(chan reply, 15)
	for i := range 15 {
		h.callInto(queued, sids[(i+1)/4], fmt.Sprintf("q%d", i), "queued", nil)
	}
	eventually(t, 2*time.Second, "the call class fills", func() bool { return h.tr.Stats().Calls == 16 })
	wantBusy(t, "a 17th call", h.post(sids[3], callBody("x", "queued", nil)), "MaxCalls", 16, `"x"`)

	// q3 is the first call of sids[1].
	r := timed(t, "cancelled", controlBound, func() reply { return h.post(sids[1], cancelBody("q3")) })
	if r.status != http.StatusAccepted {
		t.Fatalf("cancelled: %d %s", r.status, r.body)
	}
	r = timed(t, "capture_cancel", controlBound, func() reply { return h.post(sids[2], callBody("c", "capture_cancel", nil)) })
	if text, isErr := r.toolText(); r.status != http.StatusOK || isErr || text != "cancelling" {
		t.Fatalf("capture_cancel: %d %s", r.status, r.body)
	}
	if n := h.f.started.Load(); n != 0 {
		t.Fatalf("%d queued calls started before the controls were answered", n)
	}
	r = await(t, queued, 2*time.Second)
	if text, isErr := r.toolText(); r.status != http.StatusOK || !isErr || !strings.Contains(text, "cancelled while queued") ||
		!strings.Contains(string(r.body), `"id":"q3"`) {
		t.Fatalf("the cancelled queued call: %d %s", r.status, r.body)
	}

	close(held.release)
	if r = await(t, holder, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("holder: %d %s", r.status, r.body)
	}
	for range 14 {
		r = await(t, queued, 2*time.Second)
		if text, _ := r.toolText(); r.status != http.StatusOK || text != "ran" {
			t.Fatalf("a queued call: %d %s", r.status, r.body)
		}
	}
	if n := h.f.started.Load(); n != 14 {
		t.Fatalf("%d queued calls ran, want 14", n)
	}
}

// TestSlowIncompleteBodiesFillOnlyTheReaders: requests that announce a
// body and send a little of it fill the readers, never the classes. While
// they do, every new request, a ping and a cancel included, gets 503
// unread; each stalled body is dropped at IngressBodyTimeout and its
// reader and bytes released; then a cancel is served, and goroutines and
// bytes are back to their baseline with the server still running (§D2.4,
// §D2.4).
func TestSlowIncompleteBodiesFillOnlyTheReaders(t *testing.T) {
	const timeout = 300 * time.Millisecond
	h := startHub(t, func(o *Options) { o.IngressBodyTimeout = timeout }, nil)
	sid := h.initialize()
	eventually(t, time.Second, "initialize's handler returns", func() bool { return h.served.Load() == 1 })
	h.client.CloseIdleConnections()
	baseline := runtime.NumGoroutine()
	base := h.tr.Stats()

	var slow []net.Conn
	start := time.Now()
	for range 16 {
		c := h.dial()
		if _, err := c.Write(append(h.rawHead(sid, 1024), "0123456789"...)); err != nil {
			t.Fatal(err)
		}
		slow = append(slow, c)
	}
	eventually(t, time.Second, "16 readers", func() bool {
		st := h.tr.Stats()
		return st.Readers == 16 && st.ReservedBytes == 16*1024
	})
	if st := h.tr.Stats(); st.Calls != 0 || st.Control != 0 || st.PageState != 0 || st.InFlight != 0 {
		t.Fatalf("the slow bodies reached a class: %+v", st)
	}

	// Headers alone get the refusal: the body is never waited for.
	c := h.dial()
	if _, err := c.Write(h.rawHead(sid, len(pingBody))); err != nil {
		t.Fatal(err)
	}
	r := timed(t, "a ping's headers", controlBound, func() reply { return readReply(t, bufio.NewReader(c)) })
	wantBusy(t, "a ping behind the readers", r, "MaxIngressReaders", 16, "null")
	if !r.close {
		t.Fatal("an unread refusal does not say it closes its connection")
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("an unread refusal keeps its connection: %v", err)
	}
	_ = c.Close()
	for _, body := range []string{pingBody, cancelBody(1), callBody(1, "capture_cancel", nil)} {
		r = timed(t, body, controlBound, func() reply { return h.post(sid, body) })
		wantBusy(t, body, r, "MaxIngressReaders", 16, "null")
	}

	for i, c := range slow {
		_ = c.SetReadDeadline(time.Now().Add(timeout + time.Second))
		if _, err := io.ReadAll(c); err != nil {
			t.Fatalf("slow connection %d still open %v after its body stalled: %v", i, time.Since(start), err)
		}
		_ = c.Close()
	}
	if d := time.Since(start); d > timeout+500*time.Millisecond {
		t.Fatalf("the slow connections closed %v after their bodies stalled", d)
	}
	eventually(t, time.Second, "the readers are released", func() bool { return h.tr.Stats() == base })

	if r = h.post(sid, cancelBody(1)); r.status != http.StatusAccepted {
		t.Fatalf("a cancel after the drop: %d %s", r.status, r.body)
	}
	if r = h.post(sid, pingBody); r.status != http.StatusOK {
		t.Fatalf("a ping after the drop: %d %s", r.status, r.body)
	}
	h.client.CloseIdleConnections()
	waitGoroutines(t, baseline)
	if st := h.tr.Stats(); st != base {
		t.Fatalf("Stats %+v, baseline %+v", st, base)
	}
}

// TestControlClassSaturation: with MaxControlInFlight controls running,
// the next control of every kind gets 503 at once, calls and page_state
// are not affected, and controls are served again as soon as one frees
// (§D2.4). Notifications and DELETE are answered under the transport's
// lock and hold no place, so a cancel gets through a full class: a client
// never retries a notification.
func TestControlClassSaturation(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxControlInFlight = 4 }, nil)
	a, b, idle := h.initialize(), h.initialize(), h.initialize()
	var answers []<-chan reply
	var held []*call
	for i := range 4 {
		answers = append(answers, h.callAsync(a, fmt.Sprintf("c%d", i), "capture_cancel", map[string]bool{"block": true}))
		held = append(held, h.f.nextCall(t))
	}
	if st := h.tr.Stats(); st.Control != 4 || st.Calls != 0 {
		t.Fatalf("Stats %+v", st)
	}
	busy := func(what, sid, body, id string) {
		t.Helper()
		r := timed(t, what, controlBound, func() reply { return h.post(sid, body) })
		wantBusy(t, what, r, "MaxControlInFlight", 4, id)
	}
	busy("ping", a, pingBody, `"ping"`)
	busy("ping from another session", b, pingBody, `"ping"`)
	busy("tools/list", b, toolsList, `"list"`)
	busy("capture_cancel", b, callBody("x", "capture_cancel", nil), `"x"`)
	busy("initialize", "", initBody, "0")

	r := timed(t, "initialized", controlBound, func() reply { return h.post(b, initialized) })
	if r.status != http.StatusAccepted {
		t.Fatalf("initialized with the class full: %d %s", r.status, r.body)
	}
	r = timed(t, "DELETE", controlBound, func() reply { return h.do(http.MethodDelete, idle, "") })
	if r.status != http.StatusNoContent {
		t.Fatalf("DELETE with the class full: %d %s", r.status, r.body)
	}

	if r = h.post(b, callBody(5, "slow", map[string]int{"ms": 1})); r.status != http.StatusOK {
		t.Fatalf("a call: %d %s", r.status, r.body)
	}
	if r = h.post(b, callBody(6, "page_state", nil)); r.status != http.StatusOK {
		t.Fatalf("page_state: %d %s", r.status, r.body)
	}
	if r = h.post(b, readsBody(pageStateURI)); r.status != http.StatusOK {
		t.Fatalf("the page state resource: %d %s", r.status, r.body)
	}

	r = timed(t, "cancelled", controlBound, func() reply { return h.post(a, cancelBody("c0")) })
	if r.status != http.StatusAccepted {
		t.Fatalf("a cancel with the class full: %d %s", r.status, r.body)
	}
	if err := held[0].end(t); err == nil {
		t.Fatal("the cancel sent with the class full did not end its call")
	}
	if r = await(t, answers[0], 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("the cancelled control: %d %s", r.status, r.body)
	}
	eventually(t, time.Second, "a control frees", func() bool { return h.tr.Stats().Control == 3 })
	if r = timed(t, "ping once freed", controlBound, func() reply { return h.post(a, pingBody) }); r.status != http.StatusOK {
		t.Fatalf("ping once freed: %d %s", r.status, r.body)
	}
	for i, c := range held[1:] {
		close(c.release)
		await(t, answers[i+1], 2*time.Second)
	}
	if st := h.tr.Stats(); st.Control != 0 {
		t.Fatalf("Stats %+v", st)
	}
}

// TestTheIngressKeepsAReserveForSmallBodies: bodies over 64 KiB that
// stall take every reader but one, and every byte but 64 KiB, so a small
// cancel, capture_cancel or ping is still read while they do (§D2.4).
func TestTheIngressKeepsAReserveForSmallBodies(t *testing.T) {
	stall := func(t *testing.T, h *hub, sid string, n, conns int) {
		t.Helper()
		for range conns {
			if _, err := h.dial().Write(append(h.rawHead(sid, n), "0123456789"...)); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, time.Second, "the stalled readers", func() bool { return h.tr.Stats().Readers == conns })
	}
	headers := func(t *testing.T, h *hub, sid string, n int) reply {
		t.Helper()
		c := h.dial()
		defer func() { _ = c.Close() }()
		if _, err := c.Write(h.rawHead(sid, n)); err != nil {
			t.Fatal(err)
		}
		return timed(t, "a large body's headers", controlBound, func() reply { return readReply(t, bufio.NewReader(c)) })
	}
	smallControlsGetThrough := func(t *testing.T, h *hub, sid string) {
		t.Helper()
		ans := h.callAsync(sid, "b", "block", nil)
		held := h.f.nextCall(t)
		if r := timed(t, "ping", controlBound, func() reply { return h.post(sid, pingBody) }); r.status != http.StatusOK {
			t.Fatalf("ping: %d %s", r.status, r.body)
		}
		r := timed(t, "capture_cancel", controlBound, func() reply { return h.post(sid, callBody("c", "capture_cancel", nil)) })
		if text, _ := r.toolText(); r.status != http.StatusOK || text != "cancelling" {
			t.Fatalf("capture_cancel: %d %s", r.status, r.body)
		}
		if r = timed(t, "cancelled", controlBound, func() reply { return h.post(sid, cancelBody("b")) }); r.status != http.StatusAccepted {
			t.Fatalf("cancelled: %d %s", r.status, r.body)
		}
		if err := held.end(t); err == nil {
			t.Fatal("the cancel did not end its call")
		}
		await(t, ans, 2*time.Second)
	}

	t.Run("readers", func(t *testing.T) {
		h := startHub(t, nil, nil)
		sid := h.initialize()
		stall(t, h, sid, 100<<10, 15)
		wantBusyKept(t, "a 16th large body", headers(t, h, sid, 100<<10), "MaxIngressReaders", 16, 1, "null")
		smallControlsGetThrough(t, h, sid)
	})
	t.Run("bytes", func(t *testing.T) {
		h := startHub(t, func(o *Options) { o.MaxIngressBytes, o.MaxMessageBytes = 2<<20, 1<<20 }, nil)
		sid := h.initialize()
		stall(t, h, sid, 1<<20, 1)
		wantBusyKept(t, "a second 1 MiB body", headers(t, h, sid, 1<<20), "MaxIngressBytes", 2<<20, 64<<10, "null")
		smallControlsGetThrough(t, h, sid)
	})
}

// TestHeaderOnlyRejection: a Content-Length over what is left of
// MaxIngressBytes gets 503, and one over MaxMessageBytes 413, from the
// headers alone; a small body still fits beside a stalled one (§D2.4).
func TestHeaderOnlyRejection(t *testing.T) {
	h := startHub(t, func(o *Options) {
		o.MaxIngressBytes = 768 << 10
		o.MaxMessageBytes = 512 << 10
	}, nil)
	sid := h.initialize()
	stalled := h.dial()
	if _, err := stalled.Write(append(h.rawHead(sid, 500<<10), "0123456789"...)); err != nil {
		t.Fatal(err)
	}
	eventually(t, time.Second, "one reader", func() bool { return h.tr.Stats().ReservedBytes == 500<<10 })

	headers := func(what string, n int) reply {
		t.Helper()
		c := h.dial()
		defer func() { _ = c.Close() }()
		if _, err := c.Write(h.rawHead(sid, n)); err != nil {
			t.Fatal(err)
		}
		return timed(t, what, controlBound, func() reply { return readReply(t, bufio.NewReader(c)) })
	}
	r := headers("over the bytes left", 300<<10)
	wantBusyKept(t, "over the bytes left", r, "MaxIngressBytes", 768<<10, 64<<10, "null")
	if !r.close {
		t.Fatal("an unread refusal does not say it closes its connection")
	}
	if r = h.post(sid, pingBody); r.status != http.StatusOK {
		t.Fatalf("a small body beside the stalled one: %d %s", r.status, r.body)
	}
	if r = headers("over MaxMessageBytes", 600<<10); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over MaxMessageBytes: %d %s", r.status, r.body)
	}
	if r = headers("over the guard's bound", 2<<20); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("over the guard's bound: %d %s", r.status, r.body)
	}
	_ = stalled.Close()
	eventually(t, 3*time.Second, "the reader is released", func() bool {
		st := h.tr.Stats()
		return st.Readers == 0 && st.ReservedBytes == 0
	})
}

// TestPageStateHasItsOwnClass: page_state is admitted with the call class
// full, MaxPageStateInFlight bounds it, the tool and the resource alike,
// and calls are admitted with it full (§D2.4).
func TestPageStateHasItsOwnClass(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxCalls, o.MaxCallsPerSession = 4, 4 }, nil)
	a := h.initialize()
	block := map[string]bool{"block": true}
	var states, calls []<-chan reply
	var heldStates, heldCalls []*call
	for i := range 3 {
		states = append(states, h.callAsync(a, fmt.Sprintf("p%d", i), "page_state", block))
		heldStates = append(heldStates, h.f.nextCall(t))
	}
	for i := range 4 {
		calls = append(calls, h.callAsync(a, fmt.Sprintf("c%d", i), "block", nil))
		heldCalls = append(heldCalls, h.f.nextCall(t))
	}
	wantBusy(t, "a fifth call", h.post(a, callBody("x", "slow", map[string]int{"ms": 1})), "MaxCalls", 4, `"x"`)
	wantBusy(t, "a plain resource", h.post(a, readsBody(otherURI)), "MaxCalls", 4, `"r"`)

	states = append(states, h.callAsync(a, "p3", "page_state", block))
	heldStates = append(heldStates, h.f.nextCall(t))
	if st := h.tr.Stats(); st.PageState != 4 || st.Calls != 4 {
		t.Fatalf("Stats %+v", st)
	}
	wantBusy(t, "a fifth page_state", h.post(a, callBody("p4", "page_state", nil)), "MaxPageStateInFlight", 4, `"p4"`)
	wantBusy(t, "the page state resource", h.post(a, readsBody(pageStateURI)), "MaxPageStateInFlight", 4, `"r"`)
	if r := h.post(a, pingBody); r.status != http.StatusOK {
		t.Fatalf("ping: %d %s", r.status, r.body)
	}

	close(heldCalls[0].release)
	await(t, calls[0], 2*time.Second)
	eventually(t, time.Second, "a call frees", func() bool { return h.tr.Stats().Calls == 3 })
	if r := h.post(a, callBody("y", "slow", map[string]int{"ms": 1})); r.status != http.StatusOK {
		t.Fatalf("a call with page_state full: %d %s", r.status, r.body)
	}
	for i, c := range heldStates {
		close(c.release)
		if r := await(t, states[i], 2*time.Second); r.status != http.StatusOK {
			t.Fatalf("page_state %d: %d %s", i, r.status, r.body)
		}
	}
	eventually(t, time.Second, "page_state frees", func() bool { return h.tr.Stats().PageState == 0 })
	if r := h.post(a, readsBody(pageStateURI)); r.status != http.StatusOK {
		t.Fatalf("the page state resource once freed: %d %s", r.status, r.body)
	}
	for i, c := range heldCalls[1:] {
		close(c.release)
		await(t, calls[i+1], 2*time.Second)
	}
}

// TestRequestBytesAreRetainedArguments: MaxRequestBytes counts the
// arguments the admitted calls hold, and frees them as each completes.
func TestRequestBytesAreRetainedArguments(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxRequestBytes = 1 << 20 }, nil)
	a := h.initialize()
	args := map[string]string{"pad": strings.Repeat("a", 600<<10)}
	first := h.callAsync(a, 1, "block", args)
	held := h.f.nextCall(t)
	if st := h.tr.Stats(); st.RequestBytes < 600<<10 || st.RequestBytes > 600<<10+64 || st.ReservedBytes != 0 {
		t.Fatalf("Stats %+v", st)
	}
	wantBusy(t, "a second large call", await(t, h.callAsync(a, 2, "block", args), time.Second), "MaxRequestBytes", 1<<20, "2")
	if r := h.post(a, callBody(3, "slow", map[string]int{"ms": 1})); r.status != http.StatusOK {
		t.Fatalf("a small call: %d %s", r.status, r.body)
	}
	// Controls and page_state retain no request bytes.
	if r := h.post(a, callBody(4, "page_state", args)); r.status != http.StatusOK {
		t.Fatalf("a large page_state: %d %s", r.status, r.body)
	}
	close(held.release)
	if r := await(t, first, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("first: %d %s", r.status, r.body)
	}
	eventually(t, time.Second, "the bytes are released", func() bool { return h.tr.Stats().RequestBytes == 0 })
	second := h.callAsync(a, 5, "block", args)
	held = h.f.nextCall(t)
	close(held.release)
	if r := await(t, second, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("a large call once freed: %d %s", r.status, r.body)
	}
}

// TestAnOrphanedCallStillCounts: a client that drops its connection does
// not cancel its call, so the call keeps its place in the call class
// until it ends. A retry sent at once is admitted while there is room and
// refused with 503 and Retry-After after that, never left to hang.
func TestAnOrphanedCallStillCounts(t *testing.T) {
	h := startHub(t, func(o *Options) { o.MaxCalls, o.MaxCallsPerSession = 2, 2 }, nil)
	a := h.initialize()
	c := h.dial()
	if _, err := c.Write(h.rawRequest(a, callBody(1, "block", nil))); err != nil {
		t.Fatal(err)
	}
	orphan := h.f.nextCall(t)
	_ = c.Close()

	retry := h.callAsync(a, 2, "block", nil)
	retried := h.f.nextCall(t)
	for _, id := range []int{1, 3} {
		r := await(t, h.callAsync(a, id, "block", nil), time.Second)
		wantBusy(t, fmt.Sprintf("a retry with id %d", id), r, "MaxCalls", 2, fmt.Sprint(id))
	}
	if st := h.tr.Stats(); st.Calls != 2 || st.InFlight != 2 {
		t.Fatalf("Stats %+v", st)
	}
	if err := orphan.ctx.Err(); err != nil {
		t.Fatalf("the dropped connection cancelled its call: %v", err)
	}
	close(orphan.release)
	if err := orphan.end(t); err != nil {
		t.Fatalf("the orphan ended with %v", err)
	}
	eventually(t, 2*time.Second, "the orphan frees its place", func() bool { return h.tr.Stats().Calls == 1 })
	if r := await(t, h.callAsync(a, 4, "slow", map[string]int{"ms": 1}), 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("a call once the orphan ended: %d %s", r.status, r.body)
	}
	close(retried.release)
	if r := await(t, retry, 2*time.Second); r.status != http.StatusOK {
		t.Fatalf("the retry: %d %s", r.status, r.body)
	}
}
