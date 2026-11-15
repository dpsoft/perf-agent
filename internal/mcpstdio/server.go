package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
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
			// Room for two results of MaxToolErrorBytes, each at its worst
			// escaping, with their envelopes (see MaxResultBytes).
			s.o.MaxOutBytes = 4*s.o.MaxToolErrorBytes + 2*resultEnvelopeBytes
		}
	}
	if s.o.DrainTimeout <= 0 {
		s.o.DrainTimeout = defaultDrainTimeout
	}
	if s.o.StallTimeout <= 0 {
		s.o.StallTimeout = defaultStallTimeout
	}
	return s
}

// Serve reads newline-delimited JSON-RPC messages from r and writes
// responses and notifications to w. Nothing but compact JSON-RPC lines is
// ever written to w, and only by one goroutine.
//
// Nothing Serve does waits on w. The reader goroutine takes every line as
// it comes: notifications/cancelled ends the named request, a Control tool
// is answered at once, and any other request joins a queue of MaxQueued
// (beyond the one running), past which it is answered "server busy". One
// dispatcher answers the queue in order, starting each request only once
// the output backlog is under half its bounds. Every message goes through
// a bounded output queue.
//
// EOF on r means the client is gone. The handler running and the requests
// still queued run with a cancelled context and are answered in order: a
// handler that honours its context answers as cancelled (isError). A
// script that pipes requests in must keep r open until it has its answers.
//
// Serve returns:
//   - at EOF, once the queued requests are answered and the output is
//     drained: nil, or ErrClientGone when the drain took longer than
//     DrainTimeout. An input that fails to read ends the same way, and
//     Serve then returns the read error (joined to ErrClientGone when the
//     drain ran out);
//   - when ctx ends, once the output enqueued so far is drained, for at
//     most DrainTimeout: nil, or an error that is both ErrClientGone and
//     ctx.Err() when the drain ran out;
//   - at once with ErrClientGone when the client stopped reading its
//     output: a response found the backlog at its bound, a write failed,
//     or no write ended for StallTimeout while messages waited.
//
// On each path OnClose runs first, then every handler's context is
// cancelled. A handler or a write still running when Serve returns is not
// waited for. The reader goroutine owns r: it may still read from r once
// more after Serve returns, so the caller should close r.
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
	// The reader stops when the client is dead too; that is not an EOF.
	select {
	case <-ss.out.dead:
		return ss.endGone()
	default:
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
		return errors.Join(ss.out.goneErr(), ss.readErr)
	}
	return ss.readErr
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
	ss, inSession := ctx.Value(sessionKey{}).(*session)
	r, queued := ctx.Value(requestKey{}).(*request)
	if inSession && queued && !s.o.Tools[i].Control && validID(p.Meta.ProgressToken) {
		ctx = context.WithValue(ctx, progressKey{}, &progressTarget{token: p.Meta.ProgressToken, ss: ss, r: r})
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
	return toolResult(res), nil
}

// toolResult is the tools/call result for res.
func toolResult(res Result) any {
	return map[string]any{
		"content": []textContent{{Type: "text", Text: res.Text}},
		"isError": res.IsError,
	}
}

// resultEnvelopeBytes bounds what a tools/call answer adds to its escaped
// text, for an id of up to maxResultIDBytes.
const (
	resultEnvelopeBytes = 256
	maxResultIDBytes    = 64
)

// MaxResultBytes is the largest tool result text whose answer line stays
// within half of MaxOutBytes, even when JSON escaping doubles every byte
// (the worst case for a JSON text) and the id is up to 64 bytes. A result
// that size behind a backlog under half the bound, which is when the
// dispatcher starts a request, leaves room for the reader's own answers.
func (s *Server) MaxResultBytes() int {
	return max((s.o.MaxOutBytes/2-resultEnvelopeBytes)/2, 0)
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
