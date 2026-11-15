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
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Server is an MCP server. Requests are handled one at a time, in order,
// except the calls to Control tools, which are answered at once.
type Server struct {
	o   Options
	max int
	log *slog.Logger
}

// New builds a server from o.
func New(o Options) *Server {
	s := &Server{o: o, max: o.MaxMessageBytes, log: o.Log}
	if s.max <= 0 {
		s.max = defaultMaxMessageBytes
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.o.MaxQueued <= 0 {
		s.o.MaxQueued = defaultMaxQueued
	}
	if s.o.MaxOutMessages <= 0 {
		s.o.MaxOutMessages = defaultMaxOutMessages
	}
	if s.o.MaxOutBytes <= 0 {
		s.o.MaxOutBytes = defaultMaxOutBytes
		if s.o.MaxToolErrorBytes > 0 {
			s.o.MaxOutBytes = 4 * s.o.MaxToolErrorBytes
		}
	}
	if s.o.DrainTimeout <= 0 {
		s.o.DrainTimeout = defaultDrainTimeout
	}
	return s
}

// Serve reads newline-delimited JSON-RPC messages from r and writes
// responses and notifications to w. Nothing but compact JSON-RPC lines is
// ever written to w, and only by one goroutine.
//
// Nothing Serve does waits on w. The reader goroutine takes every line as
// it comes: notifications/cancelled ends the named request, a Control tool
// is answered at once, and any other request joins a queue of MaxQueued,
// beyond which it is answered "server busy". One dispatcher answers the
// queue in order. Every message goes through a bounded output queue.
//
// Serve returns:
//   - at EOF, once the queued requests are answered (their contexts are
//     cancelled first) and the output is drained: nil, or ErrClientGone
//     when the drain took longer than DrainTimeout. An input that fails
//     to read ends the same way, and Serve then returns the read error;
//   - when ctx ends, once the output enqueued so far is drained, for at
//     most DrainTimeout: nil, or an error that is both ErrClientGone and
//     ctx.Err() when the drain ran out;
//   - at once with ErrClientGone when the client stopped reading its
//     output (the output queue overflowed or a write failed).
//
// A handler or a write still running then is not waited for. The reader
// goroutine owns r: it may still read from r once more after Serve
// returns, so the caller should close r.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ss := s.newSession(ctx, w)
	defer ss.hcancel()
	go ss.out.run()
	go ss.dispatch()
	go ss.read(r)

	select {
	case <-ctx.Done():
		return ss.endContext(ctx)
	case <-ss.out.dead:
		return ss.endGone()
	case <-ss.inputEnded:
	}
	ss.end(CloseEOF)
	ss.mu.Lock()
	ss.inputDone = true
	ss.mu.Unlock()
	ss.wakeDispatcher()
	select {
	case <-ss.dispatched:
	case <-ctx.Done():
		return ss.endContext(ctx)
	case <-ss.out.dead:
		return ss.endGone()
	}
	drained := ss.out.drain(time.Now().Add(s.o.DrainTimeout))
	ss.halt()
	if !drained {
		return ss.out.goneErr()
	}
	return ss.readErr
}

// session is the state of one Serve call.
type session struct {
	s *Server
	// hctx is the parent of every handler's context; hcancel ends them all.
	hctx    context.Context
	hcancel context.CancelFunc
	out     *outQueue
	// initialized is set by the dispatcher once the answer to initialize
	// is enqueued, so a Control call answered on the reader after it
	// cannot overtake that answer.
	initialized atomic.Bool

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
	// Guarded by session.mu.
	cancel    context.CancelFunc
	cancelled bool
}

type sessionKey struct{}

func (s *Server) newSession(ctx context.Context, w io.Writer) *session {
	ss := &session{
		s:          s,
		out:        newOutQueue(w, s.o.MaxOutMessages, s.o.MaxOutBytes),
		wake:       make(chan struct{}, 1),
		inputEnded: make(chan struct{}),
		dispatched: make(chan struct{}),
	}
	ss.hctx, ss.hcancel = context.WithCancel(context.WithValue(ctx, sessionKey{}, ss))
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
	return ss.out.goneErr()
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
		return fmt.Errorf("%w (%w)", ss.out.goneErr(), ctx.Err())
	}
	return nil
}

func (ss *session) wakeDispatcher() {
	select {
	case ss.wake <- struct{}{}:
	default:
	}
}

