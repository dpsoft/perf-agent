package perfmcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// SRV-R3b: page_state, the tool and the resource, never takes the engine:
// while another session's tool holds it, both answer at once with the
// current snapshot and inbox, and record the reader.
func TestPageStateNeverTakesTheEngine(t *testing.T) {
	h := newEngineHold()
	s, a, o, _ := hubPage(t, h, func(c *Config) { c.Limits.MaxEngineWait = 30 * time.Second })
	stepClock(s, 3*time.Second)
	askEach(t, s, "b")
	done := holdTop(t, h, a, o)
	b := newMCPSession(t, s, "codex")

	start := time.Now()
	got := b.pageState(nil)
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("page_state took %v with the engine held", took)
	}
	if got.Agent.Current != "p1" || got.Agent.By != "A" || got.Agent.Seq != "2" || len(got.Asks.Rows) != 1 || got.Asks.Rows[0].ReadBy != "codex" {
		t.Errorf("page_state with the engine held: %+v", got)
	}
	askEach(t, s, "c")
	start = time.Now()
	text, msg := b.readResource(t.Context(), pageStateURI)
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Errorf("resources/read took %v with the engine held", took)
	}
	if msg != "" {
		t.Fatalf("resources/read: %s", msg)
	}
	var res pageStateJSON
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Asks.Rows) != 2 || res.Asks.Rows[1].Name != "c" || res.Asks.Rows[1].ReadBy != "codex" {
		t.Errorf("the resource with the engine held: %+v", res.Asks)
	}
	if got := envelopeNow(t, s).Inbox; got.Unread != "0" || got.ReadBy != "codex" {
		t.Errorf("the reads were not recorded: %+v", got)
	}
	select {
	case text := <-done:
		t.Fatalf("top answered before the release: %s", text)
	default:
	}
	if ran := h.tools(); slices.Contains(ran, "page_state") {
		t.Errorf("page_state held the engine: %v", ran)
	}
	close(h.release)
	if text := <-done; strings.HasPrefix(text, "isError") {
		t.Fatal(text)
	}
}

// §D4: the resource is page_state without a cursor, with the newest eight
// asks.
func TestThePageStateResourceShowsTheLastEightAsks(t *testing.T) {
	s, a, _, _ := hubPage(t, nil, nil)
	stepClock(s, 3*time.Second)
	for range 5 {
		askEach(t, s, "main", "b")
	}
	text, msg := a.readResource(t.Context(), pageStateURI)
	if msg != "" {
		t.Fatal(msg)
	}
	var got pageStateJSON
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ids(), askIDs(s, 3, 4, 5, 6, 7, 8, 9, 10)) || got.Asks.Next != askIDs(s, 10)[0] || got.Asks.Dropped != "0" {
		t.Errorf("the resource listed %v, next %q, dropped %s", got.ids(), got.Asks.Next, got.Asks.Dropped)
	}
	if got.Agent.Current != "p1" || got.Viewers != "0" || got.Note != "" {
		t.Errorf("the resource %+v", got)
	}
	// It has the tool's shape: counts are strings, and user appears once
	// the page reported a select.
	var shape map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &shape); err != nil {
		t.Fatal(err)
	}
	if _, ok := shape["user"]; ok {
		t.Errorf("user before any select: %s", text)
	}
	if string(shape["viewers"]) != `"0"` {
		t.Errorf("viewers %s", shape["viewers"])
	}
	// The resource marks what it returns: two asks are left unread.
	if got := envelopeNow(t, s).Inbox; got.Unread != "2" || got.Asks != "10" {
		t.Errorf("after the resource: %+v", got)
	}
}

// Without the live page, page_state is neither listed nor callable, its
// resource is not served, and the instructions do not name it; with it,
// all three.
func TestPageStateIsHubOnly(t *testing.T) {
	type tool struct {
		Name        string               `json:"name"`
		Annotations mcpstdio.Annotations `json:"annotations"`
		Schema      map[string]any       `json:"inputSchema"`
	}
	list := func(c *mcpSession) (tools []tool, uris []string, instructions string) {
		t.Helper()
		var tl struct {
			Result struct {
				Tools []tool `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(c.handle(t.Context(), "tools/list", map[string]any{}), &tl); err != nil {
			t.Fatal(err)
		}
		var rl struct {
			Result struct {
				Resources []struct {
					URI string `json:"uri"`
				} `json:"resources"`
			} `json:"result"`
		}
		if err := json.Unmarshal(c.handle(t.Context(), "resources/list", map[string]any{}), &rl); err != nil {
			t.Fatal(err)
		}
		for _, r := range rl.Result.Resources {
			uris = append(uris, r.URI)
		}
		var init struct {
			Result struct {
				Instructions string `json:"instructions"`
			} `json:"result"`
		}
		ss := &mcpSession{tb: t, s: c.s, ss: mcpstdio.NewSession(), nextID: 1}
		if err := json.Unmarshal(ss.handle(t.Context(), "initialize", map[string]any{"protocolVersion": "2025-11-25"}), &init); err != nil {
			t.Fatal(err)
		}
		return tl.Result.Tools, uris, init.Result.Instructions
	}

	plain := must(New(testConfig(t)))(t)
	t.Cleanup(func() { _ = plain.Close() })
	c := newMCPSession(t, plain, "codex")
	tools, uris, instructions := list(c)
	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	if want := []string{"open", "top", "path", "facts", "gpu_kernels_for", "sandwich", "diff", "source", "render_flamegraph",
		"capture", "capture_wait", "capture_cancel", "capture_list"}; !slices.Equal(names, want) {
		t.Errorf("without Page the tools are %v, want the 13 %v", names, want)
	}
	if !slices.Equal(uris, []string{"perf-agent://guide"}) {
		t.Errorf("without Page the resources are %v", uris)
	}
	if strings.Contains(instructions, "page_state") {
		t.Errorf("without Page the instructions name page_state: %q", instructions)
	}
	if text, isErr := c.call(t.Context(), "page_state", map[string]any{}); !isErr || !strings.Contains(text, "not one tool result") {
		t.Errorf("page_state without Page: isError=%v %s", isErr, text)
	}
	if _, msg := c.readResource(t.Context(), pageStateURI); msg == "" {
		t.Error("the page state resource is served without Page")
	}

	s, a, _, _ := hubPage(t, nil, nil)
	tools, uris, instructions = list(a)
	i := slices.IndexFunc(tools, func(tl tool) bool { return tl.Name == "page_state" })
	if i < 0 || i != len(tools)-1 {
		t.Fatalf("with Page page_state is not the last tool: %v", tools)
	}
	if got := tools[i].Annotations; got != (mcpstdio.Annotations{Idempotent: true}) {
		t.Errorf("page_state's annotations %+v, want readOnlyHint false and idempotentHint true", got)
	}
	if props, _ := tools[i].Schema["properties"].(map[string]any); len(props) != 2 || props["since"] == nil || props["limit"] == nil {
		t.Errorf("page_state's schema %v", tools[i].Schema)
	}
	if !slices.Equal(uris, []string{"perf-agent://guide", pageStateURI}) {
		t.Errorf("with Page the resources are %v", uris)
	}
	if !strings.HasSuffix(instructions, " When the user refers to something they selected or asked about on the page, call page_state.") {
		t.Errorf("with Page the instructions %q", instructions)
	}
	tl, ok := s.mcp.Tool("page_state")
	r, rok := s.mcp.Resource(pageStateURI)
	if !ok || !tl.Snapshot || !rok || !r.Snapshot {
		t.Error("page_state and its resource are not admitted as snapshot reads")
	}
}
