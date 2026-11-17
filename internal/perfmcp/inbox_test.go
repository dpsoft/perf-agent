package perfmcp

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// pageStateJSON is page_state's answer.
type pageStateJSON struct {
	Viewers string `json:"viewers"`
	Agent   struct {
		Seq      string `json:"seq"`
		Current  string `json:"current"`
		Selected string `json:"selected"`
		Focus    string `json:"focus"`
		By       string `json:"by"`
	} `json:"agent"`
	User *struct {
		Publication string `json:"publication"`
		Selected    string `json:"selected"`
		Name        string `json:"name"`
		Focus       string `json:"focus"`
		At          string `json:"at"`
	} `json:"user"`
	Asks struct {
		Rows []struct {
			ID          string `json:"id"`
			Publication string `json:"publication"`
			Node        string `json:"node"`
			Name        string `json:"name"`
			View        string `json:"view"`
			Digest      string `json:"digest"`
			At          string `json:"at"`
			ReadBy      string `json:"read_by"`
		} `json:"rows"`
		Next    string `json:"next"`
		Dropped string `json:"dropped"`
	} `json:"asks"`
	Note string `json:"note"`
}

// ids are the IDs of the rows.
func (p pageStateJSON) ids() []string {
	out := []string{}
	for _, r := range p.Asks.Rows {
		out = append(out, r.ID)
	}
	return out
}

// pageState calls page_state on c with args, which must succeed.
func (c *mcpSession) pageState(args map[string]any) pageStateJSON {
	c.tb.Helper()
	if args == nil {
		args = map[string]any{}
	}
	var out pageStateJSON
	c.callOK("page_state", args, &out)
	return out
}

// askIDs is "i<n>-<nonce>" for each n.
func askIDs(s *Server, ns ...int) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, s.live.inbox.askID(int64(n)))
	}
	return out
}

// askEach asks about each node of p1 named in names, 3 s apart.
func askEach(t *testing.T, s *Server, names ...string) {
	t.Helper()
	for _, name := range names {
		ask(t, s, "p1", p1Node(t, s, name))
	}
}

// §D4: selections coalesce: only the user's last select or focus is
// kept, and none of them is an ask.
func TestSelectionsCoalesce(t *testing.T) {
	s, c, _, _ := hubPage(t, nil, nil)
	stepClock(s, time.Second)
	if got := c.pageState(nil); got.User != nil {
		t.Errorf("a user before any report: %+v", got.User)
	}
	for _, name := range []string{"main", "a", "b", "c", "a"} {
		if err := s.Report(PageReport{Kind: "select", Publication: "p1", Node: p1Node(t, s, name)}); err != nil {
			t.Fatal(err)
		}
	}
	got := c.pageState(nil)
	if got.User == nil || got.User.Selected != p1Node(t, s, "a") || got.User.Name != "a" || got.User.Focus != "" ||
		got.User.Publication != "p1" || got.User.At != "2026-11-15T10:02:08Z" {
		t.Errorf("the last select %+v", got.User)
	}
	if len(got.Asks.Rows) != 0 || len(s.live.inbox.asks) != 0 {
		t.Errorf("selections made asks: %+v", got.Asks)
	}
	// A focus keeps the selection on the same publication.
	if err := s.Report(PageReport{Kind: "focus", Publication: "p1", Node: p1Node(t, s, "main")}); err != nil {
		t.Fatal(err)
	}
	if u := c.pageState(nil).User; u.Selected != p1Node(t, s, "a") || u.Focus != p1Node(t, s, "main") {
		t.Errorf("after a focus %+v", u)
	}
	if err := s.Report(PageReport{Kind: "select", Publication: "p1", Node: p1Node(t, s, "b"), Focus: p1Node(t, s, "a")}); err != nil {
		t.Fatal(err)
	}
	if u := c.pageState(nil).User; u.Selected != p1Node(t, s, "b") || u.Name != "b" || u.Focus != p1Node(t, s, "a") {
		t.Errorf("a select under a focus %+v", u)
	}
	// The same select again changes nothing, not even its time.
	at := c.pageState(nil).User.At
	if err := s.Report(PageReport{Kind: "select", Publication: "p1", Node: p1Node(t, s, "b"), Focus: p1Node(t, s, "a")}); err != nil {
		t.Fatal(err)
	}
	if u := c.pageState(nil).User; u.At != at {
		t.Errorf("a repeated select moved the time from %s to %s", at, u.At)
	}
	// None of it is in the envelope, so none of it was sent to the page.
	if got := envelopeNow(t, s).Inbox; got.Rev != "0" || got.Asks != "0" {
		t.Errorf("seven reports, no ask: %+v", got)
	}
}

