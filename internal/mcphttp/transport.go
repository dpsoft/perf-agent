package mcphttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// The headers of the Streamable HTTP transport.
const (
	HeaderSession = "Mcp-Session-Id"
	HeaderVersion = "MCP-Protocol-Version"
)

// Handler parses and answers one message at a time. *mcpstdio.Server
// satisfies it.
type Handler interface {
	Parse([]byte) (*mcpstdio.Request, []byte)
	Handle(context.Context, *mcpstdio.Session, *mcpstdio.Request) []byte
	Tool(string) (mcpstdio.Tool, bool)
	Resource(string) (mcpstdio.Resource, bool)
	FlushWarnings()
}

// Defaults for the zero Options fields.
const (
	defaultMaxSessions        = 32
	defaultMaxMessageBytes    = 4 << 20
	defaultIngressBodyTimeout = 2 * time.Second
	defaultWriteBound         = 10 * time.Second
	defaultMaxAnswerBytes     = 4 << 20

	defaultMaxIngressReaders    = 16
	defaultMaxIngressBytes      = 8 << 20
	defaultMaxControlInFlight   = 32
	defaultMaxPageStateInFlight = 4
	defaultMaxCalls             = 16
	defaultMaxCallsPerSession   = 4
	defaultMaxRequestBytes      = 16 << 20
	defaultMaxAnswersWriting    = 64
)

// Options configures a Transport.
type Options struct {
	// Lifetime is the parent of every call's context; Close cancels the
	// contexts derived from it. nil means context.Background.
	Lifetime context.Context
	// MaxSessions bounds the sessions kept; 0 means 32.
	MaxSessions int
	// MaxMessageBytes bounds a request body; 0 means 4 MiB.
	MaxMessageBytes int64
	// IngressBodyTimeout bounds the read of a request body; 0 means 2 s.
	// WriteBound bounds the write of an answer; 0 means 10 s.
	IngressBodyTimeout, WriteBound time.Duration
	// MaxAnswerBytes bounds an answer body: a longer one is replaced by a
	// small answer for the same id, an isError result "result too large
	// for the output bound" for tools/call and an internal error
	// otherwise, as the stdio output queue replaces a long line. 0 means
	// 4 MiB.
	MaxAnswerBytes int64

	// The ingress bounds the bodies being read, before any byte of one is
	// read: MaxIngressReaders bodies at once (0 means 16), whose
	// Content-Lengths sum to at most MaxIngressBytes (0 means 8 MiB). One
	// reader and 64 KiB of them are kept for bodies of at most 64 KiB, so
	// large bodies that stall never keep a control message from being
	// read. A request beyond either gets 503 unread and its connection
	// closed.
	MaxIngressReaders int
	MaxIngressBytes   int64
	// The admission classes bound the parsed messages in flight, each
	// class apart, so a full one never delays another; see Class.
	// MaxControlInFlight bounds the control requests (0 means 32);
	// notifications and DELETE, answered under the lock, are not counted.
	// MaxPageStateInFlight bounds the page-state reads (0 means 4). MaxCalls
	// bounds the calls server-wide, running or waiting in their handler
	// (0 means 16), MaxCallsPerSession those of one session (0 means 4),
	// and MaxRequestBytes the arguments the calls hold, as parsed, never
	// the raw body (0 means 16 MiB). A message beyond a cap gets 503 at
	// once, with Retry-After and a JSON-RPC error -32000 for its id naming
	// the cap.
	MaxControlInFlight   int
	MaxPageStateInFlight int
	MaxCalls             int
	MaxCallsPerSession   int
	MaxRequestBytes      int64
	// MaxAnswersWriting bounds the handlers' answers being written at
	// once (0 means 64). An answer waits for a place, keeping its call's
	// place in its class, so answers not yet read are at most this many,
	// each at most MaxAnswerBytes, for at most WriteBound. Refusals and
	// the empty answers to notifications take none.
	MaxAnswersWriting int
	// Log receives diagnostics; nil discards them. It is called on the
	// request's goroutine, so it must not block.
	Log *slog.Logger
}

// Stats is a snapshot of what a Transport holds.
type Stats struct {
	// Sessions is how many sessions the table holds, and InFlight how many
	// calls are registered in them.
	Sessions, InFlight int
	// Readers is how many request bodies are being read, and
	// ReservedBytes the sum of their Content-Length.
	Readers       int
	ReservedBytes int64
	// Control, PageState and Calls are how many messages each admission
	// class holds, and RequestBytes the arguments the calls hold.
	Control, PageState, Calls int
	RequestBytes              int64
	// Writing is how many answers hold a place to be written.
	Writing int
}

