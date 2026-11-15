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
	"strings"
	"time"
)

// CheckAddr accepts only a loopback IP literal with a port, such as
// 127.0.0.1:0 or [::1]:0. Hostnames are refused even when they name this
// machine, because resolving one is a lookup and its answer can change.
// The result is unmapped, so ::ffff:127.0.0.1 binds as 127.0.0.1.
func CheckAddr(addr string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		ip := ap.Addr().Unmap().WithZone("")
		if !ip.IsLoopback() {
			return netip.AddrPort{}, notLoopback(addr)
		}
		return netip.AddrPortFrom(ip, ap.Port()), nil
	}
	if _, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")); err == nil {
		return netip.AddrPort{}, fmt.Errorf("live address %q: missing port; write a loopback IP literal with a port such as 127.0.0.1:0", addr)
	}
	host, _, err := net.SplitHostPort(addr)
	switch {
	case err != nil || host == "":
		return netip.AddrPort{}, notLoopback(addr)
	case isIP(host):
		return netip.AddrPort{}, fmt.Errorf("live address %q: port out of range; the port must be a decimal number from 0 to 65535", addr)
	case numericShorthand(host):
		return netip.AddrPort{}, fmt.Errorf("live address %q: numeric shorthand not accepted; write a full loopback IP literal such as 127.0.0.1:0", addr)
	}
	return netip.AddrPort{}, fmt.Errorf("live address %q must be a loopback IP literal such as 127.0.0.1:0 or [::1]:0, not a hostname", addr)
}

func isIP(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

// numericShorthand reports a host such as 127.1, 2130706433 or 0x7f.0.0.1:
// every label decimal digits or 0x-prefixed hex, which some resolvers read
// as an IPv4 address. Such a host is never a hostname.
func numericShorthand(host string) bool {
	for label := range strings.SplitSeq(host, ".") {
		digits, hex := "0123456789", false
		if rest, ok := strings.CutPrefix(strings.ToLower(label), "0x"); ok {
			label, digits, hex = rest, "0123456789abcdef", true
		}
		if (label == "" && !hex) || strings.Trim(label, digits) != "" {
			return false
		}
	}
	return true
}

func notLoopback(addr string) error {
	return fmt.Errorf("live address %q is not loopback; perf-agent-mcp serves the live page to this machine only", addr)
}

// Listen binds addr after CheckAddr accepts it. It is perf-agent-mcp's only
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

// NewServer returns the HTTP/1 server for the live page. Header reads,
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
	// HTTP/1 only. The page needs nothing else, and without this net/http
	// hands a request that opens with the HTTP/2 preface to the handler.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	return &http.Server{
		Handler:           h,
		Protocols:         protocols,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
		// OPTIONS * reaches Guard, which refuses it with its headers, instead
		// of net/http's built-in 200.
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     newDroppingLogger(slog.NewLogLogger(log.Handler(), slog.LevelWarn).Writer()),
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