// §D4: the ring keeps the newest MaxInboxEntries asks and counts the
// ones it let go. A limit over the ring is clamped to it, and the default
// limit is too; a limit below 1 is refused.
func TestTheAskRingDropsTheOldest(t *testing.T) {
	s, c, _, _ := hubPage(t, nil, func(c *Config) { c.Limits.Hub.MaxInboxEntries = 3 })
	stepClock(s, 3*time.Second)
	askEach(t, s, "main", "a", "b", "c", "main")
	if got := envelopeNow(t, s).Inbox; got.Asks != "3" || got.Unread != "3" || got.Rev != "5" {
		t.Errorf("the inbox after five asks: %+v", got)
	}
	for _, args := range []map[string]any{nil, {"limit": 10}, {"limit": 3}} {
		got := c.pageState(args)
		if !slices.Equal(got.ids(), askIDs(s, 3, 4, 5)) || got.Asks.Dropped != "2" || got.Asks.Next != askIDs(s, 5)[0] {
			t.Errorf("%v: rows %v, dropped %s, next %s", args, got.ids(), got.Asks.Dropped, got.Asks.Next)
		}
		if names := []string{got.Asks.Rows[0].Name, got.Asks.Rows[1].Name, got.Asks.Rows[2].Name}; !slices.Equal(names, []string{"b", "c", "main"}) {
			t.Errorf("%v: the rows name %v", args, names)
		}
	}
	if got := c.pageState(map[string]any{"limit": 2}); !slices.Equal(got.ids(), askIDs(s, 3, 4)) || got.Note != "" {
		t.Errorf("limit 2: %v, note %q", got.ids(), got.Note)
	}
	// A cursor the ring moved past: the asks after it that it let go are
	// said to be lost.
	for _, tc := range []struct {
		since int
		note  string
	}{
		{1, "1 ask after since was dropped"},
		{0, ""},
		{2, ""},
		{3, ""},
	} {
		args := map[string]any{}
		if tc.since > 0 {
			args["since"] = askIDs(s, tc.since)[0]
		}
		got := c.pageState(args)
		if !strings.Contains(got.Note, tc.note) || tc.note == "" && got.Note != "" {
			t.Errorf("since %d: note %q, want one saying %q", tc.since, got.Note, tc.note)
		}
	}
	for _, limit := range []any{0, -1} {
		text, isErr := c.call(t.Context(), "page_state", map[string]any{"limit": limit})
		if !isErr || !strings.HasPrefix(text, "invalid arguments to page_state: limit must be at least 1") {
			t.Errorf("limit %v: isError=%v %s", limit, isErr, text)
		}
	}
	if text, isErr := c.call(t.Context(), "page_state", map[string]any{"limit": "3"}); !isErr || !strings.Contains(text, "integer") {
		t.Errorf("a string limit: isError=%v %s", isErr, text)
	}
}

// §D4: an ask of the same publication and node as a kept one within 2 s
// is the same click: it is ignored, and Report accepts it.
func TestADuplicateAskWithinTwoSecondsIsIgnored(t *testing.T) {
	s, _, _, _ := hubPage(t, nil, nil)
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	b, c := p1Node(t, s, "b"), p1Node(t, s, "c")
	ask(t, s, "p1", b)
	at = at.Add(1999 * time.Millisecond)
	ask(t, s, "p1", b)
	if got := envelopeNow(t, s).Inbox; got.Asks != "1" || got.Rev != "1" {
		t.Errorf("a duplicate within 2 s was recorded: %+v", got)
	}
	ask(t, s, "p1", c)
	if got := envelopeNow(t, s).Inbox; got.Asks != "2" || got.Rev != "2" {
		t.Errorf("another node within 2 s: %+v", got)
	}
	at = at.Add(time.Millisecond)
	ask(t, s, "p1", b)
	if got := envelopeNow(t, s).Inbox; got.Asks != "3" || got.Rev != "3" {
		t.Errorf("the same node 2 s later: %+v", got)
	}
}