// Transport is the MCP endpoint, an http.Handler for POST and DELETE.
// One mutex guards the session table and the calls in flight, and it is
// never held across a handler or a write.
type Transport struct {
	h   Handler
	o   Options
	log *slog.Logger
	// reserveReaders and reserveBytes are the part of the ingress bounds
	// kept for small bodies (ingressReserve).
	reserveReaders int
	reserveBytes   int64
	// life is the parent of every call's context; stop ends it.
	life  context.Context
	stop  context.CancelFunc
	flush func()
	// writing holds a token per handler answer being written; see
	// MaxAnswersWriting.
	writing chan struct{}

	mu       sync.Mutex
	closed   bool
	sessions map[string]*session
	tick     uint64
	inflight int
	// readers and reserved are the ingress: the bodies being read and the
	// sum of their Content-Lengths.
	readers  int
	reserved int64
	// control, pageState and calls count each admission class, and
	// requestBytes the arguments the calls hold.
	control, pageState, calls int
	requestBytes              int64
}

// New is a Transport answering through h.
func New(h Handler, o Options) *Transport {
	if o.Lifetime == nil {
		o.Lifetime = context.Background()
	}
	if o.MaxSessions <= 0 {
		o.MaxSessions = defaultMaxSessions
	}
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = defaultMaxMessageBytes
	}
	if o.IngressBodyTimeout <= 0 {
		o.IngressBodyTimeout = defaultIngressBodyTimeout
	}
	if o.WriteBound <= 0 {
		o.WriteBound = defaultWriteBound
	}
	if o.MaxAnswerBytes <= 0 {
		o.MaxAnswerBytes = defaultMaxAnswerBytes
	}
	if o.MaxIngressReaders <= 0 {
		o.MaxIngressReaders = defaultMaxIngressReaders
	}
	if o.MaxIngressBytes <= 0 {
		o.MaxIngressBytes = defaultMaxIngressBytes
	}
	if o.MaxControlInFlight <= 0 {
		o.MaxControlInFlight = defaultMaxControlInFlight
	}
	if o.MaxPageStateInFlight <= 0 {
		o.MaxPageStateInFlight = defaultMaxPageStateInFlight
	}
	if o.MaxCalls <= 0 {
		o.MaxCalls = defaultMaxCalls
	}
	if o.MaxCallsPerSession <= 0 {
		o.MaxCallsPerSession = defaultMaxCallsPerSession
	}
	if o.MaxRequestBytes <= 0 {
		o.MaxRequestBytes = defaultMaxRequestBytes
	}
	if o.MaxAnswersWriting <= 0 {
		o.MaxAnswersWriting = defaultMaxAnswersWriting
	}
	log := o.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	t := &Transport{h: h, o: o, log: log, sessions: map[string]*session{}, flush: sync.OnceFunc(h.FlushWarnings),
		writing: make(chan struct{}, o.MaxAnswersWriting)}
	t.reserveReaders, t.reserveBytes = ingressReserve(o)
	t.life, t.stop = context.WithCancel(o.Lifetime)
	return t
}

// Close cancels every call's context, so each call in flight answers at
// once, drops every session, and logs the warnings the handler only
// counted. Every later request gets 503. It returns without waiting for
// the calls; http.Server.Shutdown does that. A second Close does nothing.
func (t *Transport) Close() {
	t.mu.Lock()
	t.closed = true
	clear(t.sessions)
	t.mu.Unlock()
	t.stop()
	t.flush()
}

// Options are t's options, with the defaults New filled in.
func (t *Transport) Options() Options { return t.o }

// Stats is what t holds now.
func (t *Transport) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Stats{
		Sessions: len(t.sessions), InFlight: t.inflight,
		Readers: t.readers, ReservedBytes: t.reserved,
		Control: t.control, PageState: t.pageState, Calls: t.calls, RequestBytes: t.requestBytes,
		Writing: len(t.writing),
	}
}

// closing reports whether t takes no more requests. Called with t.mu held.
func (t *Transport) closing() bool { return t.closed || t.life.Err() != nil }

// ServeHTTP answers one POST or DELETE. It clears the write deadline on
// entry, so a call that runs longer than the server's WriteTimeout still
// answers; each answer sets its own deadline (see write).
func (t *Transport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	t.mu.Lock()
	closing := t.closing()
	t.mu.Unlock()
	if closing {
		t.refuseUnread(w, rc, http.StatusServiceUnavailable, errorBody(nil, codeServerBusy, closingText))
		return
	}
	switch r.Method {
	case http.MethodPost:
		t.post(w, rc, r)
	case http.MethodDelete:
		t.delete(w, rc, r)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		t.refuseUnread(w, rc, http.StatusMethodNotAllowed, errorBody(nil, codeInvalidRequest, "only POST and DELETE are served"))
	}
}