// halted reports whether the reader must stop.
func (ss *session) halted() bool {
	select {
	case <-ss.out.dead:
		return true
	default:
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.stopped
}

// send encodes m and enqueues it, and reports whether it was enqueued.
func (ss *session) send(m any) bool {
	line, err := encodeLine(m)
	if err != nil {
		ss.s.log.Error("encode message", "err", err)
		return false
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
	if ss.initialized.Load() && ss.s.isControlCall(env) {
		if out, _ := ss.answer(ss.hctx, env); out != nil {
			ss.send(out)
		}
		return
	}
	ss.mu.Lock()
	if n := len(ss.queue); n >= ss.s.o.MaxQueued {
		ss.mu.Unlock()
		ss.send(errorResponse(env.ID, codeServerBusy, fmt.Sprintf("server busy: %d requests queued; retry", n)))
		return
	}
	ss.queue = append(ss.queue, &request{env: env, key: compactID(env.ID)})
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
		out, initialized := ss.answer(ctx, r.env)
		ss.mu.Lock()
		ss.running = nil
		dropped := r.cancelled
		ss.mu.Unlock()
		r.cancel()
		if out != nil && !dropped {
			ss.send(out)
		}
		if initialized {
			ss.initialized.Store(true)
		}
	}
}

// next takes the first queued request and makes it the running one, or
// returns nil once there is nothing more to do.
func (ss *session) next() (*request, context.Context) {
	for {
		ss.mu.Lock()
		if ss.stopped {
			ss.mu.Unlock()
			return nil, nil
		}
		if len(ss.queue) > 0 {
			r := ss.queue[0]
			ss.queue[0] = nil
			ss.queue = ss.queue[1:]
			ctx, cancel := context.WithCancel(ss.hctx)
			r.cancel = cancel
			ss.running = r
			ss.mu.Unlock()
			return r, ctx
		}
		done := ss.inputDone
		ss.mu.Unlock()
		if done {
			return nil, nil
		}
		<-ss.wake
	}
}

func errorResponse(id json.RawMessage, code int, msg string) *outgoing {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &outgoing{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// validID reports whether raw is a JSON string or integer, the only id forms
// MCP allows (null, fractions, exponents, objects and so on are not).
func validID(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if raw[0] == '"' {
		return true
	}
	digits := bytes.TrimPrefix(raw, []byte("-"))
	if len(digits) == 0 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// marshal encodes v compactly without HTML escaping, so text such as "<b>&"
// reaches the client literally.
func marshal(v any) ([]byte, error) {
	b, err := encodeLine(v)
	if err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b, []byte("\n")), nil
}

// parse validates one line. It returns the message, or the error answer for
// a line that is not a valid message, or neither for a line that is
// dropped silently.
func (s *Server) parse(line []byte) (*envelope, *outgoing) {
	trimmed := bytes.TrimSpace(line)
	if !json.Valid(trimmed) {
		return nil, errorResponse(nil, codeParseError, "parse error: invalid JSON")
	}
	if trimmed[0] == '[' {
		return nil, errorResponse(nil, codeInvalidRequest, "batches are not supported")
	}
	if trimmed[0] != '{' {
		return nil, errorResponse(nil, codeInvalidRequest, "invalid request: a message must be a JSON object")
	}
	var env envelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, errorResponse(nil, codeInvalidRequest, "invalid request: "+err.Error())
	}
	if env.Method == "" && (len(env.Result) > 0 || len(env.Error) > 0) {
		s.log.Warn("dropping a client response; this server sends no requests")
		return nil, nil
	}
	if len(env.ID) > 0 && !validID(env.ID) {
		return nil, errorResponse(nil, codeInvalidRequest, "id must be a string or integer")
	}
	if env.JSONRPC != "2.0" {
		return nil, errorResponse(env.ID, codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	if env.Method == "" {
		return nil, errorResponse(env.ID, codeInvalidRequest, "invalid request: method is required")
	}
	return &env, nil
}

// answer handles one request and returns its response, and whether it
// completed initialize.
func (ss *session) answer(ctx context.Context, env *envelope) (*outgoing, bool) {
	// Unknown methods (server/discover included) answer method-not-found even
	// before initialize so clients can fall back to the initialize handshake.
	if env.Method != "initialize" && env.Method != "ping" && !ss.initialized.Load() && knownMethods[env.Method] {
		return errorResponse(env.ID, codeInvalidRequest, "send initialize first"), false
	}
	result, rerr := ss.s.handle(ctx, env.Method, env.Params)
	if rerr != nil {
		return errorResponse(env.ID, rerr.Code, rerr.Message), false
	}
	raw, err := marshal(result)
	if err != nil {
		ss.s.log.Error("marshal result", "method", env.Method, "err", err)
		return errorResponse(env.ID, codeInternalError, "internal error"), false
	}
	return &outgoing{JSONRPC: "2.0", ID: env.ID, Result: raw}, env.Method == "initialize"
}

var knownMethods = map[string]bool{
	"tools/list": true, "tools/call": true,
	"prompts/list": true, "prompts/get": true,
	"resources/list": true, "resources/templates/list": true, "resources/read": true,
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return s.initialize(params)
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		tools := slices.Clone(s.o.Tools)
		for i := range tools {
			if len(tools[i].InputSchema) == 0 {
				tools[i].InputSchema = json.RawMessage(`{"type":"object"}`)
			}
		}
		return map[string]any{"tools": nonNil(tools)}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	case "prompts/list":
		return map[string]any{"prompts": nonNil(slices.Clone(s.o.Prompts))}, nil
	case "prompts/get":
		return s.getPrompt(params)
	case "resources/list":
		return map[string]any{"resources": nonNil(slices.Clone(s.o.Resources))}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []struct{}{}}, nil
	case "resources/read":
		return s.readResource(params)
	}
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + echoName(method)}
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func decodeParams(params json.RawMessage, v any) *rpcError {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	return nil
}

func (s *Server) initialize(params json.RawMessage) (any, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if e := decodeParams(params, &p); e != nil {
		return nil, e
	}
	version := LatestVersion
	if slices.Contains(SupportedVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	type listChanged struct {
		ListChanged bool `json:"listChanged"`
	}
	type resourcesCap struct {
		Subscribe   bool `json:"subscribe"`
		ListChanged bool `json:"listChanged"`
	}
	res := map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools":     listChanged{},
			"prompts":   listChanged{},
			"resources": resourcesCap{},
		},
		"serverInfo": s.o.Info,
	}
	if s.o.Instructions != "" {
		res["instructions"] = s.o.Instructions
	}
	return res, nil
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if e := decodeParams(params, &p); e != nil {
		return nil, e
	}
	i := slices.IndexFunc(s.o.Tools, func(t Tool) bool { return t.Name == p.Name })
	if i < 0 || s.o.Tools[i].Handler == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + echoName(p.Name)}
	}
	if ss, ok := ctx.Value(sessionKey{}).(*session); ok && !s.o.Tools[i].Control && validID(p.Meta.ProgressToken) {
		ctx = context.WithValue(ctx, progressKey{}, &progressTarget{token: p.Meta.ProgressToken, ss: ss})
	}
	args := p.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	res, err, panicked := s.runHandler(ctx, s.o.Tools[i], args)
	if panicked {
		return nil, &rpcError{Code: codeInternalError, Message: "internal error: tool " + p.Name + " failed"}
	}
	if err != nil {
		res = Result{Text: cut(err.Error(), s.o.MaxToolErrorBytes), IsError: true}
	}
	return map[string]any{
		"content": []textContent{{Type: "text", Text: res.Text}},
		"isError": res.IsError,
	}, nil
}