// §D4: acknowledgement is a cursor each agent keeps. Two sessions, each
// passing back its own next, see every ask once, in order, whatever the
// other reads.
func TestPageStateCursors(t *testing.T) {
	s, _, _, _ := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	claude, codex := newMCPSession(t, s, "claude-code"), newMCPSession(t, s, "codex")
	seen := map[string][]string{}
	cursor := map[string]string{}
	read := func(name string, c *mcpSession, limit int) {
		t.Helper()
		args := map[string]any{"limit": limit}
		if cursor[name] != "" {
			args["since"] = cursor[name]
		}
		got := c.pageState(args)
		seen[name] = append(seen[name], got.ids()...)
		if len(got.Asks.Rows) > 0 && got.Asks.Next != got.Asks.Rows[len(got.Asks.Rows)-1].ID {
			t.Errorf("%s: next %s is not the last row's ID", name, got.Asks.Next)
		}
		if len(got.Asks.Rows) == 0 && got.Asks.Next != cursor[name] {
			t.Errorf("%s: with no rows next moved from %q to %q", name, cursor[name], got.Asks.Next)
		}
		cursor[name] = got.Asks.Next
	}
	askEach(t, s, "main", "a", "b")
	read("claude", claude, 2)
	read("codex", codex, 8)
	askEach(t, s, "c", "main")
	read("claude", claude, 2)
	read("claude", claude, 2)
	read("claude", claude, 2)
	read("codex", codex, 1)
	read("codex", codex, 8)
	read("codex", codex, 8)
	all := askIDs(s, 1, 2, 3, 4, 5)
	for name, ids := range seen {
		if !slices.Equal(ids, all) {
			t.Errorf("%s saw %v, want each of %v once", name, ids, all)
		}
	}
}

// An ask ID carries a per-process nonce, so a cursor kept
// across a restart, i40 of the run before, hides none of this run's asks:
// they all come back, with a note saying why.
func TestACursorFromAnEarlierServerReturnsEveryRetainedAsk(t *testing.T) {
	s, c, _, _ := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(s.live.inbox.nonce) {
		t.Fatalf("the nonce %q", s.live.inbox.nonce)
	}
	s.live.inbox.nonce = "3f9a01c2"
	askEach(t, s, "main", "a", "b")
	got := c.pageState(map[string]any{"since": "i40-ffffffff"})
	if !slices.Equal(got.ids(), []string{"i1-3f9a01c2", "i2-3f9a01c2", "i3-3f9a01c2"}) {
		t.Errorf("an earlier run's cursor returned %v", got.ids())
	}
	if got.Note != foreignCursorNote || !strings.Contains(got.Note, "restarted") {
		t.Errorf("the note %q", got.Note)
	}
	for _, r := range got.Asks.Rows {
		if !regexp.MustCompile(`^i[0-9]+-[0-9a-f]{8}$`).MatchString(r.ID) {
			t.Errorf("ask ID %q", r.ID)
		}
	}
	if got := c.pageState(map[string]any{"since": "i2-3f9a01c2"}); !slices.Equal(got.ids(), []string{"i3-3f9a01c2"}) || got.Note != "" {
		t.Errorf("this run's cursor: %v, note %q", got.ids(), got.Note)
	}
	// Another run's cursor after the newest of this run's still sees them.
	if got := c.pageState(map[string]any{"since": "i3-00000000"}); len(got.ids()) != 3 {
		t.Errorf("another run's i3: %v", got.ids())
	}
	// A cursor of this run's nonce past its newest ask was never given by
	// it: it is taken for another run's, not for "nothing new".
	if got := c.pageState(map[string]any{"since": "i9-3f9a01c2"}); len(got.ids()) != 3 || got.Note != foreignCursorNote {
		t.Errorf("a cursor past the newest ask: %v, note %q", got.ids(), got.Note)
	}
	for _, since := range []string{"i7", "7", "i0-3f9a01c2", "i07-3f9a01c2", "i7-3F9A01C2", "i7-3f9a01c", "x7-3f9a01c2", "i-3f9a01c2", "i99999999999999999999-3f9a01c2"} {
		text, isErr := c.call(t.Context(), "page_state", map[string]any{"since": since})
		if !isErr || !strings.HasPrefix(text, "invalid arguments to page_state: since must be an ask ID") {
			t.Errorf("since %q: isError=%v %s", since, isErr, text)
		}
	}
}

