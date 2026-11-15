package livehttp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startHub serves h on a loopback listener through NewServer and returns the
// listener address. tune may adjust the server before it starts. At cleanup
// the hub and server close and the goroutine count must return to what it
// was before the server started.
func startHub(t *testing.T, h *Hub, tune ...func(*http.Server)) string {
	t.Helper()
	noLeaks(t)
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	srv := NewServer(h, nil)
	for _, f := range tune {
		f(srv)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		h.Close()
		_ = srv.Close()
		<-served
	})
	return ln.Addr().String()
}

func noLeaks(t *testing.T) {
	t.Helper()
	baseline := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > baseline {
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<20)
				t.Errorf("%d goroutines, baseline %d:\n%s",
					runtime.NumGoroutine(), baseline, buf[:runtime.Stack(buf, true)])
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

func dialHub(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func request(t *testing.T, conn net.Conn, method, addr string) {
	t.Helper()
	if _, err := fmt.Fprintf(conn, "%s /events HTTP/1.1\r\nHost: %s\r\n\r\n", method, addr); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

// connect sends one GET and reads the response head.
func connect(t *testing.T, addr string) (*http.Response, net.Conn) {
	t.Helper()
	conn := dialHub(t, addr)
	request(t, conn, http.MethodGet, addr)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp, conn
}

type sseStream struct {
	conn net.Conn
	body *bufio.Reader
}

type sseEvent struct {
	comment string
	name    string
	data    string
}

func openStream(t *testing.T, addr string) *sseStream {
	t.Helper()
	resp, conn := connect(t, addr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	return &sseStream{conn: conn, body: bufio.NewReader(resp.Body)}
}

// next reads one event or comment block. A read error, EOF included, ends
// the stream for good: the chunked body reader keeps it.
func (s *sseStream) next(within time.Duration) (sseEvent, error) {
	_ = s.conn.SetReadDeadline(time.Now().Add(within))
	var ev sseEvent
	for {
		line, err := s.body.ReadString('\n')
		if err != nil {
			return ev, err
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if ev != (sseEvent{}) {
				return ev, nil
			}
			continue
		}
		if c, ok := strings.CutPrefix(line, ": "); ok {
			ev.comment = c
		} else if n, ok := strings.CutPrefix(line, "event: "); ok {
			ev.name = n
		} else if d, ok := strings.CutPrefix(line, "data: "); ok {
			ev.data = d
		} else {
			return ev, fmt.Errorf("unexpected line %q", line)
		}
	}
}

// nextState skips keep-alives and returns the seq of the next state event.
func (s *sseStream) nextState(t *testing.T) int {
	t.Helper()
	for {
		ev, err := s.next(5 * time.Second)
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		if ev.comment == "keep-alive" && ev.name == "" && ev.data == "" {
			continue
		}
		if ev.name != "state" || ev.comment != "" {
			t.Fatalf("event %+v, want a state event", ev)
		}
		return seqOf(t, ev.data)
	}
}

func seqOf(t *testing.T, data string) int {
	t.Helper()
	var v struct {
		Seq string `json:"seq"`
	}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("event data %.80q: %v", data, err)
	}
	n, err := strconv.Atoi(v.Seq)
	if err != nil {
		t.Fatalf("seq %q: %v", v.Seq, err)
	}
	return n
}

func state(seq, pad int) []byte {
	if pad == 0 {
		return fmt.Appendf(nil, `{"seq":"%d"}`, seq)
	}
	return fmt.Appendf(nil, `{"seq":"%d","pad":"%s"}`, seq, strings.Repeat("x", pad))
}

func waitClients(t *testing.T, h *Hub, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for h.Clients() != n {
		if time.Now().After(deadline) {
			t.Fatalf("Clients() = %d after %v, want %d", h.Clients(), within, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func mustPublish(t *testing.T, h *Hub, data []byte) {
	t.Helper()
	if err := h.Publish(data); err != nil {
		t.Fatalf("Publish(%.40q): %v", data, err)
	}
}

// stuckClient opens a stream with a small receive buffer and never reads.
func stuckClient(t *testing.T, addr string) {
	t.Helper()
	conn := dialHub(t, addr)
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetReadBuffer(4 << 10)
	}
	request(t, conn, http.MethodGet, addr)
}

func TestHubSendsTheLatestStateOnConnect(t *testing.T) {
	h := NewHub(8, time.Minute, time.Second)
	addr := startHub(t, h)
	mustPublish(t, h, []byte(`{"seq":"1"}`))
	for _, client := range []string{"first", "second"} {
		s := openStream(t, addr)
		ev, err := s.next(5 * time.Second)
		if err != nil {
			t.Fatalf("%s client: %v", client, err)
		}
		if want := (sseEvent{name: "state", data: `{"seq":"1"}`}); ev != want {
			t.Errorf("%s client: first event %+v, want %+v", client, ev, want)
		}
	}
}

func TestHubConvergesOnTheNewestState(t *testing.T) {
	h := NewHub(8, time.Minute, time.Second)
	addr := startHub(t, h)
	s := openStream(t, addr)
	waitClients(t, h, 1, 5*time.Second)
	for i := 1; i <= 200; i++ {
		mustPublish(t, h, state(i, 0))
	}
	last := 0
	for last != 200 {
		seq := s.nextState(t)
		if seq <= last {
			t.Fatalf("seq %d after %d: states went backwards or repeated", seq, last)
		}
		last = seq
	}
}

func TestAStuckClientNeverBlocksPublish(t *testing.T) {
	h := NewHub(8, time.Minute, 10*time.Second)
	addr := startHub(t, h)
	stuckClient(t, addr)
	waitClients(t, h, 1, 5*time.Second)
	s := openStream(t, addr)
	waitClients(t, h, 2, 5*time.Second)

	published := make(chan error, 1)
	go func() {
		for i := 1; i <= 10000; i++ {
			if err := h.Publish(state(i, 1<<10)); err != nil {
				published <- err
				return
			}
		}
		published <- nil
	}()
	select {
	case err := <-published:
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("10000 Publish calls did not return within 1s with a stuck client")
	}
	for last := 0; last != 10000; {
		last = s.nextState(t)
	}
}

func TestAStuckClientIsDropped(t *testing.T) {
	h := NewHub(8, time.Minute, 100*time.Millisecond)
	addr := startHub(t, h)
	stuckClient(t, addr)
	waitClients(t, h, 1, 5*time.Second)

	start := time.Now()
	for i := 1; i <= 256; i++ {
		mustPublish(t, h, state(i, 64<<10))
		time.Sleep(time.Millisecond)
	}
	waitClients(t, h, 0, 5*time.Second-time.Since(start))
}

func TestHubLimitsClients(t *testing.T) {
	h := NewHub(2, time.Minute, time.Second)
	addr := startHub(t, h)
	a := openStream(t, addr)
	openStream(t, addr)
	waitClients(t, h, 2, 5*time.Second)

	resp, _ := connect(t, addr)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("third client: status %d, want 503", resp.StatusCode)
	}
	if got := h.Clients(); got != 2 {
		t.Errorf("Clients() = %d after a refused connect, want 2", got)
	}

	_ = a.conn.Close()
	waitClients(t, h, 1, 5*time.Second)
	openStream(t, addr)
	waitClients(t, h, 2, 5*time.Second)
}

func TestHubSendsKeepAlives(t *testing.T) {
	h := NewHub(8, 50*time.Millisecond, time.Second)
	addr := startHub(t, h)
	s := openStream(t, addr)
	ev, err := s.next(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("no keep-alive within 500ms: %v", err)
	}
	if want := (sseEvent{comment: "keep-alive"}); ev != want {
		t.Errorf("got %+v, want %+v", ev, want)
	}
}

func TestHubCloseEndsStreamsAndRefusesNew(t *testing.T) {
	h := NewHub(8, time.Minute, time.Second)
	addr := startHub(t, h)
	mustPublish(t, h, state(1, 0))
	streams := []*sseStream{openStream(t, addr), openStream(t, addr)}
	for _, s := range streams {
		if seq := s.nextState(t); seq != 1 {
			t.Fatalf("first state seq %d, want 1", seq)
		}
	}
	waitClients(t, h, 2, 5*time.Second)

	h.Close()
	h.Close()
	for i, s := range streams {
		ev, err := s.next(time.Second)
		if !errors.Is(err, io.EOF) {
			t.Errorf("stream %d after Close: %+v, %v; want EOF", i, ev, err)
		}
	}
	if got := h.Clients(); got != 0 {
		t.Errorf("Clients() = %d after Close, want 0", got)
	}
	resp, _ := connect(t, addr)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("connect after Close: status %d, want 503", resp.StatusCode)
	}
}

func TestPublishRefusesMultilineData(t *testing.T) {
	h := NewHub(8, time.Minute, time.Second)
	addr := startHub(t, h)
	mustPublish(t, h, state(1, 0))
	s := openStream(t, addr)
	if seq := s.nextState(t); seq != 1 {
		t.Fatalf("first state seq %d, want 1", seq)
	}

	for _, data := range []string{"a\nb", "a\rb", "\n", `{"seq":"9"}` + "\r\n"} {
		if err := h.Publish([]byte(data)); !errors.Is(err, ErrMultiline) {
			t.Errorf("Publish(%q) = %v, want ErrMultiline", data, err)
		}
	}
	// Nothing was sent or stored: the next event either client sees is the
	// next valid state, and a new client still starts from the old one.
	late := openStream(t, addr)
	if seq := late.nextState(t); seq != 1 {
		t.Errorf("new client's first state seq %d, want 1", seq)
	}
	mustPublish(t, h, state(2, 0))
	for _, c := range []*sseStream{s, late} {
		if seq := c.nextState(t); seq != 2 {
			t.Errorf("next state seq %d, want 2", seq)
		}
	}
}

func TestPublishRefusesEmptyData(t *testing.T) {
	h := NewHub(8, time.Minute, time.Second)
	addr := startHub(t, h)
	mustPublish(t, h, state(1, 0))
	s := openStream(t, addr)
	if seq := s.nextState(t); seq != 1 {
		t.Fatalf("first state seq %d, want 1", seq)
	}

	// EventSource drops an event whose data is empty, so the page would stay
	// on the old state without a sign.
	for _, data := range [][]byte{nil, {}} {
		if err := h.Publish(data); !errors.Is(err, ErrEmpty) {
			t.Errorf("Publish(%#v) = %v, want ErrEmpty", data, err)
		}
	}
	late := openStream(t, addr)
	if seq := late.nextState(t); seq != 1 {
		t.Errorf("new client's first state seq %d, want 1", seq)
	}
	mustPublish(t, h, state(2, 0))
	for _, c := range []*sseStream{s, late} {
		if seq := c.nextState(t); seq != 2 {
			t.Errorf("next state seq %d, want 2", seq)
		}
	}
}

func TestAReconnectGetsTheCurrentState(t *testing.T) {
	h := NewHub(8, time.Minute, time.Second)
	addr := startHub(t, h)
	s := openStream(t, addr)
	waitClients(t, h, 1, 5*time.Second)
	for i := 1; i <= 3; i++ {
		mustPublish(t, h, state(i, 0))
	}
	for last := 0; last != 3; {
		last = s.nextState(t)
	}
	_ = s.conn.Close()
	waitClients(t, h, 0, 5*time.Second)

	mustPublish(t, h, state(4, 0))
	if seq := openStream(t, addr).nextState(t); seq != 4 {
		t.Errorf("reconnect's first state seq %d, want 4", seq)
	}
}

// NewServer bounds every response by WriteTimeout; a stream that keeps
// writing must outlive it. The server here gets a short WriteTimeout so the
// test needs well under a second, not the production 30 s. The stream also
// outlives several of the hub's own writeTimeout, so a deadline set only once
// at connect would end it too.
func TestAHealthyStreamOutlivesTheServerWriteTimeout(t *testing.T) {
	const serverWriteTimeout = 100 * time.Millisecond
	h := NewHub(8, 20*time.Millisecond, 200*time.Millisecond)
	addr := startHub(t, h, func(s *http.Server) { s.WriteTimeout = serverWriteTimeout })
	s := openStream(t, addr)

	start := time.Now()
	keepAlives := 0
	for time.Since(start) < 8*serverWriteTimeout {
		ev, err := s.next(time.Second)
		if err != nil {
			t.Fatalf("stream ended after %v (%d keep-alives): %v", time.Since(start), keepAlives, err)
		}
		if ev.comment == "keep-alive" {
			keepAlives++
		}
	}
	if keepAlives < 10 {
		t.Errorf("%d keep-alives in %v, want at least 10", keepAlives, time.Since(start))
	}
	mustPublish(t, h, state(1, 0))
	if seq := s.nextState(t); seq != 1 {
		t.Errorf("state seq %d, want 1", seq)
	}
}

// In production keepAlive (15 s) is longer than the hub's writeTimeout
// (10 s), so the deadline from the previous write has always passed when the
// next keep-alive goes out, and the server's WriteTimeout has passed too.
// Each write must set a fresh deadline before it starts.
func TestAStreamSurvivesKeepAlivesLongerThanItsWriteTimeout(t *testing.T) {
	const keepAlive, writeTimeout = 300 * time.Millisecond, 100 * time.Millisecond
	h := NewHub(8, keepAlive, writeTimeout)
	addr := startHub(t, h, func(s *http.Server) { s.WriteTimeout = 150 * time.Millisecond })
	s := openStream(t, addr)
	for i := range 3 {
		ev, err := s.next(time.Second)
		if err != nil {
			t.Fatalf("keep-alive %d: %v", i+1, err)
		}
		if want := (sseEvent{comment: "keep-alive"}); ev != want {
			t.Fatalf("event %d: %+v, want %+v", i+1, ev, want)
		}
	}
	time.Sleep(250 * time.Millisecond)
	mustPublish(t, h, state(1, 0))
	if seq := s.nextState(t); seq != 1 {
		t.Errorf("state seq %d, want 1", seq)
	}
}

func head(t *testing.T, addr string) *http.Response {
	t.Helper()
	conn := dialHub(t, addr)
	request(t, conn, http.MethodHead, addr)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodHead})
	if err != nil {
		t.Fatalf("read HEAD response: %v", err)
	}
	return resp
}

func TestHubAnswersHeadWithoutAStream(t *testing.T) {
	h := NewHub(1, time.Minute, time.Second)
	addr := startHub(t, h)
	resp := head(t, addr)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("HEAD: status %d, Content-Type %q; want 200 text/event-stream",
			resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if got := h.Clients(); got != 0 {
		t.Errorf("Clients() = %d after HEAD, want 0", got)
	}

	openStream(t, addr)
	waitClients(t, h, 1, 5*time.Second)
	if got := head(t, addr).StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("HEAD with the hub full: status %d, want 503", got)
	}
	h.Close()
	if got := head(t, addr).StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("HEAD after Close: status %d, want 503", got)
	}
}

func TestNewHubRefusesNonPositiveLimits(t *testing.T) {
	for _, tc := range []struct {
		max                     int
		keepAlive, writeTimeout time.Duration
	}{
		{0, time.Second, time.Second},
		{1, 0, time.Second},
		{1, time.Second, -time.Second},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewHub(%d, %v, %v) did not panic", tc.max, tc.keepAlive, tc.writeTimeout)
				}
			}()
			NewHub(tc.max, tc.keepAlive, tc.writeTimeout)
		}()
	}
}

