package mcpstdio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type client struct {
	t     *testing.T
	w     *io.PipeWriter
	r     *bufio.Reader
	lines [][]byte
	done  chan error
}

func testOptions() Options {
	return Options{
		Info:         Implementation{Name: "test-server", Title: "Test", Version: "0.0.1"},
		Instructions: "be nice",
		Tools: []Tool{
			{
				Name: "echo", Title: "Echo", Description: "echoes",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Annotations: Annotations{ReadOnly: true, Idempotent: true},
				Handler: func(_ context.Context, args json.RawMessage) (Result, error) {
					return Result{Text: string(args)}, nil
				},
			},
			{
				Name: "fail", Description: "fails",
				Handler: func(context.Context, json.RawMessage) (Result, error) {
					return Result{}, errors.New("call open first")
				},
			},
			{
				Name: "boom", Description: "panics",
				Handler: func(context.Context, json.RawMessage) (Result, error) {
					panic("kaboom")
				},
			},
		},
		Prompts:   []Prompt{{Name: "p", Description: "d", Text: "hello"}},
		Resources: []Resource{{URI: "mem://r", Name: "r", MIMEType: "text/plain", Text: "body"}},
	}
}

func newClient(t *testing.T, o Options) *client {
	t.Helper()
	sr, cw := io.Pipe()
	cr, sw := io.Pipe()
	c := &client{t: t, w: cw, r: bufio.NewReaderSize(cr, 1<<20), done: make(chan error, 1)}
	s := New(o)
	go func() {
		err := s.Serve(t.Context(), sr, sw)
		_ = sw.Close()
		c.done <- err
	}()
	t.Cleanup(func() {
		_ = cw.Close()
		<-c.done
	})
	return c
}

func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.w, line+"\n"); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *client) recvRaw() []byte {
	c.t.Helper()
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	c.lines = append(c.lines, line)
	return line
}

func (c *client) recv() response {
	c.t.Helper()
	var r response
	if err := json.Unmarshal(c.recvRaw(), &r); err != nil {
		c.t.Fatalf("decode: %v", err)
	}
	return r
}

func (c *client) call(line string) response {
	c.t.Helper()
	c.send(line)
	return c.recv()
}

func (c *client) init() {
	c.t.Helper()
	r := c.call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if r.Error != nil {
		c.t.Fatalf("initialize: %+v", r.Error)
	}
}

func wantErr(t *testing.T, r response, code int) {
	t.Helper()
	if r.Error == nil || r.Error.Code != code {
		t.Fatalf("want error %d, got %+v result=%s", code, r.Error, r.Result)
	}
}

func initVersion(t *testing.T, c *client, v string) (string, response) {
	t.Helper()
	r := c.call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + v + `"}}`)
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatalf("result: %v (%s)", err, r.Result)
	}
	return res.ProtocolVersion, r
}

