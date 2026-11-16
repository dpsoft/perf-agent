package mcphttp

import (
	"io"
	"net/http"
	"strconv"
	"time"
)

// refuseReadWindow is how long the body of a request refused unread may
// keep arriving before the connection is dropped.
const refuseReadWindow = 100 * time.Millisecond

// readBody reads r's body, at most MaxMessageBytes, under a read deadline
// of IngressBodyTimeout, and clears that deadline once the body is read,
// so the wait for the handler that follows is not cut by it: net/http
// keeps reading the connection in the background, to see the client go,
// and a deadline left there would fail that read. net/http clears it as
// well when a body is read to its end; this clear covers every other
// way a read can stop.
// While it reads, the request counts as a reader holding its
// Content-Length.
func (t *Transport) readBody(w http.ResponseWriter, rc *http.ResponseController, r *http.Request) ([]byte, error) {
	reserved := max(r.ContentLength, 0)
	t.mu.Lock()
	t.readers++
	t.reserved += reserved
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.readers--
		t.reserved -= reserved
		t.mu.Unlock()
	}()
	_ = rc.SetReadDeadline(time.Now().Add(t.o.IngressBodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, t.o.MaxMessageBytes))
	_ = rc.SetReadDeadline(time.Time{})
	return body, err
}

// write answers status with body, which is JSON or empty, under a write
// deadline of WriteBound set just before it, so a client that stops
// reading holds its handler for at most that long. The deadline set on
// entry was cleared, so the wait for the handler is not bounded by the
// server's WriteTimeout.
func (t *Transport) write(w http.ResponseWriter, rc *http.ResponseController, status int, body []byte) {
	if len(body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	_ = rc.SetWriteDeadline(time.Now().Add(t.o.WriteBound))
	w.WriteHeader(status)
	if len(body) == 0 {
		return
	}
	if _, err := w.Write(body); err != nil {
		t.log.Debug("mcp answer not delivered", "status", status, "bytes", len(body), "err", err)
	}
}

// refuseUnread answers status with body without reading the request's body:
// the body gets refuseReadWindow to arrive and the connection is closed,
// so net/http's drain of an unread body never waits on a slow client.
func (t *Transport) refuseUnread(w http.ResponseWriter, rc *http.ResponseController, status int, body []byte) {
	_ = rc.SetReadDeadline(time.Now().Add(refuseReadWindow))
	w.Header().Set("Connection", "close")
	t.write(w, rc, status, body)
}
