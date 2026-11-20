package perfmcp

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// askWith reports an ask of node on pub carrying question, which must be
// accepted.
func askWith(t *testing.T, s *Server, pub, node, question string) {
	t.Helper()
	if err := s.Report(PageReport{Kind: "ask", Publication: pub, Node: node, Question: question}); err != nil {
		t.Fatalf("ask %s %s %q: %v", pub, node, question, err)
	}
}

// rawRows are page_state's ask rows as JSON objects, so a test sees which
// keys a row has.
func rawRows(t *testing.T, c *mcpSession) []map[string]json.RawMessage {
	t.Helper()
	var out struct {
		Asks struct {
			Rows []map[string]json.RawMessage `json:"rows"`
		} `json:"asks"`
	}
	c.callOK("page_state", map[string]any{}, &out)
	return out.Asks.Rows
}

// inboxKeys is the inbox part of s's envelope as a JSON object.
func inboxKeys(t *testing.T, s *Server) map[string]string {
	t.Helper()
	var env struct {
		Inbox map[string]string `json:"inbox"`
	}
	if err := json.Unmarshal(s.PageSnapshot().State(), &env); err != nil {
		t.Fatal(err)
	}
	return env.Inbox
}

// An ask may carry the question the person typed: page_state returns it,
// folded and trimmed, labelled as typed on the page; an ask without one
// has neither key, as before. The envelope carries the newest ask's.
func TestAnAskRowCarriesItsQuestion(t *testing.T) {
	s, c, _, _ := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	b, main := p1Node(t, s, "b"), p1Node(t, s, "main")
	askWith(t, s, "p1", b, "  why is\r\nthis\nhot?\u2028 ")
	if got := inboxKeys(t, s)["question"]; got != "why is this hot?" {
		t.Errorf("the envelope's question %q", got)
	}
	ask(t, s, "p1", main)
	if _, ok := inboxKeys(t, s)["question"]; ok {
		t.Errorf("a plain newest ask left a question in the envelope: %v", inboxKeys(t, s))
	}
	rows := rawRows(t, c)
	if len(rows) != 2 {
		t.Fatalf("%d rows", len(rows))
	}
	var q, src string
	if json.Unmarshal(rows[0]["question"], &q) != nil || json.Unmarshal(rows[0]["question_source"], &src) != nil {
		t.Fatalf("the first row %v", rows[0])
	}
	if q != "why is this hot?" || src != "typed on the page by the person viewing it" {
		t.Errorf("question %q, source %q", q, src)
	}
	if _, ok := rows[1]["question"]; ok {
		t.Errorf("a plain ask has a question: %v", slices.Sorted(maps.Keys(rows[1])))
	}
	if _, ok := rows[1]["question_source"]; ok {
		t.Errorf("a plain ask has a question_source: %v", slices.Sorted(maps.Keys(rows[1])))
	}
	// The resource answers the same rows.
	text, msg := c.readResource(t.Context(), pageStateURI)
	if msg != "" || !strings.Contains(text, `"question":"why is this hot?","question_source":"typed on the page by the person viewing it"`) {
		t.Errorf("the resource: %s %s", msg, text)
	}
}

// The question is normalised in one place: line breaks fold to spaces,
// surrounding space is trimmed, and a blank one is no question. One over
// 280 characters, with a control or bidi formatting character, or not
// UTF-8 is refused, and the reason never repeats it.
func TestQuestionIsNormalisedOrRefused(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"why?", "why?"},
		{" \n\r\n ", ""},
		{"", ""},
		{"a\r\nb\rc\nd\u2029e", "a b c d e"},
		{strings.Repeat("😀", 280), strings.Repeat("😀", 280)},
		{"  " + strings.Repeat("x", 280) + "\n", strings.Repeat("x", 280)},
		{"</script><img src=x onerror=alert(1)>", "</script><img src=x onerror=alert(1)>"},
		{"👨\u200d👩\u200d👧 zero\u200bwidth", "👨\u200d👩\u200d👧 zero\u200bwidth"},
	} {
		got, err := normalizeQuestion(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%q: %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	// A tab is a control character: only line breaks fold.
	for _, tc := range []struct{ what, in, reason string }{
		{"281 characters", strings.Repeat("x", 281), "over 280 characters"},
		{"281 emoji", strings.Repeat("😀", 281), "over 280 characters"},
		{"invalid UTF-8", "SECRET\xff", "not valid UTF-8"},
		{"a NUL", "SECRET\x00", "control"},
		{"a tab", "SECRET\tx", "control"},
		{"an escape", "SECRET\x1b[2J", "control"},
		{"DEL", "SECRET\x7f", "control"},
		{"a C1 control", "SECRET\u0085", "control"},
		{"a C1 CSI", "SECRET\u009b", "control"},
		{"an RTL override", "SECRET\u202egnp.exe", "bidirectional"},
		{"an LRE", "SECRET\u202a", "bidirectional"},
		{"an isolate", "SECRET\u2067", "bidirectional"},
		{"an RLM", "SECRET\u200f", "bidirectional"},
		{"an ALM", "SECRET\u061c", "bidirectional"},
	} {
		got, err := normalizeQuestion(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: %q, %v; want a refusal saying %q", tc.what, got, err, tc.reason)
			continue
		}
		if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "xxxx") || strings.Contains(err.Error(), "😀") {
			t.Errorf("%s: the reason %q repeats the question", tc.what, err)
		}
	}
}

