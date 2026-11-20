package livehttp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	// maxInboxBody bounds one page report: room for an ask's
	// 280-character question in any script.
	maxInboxBody = 2048
	// inboxBodyTimeout bounds the wait for a report's body.
	inboxBodyTimeout = 2 * time.Second
)

// Rate is a token bucket: PerSecond tokens are added each second, up to
// Burst.
type Rate struct{ PerSecond, Burst int }

// InboxGuard admits a POST to /s/<page>/inbox only from the live page
// itself. It is never mounted without Front, which owns the Host check (DNS
// rebinding) and the security headers; InboxGuard does neither. In order:
//   - a wrong or missing page token, compared in constant time, gets 404;
//   - a method other than POST gets 405;
//   - Origin must appear once, be non-empty and equal http://<Host>, and
//     Sec-Fetch-Site must appear once and be same-origin, else 403;
//   - Content-Type must be application/json, else 400. It is not a simple
//     type, so another origin would need a CORS preflight, which never
//     succeeds here;
//   - X-Perf-Agent-Page must be 1, else 403;
//   - the body needs a Content-Length, else 411 (chunked included), of at
//     most 2048 bytes, else 413;
//   - the bucket, one per guard, must hold a token, else 429 with
//     Retry-After: 1. Refused requests take none.
//
// Every refusal is answered from the headers and closes the connection. Then
// the whole body is read under a 2 s read deadline (408 when it does not
// arrive) and next gets it in memory. An empty page token admits nothing.
// Both rates must be positive.
func InboxGuard(page Token, rate Rate, next http.Handler) http.Handler {
	return newInboxGuard(page, rate, time.Now, next)
}

func newInboxGuard(page Token, rate Rate, now func() time.Time, next http.Handler) http.Handler {
	if rate.PerSecond <= 0 || rate.Burst <= 0 {
		panic(fmt.Sprintf("livehttp.InboxGuard: rate %+v must be positive", rate))
	}
	b := &bucket{rate: rate, now: now, tokens: int64(rate.Burst) * tokenUnit, last: now()}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seg, after, ok := pageSegment(r.URL.Path)
		if !ok || after != "inbox" || !samePageToken(seg, page) {
			refuse(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			refuse(w, http.StatusMethodNotAllowed, "only POST is served")
			return
		}
		if origin, ok := single(r.Header, "Origin"); !ok || origin == "" || origin != "http://"+r.Host {
			refuse(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if site, ok := single(r.Header, "Sec-Fetch-Site"); !ok || site != "same-origin" {
			refuse(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if !isJSON(r.Header) {
			refuse(w, http.StatusBadRequest, "Content-Type must be application/json")
			return
		}
		if mark, ok := single(r.Header, "X-Perf-Agent-Page"); !ok || mark != "1" {
			refuse(w, http.StatusForbidden, "not sent by the page")
			return
		}
		if code, msg := lengthProblem(r, maxInboxBody); code != 0 {
			refuse(w, code, msg)
			return
		}
		if !b.take() {
			w.Header().Set("Retry-After", "1")
			refuse(w, http.StatusTooManyRequests, "too many reports")
			return
		}

		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Now().Add(inboxBodyTimeout))
		body := make([]byte, r.ContentLength)
		if _, err := io.ReadFull(r.Body, body); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, os.ErrDeadlineExceeded) {
				code = http.StatusRequestTimeout
			}
			refuse(w, code, "request body not received")
			return
		}
		_ = rc.SetReadDeadline(time.Time{})
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r)
	})
}

// tokenUnit is one token in the bucket's integer count: a token is earned
// every second per unit of PerSecond, so one nanosecond earns PerSecond.
const tokenUnit = int64(time.Second)

// bucket is a token bucket refilled from the clock on each take; it runs no
// goroutine. tokens counts in tokenUnit, so refills do not round.
type bucket struct {
	rate Rate
	now  func() time.Time

	mu     sync.Mutex
	tokens int64
	last   time.Time
}

func (b *bucket) take() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	full := int64(b.rate.Burst) * tokenUnit
	if elapsed := int64(t.Sub(b.last)); elapsed > 0 {
		// Compared before multiplying, so a long idle cannot overflow.
		if elapsed >= full/int64(b.rate.PerSecond) {
			b.tokens = full
		} else {
			b.tokens = min(full, b.tokens+elapsed*int64(b.rate.PerSecond))
		}
	}
	b.last = t
	if b.tokens < tokenUnit {
		return false
	}
	b.tokens -= tokenUnit
	return true
}
