// Package mcpstdio is a minimal Model Context Protocol server over
// newline-delimited JSON-RPC 2.0 on a reader and a writer (stdio). It knows
// nothing about the application: callers register tools, prompts and static
// resources. It opens no network connection.
package mcpstdio

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"
)

// LatestVersion is the protocol revision answered when the client asks for a
// revision the server does not speak.
const LatestVersion = "2025-11-25"

// SupportedVersions lists the protocol revisions the server echoes back.
var SupportedVersions = []string{"2025-11-25", "2025-06-18"}

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	// codeServerBusy answers a request that finds the request queue full.
	codeServerBusy = -32000
)

// Defaults for the zero Options fields.
const (
	defaultMaxMessageBytes = 4 << 20
	defaultMaxQueued       = 8
	defaultMaxOutMessages  = 64
	defaultMaxOutBytes     = 4 << 20
	defaultDrainTimeout    = time.Second
	defaultStallTimeout    = 30 * time.Second
)

// ErrClientGone is what Serve returns when the client stopped reading its
// output: a message did not fit in the output queue, a write failed, or the
// final drain ran out of time.
var ErrClientGone = errors.New("mcpstdio: the client stopped reading its output")

// CloseReason says why a session ended.
type CloseReason int

const (
	// CloseEOF: the input ended (or could not be read).
	CloseEOF CloseReason = iota
	// CloseClientGone: the client stopped reading its output.
	CloseClientGone
	// CloseContext: the context passed to Serve ended.
	CloseContext
)

func (r CloseReason) String() string {
	switch r {
	case CloseEOF:
		return "eof"
	case CloseClientGone:
		return "client gone"
	case CloseContext:
		return "context done"
	}
	return "unknown"
}

// Implementation identifies the server to the client.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// Result is the outcome of a tool call: one text block.
type Result struct {
	Text    string
	IsError bool
}

// Handler runs a tool. A returned error becomes an isError result carrying
// err.Error() as its text.
type Handler func(ctx context.Context, args json.RawMessage) (Result, error)

// Annotations are the MCP tool behaviour hints.
type Annotations struct {
	ReadOnly    bool `json:"readOnlyHint"`
	Destructive bool `json:"destructiveHint"`
	Idempotent  bool `json:"idempotentHint"`
	OpenWorld   bool `json:"openWorldHint"`
}

// Tool is a callable registered with the server.
type Tool struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations Annotations     `json:"annotations"`
	Handler     Handler         `json:"-"`
	// Control marks a tool whose calls are answered on the reader
	// goroutine, ahead of the queued requests, once initialize has
	// completed (before that they queue like any request). Its Handler
	// must not block: while it runs no input is read, so it must do no
	// I/O, wait on nothing and take no lock that is held across I/O or
	// across another handler. Progress does nothing in it.
	Control bool `json:"-"`
}

// Prompt is a static prompt that expands to one user message.
type Prompt struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Text        string `json:"-"`
}

// Resource is a static text resource.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
	Text        string `json:"-"`
}

// Options configures a Server.
type Options struct {
	Info         Implementation
	Instructions string
	Tools        []Tool
	Prompts      []Prompt
	Resources    []Resource
	// MaxMessageBytes bounds one input line; 0 means 4 MiB.
	MaxMessageBytes int
	// MaxToolErrorBytes bounds the text of a tool error; longer text is cut
	// and ends in "…". 0 means no bound.
	MaxToolErrorBytes int
	// Log receives diagnostics; nil discards them. It must not write to the
	// stream passed to Serve.
	Log *slog.Logger
	// MaxQueued bounds the requests waiting behind the one being handled;
	// one more is answered "server busy" at once. 0 means 8.
	MaxQueued int
	// MaxOutMessages and MaxOutBytes bound the messages waiting to be
	// written. A response that finds the backlog already at a bound means
	// the client stopped reading, so one response of any size fits behind
	// a smaller backlog. A request is started only once the backlog is
	// under half of both. 0 means 64 messages, and 4 × MaxToolErrorBytes
	// (4 MiB when that is 0).
	MaxOutMessages int
	MaxOutBytes    int
	// DrainTimeout bounds how long Serve, once the input or its context
	// ended, waits for the queued output to be written. 0 means 1 s.
	DrainTimeout time.Duration
	// StallTimeout is how long the output may make no progress, with
	// messages waiting, before the client is taken as gone. 0 means 30 s.
	StallTimeout time.Duration
	// OnClose, when set, is called once, on Serve's goroutine, when the
	// session starts to end and before the handlers' contexts are
	// cancelled. It must not block.
	OnClose func(CloseReason)
}

// envelope is the incoming JSON-RPC message. id stays raw so it is echoed
// byte for byte; an absent id (len 0) marks a notification.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	// Result and Error are only present on a client's response to a server
	// request, which this server never makes.
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type outgoing struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