// TestHubUnderChurn runs publishers, clients that join, read and leave, and
// a racing double Close, all at once. Clients() stays within the limit, each
// publisher's states arrive in order on every connection, no slot leaks, and
// after Close every connect is refused.
func TestHubUnderChurn(t *testing.T) {
	const maxClients, publishers, clients = 4, 3, 8
	h := NewHub(maxClients, 10*time.Millisecond, time.Second)
	addr := startHub(t, h)
	var closing atomic.Bool

	churn := func(d, closeAt time.Duration) {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for p := range publishers {
			wg.Go(func() {
				for seq := 1; ; seq++ {
					select {
					case <-stop:
						return
					default:
					}
					if err := h.Publish(fmt.Appendf(nil, `{"p":"%d","seq":%d}`, p, seq)); err != nil {
						t.Errorf("Publish: %v", err)
						return
					}
					time.Sleep(50 * time.Microsecond)
				}
			})
		}
		for range clients {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					churnOnce(t, addr, &closing)
				}
			})
		}
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if n := h.Clients(); n < 0 || n > maxClients {
					t.Errorf("Clients() = %d, want 0..%d", n, maxClients)
				}
				time.Sleep(100 * time.Microsecond)
			}
		})
		if closeAt > 0 {
			wg.Go(func() {
				time.Sleep(closeAt)
				closing.Store(true)
				var cw sync.WaitGroup
				cw.Go(h.Close)
				cw.Go(h.Close)
				cw.Wait()
			})
		}
		time.Sleep(d)
		close(stop)
		wg.Wait()
	}

	churn(700*time.Millisecond, 0)
	waitClients(t, h, 0, 2*time.Second)
	openStream(t, addr)
	waitClients(t, h, 1, 2*time.Second)

	churn(800*time.Millisecond, 100*time.Millisecond+rand.N(500*time.Millisecond))
	if got := h.Clients(); got != 0 {
		t.Errorf("Clients() = %d after Close, want 0", got)
	}
	if resp, _ := connect(t, addr); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("connect after Close: status %d, want 503", resp.StatusCode)
	}
}

