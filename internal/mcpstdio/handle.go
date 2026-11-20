package mcpstdio

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// maxLabelBytes bounds a client label.
const maxLabelBytes = 64

// defaultLabel is the label of a client that sent no usable name.
const defaultLabel = "client"

// Session is the protocol state of one MCP session: whether initialize
// completed, the protocol revision negotiated, and the client's label. A
// transport without Serve makes one per session and passes it to Handle.
// It is safe for concurrent use.
type Session struct {
	initialized atomic.Bool
	// claimed is held by the initialize Handle is answering, and kept once
	// one succeeded, so a session's label and version never change.
	claimed atomic.Bool
	mu      sync.Mutex
	version string
	label   string
}

// NewSession is a session before initialize.
func NewSession() *Session { return &Session{} }

// Initialized reports whether the answer to initialize was produced.
func (s *Session) Initialized() bool { return s.initialized.Load() }

// Version is the protocol revision initialize negotiated, "" before it.
func (s *Session) Version() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.version
}

// Label is the client's label from initialize's clientInfo.name: at most
// 64 bytes of printable ASCII, else "client". It is "" before initialize.
func (s *Session) Label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.label
}

func (s *Session) record(version, label string) {
	s.mu.Lock()
	s.version, s.label = version, label
	s.mu.Unlock()
}

// clientLabel is name when it is a usable label, else "client".
func clientLabel(name string) string {
	if name == "" || len(name) > maxLabelBytes {
		return defaultLabel
	}
	for i := range len(name) {
		if c := name[i]; c < 0x20 || c > 0x7e {
			return defaultLabel
		}
	}
	return name
}

type protoKey struct{}

func withSession(ctx context.Context, ps *Session) context.Context {
	return context.WithValue(ctx, protoKey{}, ps)
}

// ClientLabel is the label of the session ctx belongs to, "" outside one.
func ClientLabel(ctx context.Context) string {
	if ps, ok := ctx.Value(protoKey{}).(*Session); ok {
		return ps.Label()
	}
	return ""
}

// WithReceived is ctx carrying the time a request was received; Received
// reads it back.
func WithReceived(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, receivedKey{}, at)
}

// Request is one parsed JSON-RPC request or notification.
type Request struct {
	Method string
	// ID is the id as sent; empty for a notification.
	ID     json.RawMessage
	Params json.RawMessage
	// Tool is the tool a tools/call names, and URI the resource a
	// resources/read names. Both are empty when the params do not say.
	Tool, URI string
}

// Notification reports whether r has no id, so it is never answered.
func (r *Request) Notification() bool { return len(r.ID) == 0 }

// Parse validates one message. It returns the request, or the encoded
// error answer (compact JSON, no trailing newline) for a message that is
// not valid, or neither for a client response, which is dropped.
func (s *Server) Parse(msg []byte) (*Request, []byte) {
	env, bad := s.parse(msg)
	if bad != nil {
		b, err := marshal(bad)
		if err != nil {
			s.warn("encode message", "", slog.LevelError, "err", err)
			return nil, nil
		}
		return nil, b
	}
	if env == nil {
		return nil, nil
	}
	r := &Request{Method: env.Method, ID: env.ID, Params: env.Params}
	switch env.Method {
	case "tools/call":
		var p struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			r.Tool = p.Name
		}
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if json.Unmarshal(env.Params, &p) == nil {
			r.URI = p.URI
		}
	}
	return r, nil
}

// Handle answers r as the stdio dispatcher does, with ss's state, and
// returns the compact JSON answer without a trailing newline; nil for a
// notification. The handler's context carries ss, so ClientLabel works. It
// installs no progress target, so Progress returns false, and it calls no
// OnClose. Calls on one session need not be serialized. Unlike stdio, it
// refuses an initialize on a session that has one answered or running, so
// a session's label and version never change; one that failed leaves the
// session free for another.
func (s *Server) Handle(ctx context.Context, ss *Session, r *Request) []byte {
	if r.Notification() {
		return nil
	}
	if r.Method == "initialize" {
		if !ss.claimed.CompareAndSwap(false, true) {
			b, err := marshal(errorResponse(r.ID, codeInvalidRequest, "initialize was already sent on this session"))
			if err != nil {
				s.warn("encode message", "", slog.LevelError, "err", err)
			}
			return b
		}
		defer func() {
			if !ss.initialized.Load() {
				ss.claimed.Store(false)
			}
		}()
	}
	env := &envelope{JSONRPC: "2.0", ID: r.ID, Method: r.Method, Params: r.Params}
	out, initialized := s.answer(withSession(ctx, ss), ss, env)
	b, err := marshal(out)
	if err != nil {
		s.warn("encode message", "", slog.LevelError, "err", err)
		b, _ = marshal(errorResponse(r.ID, codeInternalError, "internal error"))
	}
	if initialized {
		ss.initialized.Store(true)
	}
	return b
}

// Tool is the registered tool called name.
func (s *Server) Tool(name string) (Tool, bool) {
	if i := slices.IndexFunc(s.o.Tools, func(t Tool) bool { return t.Name == name }); i >= 0 {
		return s.o.Tools[i], true
	}
	return Tool{}, false
}

// Resource is the registered resource with uri.
func (s *Server) Resource(uri string) (Resource, bool) {
	if i := slices.IndexFunc(s.o.Resources, func(r Resource) bool { return r.URI == uri }); i >= 0 {
		return s.o.Resources[i], true
	}
	return Resource{}, false
}
