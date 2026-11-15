package perfmcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testClient drives a Server over in-process pipes.
type testClient struct {
	t      *testing.T
	srv    *Server
	w      *io.PipeWriter
	r      *bufio.Reader
	nextID int
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
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c *testClient) rpc(method string, params any) rpcResponse {
	c.t.Helper()
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.nextID++
	if _, err := c.w.Write(append(req, '\n')); err != nil {
		c.t.Fatalf("write: %v", err)
	}
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var res rpcResponse
	if err := json.Unmarshal(line, &res); err != nil {
		c.t.Fatalf("decode %s: %v", line, err)
	}
	return res
}

// call runs a tool and returns its text and isError.
func (c *testClient) call(tool string, args any) (text string, isError bool) {
	c.t.Helper()
	res := c.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	if res.Error != nil {
		c.t.Fatalf("tools/call %s: protocol error %+v", tool, res.Error)
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

// liveURL starts s's live page and returns its URL.
func liveURL(t *testing.T, s *Server) string {
	t.Helper()
	return must(s.ensureLive())(t)
}

// httpGet requests url from a live page with the headers hdr ("Host"
// sets the request's Host) and returns the response with its body read.
func httpGet(t *testing.T, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	return httpDo(t, http.MethodGet, url, hdr)
}

// httpDo is httpGet with another method. It follows no redirect and keeps
// no connection open. Any goroutine may call it: an error is reported with
// Errorf, and the response is then empty, with status 0.
func httpDo(t *testing.T, method, url string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Errorf("%s %s: %v", method, url, err)
		return &http.Response{Header: http.Header{}}, nil
	}
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		t.Errorf("%s %s: %v", method, url, err)
		return &http.Response{Header: http.Header{}}, nil
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Errorf("%s %s: reading the body: %v", method, url, err)
	}
	return res, body
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