// §D4: the first page_state that returns an ask records its reader, once:
// a later reader leaves it, and moves no revision.
func TestReadsAreMarkedOnce(t *testing.T) {
	s, _, _, _ := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	askEach(t, s, "main", "a")
	codex, claude := newMCPSession(t, s, "codex"), newMCPSession(t, s, "claude-code")
	if got := envelopeNow(t, s).Inbox; got.Rev != "2" || got.Unread != "2" || got.ReadBy != "" {
		t.Fatalf("before any read: %+v", got)
	}
	first := codex.pageState(map[string]any{"limit": 1})
	if first.Asks.Rows[0].ReadBy != "codex" {
		t.Errorf("codex's read returned read_by %q", first.Asks.Rows[0].ReadBy)
	}
	if got := envelopeNow(t, s).Inbox; got.Rev != "3" || got.Unread != "1" || got.ReadBy != "" {
		t.Errorf("after codex read the older ask: %+v", got)
	}
	codex.pageState(nil)
	if got := envelopeNow(t, s).Inbox; got.Rev != "4" || got.Unread != "0" || got.ReadBy != "codex" {
		t.Errorf("after codex read both: %+v", got)
	}
	later := claude.pageState(nil)
	for _, r := range later.Asks.Rows {
		if r.ReadBy != "codex" {
			t.Errorf("claude-code sees %s read by %q", r.ID, r.ReadBy)
		}
	}
	if got := envelopeNow(t, s).Inbox; got.Rev != "4" || got.ReadBy != "codex" {
		t.Errorf("a second reader moved the inbox: %+v", got)
	}
	// A read that returns nothing marks nothing.
	claude.pageState(map[string]any{"since": later.Asks.Next})
	if got := envelopeNow(t, s).Inbox; got.Rev != "4" {
		t.Errorf("an empty read moved rev: %+v", got)
	}
}

// Two sessions reading at once never both claim the first read of an
// ask: each answer names the one reader the inbox records.
func TestTwoReadersNeverBothClaimARead(t *testing.T) {
	s, _, _, _ := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	codex, claude := newMCPSession(t, s, "codex"), newMCPSession(t, s, "claude-code")
	for i := range 50 {
		askEach(t, s, []string{"main", "a", "b", "c"}[i%4])
		id := askIDs(s, i+1)[0]
		var wg sync.WaitGroup
		var got [2]string
		for j, c := range []*mcpSession{codex, claude} {
			wg.Go(func() {
				text, isErr := c.call(context.Background(), "page_state", map[string]any{"since": askIDs(s, max(i, 1))[0]})
				var out pageStateJSON
				if isErr || json.Unmarshal([]byte(text), &out) != nil {
					t.Errorf("page_state: %s", text)
					return
				}
				for _, r := range out.Asks.Rows {
					if r.ID == id {
						got[j] = r.ReadBy
					}
				}
			})
		}
		wg.Wait()
		if i == 0 {
			continue // since i1 lists nothing; i1 was read below
		}
		recorded := s.live.inbox.asks[len(s.live.inbox.asks)-1].readBy
		if got[0] != recorded || got[1] != recorded {
			t.Fatalf("ask %s: codex was told %q, claude-code %q, the inbox recorded %q", id, got[0], got[1], recorded)
		}
	}
}

// readResource reads uri on c and returns its one text.
func (c *mcpSession) readResource(ctx context.Context, uri string) (text string, errMsg string) {
	c.tb.Helper()
	var res struct {
		Result struct {
			Contents []struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	out := c.handle(ctx, "resources/read", map[string]any{"uri": uri})
	if err := json.Unmarshal(out, &res); err != nil {
		c.tb.Fatalf("resources/read %s: %s: %v", uri, out, err)
	}
	if res.Error != nil {
		return "", res.Error.Message
	}
	if len(res.Result.Contents) != 1 || res.Result.Contents[0].URI != uri || res.Result.Contents[0].MIMEType != "application/json" {
		c.tb.Fatalf("resources/read %s: %s", uri, out)
	}
	return res.Result.Contents[0].Text, ""
}
