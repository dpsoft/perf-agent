// Package mcpstdio is a minimal Model Context Protocol server over
// newline-delimited JSON-RPC 2.0 on a reader and a writer (stdio). It knows
// nothing about the application: callers register tools, prompts and static
// resources. It opens no network connection.
package mcpstdio

import (
	"context"
	"encoding/json"
	"log/slog"
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
)

// defaultMaxMessageBytes bounds one input line when Options.MaxMessageBytes is 0.
const defaultMaxMessageBytes = 4 << 20

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
	// Log receives diagnostics; nil discards them. It must not write to the
	// stream passed to Serve.
	Log *slog.Logger
}

// envelope is the incoming JSON-RPC message. id stays raw so it is echoed
// byte for byte; an absent id (len 0) marks a notification.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
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