func TestInitializeEchoesASupportedVersion(t *testing.T) {
	c := newClient(t, testOptions())
	v, r := initVersion(t, c, "2025-06-18")
	if v != "2025-06-18" {
		t.Fatalf("version = %q", v)
	}
	var res struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
		ServerInfo   struct{ Name string }      `json:"serverInfo"`
		Instructions string                     `json:"instructions"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tools", "prompts", "resources"} {
		if _, ok := res.Capabilities[k]; !ok {
			t.Errorf("capability %s missing", k)
		}
	}
	if string(res.Capabilities["tools"]) != `{"listChanged":false}` {
		t.Errorf("tools capability = %s", res.Capabilities["tools"])
	}
	if res.ServerInfo.Name != "test-server" || res.Instructions != "be nice" {
		t.Errorf("serverInfo/instructions = %+v", res)
	}
}

func TestInitializeAnswersLatestForOtherVersions(t *testing.T) {
	for _, in := range []string{"2024-11-05", "2026-07-28"} {
		c := newClient(t, testOptions())
		if v, _ := initVersion(t, c, in); v != LatestVersion {
			t.Errorf("%s -> %q", in, v)
		}
	}
}

func TestServerDiscoverIsMethodNotFoundSoClientsFallBack(t *testing.T) {
	c := newClient(t, testOptions())
	wantErr(t, c.call(`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`), -32601)
	if v, _ := initVersion(t, c, "2025-11-25"); v != "2025-11-25" {
		t.Fatalf("version = %q", v)
	}
	r := c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if r.Error != nil {
		t.Fatalf("tools/list: %+v", r.Error)
	}
}

func TestRequestsBeforeInitializeAreRefused(t *testing.T) {
	c := newClient(t, testOptions())
	r := c.call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	wantErr(t, r, -32600)
	if !strings.Contains(r.Error.Message, "send initialize first") {
		t.Errorf("message = %q", r.Error.Message)
	}
	r = c.call(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if r.Error != nil || string(r.Result) != "{}" {
		t.Fatalf("ping = %+v %s", r.Error, r.Result)
	}
}

func TestToolsListIsDeterministic(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	a := c.recvRaw()
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	b := c.recvRaw()
	if !bytes.Equal(a, b) {
		t.Fatalf("lists differ:\n%s\n%s", a, b)
	}
	var r struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
				Annotations map[string]bool `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(a, &r); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range r.Result.Tools {
		names = append(names, tl.Name)
		if len(tl.InputSchema) == 0 {
			t.Errorf("%s has no inputSchema", tl.Name)
		}
	}
	if strings.Join(names, ",") != "echo,fail,boom" {
		t.Fatalf("order = %v", names)
	}
	an := r.Result.Tools[0].Annotations
	if !an["readOnlyHint"] || !an["idempotentHint"] || an["destructiveHint"] {
		t.Errorf("annotations = %v", an)
	}
	for _, k := range []string{"readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"} {
		if _, ok := an[k]; !ok {
			t.Errorf("annotation %s absent", k)
		}
	}
}

func TestToolErrorsAreResultsWithIsError(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	r := c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fail","arguments":{}}}`)
	if r.Error != nil {
		t.Fatalf("protocol error: %+v", r.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
		Content []struct{ Type, Text string }
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) != 1 || res.Content[0].Text != "call open first" || res.Content[0].Type != "text" {
		t.Fatalf("result = %+v", res)
	}
	r = c.call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"a":1}}}`)
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Content[0].Text != `{"a":1}` {
		t.Fatalf("echo = %+v", res)
	}
}

func TestUnknownToolIsInvalidParams(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	wantErr(t, c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope"}}`), -32602)
	wantErr(t, c.call(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"nope"}}`), -32602)
	wantErr(t, c.call(`{"jsonrpc":"2.0","id":4,"method":"resources/read","params":{"uri":"mem://nope"}}`), -32602)
}

func TestPromptsAndResources(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	r := c.call(`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"p"}}`)
	if !strings.Contains(string(r.Result), `"role":"user"`) || !strings.Contains(string(r.Result), `hello`) {
		t.Errorf("prompt = %s", r.Result)
	}
	r = c.call(`{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"mem://r"}}`)
	if !strings.Contains(string(r.Result), `"text":"body"`) {
		t.Errorf("resource = %s", r.Result)
	}
	for i, m := range []string{"prompts/list", "resources/list"} {
		r = c.call(`{"jsonrpc":"2.0","id":` + string(rune('5'+i)) + `,"method":"` + m + `"}`)
		if r.Error != nil {
			t.Errorf("%s: %+v", m, r.Error)
		}
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	r := c.call(`{"jsonrpc":"2.0","id":77,"method":"ping"}`)
	if string(r.ID) != "77" || r.Error != nil {
		t.Fatalf("next line was %+v", r)
	}
}

func TestMalformedJSONIsAParseErrorAndTheServerGoesOn(t *testing.T) {
	c := newClient(t, testOptions())
	r := c.call(`{oops`)
	wantErr(t, r, -32700)
	if string(r.ID) != "null" {
		t.Errorf("id = %s", r.ID)
	}
	if r := c.call(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); r.Error != nil {
		t.Fatalf("ping: %+v", r.Error)
	}
}

func TestOversizedMessageIsRefusedAndTheServerGoesOn(t *testing.T) {
	o := testOptions()
	o.MaxMessageBytes = 1024
	c := newClient(t, o)
	pad := strings.Repeat("x", 4096)
	r := c.call(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + pad + `"}}`)
	wantErr(t, r, -32600)
	if !strings.Contains(r.Error.Message, "MaxMessageBytes") || !strings.Contains(r.Error.Message, "1024") {
		t.Errorf("message = %q", r.Error.Message)
	}
	if r := c.call(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); r.Error != nil || string(r.ID) != "2" {
		t.Fatalf("ping: %+v", r)
	}
}

