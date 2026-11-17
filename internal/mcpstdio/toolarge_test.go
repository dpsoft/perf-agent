package mcpstdio

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// A result of MaxResultBytes control bytes escapes six-fold (\x01 is
// \u0001), past half of MaxOutBytes: the client gets a small isError
// answer for the same id, and the session goes on.
func TestAnAnswerOverHalfTheOutputBoundIsRefused(t *testing.T) {
	o := withTools(controlTool())
	o.MaxToolErrorBytes = 64 << 10
	n := New(o).MaxResultBytes()
	for _, tc := range []struct{ name, text string }{
		{"control", strings.Repeat("\x01", n)},
		{"invalid", strings.Repeat("\xff", n)},
		{"quotes", strings.Repeat(`"`, n)},
	} {
		o.Tools = append(o.Tools, Tool{Name: tc.name, Handler: func(context.Context, json.RawMessage) (Result, error) {
			return Result{Text: tc.text}, nil
		}})
	}
	closes := newCloseLog(&o)
	c := newClient(t, o)
	c.init()
	for i, name := range []string{"control", "invalid"} {
		r := c.call(callLine(2+i, name, "{}"))
		wantID(t, r, []string{"2", "3"}[i])
		text, isErr := toolText(t, r)
		if !isErr || text != "result too large for the output bound" {
			t.Fatalf("%s: a %d-byte line answered isError=%v %.80q", name, len(c.lines[len(c.lines)-1]), isErr, text)
		}
	}
	r := c.call(callLine(4, "quotes", "{}"))
	if text, isErr := toolText(t, r); isErr || len(text) != n {
		t.Fatalf("a %d-byte result of quotes was not delivered (isError=%v, %d bytes)", n, isErr, len(text))
	}
	r = c.call(callLine(5, "ctl", "{}"))
	wantID(t, r, "5")
	if text, isErr := toolText(t, r); text != "ctl" || isErr {
		t.Fatalf("ctl = %q isError=%v", text, isErr)
	}
	r = c.call(`{"jsonrpc":"2.0","id":6,"method":"ping"}`)
	if r.Error != nil {
		t.Fatalf("ping after the refused answers: %+v", r.Error)
	}
	if got := closes.reasons(); len(got) != 0 {
		t.Fatalf("OnClose = %v while the client is alive", got)
	}
}

// A Control tool's answer is bounded the same way on the reader.
func TestAControlAnswerOverHalfTheOutputBoundIsRefused(t *testing.T) {
	o := testOptions()
	o.MaxToolErrorBytes = 64 << 10
	n := New(o).MaxResultBytes()
	o.Tools = append(o.Tools, Tool{Name: "bigctl", Control: true, Handler: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Text: strings.Repeat("\x01", n)}, nil
	}})
	c := newClient(t, o)
	c.init()
	r := c.call(callLine(2, "bigctl", "{}"))
	wantID(t, r, "2")
	if text, isErr := toolText(t, r); !isErr || text != "result too large for the output bound" {
		t.Fatalf("isError=%v %.80q", isErr, text)
	}
	r = c.call(callLine(3, "echo", `{"a":1}`))
	if text, isErr := toolText(t, r); isErr || text != `{"a":1}` {
		t.Fatalf("echo after the refused answer = %q isError=%v", text, isErr)
	}
}

// An id over 1 KiB is refused at ingress, with a null id, so no answer
// echoes it: the answer that replaces a too-large one stays small.
func TestALongIDIsRefused(t *testing.T) {
	c := newClient(t, testOptions())
	c.init()
	long := `"` + strings.Repeat("i", maxIDBytes) + `"`
	r := c.call(`{"jsonrpc":"2.0","id":` + long + `,"method":"ping"}`)
	wantErr(t, r, codeInvalidRequest)
	if string(r.ID) != "null" || !strings.Contains(r.Error.Message, "1024") {
		t.Fatalf("id %s, error %+v", r.ID, r.Error)
	}
	ok := `"` + strings.Repeat("i", maxIDBytes-2) + `"`
	if r := c.call(`{"jsonrpc":"2.0","id":` + ok + `,"method":"ping"}`); r.Error != nil || string(r.ID) != ok {
		t.Fatalf("a 1024-byte id: id %.20s…, error %+v", r.ID, r.Error)
	}
}

// MaxAnswerBytes is stdio's own threshold: an answer Handle returns is
// sent as it is over stdio exactly when it is at most MaxAnswerBytes.
func TestMaxAnswerBytesIsTheStdioThreshold(t *testing.T) {
	o := withTools(controlTool())
	o.MaxToolErrorBytes = 64 << 10
	s := New(o)
	n, limit := s.MaxResultBytes(), s.MaxAnswerBytes()
	if want := s.o.MaxOutBytes/2 - 1; limit != want {
		t.Fatalf("MaxAnswerBytes = %d, want %d", limit, want)
	}
	for _, tc := range []struct {
		name, text string
		fits       bool
	}{
		{"quotes", strings.Repeat(`"`, n), true},
		{"control", strings.Repeat("\x01", n), false},
	} {
		o.Tools = append(o.Tools, Tool{Name: tc.name, Handler: func(context.Context, json.RawMessage) (Result, error) {
			return Result{Text: tc.text}, nil
		}})
	}
	s = New(o)
	ss := NewSession()
	hello, _ := s.Parse([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`))
	s.Handle(t.Context(), ss, hello)
	for i, name := range []string{"quotes", "control"} {
		req, bad := s.Parse([]byte(callLine(2+i, name, "{}")))
		if bad != nil {
			t.Fatalf("%s: %s", name, bad)
		}
		out := s.Handle(t.Context(), ss, req)
		// stdio's rule: the line with its newline fits in half of MaxOutBytes.
		stdioSends := 2*(len(out)+1) <= s.o.MaxOutBytes
		if fits := len(out) <= limit; fits != stdioSends || fits != (name == "quotes") {
			t.Errorf("%s: a %d-byte answer fits MaxAnswerBytes %d: %v; stdio sends it: %v", name, len(out), limit, fits, stdioSends)
		}
	}
}
