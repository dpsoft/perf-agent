package perfmcp

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

var updateDiffGolden = flag.Bool("update-diff-golden", false, "rewrite testdata/diff.golden.json from the diff tool")

// diffGoldenCall is one recorded diff call and its answer.
type diffGoldenCall struct {
	Args    map[string]any `json:"args"`
	IsError bool           `json:"is_error"`
	Text    string         `json:"text"`
}

// TestDiffToolUnchangedByTheRefactor replays the diff calls recorded before
// page_diff shared diff's comparison: pages, a cursor, work counts, a
// cache hit, a GPU pair and every refusal give the same bytes.
func TestDiffToolUnchangedByTheRefactor(t *testing.T) {
	cfg := testConfig(t)
	for name, raw := range map[string][]byte{
		"base.pb.gz": pmfixture.CPUBasic(), "new.pb.gz": pmfixture.CPUBasicNew(), "gpu.pb.gz": pmfixture.GPUBasic(),
	} {
		writeFixture(t, cfg.ProfilesDir, name, raw)
	}
	c := newTestServer(t, cfg)
	var b, n, g openOut
	c.callOK("open", map[string]any{"path": "base.pb.gz"}, &b)
	c.callOK("open", map[string]any{"path": "new.pb.gz"}, &n)
	c.callOK("open", map[string]any{"path": "gpu.pb.gz"}, &g)
	pair := func(extra map[string]any) map[string]any {
		m := map[string]any{"base_handle": b.Handle, "base_view": b.DefaultView, "new_handle": n.Handle, "new_view": n.DefaultView}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	var calls []diffGoldenCall
	record := func(args map[string]any) diffGoldenCall {
		text, isErr := c.call("diff", args)
		call := diffGoldenCall{Args: args, IsError: isErr, Text: text}
		calls = append(calls, call)
		return call
	}
	record(pair(nil))
	first := record(pair(map[string]any{"limit": 2}))
	var page struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(first.Text), &page); err != nil || page.NextCursor == "" {
		t.Fatalf("the first page has no cursor: %v %s", err, first.Text)
	}
	record(pair(map[string]any{"limit": 2, "cursor": page.NextCursor}))
	record(pair(map[string]any{"work_base": 10, "work_new": 20}))
	record(pair(nil)) // a cache hit
	record(map[string]any{"base_handle": g.Handle, "base_view": g.DefaultView, "new_handle": g.Handle, "new_view": g.DefaultView})
	record(pair(map[string]any{"work_base": -1}))
	record(pair(map[string]any{"limit": 0}))
	record(pair(map[string]any{"base_handle": "h-nope"}))
	record(pair(map[string]any{"new_handle": "h-nope"}))
	record(pair(map[string]any{"new_view": "cpu/nanoseconds#7"}))
	record(pair(map[string]any{"new_handle": g.Handle, "new_view": g.DefaultView}))
	record(pair(map[string]any{"cursor": "c1.1.0000000000000000"}))

	got, err := json.MarshalIndent(calls, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "diff.golden.json")
	if *updateDiffGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFile(t, golden); !bytes.Equal(got, want) {
		t.Errorf("the diff tool's answers differ from %s:\n%s", golden, got)
	}
}