func TestBatchIsRefused(t *testing.T) {
	c := newClient(t, testOptions())
	r := c.call(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
	wantErr(t, r, -32600)
	if !strings.Contains(r.Error.Message, "batches are not supported") {
		t.Errorf("message = %q", r.Error.Message)
	}
}

func TestIDsAreEchoedVerbatim(t *testing.T) {
	c := newClient(t, testOptions())
	for _, id := range []string{`"abc"`, `7`, `9007199254740993`} {
		raw := func() []byte {
			c.send(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
			return c.recvRaw()
		}()
		if !bytes.Contains(raw, []byte(`"id":`+id+`,`)) && !bytes.Contains(raw, []byte(`"id":`+id+`}`)) {
			t.Errorf("id %s not echoed in %s", id, raw)
		}
	}
}

func TestAPanickingHandlerIsAnInternalError(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	wantErr(t, c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"boom"}}`), -32603)
	if r := c.call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo"}}`); r.Error != nil {
		t.Fatalf("after panic: %+v", r.Error)
	}
}

func TestEveryOutputLineIsJSONRPC(t *testing.T) {
	o := testOptions()
	o.MaxMessageBytes = 2048
	c := newClient(t, o)
	c.call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	c.init()
	c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"s":"a\nb<>&"}}}`)
	c.call(`{oops`)
	c.call(`[]`)
	c.call(`{"jsonrpc":"2.0","id":3,"method":"nope"}`)
	c.call(`{"jsonrpc":"2.0","id":4,"method":"ping","params":"` + strings.Repeat("y", 4096) + `"}`)
	if len(c.lines) < 6 {
		t.Fatalf("only %d lines", len(c.lines))
	}
	for _, l := range c.lines {
		if !bytes.HasSuffix(l, []byte("\n")) || bytes.Contains(l[:len(l)-1], []byte("\n")) {
			t.Errorf("bad framing: %q", l)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(l, &m); err != nil || string(m["jsonrpc"]) != `"2.0"` {
			t.Errorf("not JSON-RPC: %q (%v)", l, err)
		}
		if bytes.ContainsAny(l[:len(l)-1], " \t") && bytes.Contains(l, []byte(`": `)) {
			t.Errorf("not compact: %q", l)
		}
	}
}

func TestServeReturnsAtEOF(t *testing.T) {
	c := newClient(t, testOptions())
	_ = c.w.Close()
	if err := <-c.done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
	c.done <- nil // Cleanup waits on done
}

func TestServeReturnsWhenContextEnds(t *testing.T) {
	sr, cw := io.Pipe()
	defer func() { _ = cw.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- New(testOptions()).Serve(ctx, sr, io.Discard) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v", err)
	}
}