// post answers one JSON-RPC message. Its body is read only with a reader
// and its Content-Length reserved, both given back once it is parsed;
// then the message is admitted to its class (see Class) or refused.
func (t *Transport) post(w http.ResponseWriter, rc *http.ResponseController, r *http.Request) {
	if r.ContentLength > t.o.MaxMessageBytes {
		t.refuseUnread(w, rc, http.StatusRequestEntityTooLarge, errorBody(nil, codeInvalidRequest,
			fmt.Sprintf("message exceeds MaxMessageBytes (%d bytes)", t.o.MaxMessageBytes)))
		return
	}
	done, busy := t.reserveIngress(r.ContentLength)
	if busy != "" {
		t.log.Debug("mcp request refused unread", "reason", busy)
		w.Header().Set("Retry-After", "1")
		t.refuseUnread(w, rc, http.StatusServiceUnavailable, errorBody(nil, codeServerBusy, busy))
		return
	}
	req, bad, received, err := t.parseBody(w, rc, r)
	done()
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			w.Header().Set("Connection", "close")
			t.write(w, rc, http.StatusRequestEntityTooLarge, errorBody(nil, codeInvalidRequest,
				fmt.Sprintf("message exceeds MaxMessageBytes (%d bytes)", t.o.MaxMessageBytes)))
			return
		}
		w.Header().Set("Connection", "close")
		t.write(w, rc, http.StatusBadRequest, errorBody(nil, codeParseError, "the request body could not be read"))
		return
	}
	if bad != nil {
		t.write(w, rc, http.StatusBadRequest, bad)
		return
	}
	if req != nil && req.Method == "initialize" && !req.Notification() {
		t.initialize(w, rc, req, received)
		return
	}
	var id json.RawMessage
	class := ClassControl
	if req != nil {
		id = req.ID
		class = classify(t.h, req)
	}

	t.mu.Lock()
	s, status, refusal := t.find(r, id)
	if status != 0 {
		t.mu.Unlock()
		t.write(w, rc, status, refusal)
		return
	}
	if req == nil {
		// A client's response: this server sends no request, so it is
		// dropped, as stdio drops it.
		t.mu.Unlock()
		t.write(w, rc, http.StatusAccepted, nil)
		return
	}
	if req.Notification() {
		// A notification is answered under the lock and holds nothing
		// after, so it takes no place in the control class: a full class
		// never refuses one, and a cancel, which a client never retries,
		// is never lost.
		if req.Method == "notifications/cancelled" {
			t.cancelNamed(s, req.Params)
		}
		t.mu.Unlock()
		t.write(w, rc, http.StatusAccepted, nil)
		return
	}
	tk, busy := t.take(class, s, int64(len(req.Params)))
	if busy != "" {
		t.mu.Unlock()
		t.refuseBusy(w, rc, req.ID, busy)
		return
	}
	key := compactID(req.ID)
	ctx, cancel := context.WithCancel(t.life)
	defer cancel()
	if !t.register(s, key, cancel) {
		t.release(tk)
		t.mu.Unlock()
		t.write(w, rc, http.StatusBadRequest, errorBody(req.ID, codeInvalidRequest, "a request with this id is in flight in this session"))
		return
	}
	t.mu.Unlock()

	out := t.bound(req, t.h.Handle(mcpstdio.WithReceived(ctx, received), s.proto, req))
	if t.takeWriting() {
		defer t.doneWriting()
	}

	t.mu.Lock()
	t.unregister(s, key)
	t.release(tk)
	t.mu.Unlock()
	t.write(w, rc, http.StatusOK, out)
}

// takeWriting waits for a place to write a handler's answer, and reports
// whether it took one: once the transport closes, an answer is written
// without one, so a call ended by Close is answered at once.
func (t *Transport) takeWriting() bool {
	select {
	case t.writing <- struct{}{}:
		return true
	case <-t.life.Done():
		return false
	}
}

func (t *Transport) doneWriting() { <-t.writing }

// refuseBusy answers 503 with Retry-After and a JSON-RPC error for id
// carrying text, for a message that was read.
func (t *Transport) refuseBusy(w http.ResponseWriter, rc *http.ResponseController, id json.RawMessage, text string) {
	t.log.Debug("mcp request refused", "reason", text)
	w.Header().Set("Retry-After", "1")
	t.write(w, rc, http.StatusServiceUnavailable, errorBody(id, codeServerBusy, text))
}

