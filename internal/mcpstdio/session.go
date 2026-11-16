package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// session is the state of one Serve call.
type session struct {
	s *Server
	// hctx is the parent of every handler's context; hcancel ends them all.
	hctx    context.Context
	hcancel context.CancelFunc
	out     *outQueue
	// proto is the protocol state. Its initialized flag is set by the
	// dispatcher once the answer to initialize is enqueued, so a Control
	// call answered on the reader after it cannot overtake that answer.
	proto *Session

	wake       chan struct{} // cap 1: the dispatcher has work or must stop
	inputEnded chan struct{} // closed when the reader stops
	readErr    error         // why the input ended, when not at EOF; set before inputEnded closes
	dispatched chan struct{} // closed when the dispatcher returns
	endOnce    sync.Once

	mu        sync.Mutex
	queue     []*request
	running   *request
	inputDone bool // no more requests will be queued
	stopped   bool // stop reading and dispatching
}

// request is a queued or running request.
type request struct {
	env *envelope
	key string // the id as compact JSON, which notifications/cancelled matches
	// received is when the reader took the request, before it queued.
	received time.Time
	// Guarded by session.mu.
	cancel    context.CancelFunc
	cancelled bool
}

type (
	sessionKey  struct{}
	requestKey  struct{}
	receivedKey struct{}
)

func (s *Server) newSession(ctx context.Context, w io.Writer) *session {
	ss := &session{
		s:          s,
		out:        newOutQueue(w, s.o.MaxOutMessages, s.o.MaxOutBytes, s.o.StallTimeout),
		wake:       make(chan struct{}, 1),
		inputEnded: make(chan struct{}),
		dispatched: make(chan struct{}),
		proto:      NewSession(),
	}
	// The handlers' contexts do not inherit ctx's cancellation: end
	// cancels them after OnClose, on every path, ctx's end included.
	ss.hctx, ss.hcancel = context.WithCancel(context.WithValue(withSession(context.WithoutCancel(ctx), ss.proto), sessionKey{}, ss))
	return ss
}

// end calls OnClose and cancels every handler, once.
func (ss *session) end(why CloseReason) {
	ss.endOnce.Do(func() {
		if f := ss.s.o.OnClose; f != nil {
			f(why)
		}
		ss.hcancel()
	})
}

// halt stops the reader and the dispatcher and closes the output.
func (ss *session) halt() {
	ss.mu.Lock()
	ss.stopped = true
	ss.mu.Unlock()
	ss.wakeDispatcher()
	ss.out.shutdown()
}

// endGone ends a session whose client stopped reading.
func (ss *session) endGone() error {
	ss.end(CloseClientGone)
	ss.halt()
	return errors.Join(ss.out.goneErr(), ss.inputErr())
}

// endContext ends a session whose context is done: no new request is
// dispatched, and the output enqueued until the running handler returns or
// the drain deadline passes is written.
func (ss *session) endContext(ctx context.Context) error {
	ss.end(CloseContext)
	deadline := time.Now().Add(ss.s.o.DrainTimeout)
	ss.mu.Lock()
	ss.stopped = true
	ss.mu.Unlock()
	ss.wakeDispatcher()
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-ss.dispatched:
	case <-ss.out.dead:
	case <-t.C:
	}
	drained := ss.out.drain(deadline)
	ss.halt()
	if !drained {
		// It wraps ctx.Err() too: the session ended because ctx did.
		return errors.Join(fmt.Errorf("%w (%w)", ss.out.goneErr(), ctx.Err()), ss.inputErr())
	}
	return ss.inputErr()
}

// clientDead reports whether the output has marked the client dead.
func (ss *session) clientDead() bool {
	select {
	case <-ss.out.dead:
		return true
	default:
		return false
	}
}

// inputErr is the read error that ended the input, if the input has ended.
func (ss *session) inputErr() error {
	select {
	case <-ss.inputEnded:
		return ss.readErr
	default:
		return nil
	}
}

func (ss *session) wakeDispatcher() {
	select {
	case ss.wake <- struct{}{}:
	default:
	}
}