// The 2 s duplicate window keys on the publication, the node and the
// question as the inbox keeps it: another question about the same frame
// is another ask; the same one, however it was spaced, is the same click.
func TestQuestionDedupKeysOnTheText(t *testing.T) {
	s, _, _, _ := hubPage(t, nil, nil)
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	b := p1Node(t, s, "b")
	count := func(what, want string) {
		t.Helper()
		if got := envelopeNow(t, s).Inbox; got.Asks != want || got.Rev != want {
			t.Errorf("%s: %+v, want %s asks at rev %s", what, got, want, want)
		}
	}
	askWith(t, s, "p1", b, "why?")
	count("the first question", "1")
	at = at.Add(time.Second)
	askWith(t, s, "p1", b, " why?\n")
	count("the same question within 2 s", "1")
	askWith(t, s, "p1", b, "how?")
	count("another question within 2 s", "2")
	ask(t, s, "p1", b)
	count("a plain ask within 2 s", "3")
	askWith(t, s, "p1", b, "   ")
	count("a blank question, a plain ask again", "3")
	at = at.Add(2 * time.Second)
	askWith(t, s, "p1", b, "why?")
	count("the first question 3 s later", "4")
}

// A question changes nothing but its ask row: an ask with one moves the
// inbox exactly as one without, never the engine part, the publications or
// the user's view; a select or focus with a question, and a refused
// question, change nothing at all.
func TestAQuestionChangesOnlyTheAskRow(t *testing.T) {
	s, c, _, notified := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	a, b := p1Node(t, s, "a"), p1Node(t, s, "b")
	if err := s.Report(PageReport{Kind: "select", Publication: "p1", Node: a}); err != nil {
		t.Fatal(err)
	}
	before, dump := countersOf(s), dumpLive(s)
	env := envelopeNow(t, s)
	user := c.pageState(nil).User
	for len(notified) > 0 {
		<-notified
	}

	ask(t, s, "p1", b)
	plain := envelopeNow(t, s)
	askWith(t, s, "p1", b, "is this the allocator?")
	asked := envelopeNow(t, s)
	if plain.Inbox.Rev != "1" || plain.Inbox.Asks != "1" || asked.Inbox.Rev != "2" || asked.Inbox.Asks != "2" || asked.Inbox.Unread != "2" {
		t.Errorf("a plain ask %+v, then one with a question %+v", plain.Inbox, asked.Inbox)
	}
	if len(notified) != 2 {
		t.Errorf("%d envelopes notified for two asks", len(notified))
	}
	for _, e := range []envelopeJSON{plain, asked} {
		if string(e.raw) != string(env.raw) {
			t.Errorf("an ask changed the engine part: %s, then %s", env.raw, e.raw)
		}
	}
	for what, r := range map[string]PageReport{
		"a select with a question": {Kind: "select", Publication: "p1", Node: b, Question: "x"},
		"a focus with a question":  {Kind: "focus", Publication: "p1", Node: b, Question: "x"},
		"a refused question":       {Kind: "ask", Publication: "p1", Node: a, Question: "x\x00"},
		"an over-long question":    {Kind: "ask", Publication: "p1", Node: a, Question: strings.Repeat("x", 281)},
	} {
		err := s.Report(r)
		if !errors.Is(err, ErrBadReport) {
			t.Errorf("%s: %v", what, err)
		}
		if what == "a select with a question" && (err == nil || !strings.Contains(err.Error(), "only an ask carries a question")) {
			t.Errorf("%s: the reason %v", what, err)
		}
	}
	if got := envelopeNow(t, s); got.Inbox != asked.Inbox || string(got.raw) != string(env.raw) {
		t.Errorf("refused reports moved the envelope: %+v", got.Inbox)
	}
	if after := countersOf(s); after != before {
		t.Errorf("asks changed the engine: %+v, then %+v", before, after)
	}
	if after := dumpLive(s); !equalDumps(dump, after) {
		t.Error("asks changed the live state")
	}
	if got := c.pageState(nil); got.User == nil || *got.User != *user {
		t.Errorf("the user's view moved from %+v to %+v", user, got.User)
	}
}