func TestInvalidIDsAreRefused(t *testing.T) {
	c := newClient(t, testOptions())
	for _, id := range []string{`null`, `{"a":1}`, `1.5`, `1e3`, `true`, `[1]`} {
		r := c.call(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
		wantErr(t, r, -32600)
		if string(r.ID) != "null" || !strings.Contains(r.Error.Message, "id must be a string or integer") {
			t.Errorf("id %s: %+v id=%s", id, r.Error, r.ID)
		}
	}
	if r := c.call(`{"jsonrpc":"2.0","id":-5,"method":"ping"}`); r.Error != nil || string(r.ID) != "-5" {
		t.Fatalf("negative integer id: %+v", r)
	}
}

func TestMalformedEnvelopesAreInvalidRequests(t *testing.T) {
	c := newClient(t, testOptions())
	for _, line := range []string{
		`42`, `"str"`, `null`, `true`,
		`{}`,
		`{"id":1,"method":"ping"}`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1}`,
		`{"jsonrpc":"2.0","id":1,"method":7}`,
	} {
		r := c.call(line)
		wantErr(t, r, -32600)
	}
	if r := c.call(`{}`); string(r.ID) != "null" {
		t.Errorf("id = %s", r.ID)
	}
	if r := c.call(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); r.Error != nil {
		t.Fatalf("ping: %+v", r.Error)
	}
}

func TestClientResponsesAreDroppedSilently(t *testing.T) {
	c := newClient(t, testOptions())
	c.send(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"error":{"code":-1,"message":"x"}}`)
	r := c.call(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if string(r.ID) != "3" || r.Error != nil {
		t.Fatalf("next line was %+v", r)
	}
}

func TestResultsAreNotHTMLEscaped(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"t":"<b>&"}}}`)
	if raw := c.recvRaw(); !bytes.Contains(raw, []byte(`<b>&`)) || bytes.Contains(raw, []byte(`\u003c`)) {
		t.Fatalf("escaped: %s", raw)
	}
}

func TestLineLargerThanTheReadBufferIsDiscarded(t *testing.T) {
	o := testOptions()
	o.MaxMessageBytes = 1024
	c := newClient(t, o)
	pad := strings.Repeat("z", 1<<20)
	r := c.call(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + pad + `"}}`)
	wantErr(t, r, -32600)
	if r := c.call(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); r.Error != nil || string(r.ID) != "2" {
		t.Fatalf("ping: %+v", r)
	}
}

func TestEmptyOptionalFieldsAreOmitted(t *testing.T) {
	o := testOptions()
	o.Instructions = ""
	o.Resources = []Resource{{URI: "mem://n", Name: "n", Text: "t"}}
	c := newClient(t, o)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if raw := c.recvRaw(); bytes.Contains(raw, []byte("instructions")) {
		t.Errorf("instructions present: %s", raw)
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"mem://n"}}`)
	if raw := c.recvRaw(); bytes.Contains(raw, []byte("mimeType")) {
		t.Errorf("mimeType present: %s", raw)
	}
}

func TestMalformedArrayIsAParseErrorAndEmptyArrayIsABatch(t *testing.T) {
	c := newClient(t, testOptions())
	r := c.call(`[oops`)
	wantErr(t, r, -32700)
	if string(r.ID) != "null" {
		t.Errorf("id = %s", r.ID)
	}
	r = c.call(`[]`)
	wantErr(t, r, -32600)
	if !strings.Contains(r.Error.Message, "batches are not supported") {
		t.Errorf("message = %q", r.Error.Message)
	}
	if r := c.call(`{"jsonrpc":"2.0","id":2,"method":"ping"}`); r.Error != nil {
		t.Fatalf("ping: %+v", r.Error)
	}
}

func TestToolErrorTextAndUnknownNamesAreBounded(t *testing.T) {
	o := testOptions()
	o.MaxToolErrorBytes = 100
	o.Tools = append(o.Tools, Tool{Name: "long", Description: "fails at length",
		Handler: func(context.Context, json.RawMessage) (Result, error) {
			return Result{}, errors.New(strings.Repeat("é", 5000))
		}})
	c := newClient(t, o)
	c.init()
	r := c.call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"long","arguments":{}}}`)
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result %s: %v", r.Result, err)
	}
	if text := res.Content[0].Text; len(text) > 100 || !strings.HasSuffix(text, "…") || !utf8.ValidString(text) {
		t.Errorf("error text is %d bytes: %q", len(text), text)
	}

	huge := strings.Repeat("z", 100000)
	for _, req := range []string{
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + huge + `"}}`,
		`{"jsonrpc":"2.0","id":4,"method":"prompts/get","params":{"name":"` + huge + `"}}`,
		`{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"` + huge + `"}}`,
	} {
		r := c.call(req)
		if r.Error == nil || len(r.Error.Message) > 200 {
			t.Errorf("%.60s: error %d bytes", req, len(r.Error.Message))
		}
	}
}

func TestAnUnknownMethodIsEchoedBounded(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	r := c.call(`{"jsonrpc":"2.0","id":2,"method":"` + strings.Repeat("m", 100000) + `"}`)
	if r.Error == nil || r.Error.Code != -32601 || len(r.Error.Message) > 200 || !strings.Contains(r.Error.Message, "mmmm") {
		t.Errorf("error = %.300v", r.Error)
	}
}