// runHandler calls the tool and turns a panic into a flag so one bad handler
// cannot take the server down.
func (s *Server) runHandler(ctx context.Context, t Tool, args json.RawMessage) (res Result, err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("tool handler panicked", "tool", t.Name, "panic", fmt.Sprint(r))
			panicked = true
		}
	}()
	res, err = t.Handler(ctx, args)
	return res, err, false
}

func (s *Server) getPrompt(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name string `json:"name"`
	}
	if e := decodeParams(params, &p); e != nil {
		return nil, e
	}
	i := slices.IndexFunc(s.o.Prompts, func(x Prompt) bool { return x.Name == p.Name })
	if i < 0 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown prompt: " + echoName(p.Name)}
	}
	pr := s.o.Prompts[i]
	res := map[string]any{
		"messages": []map[string]any{{
			"role":    "user",
			"content": textContent{Type: "text", Text: pr.Text},
		}},
	}
	if pr.Description != "" {
		res["description"] = pr.Description
	}
	return res, nil
}

func (s *Server) readResource(params json.RawMessage) (any, *rpcError) {
	var p struct {
		URI string `json:"uri"`
	}
	if e := decodeParams(params, &p); e != nil {
		return nil, e
	}
	i := slices.IndexFunc(s.o.Resources, func(x Resource) bool { return x.URI == p.URI })
	if i < 0 {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource: " + echoName(p.URI)}
	}
	r := s.o.Resources[i]
	content := map[string]string{"uri": r.URI, "text": r.Text}
	if r.MIMEType != "" {
		content["mimeType"] = r.MIMEType
	}
	return map[string]any{"contents": []map[string]string{content}}, nil
}

// maxEchoName is the most characters of a client's tool, prompt or
// resource name an error repeats.
const maxEchoName = 64

// echoName is name cut to maxEchoName characters, quoted.
func echoName(name string) string {
	if r := []rune(name); len(r) > maxEchoName {
		name = string(r[:maxEchoName-1]) + "…"
	}
	return strconv.Quote(name)
}

// cut is s bounded to limit bytes (no bound when limit <= 0), cut at a
// rune boundary and ending in "…" when cut.
func cut(s string, limit int) string {
	const ellipsis = "…"
	if limit <= 0 || len(s) <= limit {
		return s
	}
	n := max(limit-len(ellipsis), 0)
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + ellipsis
}

type progressKey struct{}

// progressTarget is where a request's progress goes.
type progressTarget struct {
	token json.RawMessage
	ss    *session
}

// Progress sends a notifications/progress for the tool call ctx belongs to,
// through the output queue, and reports whether it was enqueued. It sends
// nothing, and returns false, when the call carried no progressToken, when
// ctx is done (the call was cancelled or has been answered), or outside a
// tool call. A total <= 0 and an empty message are left out. It never
// waits; a notification that does not fit the output queue marks the
// client dead, as any message does.
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
	return p.ss.send(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  params `json:"params"`
	}{"2.0", "notifications/progress", params{p.token, progress, max(total, 0), message}})
}
