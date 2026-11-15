package livehttp

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCheckAddrAcceptsLoopbackLiterals(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "127.0.0.2:7777", "[::1]:0", "[::ffff:127.0.0.1]:0"} {
		ap, err := CheckAddr(addr)
		if err != nil {
			t.Errorf("CheckAddr(%q) = %v, want nil", addr, err)
			continue
		}
		if !ap.Addr().IsLoopback() || ap.Addr().Is4In6() {
			t.Errorf("CheckAddr(%q) = %v, want an unmapped loopback address", addr, ap)
		}
	}
}

func TestCheckAddrRefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		addr, want string
	}{
		{"0.0.0.0:0", "not loopback"},
		{"[::]:0", "not loopback"},
		{"192.168.1.10:0", "not loopback"},
		{"10.0.0.1:80", "not loopback"},
		{":0", "not loopback"},
		{"", "not loopback"},
		{"localhost:0", "hostname"},
		{"example.com:80", "hostname"},
	} {
		ap, err := CheckAddr(tc.addr)
		if err == nil {
			t.Errorf("CheckAddr(%q) = %v, nil; want an error", tc.addr, ap)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("CheckAddr(%q) error %q does not say %q", tc.addr, err, tc.want)
		}
		if !strings.Contains(err.Error(), "\""+tc.addr+"\"") {
			t.Errorf("CheckAddr(%q) error %q does not quote the address", tc.addr, err)
		}
	}
}

func TestListenBindsOnlyTheCheckedAddress(t *testing.T) {
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ap, err := netip.ParseAddrPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("listener address %q: %v", ln.Addr(), err)
	}
	if ap.Addr() != netip.MustParseAddr("127.0.0.1") || ap.Port() == 0 {
		t.Errorf("listener bound %v, want 127.0.0.1:<port>", ap)
	}

	bad, err := Listen("0.0.0.0:0")
	if err == nil {
		_ = bad.Close()
		t.Fatalf("Listen(0.0.0.0:0) bound %v, want an error", bad.Addr())
	}
	if bad != nil {
		t.Errorf("Listen(0.0.0.0:0) returned a listener with its error")
	}
}

func TestTokensAreLongURLSafeAndDistinct(t *testing.T) {
	shape := regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	seen := make(map[Token]bool, 1000)
	for range 1000 {
		tok, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if len(tok) != 43 || !shape.MatchString(string(tok)) {
			t.Fatalf("token %q is not 43 base64url characters", tok)
		}
		if seen[tok] {
			t.Fatalf("token %q repeated", tok)
		}
		seen[tok] = true
	}
}

func TestServerBoundsRequestHeaders(t *testing.T) {
	g := startGuarded(t)
	req := g.request(t, http.MethodGet, "/s/"+string(g.tok)+"/")
	req.Header.Set("X-Filler", strings.Repeat("a", 16<<10))
	if got := g.status(t, req); got != http.StatusRequestHeaderFieldsTooLarge {
		t.Errorf("16 KiB header: status %d, want 431", got)
	}
	if n := g.calls.Load(); n != 0 {
		t.Errorf("next called %d times for an oversized request", n)
	}
}

func TestServerSetsItsTimeoutsAndBounds(t *testing.T) {
	srv := NewServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	for _, tc := range []struct {
		name      string
		got, want time.Duration
	}{
		{"ReadHeaderTimeout", srv.ReadHeaderTimeout, 5 * time.Second},
		{"IdleTimeout", srv.IdleTimeout, 60 * time.Second},
		{"WriteTimeout", srv.WriteTimeout, 30 * time.Second},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	if srv.MaxHeaderBytes != 8<<10 {
		t.Errorf("MaxHeaderBytes = %d, want %d", srv.MaxHeaderBytes, 8<<10)
	}
	if srv.ErrorLog == nil {
		t.Errorf("ErrorLog is nil; net/http would write to the standard logger")
	}
}

// stuckHandler blocks every Handle call until release is closed, as a
// TextHandler does while stderr is not drained.
type stuckHandler struct {
	release chan struct{}
	mu      sync.Mutex
	msgs    []string
	levels  []slog.Level
}

func (h *stuckHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *stuckHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *stuckHandler) WithGroup(string) slog.Handler            { return h }

func (h *stuckHandler) Handle(_ context.Context, r slog.Record) error {
	<-h.release
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	h.levels = append(h.levels, r.Level)
	return nil
}

func TestErrorLogNeverBlocksOnAStuckLogger(t *testing.T) {
	h := &stuckHandler{release: make(chan struct{})}
	srv := NewServer(http.NotFoundHandler(), slog.New(h))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			srv.ErrorLog.Printf("http: superfluous response.WriteHeader call")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(h.release)
		t.Fatal("ErrorLog blocked behind a stuck slog handler")
	}

	close(h.release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.msgs)
		h.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.msgs) == 0 {
		t.Fatal("no ErrorLog line reached the handler once it was released")
	}
	if len(h.msgs) > 2 {
		t.Errorf("%d lines reached the handler, want at most the one in flight and the one queued", len(h.msgs))
	}
	if h.msgs[0] != "http: superfluous response.WriteHeader call" || h.levels[0] != slog.LevelWarn {
		t.Errorf("first line %q at %v, want the message at WARN without its newline", h.msgs[0], h.levels[0])
	}
}

func TestErrorLogCopiesTheLine(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	h := slog.NewTextHandler(lockedWriter{&mu, &buf}, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	})
	srv := NewServer(http.NotFoundHandler(), slog.New(h))
	srv.ErrorLog.Print("first line")

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if out != "" {
			if want := "level=WARN msg=\"first line\"\n"; out != want {
				t.Errorf("logged %q, want %q", out, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("ErrorLog line never reached the handler")
		}
		time.Sleep(time.Millisecond)
	}
}

type lockedWriter struct {
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
