package perfmcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// testClient drives a Server over in-process pipes.
type testClient struct {
	t      *testing.T
	srv    *Server
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int
	// held are answers recv read while it looked for another id.
	held map[int]rpcResponse
}

// newTestServer starts cfg's server, completes the initialize handshake and
// stops the server when the test ends.
func newTestServer(t *testing.T, cfg Config) *testClient {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := srv.Serve(t.Context(), sr, sw)
		_ = sw.Close()
		done <- err
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	c := &testClient{t: t, srv: srv, w: cw, r: bufio.NewReaderSize(cr, 1<<20), nextID: 1}
	res := c.rpc("initialize", map[string]any{"protocolVersion": "2025-11-25"})
	if res.Error != nil {
		t.Fatalf("initialize: %+v", res.Error)
	}
	return c
}

type rpcResponse struct {
	ID     *int            `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *testClient) rpc(method string, params any) rpcResponse {
	c.t.Helper()
	res, _ := c.recv(c.send(method, params))
	return res
}

// send writes a request without waiting for its answer and returns its
// id; recv reads it.
func (c *testClient) send(method string, params any) int {
	c.t.Helper()
	id := c.nextID
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.nextID++
	if _, err := c.w.Write(append(req, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	return id
}

// sendTool sends a tools/call of tool with args; meta, when not nil, is
// its _meta.
func (c *testClient) sendTool(tool string, args, meta any) int {
	c.t.Helper()
	params := map[string]any{"name": tool, "arguments": args}
	if meta != nil {
		params["_meta"] = meta
	}
	return c.send("tools/call", params)
}

// recv reads lines until the answer to id, and returns it with the
// notifications read on the way. Each line must be one JSON-RPC message;
// answers to other ids are held for their own recv.
func (c *testClient) recv(id int) (rpcResponse, []rpcResponse) {
	c.t.Helper()
	if res, ok := c.held[id]; ok {
		delete(c.held, id)
		return res, nil
	}
	var notes []rpcResponse
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			c.t.Fatalf("read: %v", err)
		}
		var res rpcResponse
		if err := json.Unmarshal(line, &res); err != nil {
			c.t.Fatalf("a line that is not one JSON-RPC message: %s: %v", line, err)
		}
		switch {
		case res.ID == nil:
			notes = append(notes, res)
		case *res.ID == id:
			return res, notes
		default:
			if c.held == nil {
				c.held = map[int]rpcResponse{}
			}
			c.held[*res.ID] = res
		}
	}
}

// toolText is a tools/call answer's text and isError.
func (c *testClient) toolText(res rpcResponse) (text string, isError bool) {
	c.t.Helper()
	if res.Error != nil {
		c.t.Fatalf("tools/call: protocol error %+v", res.Error)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		c.t.Fatalf("decode result %s: %v", res.Result, err)
	}
	if len(out.Content) != 1 || out.Content[0].Type != "text" {
		c.t.Fatalf("want one text block, got %s", res.Result)
	}
	return out.Content[0].Text, out.IsError
}

// call runs a tool and returns its text and isError.
func (c *testClient) call(tool string, args any) (text string, isError bool) {
	c.t.Helper()
	return c.toolText(c.rpc("tools/call", map[string]any{"name": tool, "arguments": args}))
}

// callOK runs a tool that must succeed and decodes its JSON text into v.
func (c *testClient) callOK(tool string, args, v any) string {
	c.t.Helper()
	text, isErr := c.call(tool, args)
	if isErr {
		c.t.Fatalf("%s %v: unexpected tool error: %s", tool, args, text)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(text), v); err != nil {
			c.t.Fatalf("%s: decode %s: %v", tool, text, err)
		}
	}
	return text
}

// writeFixture writes b to dir/name.
func writeFixture(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// testConfig is a config over a fresh profiles root with default limits.
func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{ProfilesDir: t.TempDir(), Limits: DefaultLimits(), Version: "test"}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// must(f())(t) is f's value, failing the test if f returned an error.
func must[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(fmt.Errorf("unexpected error: %w", err))
		}
		return v
	}
}

// testPageURL is the page URL the tests' live servers are configured
// with: nothing serves it.
const testPageURL = "http://127.0.0.1:7341/s/test-token/"

// livePage is the live page engine as the tests configure it.
func livePage() *PageConfig { return &PageConfig{URL: testPageURL} }

// liveURL starts s's live page and returns its URL.
func liveURL(t *testing.T, s *Server) string {
	t.Helper()
	return must(s.ensureLive())(t)
}

// lockedBuffer is a log destination several goroutines may write at once.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// mustBoth is must for a call with two results.
func mustBoth[A, B any](a A, b B, err error) func(*testing.T) (A, B) {
	return func(t *testing.T) (A, B) {
		t.Helper()
		if err != nil {
			t.Fatal(fmt.Errorf("unexpected error: %w", err))
		}
		return a, b
	}
}
