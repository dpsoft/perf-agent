package perfpage

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// quoted is s as a JSON string the way a browser's JSON.stringify writes
// it: nothing but quotes, backslashes and controls escaped.
func quoted(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// askQuestionBody is an ask of node on p1 whose question member is the
// JSON text raw, as written.
func askQuestionBody(node, raw string) string {
	return `{"kind":"ask","publication":"p1","node":"` + node + `","focus":"","question":` + raw + `}`
}

// pageStateRows is the agent's page_state rows, as JSON objects.
func pageStateRows(t *testing.T, agent *client) []map[string]any {
	t.Helper()
	var out struct {
		Asks struct {
			Rows []map[string]any `json:"rows"`
		} `json:"asks"`
	}
	agent.callJSON("page_state", map[string]any{}, &out)
	return out.Asks.Rows
}

// The question the person typed goes from the page's report through the
// inbox to page_state, trimmed and with its line breaks folded, labelled
// as typed on the page; the envelope carries it as the newest ask's.
func TestAnAskCarriesItsQuestion(t *testing.T) {
	p := newPage(t, nil)
	o := p.c.open("cpu.pb.gz")
	o.explain(nil)
	b, c := o.nodeID("b"), o.nodeID("c")
	h := Inbox(p.srv)
	agent := p.session()

	if code, body := report(h, http.MethodPost, askQuestionBody(b, quoted(t, "  why is b\nso hot?  "))); code != http.StatusNoContent {
		t.Fatalf("an ask with a question: %d %s", code, body)
	}
	var env struct {
		Inbox map[string]string `json:"inbox"`
	}
	if err := json.Unmarshal(p.get("state"), &env); err != nil || env.Inbox["question"] != "why is b so hot?" {
		t.Errorf("the envelope's inbox %v (%v)", env.Inbox, err)
	}
	if code, body := report(h, http.MethodPost, askBody("p1", c)); code != http.StatusNoContent {
		t.Fatalf("a plain ask: %d %s", code, body)
	}
	rows := pageStateRows(t, agent)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	if rows[0]["node"] != b || rows[0]["question"] != "why is b so hot?" || rows[0]["question_source"] != "typed on the page by the person viewing it" {
		t.Errorf("the asked row %v", rows[0])
	}
	if _, ok := rows[1]["question"]; ok {
		t.Errorf("the plain ask's row %v", rows[1])
	}
	if _, ok := rows[1]["question_source"]; ok {
		t.Errorf("the plain ask's row %v", rows[1])
	}
}

// The question's bounds at the page's report: 280 characters, four-byte
// ones included, fit the body; anything over, not UTF-8, or holding a
// control or bidi formatting character is refused with 422 and a reason
// that repeats none of it. Line breaks fold; a blank question is a plain
// ask.
func TestQuestionBounds(t *testing.T) {
	p := newPage(t, nil)
	o := p.c.open("cpu.pb.gz")
	o.explain(nil)
	nodes := []string{o.nodeID("main"), o.nodeID("a"), o.nodeID("b"), o.nodeID("c")}
	h := Inbox(p.srv)
	agent := p.session()

	emoji := askQuestionBody(nodes[0], quoted(t, strings.Repeat("😀", 280)))
	if len(emoji) > 2048 {
		t.Fatalf("280 emoji make a %d-byte body", len(emoji))
	}
	for i, body := range []string{
		emoji,
		askQuestionBody(nodes[1], quoted(t, "line\u00a0one\r\n  line\ttwo\u200f")),
		askQuestionBody(nodes[2], quoted(t, " \n ")),
		askQuestionBody(nodes[3], `"😀 ok"`),
	} {
		if code, msg := report(h, http.MethodPost, body); code != http.StatusNoContent {
			t.Errorf("accepted body %d: %d %s", i, code, msg)
		}
	}
	rows := pageStateRows(t, agent)
	if len(rows) != 4 {
		t.Fatalf("%d rows", len(rows))
	}
	if rows[0]["question"] != strings.Repeat("😀", 280) || rows[1]["question"] != "line one line two" || rows[3]["question"] != "😀 ok" {
		t.Errorf("the questions kept: %q, %q, %q", rows[0]["question"], rows[1]["question"], rows[3]["question"])
	}
	if _, ok := rows[2]["question"]; ok {
		t.Errorf("a blank question was kept: %v", rows[2])
	}

	for _, tc := range []struct{ what, raw, reason string }{
		{"281 characters", quoted(t, "SECRET"+strings.Repeat("x", 275)), "over 280 characters"},
		{"281 emoji", quoted(t, strings.Repeat("😀", 281)), "over 280 characters"},
		{"invalid UTF-8", "\"SECRET\xff\"", "UTF-8"},
		{"an unpaired surrogate", `"SECRET\ud800"`, "UTF-8"},
		{"a reversed pair", `"SECRET\ude00\ud83d"`, "UTF-8"},
		{"an escaped NUL", `"SECRET\u0000"`, "control"},
		{"a raw NUL", "\"SECRET\x00\"", "the body must be one JSON object"},
		{"a C1 control", quoted(t, "SECRET\u009b"), "control"},
		{"an RTL override", quoted(t, "SECRET\u202egnp.exe"), "bidirectional"},
		{"an isolate", quoted(t, "SECRET\u2066x\u2069"), "bidirectional"},
		{"tag characters", quoted(t, "SECRET hi\U000e0069\U000e0067\U000e006e"), "invisible"},
		{"a zero-width space", quoted(t, "SECRET\u200bx"), "invisible"},
		{"alternating joiners", quoted(t, "SECRET Why?"+strings.Repeat("\u200c\u200d", 40)), "invisible"},
		{"a Hangul filler", quoted(t, "SECRET \u3164"), "invisible"},
		{"a number", `7`, "the body must be one JSON object"},
		{"null", `null`, "the body must be one JSON object"},
	} {
		code, msg := report(h, http.MethodPost, askQuestionBody(nodes[0], tc.raw))
		if code != http.StatusUnprocessableEntity || !strings.HasPrefix(msg, "invalid page report: ") || !strings.Contains(msg, tc.reason) {
			t.Errorf("%s: %d %q, want 422 saying %q", tc.what, code, msg, tc.reason)
		}
		for _, echoed := range []string{"SECRET", "xxxx", "😀", "gnp"} {
			if strings.Contains(msg, echoed) {
				t.Errorf("%s: the reason %q repeats %q", tc.what, msg, echoed)
			}
		}
	}
	// A question twice is refused like any member twice.
	twice := `{"kind":"ask","publication":"p1","node":"` + nodes[0] + `","question":"a","question":"b"}`
	if code, msg := report(h, http.MethodPost, twice); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "the body must be one JSON object") {
		t.Errorf("a question twice: %d %q", code, msg)
	}
	if got := p.envelope().Inbox; got.Asks != "4" {
		t.Errorf("refused questions moved the inbox: %+v", got)
	}
}

// The page's report body is at most 2048 bytes: 2048 are decoded, 2049
// are refused unread.
func TestTheInboxBodyCapIs2048(t *testing.T) {
	p := newPage(t, nil)
	o := p.c.open("cpu.pb.gz")
	o.explain(nil)
	h := Inbox(p.srv)
	base := askQuestionBody(o.nodeID("b"), `"why?"`)
	pad := func(n int) string { return base[:len(base)-1] + strings.Repeat(" ", n-len(base)) + "}" }
	if code, msg := report(h, http.MethodPost, pad(2048)); code != http.StatusNoContent {
		t.Errorf("2048 bytes: %d %s", code, msg)
	}
	if code, msg := report(h, http.MethodPost, pad(2049)); code != http.StatusUnprocessableEntity || !strings.Contains(msg, "over 2048 bytes") {
		t.Errorf("2049 bytes: %d %s", code, msg)
	}
}
