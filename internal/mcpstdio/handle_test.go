package mcpstdio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// handleLine runs one message through Parse and Handle.
func handleLine(t *testing.T, s *Server, ss *Session, line string) []byte {
	t.Helper()
	r, bad := s.Parse([]byte(line))
	if bad != nil {
		return bad
	}
	if r == nil {
		return nil
	}
	return s.Handle(t.Context(), ss, r)
}

func TestHandleAnswersAsServeDoes(t *testing.T) {
	var want [][]byte
	c := newClient(t, goldenOptions())
	for _, line := range goldenScript {
		c.send(line)
		var env struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal([]byte(line), &env)
		if len(env.ID) == 0 && line[0] == '{' && !strings.Contains(line, `"jsonrpc":"1.0"`) {
			continue
		}
		want = append(want, bytes.TrimSuffix(c.recvRaw(), []byte("\n")))
	}
	s := New(goldenOptions())
	ss := NewSession()
	var got [][]byte
	for _, line := range goldenScript {
		if out := handleLine(t, s, ss, line); out != nil {
			got = append(got, out)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("%d answers, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("answer %d:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

func TestHandleRefusesBeforeInitialize(t *testing.T) {
	s := New(testOptions())
	ss := NewSession()
	if out := handleLine(t, s, ss, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`); !strings.Contains(string(out), "send initialize first") {
		t.Fatalf("got %s", out)
	}
	if out := handleLine(t, s, ss, `{"jsonrpc":"2.0","id":2,"method":"ping"}`); !strings.Contains(string(out), `"result":{}`) {
		t.Fatalf("got %s", out)
	}
	if ss.Initialized() {
		t.Fatal("initialized before initialize")
	}
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":3,"method":"initialize"}`)
	if !ss.Initialized() {
		t.Fatal("not initialized after initialize")
	}
}

func TestInitializeRecordsTheClientLabel(t *testing.T) {
	cases := []struct{ name, params, label, version string }{
		{"plain", `{"protocolVersion":"2025-06-18","clientInfo":{"name":"claude-code"}}`, "claude-code", "2025-06-18"},
		{"64 bytes", `{"clientInfo":{"name":"` + strings.Repeat("a", 64) + `"}}`, strings.Repeat("a", 64), LatestVersion},
		{"65 bytes", `{"clientInfo":{"name":"` + strings.Repeat("a", 65) + `"}}`, "client", LatestVersion},
		{"newline", `{"clientInfo":{"name":"a\nb"}}`, "client", LatestVersion},
		{"non-ascii", `{"clientInfo":{"name":"é"}}`, "client", LatestVersion},
		{"absent", `{}`, "client", LatestVersion},
		{"unknown version", `{"protocolVersion":"1999-01-01"}`, "client", LatestVersion},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(testOptions())
			ss := NewSession()
			handleLine(t, s, ss, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":`+c.params+`}`)
			if ss.Label() != c.label || ss.Version() != c.version {
				t.Fatalf("label %q version %q, want %q %q", ss.Label(), ss.Version(), c.label, c.version)
			}
		})
	}
	if NewSession().Label() != "" || NewSession().Version() != "" {
		t.Fatal("a fresh session has a label or version")
	}
}

func TestClientLabelReachesTheHandler(t *testing.T) {
	var seen string
	o := testOptions()
	o.Tools = []Tool{{Name: "who", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		seen = ClientLabel(ctx)
		return Result{Text: "ok"}, nil
	}}}
	s := New(o)
	ss := NewSession()
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"zed"}}}`)
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"who"}}`)
	if seen != "zed" {
		t.Fatalf("label %q", seen)
	}
	if got := ClientLabel(t.Context()); got != "" {
		t.Fatalf("outside a session: %q", got)
	}
}

func TestWithReceivedIsWhatReceivedReads(t *testing.T) {
	if _, ok := Received(t.Context()); ok {
		t.Fatal("Received ok on a bare context")
	}
	at := time.Unix(1700000000, 0)
	got, ok := Received(WithReceived(t.Context(), at))
	if !ok || !got.Equal(at) {
		t.Fatalf("got %v %v", got, ok)
	}
}

