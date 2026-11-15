package livehttp

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// CheckAddr accepts only a loopback IP literal with a port, such as
// 127.0.0.1:0 or [::1]:0. Hostnames are refused even when they name this
// machine, because resolving one is a lookup and its answer can change.
// The result is unmapped, so ::ffff:127.0.0.1 binds as 127.0.0.1.
func CheckAddr(addr string) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil && host != "" {
			if _, ipErr := netip.ParseAddr(host); ipErr != nil {
				return netip.AddrPort{}, fmt.Errorf("live address %q must be a loopback IP literal such as 127.0.0.1:0 or [::1]:0, not a hostname", addr)
			}
		}
		return netip.AddrPort{}, notLoopback(addr)
	}
	ip := ap.Addr().Unmap().WithZone("")
	if !ip.IsLoopback() {
		return netip.AddrPort{}, notLoopback(addr)
	}
	return netip.AddrPortFrom(ip, ap.Port()), nil
}

func notLoopback(addr string) error {
	return fmt.Errorf("live address %q is not loopback; perf-agent-mcp serves the live page to this machine only", addr)
}

// Listen binds addr after CheckAddr accepts it. It is the module's only
// listening call.
func Listen(addr string) (net.Listener, error) {
	ap, err := CheckAddr(addr)
	if err != nil {
		return nil, err
	}
	return net.Listen("tcp", ap.String())
}

// Token is the per-run secret that forms the first path segment of every
// live page URL. It must never be logged.
type Token string

// NewToken returns 32 bytes from crypto/rand as unpadded base64url: 43
// characters, all safe in a URL path segment.
func NewToken() (Token, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("live page token: %w", err)
	}
	return Token(base64.RawURLEncoding.EncodeToString(b[:])), nil
}

// NewServer returns the http.Server for the live page. Header reads,
// idle connections and header size are bounded, and so is every response
// write: a tab that stops reading a large model holds its goroutine and
// snapshot for at most WriteTimeout. Streaming handlers extend their own
// deadline per write through http.ResponseController.
//
// net/http's own error lines go to log at WARN through a writer that never
// blocks: when the previous line is still being written, for example because
// stderr is not drained, the new one is dropped. So no browser request can
// wait on the logger that tool calls also use.
func NewServer(h http.Handler, log *slog.Logger) *http.Server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		ErrorLog:          newDroppingLogger(slog.NewLogLogger(log.Handler(), slog.LevelWarn).Writer()),
	}
}

// droppingWriter hands each line to one background goroutine at a time and
// drops lines that arrive while one is in flight.
type droppingWriter struct {
	out  io.Writer
	busy chan struct{}
}

func newDroppingLogger(out io.Writer) *log.Logger {
	return log.New(&droppingWriter{out: out, busy: make(chan struct{}, 1)}, "", 0)
}

func (w *droppingWriter) Write(p []byte) (int, error) {
	select {
	case w.busy <- struct{}{}:
	default:
		return len(p), nil
	}
	// log.Logger reuses p once Write returns.
	line := bytes.Clone(p)
	go func() {
		defer func() { <-w.busy }()
		_, _ = w.out.Write(line)
	}()
	return len(p), nil
}