// find is the session of a request other than initialize, checked in
// order: the session, then the protocol version, which is checked only
// for a session that exists, so a client probing with a newer revision and
// no session always gets the no-session answer it falls back on. A missing
// MCP-Protocol-Version means the session's negotiated one. A refusal
// returns its status and body, which carries id. Called with t.mu held.
func (t *Transport) find(r *http.Request, id json.RawMessage) (*session, int, []byte) {
	if t.closing() {
		return nil, http.StatusServiceUnavailable, errorBody(id, codeServerBusy, closingText)
	}
	s, status := t.lookup(r)
	switch status {
	case http.StatusBadRequest:
		return nil, status, errorBody(id, codeInvalidRequest, noSessionText)
	case http.StatusNotFound:
		return nil, status, errorBody(id, codeNoSession, unknownSessionText)
	}
	if v := r.Header.Values(HeaderVersion); len(v) > 0 && (len(v) > 1 || !slices.Contains(mcpstdio.SupportedVersions, v[0])) {
		return nil, http.StatusBadRequest, errorBody(id, codeInvalidRequest,
			"unsupported "+HeaderVersion+"; supported: "+strings.Join(mcpstdio.SupportedVersions, ", "))
	}
	return s, 0, nil
}

// initialize answers initialize on a new session, and keeps the session
// when initialize succeeded. An Mcp-Session-Id sent with it is ignored, so
// a client whose session is gone starts again. The table makes room by
// evicting the least recently used idle session; with every session busy
// the answer is 503. It is a control message.
func (t *Transport) initialize(w http.ResponseWriter, rc *http.ResponseController, req *mcpstdio.Request, received time.Time) {
	t.mu.Lock()
	tk, busy := t.takeControl()
	t.mu.Unlock()
	if busy != "" {
		t.refuseBusy(w, rc, req.ID, busy)
		return
	}
	ctx, cancel := context.WithCancel(t.life)
	defer cancel()
	s := &session{id: newSessionID(), proto: mcpstdio.NewSession(), calls: map[string]context.CancelFunc{}}
	out := t.bound(req, t.h.Handle(mcpstdio.WithReceived(ctx, received), s.proto, req))
	if t.takeWriting() {
		defer t.doneWriting()
	}
	t.mu.Lock()
	t.release(tk)
	if !s.proto.Initialized() {
		t.mu.Unlock()
		t.write(w, rc, http.StatusOK, out)
		return
	}
	if t.closing() {
		t.mu.Unlock()
		t.write(w, rc, http.StatusServiceUnavailable, errorBody(req.ID, codeServerBusy, closingText))
		return
	}
	added, evicted := t.admit(s)
	t.mu.Unlock()
	if evicted > 0 {
		t.log.Debug("mcp sessions evicted", "evicted", evicted)
	}
	if !added {
		w.Header().Set("Retry-After", "1")
		t.write(w, rc, http.StatusServiceUnavailable, errorBody(req.ID, codeServerBusy,
			fmt.Sprintf("server busy: MaxSessions=%d reached; retry", t.o.MaxSessions)))
		return
	}
	w.Header().Set(HeaderSession, s.id)
	t.write(w, rc, http.StatusOK, out)
}

// delete ends a session: its calls are cancelled and it is forgotten. Jobs
// it started are the server's and go on. It is a control message answered
// under the lock, so like a notification it takes no place in the class.
func (t *Transport) delete(w http.ResponseWriter, rc *http.ResponseController, r *http.Request) {
	t.mu.Lock()
	s, status, refusal := t.find(r, nil)
	if status != 0 {
		t.mu.Unlock()
		t.write(w, rc, status, refusal)
		return
	}
	t.remove(s)
	t.mu.Unlock()
	t.write(w, rc, http.StatusNoContent, nil)
}

// bound replaces an answer longer than MaxAnswerBytes by a small one for
// the same id.
func (t *Transport) bound(req *mcpstdio.Request, out []byte) []byte {
	if int64(len(out)) <= t.o.MaxAnswerBytes {
		return out
	}
	t.log.Warn("mcp answer over MaxAnswerBytes replaced", "method", req.Method, "bytes", len(out))
	if req.Method == "tools/call" {
		if b, err := encode(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  any             `json:"result"`
		}{"2.0", req.ID, map[string]any{
			"content": []map[string]string{{"type": "text", "text": tooLargeText}},
			"isError": true,
		}}); err == nil {
			return b
		}
	}
	return errorBody(req.ID, codeInternalError, "internal error: "+tooLargeText)
}

// JSON-RPC error codes, and the fixed texts of the transport's refusals.
// None repeats what the client sent but its id.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeInternalError  = -32603
	codeServerBusy     = -32000
	codeNoSession      = -32001

	noSessionText      = "no session: send initialize first"
	unknownSessionText = "unknown session: send initialize again"
	closingText        = "server closing"
	tooLargeText       = "result too large for the output bound"
)

// errorBody is a JSON-RPC error answer for id (null when there is none).
func errorBody(id json.RawMessage, code int, msg string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	type rpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	b, err := encode(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   rpcError        `json:"error"`
	}{"2.0", id, rpcError{code, msg}})
	if err != nil {
		// id came from Parse, so it is valid JSON; this is unreachable.
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return b
}

// encode is v as compact JSON without HTML escaping or a trailing newline,
// as mcpstdio writes its answers.
func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}