func TestADynamicResourceIsReadEachTime(t *testing.T) {
	var n int
	var ctxLabel string
	fail := false
	o := testOptions()
	o.Resources = []Resource{{URI: "mem://dyn", Name: "dyn", MIMEType: "text/plain", Snapshot: true,
		Text: "static",
		Read: func(ctx context.Context) (string, error) {
			n++
			ctxLabel = ClientLabel(ctx)
			if fail {
				return "", errors.New("the page is gone")
			}
			return fmt.Sprintf("read %d", n), nil
		}}}
	s := New(o)
	ss := NewSession()
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"zed"}}}`)
	read := `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"mem://dyn"}}`
	if out := string(handleLine(t, s, ss, read)); !strings.Contains(out, `"text":"read 1"`) {
		t.Fatal(out)
	}
	if out := string(handleLine(t, s, ss, read)); !strings.Contains(out, `"text":"read 2"`) {
		t.Fatal(out)
	}
	if ctxLabel != "zed" {
		t.Fatalf("label %q", ctxLabel)
	}
	fail = true
	out := string(handleLine(t, s, ss, read))
	if !strings.Contains(out, `"code":-32603`) || !strings.Contains(out, "the page is gone") {
		t.Fatal(out)
	}
	if strings.Contains(string(handleLine(t, s, ss, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)), "Snapshot") {
		t.Fatal("Snapshot is listed")
	}
	if r, ok := s.Resource("mem://dyn"); !ok || !r.Snapshot {
		t.Fatal("Resource lookup")
	}
	if _, ok := s.Resource("mem://none"); ok {
		t.Fatal("Resource found a stranger")
	}
}

func TestAResourceReadThatPanicsOrOverrunsIsBounded(t *testing.T) {
	o := testOptions()
	o.MaxToolErrorBytes = 20
	o.Resources = []Resource{
		{URI: "mem://boom", Name: "b", Read: func(context.Context) (string, error) { panic("secret /home/x") }},
		{URI: "mem://long", Name: "l", Read: func(context.Context) (string, error) {
			return "", errors.New(strings.Repeat("e", 100))
		}},
	}
	s := New(o)
	ss := NewSession()
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":0,"method":"initialize"}`)
	out := string(handleLine(t, s, ss, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"mem://boom"}}`))
	if !strings.Contains(out, `"code":-32603`) || !strings.Contains(out, `internal error: resource \"mem://boom\" failed`) || strings.Contains(out, "secret") {
		t.Fatal(out)
	}
	out = string(handleLine(t, s, ss, `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"mem://long"}}`))
	if !strings.Contains(out, `"code":-32603`) || strings.Contains(out, strings.Repeat("e", 21)) || !strings.Contains(out, "…") {
		t.Fatal(out)
	}
}

func TestToolLookup(t *testing.T) {
	o := testOptions()
	o.Tools[0].Snapshot = true
	s := New(o)
	if tl, ok := s.Tool("echo"); !ok || !tl.Snapshot {
		t.Fatal("Tool lookup")
	}
	if _, ok := s.Tool("zzz"); ok {
		t.Fatal("Tool found a stranger")
	}
}

func TestHandleSendsNoProgress(t *testing.T) {
	var sent = true
	o := testOptions()
	o.Tools = []Tool{{Name: "p", Handler: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		sent = Progress(ctx, 1, 2, "m")
		return Result{Text: "ok"}, nil
	}}}
	s := New(o)
	ss := NewSession()
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	handleLine(t, s, ss, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"p","_meta":{"progressToken":"t"}}}`)
	if sent {
		t.Fatal("Progress returned true through Handle")
	}
}

func TestParseRefusesWhatServeRefuses(t *testing.T) {
	lines := []string{
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`7`,
		`{not json`,
		`{"jsonrpc":"1.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"` + strings.Repeat("x", 1025) + `","method":"ping"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1}`,
	}
	c := newClient(t, testOptions())
	s := New(testOptions())
	for _, line := range lines {
		c.send(line)
		want := bytes.TrimSuffix(c.recvRaw(), []byte("\n"))
		r, bad := s.Parse([]byte(line))
		if r != nil || !bytes.Equal(bad, want) {
			t.Errorf("%.30s: got %s (%v), want %s", line, bad, r, want)
		}
	}
	r, bad := s.Parse([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if bad != nil || r == nil || !r.Notification() || r.Method != "notifications/initialized" {
		t.Fatalf("notification: %v %s", r, bad)
	}
	r, _ = s.Parse([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo"}}`))
	if r == nil || r.Tool != "echo" || r.Notification() || string(r.ID) != "4" {
		t.Fatalf("tools/call: %+v", r)
	}
	r, _ = s.Parse([]byte(`{"jsonrpc":"2.0","id":5,"method":"resources/read","params":{"uri":"mem://r"}}`))
	if r == nil || r.URI != "mem://r" {
		t.Fatalf("resources/read: %+v", r)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestFlushWarningsLogsTheCounts(t *testing.T) {
	var buf syncBuf
	o := testOptions()
	o.Log = slog.New(slog.NewTextHandler(&buf, nil))
	s := New(o)
	for range 3 {
		if r, bad := s.Parse([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)); r != nil || bad != nil {
			t.Fatal("a client response was not dropped")
		}
	}
	s.FlushWarnings()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "warnings not logged") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	out := buf.String()
	if strings.Count(out, "warnings not logged") != 1 || !strings.Contains(out, "=2") {
		t.Fatalf("log: %s", out)
	}
}
