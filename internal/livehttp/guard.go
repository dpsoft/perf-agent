package livehttp

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// CSP is the Content-Security-Policy of every live page response: scripts,
// styles, fetches and images from the server itself only, no inline code,
// no forms and no framing.
const CSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// ExportCSP replaces CSP on export downloads. The export is a standalone page
// meant to be saved; should a browser render it here instead, it runs no
// script and gets an opaque origin, so it can read nothing from the server.
const ExportCSP = "sandbox; default-src 'none'"

var securityHeaders = [...][2]string{
	{"Content-Security-Policy", CSP},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "no-referrer"},
	{"Cache-Control", "no-store"},
	{"Cross-Origin-Opener-Policy", "same-origin"},
	{"Cross-Origin-Resource-Policy", "same-origin"},
	{"X-Frame-Options", "DENY"},
}

// Guard admits a request to next only if it has no body, is a read, names this machine
// in Host, comes from the page itself (or is a top-level navigation to the
// shell) and carries tok as its first path segment. next sees the path with
// the /s/<tok> prefix removed. Every response, refusals included, carries
// the security headers; a handler may replace Content-Security-Policy.
func Guard(tok Token, next http.Handler) http.Handler {
	prefix := "/s/" + string(tok)
	inner := http.StripPrefix(prefix, next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		for _, kv := range securityHeaders {
			h.Set(kv[0], kv[1])
		}

		// A request that announces a body would park this goroutine while
		// net/http drains it after the handler, with no deadline, because the
		// server sets no ReadTimeout (it would end streams). No route takes a
		// body, so refuse it, stop reading soon and drop the connection.
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			h.Set("Connection", "close")
			http.Error(w, "request bodies are not accepted", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			h.Set("Allow", "GET, HEAD")
			http.Error(w, "only GET and HEAD are served", http.StatusMethodNotAllowed)
			return
		}
		if !loopbackHost(r.Host) {
			http.Error(w, "bad Host", http.StatusForbidden)
			return
		}
		if !isShellNavigation(r) {
			if origin, ok := single(r.Header, "Origin"); ok && origin != "http://"+r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
			if site, ok := single(r.Header, "Sec-Fetch-Site"); ok && site != "same-origin" && site != "none" {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}

		rest, ok := strings.CutPrefix(r.URL.Path, "/s/")
		seg, _, slash := strings.Cut(rest, "/")
		if !ok || seg == "" || subtle.ConstantTimeCompare([]byte(seg), []byte(tok)) != 1 {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if !slash {
			h.Set("Location", prefix+"/")
			w.WriteHeader(http.StatusMovedPermanently)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// isShellNavigation reports a top-level document navigation to the shell
// route, /s/<segment>/ or /s/<segment>. Such a request skips the Origin and
// Sec-Fetch-Site checks, so a link from another site can open the page; the
// shell holds no data and its own requests are all same-origin. The token
// itself is checked afterwards for every request, so a navigation with a
// wrong token still gets the same 404 as any other.
func isShellNavigation(r *http.Request) bool {
	if mode, _ := single(r.Header, "Sec-Fetch-Mode"); mode != "navigate" {
		return false
	}
	if dest, _ := single(r.Header, "Sec-Fetch-Dest"); dest != "document" {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/s/")
	if !ok {
		return false
	}
	seg, after, _ := strings.Cut(rest, "/")
	return seg != "" && after == ""
}

// loopbackHost accepts host:port where host is localhost, an IPv4 loopback
// literal or ::1, and port is any decimal port. A Host without a port is
// refused: the browser always sends the random port.
func loopbackHost(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" {
		return false
	}
	return (ip.Is4() && ip.IsLoopback()) || ip == netip.IPv6Loopback()
}

// single returns the header's value when it is present. A header sent more
// than once is treated as present with a value no check accepts.
func single(h http.Header, key string) (string, bool) {
	switch vals := h.Values(key); len(vals) {
	case 0:
		return "", false
	case 1:
		return vals[0], true
	default:
		return "\x00", true
	}
}
