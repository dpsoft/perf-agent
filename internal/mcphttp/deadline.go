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
// of IngressBodyTimeout. Once the whole body is read it clears that
// deadline, so the wait for the handler that follows is not cut by it:
// net/http keeps reading the connection in the background, to see the
// client go, and a deadline left there would fail that read (net/http
// clears it as well when a body is read to its end). A read that failed
// keeps the deadline: after the handler returns, net/http drains the
// unread rest of the body before it reuses the connection, and a body that
// stalled would hold that drain, the connection and its goroutine for as
// long as the client waits. With the deadline the drain fails and the
// connection is closed. The caller holds a reader and the body's bytes
// (reserveIngress) while it reads.
func (t *Transport) readBody(w http.ResponseWriter, rc *http.ResponseController, r *http.Request) ([]byte, error) {
	_ = rc.SetReadDeadline(time.Now().Add(t.o.IngressBodyTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, t.o.MaxMessageBytes))
	if err != nil {
		return nil, err
	}
	_ = rc.SetReadDeadline(time.Time{})
	return body, nil
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