// churnOnce connects, reads a few events and leaves. It runs on its own
// goroutine, so it reports with t.Errorf and returns.
func churnOnce(t *testing.T, addr string, closing *atomic.Bool) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Errorf("dial: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "GET /events HTTP/1.1\r\nHost: %s\r\n\r\n", addr); err != nil {
		t.Errorf("write request: %v", err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Errorf("read response: %v", err)
		return
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		time.Sleep(time.Millisecond)
		return
	default:
		t.Errorf("status %d, want 200 or 503", resp.StatusCode)
		return
	}
	s := &sseStream{conn: conn, body: bufio.NewReader(resp.Body)}
	last := map[string]int{}
	for range rand.N(8) + 1 {
		ev, err := s.next(5 * time.Second)
		if err != nil {
			if !errors.Is(err, io.EOF) || !closing.Load() {
				t.Errorf("read event: %v", err)
			}
			return
		}
		if ev.comment == "keep-alive" {
			continue
		}
		var v struct {
			P   string `json:"p"`
			Seq int    `json:"seq"`
		}
		if err := json.Unmarshal([]byte(ev.data), &v); err != nil || ev.name != "state" {
			t.Errorf("event %+v: %v", ev, err)
			return
		}
		if v.Seq <= last[v.P] {
			t.Errorf("publisher %s: seq %d after %d on one connection", v.P, v.Seq, last[v.P])
			return
		}
		last[v.P] = v.Seq
	}
}