// halted reports whether the reader must stop.
func (ss *session) halted() bool {
	if ss.clientDead() {
		return true
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.stopped
}

// send encodes the response m and enqueues it, and reports whether it was
// enqueued. It uses the fatal enqueue: a response refused because the
// backlog is at its bound marks the client dead. The reader's own answers
// (server busy, parse and invalid-request errors, Control results) go
// through it by design, as the dispatcher's do. A notification must never
// go through send: notifications use the lossy enqueueNote (see Progress).
func (ss *session) send(m any) bool {
	line, err := encodeLine(m)
	if err != nil {
		ss.s.warn("encode message", "", slog.LevelError, "err", err)
		return false
	}
	return ss.out.enqueue(line)
}

// tooLargeText answers a call whose answer line would exceed half of
// MaxOutBytes.
const tooLargeText = "result too large for the output bound"

// sendAnswer encodes the answer to env and enqueues it like send. A line
// over half of MaxOutBytes is never enqueued: behind a backlog under half
// the bound, which is when a request starts, it could fill the backlog to
// the bound, and then the reader's own small answers would mark a live
// client dead. Escaping makes the line's size hard to foresee (a control
// byte or an invalid UTF-8 byte becomes six), so the line itself is
// measured, and replaced by a small answer for the same id: an isError
// result for tools/call, an internal error otherwise.
func (ss *session) sendAnswer(env *envelope, out *outgoing) bool {
	line, err := encodeLine(out)
	if err != nil {
		ss.s.warn("encode message", "", slog.LevelError, "err", err)
		return false
	}
	if 2*len(line) > ss.s.o.MaxOutBytes {
		ss.s.warn("answer over half the output bound replaced", "", slog.LevelWarn, "method", env.Method, "bytes", len(line))
		small := errorResponse(env.ID, codeInternalError, "internal error: "+tooLargeText)
		if env.Method == "tools/call" {
			if raw, err := marshal(toolResult(Result{Text: tooLargeText, IsError: true})); err == nil {
				small = &outgoing{JSONRPC: "2.0", ID: env.ID, Result: raw}
			}
		}
		if line, err = encodeLine(small); err != nil {
			ss.s.warn("encode message", "", slog.LevelError, "err", err)
			return false
		}
	}
	return ss.out.enqueue(line)
}

// encodeLine is m as one compact JSON line without HTML escaping.
func encodeLine(m any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b) // compact, newline-terminated
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// read splits r into lines and takes each one as it comes. A line longer
// than the limit is answered once as oversized and the remainder of it is
// read and dropped, never held. Nothing here waits but r.Read.
func (ss *session) read(r io.Reader) {
	defer close(ss.inputEnded)
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if ss.halted() {
			return
		}
		if !over {
			if len(buf)+len(chunk) > ss.s.max+1 {
				over = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil || errors.Is(err, io.EOF):
			complete := err == nil
			if !over {
				buf = bytes.TrimSuffix(buf, []byte("\n"))
				over = len(buf) > ss.s.max
			}
			if over || len(bytes.TrimSpace(buf)) > 0 {
				ss.take(buf, over)
			}
			buf, over = nil, false
			if !complete {
				return
			}
		case errors.Is(err, bufio.ErrBufferFull):
			// keep accumulating (or discarding) the same line
		default:
			ss.readErr = err
			return
		}
	}
}

// take handles one input line on the reader goroutine.
func (ss *session) take(line []byte, oversized bool) {
	if oversized {
		ss.send(errorResponse(nil, codeInvalidRequest,
			fmt.Sprintf("message exceeds MaxMessageBytes (%d bytes); it was discarded", ss.s.max)))
		return
	}
	env, bad := ss.s.parse(line)
	if bad != nil {
		ss.send(bad)
		return
	}
	if env == nil {
		return
	}
	if len(env.ID) == 0 {
		// Notification: never answered.
		if env.Method == "notifications/cancelled" {
			ss.cancelRequest(env.Params)
		}
		return
	}
	if ss.proto.Initialized() && ss.s.isControlCall(env) {
		if out, _ := ss.s.answer(ss.hctx, ss.proto, env); out != nil {
			ss.sendAnswer(env, out)
		}
		return
	}
	ss.mu.Lock()
	// The running request counts too: once a request is answered the
	// client may send another before the dispatcher takes the next one, so
	// 1+MaxQueued outstanding requests always fit.
	outstanding := len(ss.queue)
	if ss.running != nil {
		outstanding++
	}
	if outstanding > ss.s.o.MaxQueued {
		ss.mu.Unlock()
		ss.send(errorResponse(env.ID, codeServerBusy, fmt.Sprintf("server busy: %d requests queued; retry", ss.s.o.MaxQueued)))
		return
	}
	ss.queue = append(ss.queue, &request{env: env, key: compactID(env.ID), received: time.Now()})
	ss.mu.Unlock()
	ss.wakeDispatcher()
}

// isControlCall reports whether env calls a Control tool.
func (s *Server) isControlCall(env *envelope) bool {
	if env.Method != "tools/call" {
		return false
	}
	var p struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(env.Params, &p) != nil {
		return false
	}
	i := slices.IndexFunc(s.o.Tools, func(t Tool) bool { return t.Name == p.Name })
	return i >= 0 && s.o.Tools[i].Control
}

// cancelRequest ends the request notifications/cancelled names: the running
// one is cancelled and its answer dropped, a queued one is dropped
// unanswered, and an unknown id is ignored.
func (ss *session) cancelRequest(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || !validID(p.RequestID) {
		return
	}
	key := compactID(p.RequestID)
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if r := ss.running; r != nil && r.key == key {
		r.cancelled = true
		r.cancel()
		return
	}
	if i := slices.IndexFunc(ss.queue, func(r *request) bool { return r.key == key }); i >= 0 {
		ss.queue = slices.Delete(ss.queue, i, i+1)
	}
}

func compactID(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

// dispatch answers the queued requests one at a time, in order, until the
// input is done and the queue empty, or the session stops.
func (ss *session) dispatch() {
	defer close(ss.dispatched)
	for {
		r, ctx := ss.next()
		if r == nil {
			return
		}
		out, initialized := ss.s.answer(ctx, ss.proto, r.env)
		ss.mu.Lock()
		ss.running = nil
		dropped := r.cancelled
		ss.mu.Unlock()
		r.cancel()
		if out != nil && !dropped {
			ss.sendAnswer(r.env, out)
		}
		if initialized {
			ss.proto.initialized.Store(true)
		}
	}
}

// next takes the first queued request and makes it the running one, or
// returns nil once there is nothing more to do. It starts a request only
// while the output backlog is under half its bounds, so a slow but live
// client holds requests back rather than overflowing the output. The
// reader goes on meanwhile: cancels, EOF and the end of ctx are still seen.
func (ss *session) next() (*request, context.Context) {
	for {
		if ss.clientDead() {
			// No answer can be delivered: start nothing more.
			return nil, nil
		}
		ss.mu.Lock()
		if ss.stopped {
			ss.mu.Unlock()
			return nil, nil
		}
		if len(ss.queue) == 0 {
			done := ss.inputDone
			ss.mu.Unlock()
			if done {
				return nil, nil
			}
			<-ss.wake
			continue
		}
		ss.mu.Unlock()
		if ok, changed := ss.out.room(); !ok {
			select {
			case <-changed:
			case <-ss.wake:
			case <-ss.out.dead:
			}
			continue
		}
		ss.mu.Lock()
		if ss.stopped || len(ss.queue) == 0 || ss.clientDead() {
			ss.mu.Unlock()
			continue
		}
		r := ss.queue[0]
		ss.queue[0] = nil
		ss.queue = ss.queue[1:]
		ctx, cancel := context.WithCancel(context.WithValue(ss.hctx, requestKey{}, r))
		r.cancel = cancel
		ss.running = r
		ss.mu.Unlock()
		return r, ctx
	}
}

// answer handles one request and returns its response, and whether it
// completed initialize.
func (s *Server) answer(ctx context.Context, ps *Session, env *envelope) (*outgoing, bool) {
	// Unknown methods (server/discover included) answer method-not-found even
	// before initialize so clients can fall back to the initialize handshake.
	if env.Method != "initialize" && env.Method != "ping" && !ps.Initialized() && knownMethods[env.Method] {
		return errorResponse(env.ID, codeInvalidRequest, "send initialize first"), false
	}
	result, rerr := s.handle(ctx, ps, env.Method, env.Params)
	if rerr != nil {
		return errorResponse(env.ID, rerr.Code, rerr.Message), false
	}
	raw, err := marshal(result)
	if err != nil {
		s.warn("marshal result", "", slog.LevelError, "method", env.Method, "err", err)
		return errorResponse(env.ID, codeInternalError, "internal error"), false
	}
	return &outgoing{JSONRPC: "2.0", ID: env.ID, Result: raw}, env.Method == "initialize"
}

type progressKey struct{}

// progressTarget is where a request's progress goes.
type progressTarget struct {
	token json.RawMessage
	ss    *session
	r     *request
}

// Progress sends a notifications/progress for the tool call ctx belongs to,
// through the output queue, and reports whether it was enqueued. It sends
// nothing, and returns false, when the call carried no progressToken, when
// the call was cancelled or has been answered, outside a tool call, and in
// a Control tool. A total <= 0 and an empty message are left out. It never
// waits. Notifications are lossy: one is dropped (false) while the output
// backlog is at half its bounds or more, and that never ends the session.
func Progress(ctx context.Context, progress, total float64, message string) bool {
	p, ok := ctx.Value(progressKey{}).(*progressTarget)
	if !ok || ctx.Err() != nil {
		return false
	}
	type params struct {
		ProgressToken json.RawMessage `json:"progressToken"`
		Progress      float64         `json:"progress"`
		Total         float64         `json:"total,omitzero"`
		Message       string          `json:"message,omitzero"`
	}
	line, err := encodeLine(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  params `json:"params"`
	}{"2.0", "notifications/progress", params{p.token, progress, max(total, 0), message}})
	if err != nil {
		return false
	}
	// Under ss.mu the dispatcher cannot answer the call meanwhile, so no
	// notification follows the call's answer.
	p.ss.mu.Lock()
	defer p.ss.mu.Unlock()
	if p.ss.running != p.r || p.r.cancelled {
		return false
	}
	return p.ss.out.enqueueNote(line)
}

// Received is when the request ctx belongs to was read from the client,
// before it waited in the queue; ok is false for a context of no queued
// request (a Control call runs as it is read). A time set with
// WithReceived takes precedence over the request's own.
func Received(ctx context.Context) (at time.Time, ok bool) {
	if t, ok := ctx.Value(receivedKey{}).(time.Time); ok {
		return t, true
	}
	r, ok := ctx.Value(requestKey{}).(*request)
	if !ok {
		return time.Time{}, false
	}
	return r.received, true
}
