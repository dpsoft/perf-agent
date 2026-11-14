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
)

// Server is an MCP server. Requests are handled one at a time, in order.
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
	return s
}

// inputLine is one line read from the client.
type inputLine struct {
	data      []byte
	oversized bool
	err       error
}

// Serve reads newline-delimited JSON-RPC messages from r and writes
// responses to w until r reaches EOF or ctx ends, both of which return nil.
// Nothing but compact JSON-RPC lines is ever written to w.
//
// A reader goroutine owns r: it may still read from r once more after Serve
// returns, so the caller should close r. A blocked w.Write is not
// interrupted by ctx.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := make(chan inputLine)
	go s.readLines(ctx, r, lines)

	initialized := false
	for {
		var in inputLine
		var ok bool
		select {
		case <-ctx.Done():
			return nil
		case in, ok = <-lines:
			if !ok {
				return nil
			}
		}
		if in.err != nil {
			return in.err
		}
		var out *outgoing
		if in.oversized {
			out = errorResponse(nil, codeInvalidRequest,
				fmt.Sprintf("message exceeds MaxMessageBytes (%d bytes); it was discarded", s.max))
		} else {
			out = s.dispatch(ctx, in.data, &initialized)
		}
		if out == nil {
			continue
		}
		if err := writeMessage(w, out); err != nil {
			return err
		}
	}
}

// readLines splits r into lines. A line longer than the limit is reported
// once as oversized and the remainder of it is read and dropped, never held.
func (s *Server) readLines(ctx context.Context, r io.Reader, out chan<- inputLine) {
	defer close(out)
	send := func(l inputLine) bool {
		select {
		case out <- l:
			return true
		case <-ctx.Done():
			return false
		}
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var buf []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !over {
			if len(buf)+len(chunk) > s.max+1 {
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
				over = len(buf) > s.max
			}
			if over || len(bytes.TrimSpace(buf)) > 0 {
				if !send(inputLine{data: buf, oversized: over}) {
					return
				}
			}
			buf, over = nil, false
			if !complete {
				return
			}
		case errors.Is(err, bufio.ErrBufferFull):
			// keep accumulating (or discarding) the same line
		default:
			send(inputLine{err: err})
			return
		}
	}
}

func writeMessage(w io.Writer, m *outgoing) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b) // compact, newline-terminated
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return err
	}
	_, err := w.Write(b.Bytes())
	return err
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
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// dispatch handles one line and returns the response, or nil for none.
func (s *Server) dispatch(ctx context.Context, line []byte, initialized *bool) *outgoing {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		return errorResponse(nil, codeInvalidRequest, "batches are not supported")
	}
	if !json.Valid(trimmed) {
		return errorResponse(nil, codeParseError, "parse error: invalid JSON")
	}
	if trimmed[0] != '{' {
		return errorResponse(nil, codeInvalidRequest, "invalid request: a message must be a JSON object")
	}
	var env envelope
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return errorResponse(nil, codeInvalidRequest, "invalid request: "+err.Error())
	}
	if env.Method == "" && (len(env.Result) > 0 || len(env.Error) > 0) {
		s.log.Warn("dropping a client response; this server sends no requests")
		return nil
	}
	if len(env.ID) > 0 && !validID(env.ID) {
		return errorResponse(nil, codeInvalidRequest, "id must be a string or integer")
	}
	if env.JSONRPC != "2.0" {
		return errorResponse(env.ID, codeInvalidRequest, `invalid request: jsonrpc must be "2.0"`)
	}
	if env.Method == "" {
		return errorResponse(env.ID, codeInvalidRequest, "invalid request: method is required")
	}
	if len(env.ID) == 0 {
		// Notification: never answered.
		return nil
	}
	// Unknown methods (server/discover included) answer method-not-found even
	// before initialize so clients can fall back to the initialize handshake.
	if env.Method != "initialize" && env.Method != "ping" && !*initialized && knownMethods[env.Method] {
		return errorResponse(env.ID, codeInvalidRequest, "send initialize first")
	}
	result, rerr := s.handle(ctx, env.Method, env.Params)
	if rerr != nil {
		return errorResponse(env.ID, rerr.Code, rerr.Message)
	}
	if env.Method == "initialize" {
		*initialized = true
	}
	raw, err := marshal(result)
	if err != nil {
		s.log.Error("marshal result", "method", env.Method, "err", err)
		return errorResponse(env.ID, codeInternalError, "internal error")
	}
	return &outgoing{JSONRPC: "2.0", ID: env.ID, Result: raw}
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
	return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
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
	}
	if e := decodeParams(params, &p); e != nil {
		return nil, e
	}
	i := slices.IndexFunc(s.o.Tools, func(t Tool) bool { return t.Name == p.Name })
	if i < 0 || s.o.Tools[i].Handler == nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
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
		res = Result{Text: err.Error(), IsError: true}
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
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown prompt: " + p.Name}
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
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown resource: " + p.URI}
	}
	r := s.o.Resources[i]
	content := map[string]string{"uri": r.URI, "text": r.Text}
	if r.MIMEType != "" {
		content["mimeType"] = r.MIMEType
	}
	return map[string]any{"contents": []map[string]string{content}}, nil
}
