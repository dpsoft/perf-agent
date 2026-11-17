package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcphttp"
)

// The clients of the tests: an MCP client's calls may take a while; a
// page's reads should not.
var (
	mcpHTTP  = &http.Client{Timeout: 60 * time.Second}
	pageHTTP = &http.Client{Timeout: 30 * time.Second}
)

// protocolVersion is the MCP revision the test client speaks.
const protocolVersion = "2025-11-25"

// httpClient is a hand-written MCP client of the hub over Streamable
// HTTP, as Claude Code and Codex send it: POST /mcp with a bearer token,
// JSON in and out, and DELETE /mcp to end its session. It needs no
// dependency beyond the standard library.
type httpClient struct {
	t       testing.TB
	origin  string // http://127.0.0.1:PORT
	url     string // origin + /mcp
	bearer  string
	session string
	next    atomic.Int64
}

func newHTTPClient(t testing.TB, port int, bearer string) *httpClient {
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	return &httpClient{t: t, origin: origin, url: origin + "/mcp", bearer: bearer}
}

// answer is one HTTP answer of /mcp.
type answer struct {
	status int
	header http.Header
	body   []byte
}

// rpcAnswer is a JSON-RPC answer.
type rpcAnswer struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do sends one request to /mcp with the client's headers and returns the
// answer, without failing the test: it may run on any goroutine.
func (c *httpClient) do(method string, body []byte) (answer, error) {
	req, err := http.NewRequest(method, c.url, bytes.NewReader(body))
	if err != nil {
		return answer{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	req.Header.Set("Authorization", "Bearer "+c.bearer)
	if c.session != "" {
		req.Header.Set(mcphttp.HeaderSession, c.session)
		req.Header.Set(mcphttp.HeaderVersion, protocolVersion)
	}
	res, err := mcpHTTP.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return answer{}, err
	}
	return answer{status: res.StatusCode, header: res.Header, body: b}, nil
}

// send posts one message, a notification when id is "", without failing
// the test: it may run on any goroutine.
func (c *httpClient) send(id, method string, params any) (answer, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != "" {
		msg["id"] = json.RawMessage(id)
	}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return answer{}, err
	}
	return c.do(http.MethodPost, b)
}

// post is send that fails the test on a transport error.
func (c *httpClient) post(id, method string, params any) (int, http.Header, []byte) {
	c.t.Helper()
	a, err := c.send(id, method, params)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return a.status, a.header, a.body
}

// decode reads a 200 answer to the request with id.
func decode(a answer, id string) (rpcAnswer, error) {
	var r rpcAnswer
	if a.status != http.StatusOK {
		return r, fmt.Errorf("status %d: %s", a.status, a.body)
	}
	if err := json.Unmarshal(a.body, &r); err != nil {
		return r, fmt.Errorf("%s: %v", a.body, err)
	}
	if string(r.ID) != id {
		return r, fmt.Errorf("answer for id %s to request %s", r.ID, id)
	}
	return r, nil
}

// rpc sends a request and returns its answer, which must be a 200 for
// its id.
func (c *httpClient) rpc(method string, params any) rpcAnswer {
	c.t.Helper()
	id := strconv.FormatInt(c.next.Add(1), 10)
	r, err := decode(must(c.send(id, method, params))(c.t), id)
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	return r
}

// must is v, failing t on err.
func must[T any](v T, err error) func(testing.TB) T {
	return func(t testing.TB) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// initialize opens a session labelled label, and tells the server it is
// initialized.
func (c *httpClient) initialize(label string) {
	c.t.Helper()
	a := must(c.send("0", "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": label, "version": "1"},
	}))(c.t)
	if _, err := decode(a, "0"); err != nil {
		c.t.Fatalf("initialize: %v", err)
	}
	c.session = a.header.Get(mcphttp.HeaderSession)
	if c.session == "" {
		c.t.Fatal("initialize answered no session")
	}
	c.notify("notifications/initialized", nil)
}

// notify sends a notification, which must be accepted with an empty 202.
func (c *httpClient) notify(method string, params any) {
	c.t.Helper()
	if status, _, body := c.post("", method, params); status != http.StatusAccepted || len(body) != 0 {
		c.t.Fatalf("%s: %d %q, want an empty 202", method, status, body)
	}
}

// delete ends the client's session, which must answer 204, and forgets
// it.
func (c *httpClient) delete() {
	c.t.Helper()
	a, err := c.do(http.MethodDelete, nil)
	if err != nil {
		c.t.Fatalf("DELETE: %v", err)
	}
	if a.status != http.StatusNoContent {
		c.t.Fatalf("DELETE: %d %s, want 204", a.status, a.body)
	}
	c.session = ""
}

// get is a plain GET of path on the hub's origin, such as a page route,
// and returns the status and body.
func (c *httpClient) get(path string) (int, []byte) {
	c.t.Helper()
	res, err := pageHTTP.Get(c.origin + path)
	if err != nil {
		c.t.Fatalf("GET %s: %v", redact(path), err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		c.t.Fatalf("GET %s: %v", redact(path), err)
	}
	return res.StatusCode, b
}

// redact drops the page token from a page path or URL for a failure
// message.
func redact(u string) string {
	before, after, ok := strings.Cut(u, "/s/")
	if !ok {
		return u
	}
	_, rest, _ := strings.Cut(after, "/")
	return before + "/s/<token>/" + rest
}

// toolResult is a tools/call result.
type toolResult struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func (r rpcAnswer) tool() (string, bool, error) {
	if r.Error != nil {
		return "", false, fmt.Errorf("error %d %s", r.Error.Code, r.Error.Message)
	}
	var res toolResult
	if err := json.Unmarshal(r.Result, &res); err != nil || len(res.Content) != 1 {
		return "", false, fmt.Errorf("result %s (%v)", r.Result, err)
	}
	return res.Content[0].Text, res.IsError, nil
}

// try calls a tool and returns its text and isError.
func (c *httpClient) try(name string, args any) (string, bool) {
	c.t.Helper()
	text, isErr, err := c.rpc("tools/call", map[string]any{"name": name, "arguments": args}).tool()
	if err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	return text, isErr
}

// call calls a tool that must succeed, and returns its text.
func (c *httpClient) call(name string, args any) string {
	c.t.Helper()
	text, isErr := c.try(name, args)
	if isErr {
		c.t.Fatalf("%s: %s", name, text)
	}
	return text
}

// opened is a profile's handle at its default view.
type opened struct{ handle, view string }

func (c *httpClient) open(path string) opened {
	c.t.Helper()
	var out struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(c.call("open", map[string]any{"path": path})), &out); err != nil {
		c.t.Fatal(err)
	}
	return opened{out.Handle, out.DefaultView}
}

// publish publishes o's facts on the page and returns the publication.
func (c *httpClient) publish(o opened) string {
	c.t.Helper()
	var out struct{ Publication, URL string }
	text := c.call("page_explain", map[string]any{"handle": o.handle, "view": o.view, "explanations": []any{}})
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Publication == "" {
		c.t.Fatalf("page_explain: %s (%v)", text, err)
	}
	return out.Publication
}

// nodeID is the ID of o's first node named name.
func (c *httpClient) nodeID(o opened, name string) string {
	c.t.Helper()
	var out struct {
		Rows []struct{ Node, Name string } `json:"rows"`
	}
	text := c.call("top", map[string]any{"handle": o.handle, "view": o.view, "rows": "nodes", "limit": 200})
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		c.t.Fatal(err)
	}
	for _, r := range out.Rows {
		if r.Name == name {
			return r.Node
		}
	}
	c.t.Fatalf("no node %q in %s", name, text)
	return ""
}
