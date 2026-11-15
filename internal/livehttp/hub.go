package livehttp

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ErrMultiline is returned by Publish for data holding a line break, which
// would end the event's data line early.
var ErrMultiline = errors.New("live state must be a single line")

// ErrEmpty is returned by Publish for empty data. Every state the hub
// sends is a non-empty JSON document the page parses; an empty one is not a
// state, and a page given one could not tell what to show.
var ErrEmpty = errors.New("live state must not be empty")

// Hub streams the newest published state to every connected page as
// Server-Sent Events.
//
// Publishing never waits on a socket. Each client has a one-slot mailbox,
// and a new state replaces one that was not sent yet, so a slow client skips
// to the newest state and a stuck one costs one frame of memory. A client
// whose write does not finish within writeTimeout is dropped.
type Hub struct {
	maxClients   int
	keepAlive    time.Duration
	writeTimeout time.Duration

	done      chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	latest  []byte // the newest state, framed as an event; nil before the first Publish
	clients map[chan []byte]struct{}
	closed  bool
}

// NewHub returns a hub that admits at most maxClients streams at once,
// sends a keep-alive comment after keepAlive without an event, and allows
// each write writeTimeout to finish. All three must be positive.
func NewHub(maxClients int, keepAlive, writeTimeout time.Duration) *Hub {
	if maxClients <= 0 || keepAlive <= 0 || writeTimeout <= 0 {
		panic(fmt.Sprintf("livehttp.NewHub(%d, %v, %v): every limit must be positive", maxClients, keepAlive, writeTimeout))
	}
	return &Hub{
		maxClients:   maxClients,
		keepAlive:    keepAlive,
		writeTimeout: writeTimeout,
		done:         make(chan struct{}),
		clients:      make(map[chan []byte]struct{}),
	}
}

var keepAliveFrame = []byte(": keep-alive\n\n")

// Publish makes data the latest state, which every connected client is sent
// next and every new client is sent first. data must be one non-empty line;
// it is copied, so the caller may reuse it. Publish never blocks.
func (h *Hub) Publish(data []byte) error {
	if len(data) == 0 {
		return ErrEmpty
	}
	if bytes.ContainsAny(data, "\r\n") {
		return ErrMultiline
	}
	frame := make([]byte, 0, len("event: state\ndata: \n\n")+len(data))
	frame = append(frame, "event: state\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = frame
	for box := range h.clients {
		deliver(box, frame)
	}
	return nil
}

// deliver puts frame in a one-slot mailbox, replacing an unsent frame. The
// caller holds h.mu, so no other sender races for the slot and the send
// cannot block.
func deliver(box chan []byte, frame []byte) {
	select {
	case <-box:
	default:
	}
	box <- frame
}

// ServeHTTP streams events until the request ends, a write fails or the hub
// closes. Beyond maxClients, and after Close, it answers 503. A HEAD request
// gets the stream's headers and no stream.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	box, ok := h.join(r.Method != http.MethodHead)
	if !ok {
		http.Error(w, "live page stream unavailable", http.StatusServiceUnavailable)
		return
	}
	if box != nil {
		defer h.leave(box)
	}

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	if box == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	// The server's WriteTimeout would end the stream once it passes, so every
	// write moves the deadline to writeTimeout from now instead.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Now().Add(h.writeTimeout)); err != nil {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if rc.Flush() != nil {
		return
	}

	quiet := time.NewTimer(h.keepAlive)
	defer quiet.Stop()
	for {
		var frame []byte
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case frame = <-box:
		case <-quiet.C:
			frame = keepAliveFrame
		}
		if !h.write(w, rc, frame) {
			return
		}
		quiet.Reset(h.keepAlive)
	}
}

func (h *Hub) write(w http.ResponseWriter, rc *http.ResponseController, frame []byte) bool {
	if rc.SetWriteDeadline(time.Now().Add(h.writeTimeout)) != nil {
		return false
	}
	if _, err := w.Write(frame); err != nil {
		return false
	}
	return rc.Flush() == nil
}

// join admits a client. With stream false it only checks that one would be
// admitted and returns a nil mailbox. A new mailbox already holds the latest
// state, so it is sent first.
func (h *Hub) join(stream bool) (chan []byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || len(h.clients) >= h.maxClients {
		return nil, false
	}
	if !stream {
		return nil, true
	}
	box := make(chan []byte, 1)
	if h.latest != nil {
		box <- h.latest
	}
	h.clients[box] = struct{}{}
	return box, true
}

func (h *Hub) leave(box chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, box)
}

// Clients reports how many streams are connected.
func (h *Hub) Clients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// Close ends every stream and refuses new ones. A stream waiting for an
// event returns at once; one inside a write returns when the write finishes
// or its deadline passes. Close is idempotent.
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		clear(h.clients)
		h.mu.Unlock()
		close(h.done)
	})
}
