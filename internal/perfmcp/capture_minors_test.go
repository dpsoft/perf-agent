package perfmcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// A job's elapsed stops at its end: a wait long after a capture failed
// or finished shows how long it took, not how long ago it began.
func TestElapsedStopsAtTheEnd(t *testing.T) {
	e := newJobEnv(t, jobOpts{})
	j, v := e.run(e.request(300 * time.Millisecond))
	if v.Status != string(jobDone) {
		t.Fatalf("%+v", v)
	}
	time.Sleep(400 * time.Millisecond)
	if later := j.view(); later.Elapsed != v.Elapsed {
		t.Errorf("elapsed went from %s to %s after the end", v.Elapsed, later.Elapsed)
	}
}

// A result cut in several steps carries one cut note, the last step's,
// which says what is left; the capture's own notes stay.
func TestACutResultCarriesOneCutNote(t *testing.T) {
	big := strings.Repeat("x", 4096)
	res := map[string]any{
		"capture": map[string]any{"id": "001", "series": "s", "label": "r", "manifest": "m",
			"workload_output_tail": map[string]any{"stdout": big, "stderr": "", "untrusted": true}},
		"profiles": []any{map[string]any{"mode": "cpu", "path": "p", "open": map[string]any{
			"handle": "h", "views": []any{big}, "default_view": "v", "quality": map[string]any{}, "capabilities": big}}},
		"compare": map[string]any{"views": []any{map[string]any{"view": "v", "base_total": "1", "new_total": "2", "rows": []any{big}, "next": map[string]any{}}}},
		"notes":   []string{"a note of the capture"},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	out := (&Server{}).cutResult(b, 2048)
	var got struct {
		Notes []string `json:"notes"`
	}
	if err := json.Unmarshal(out, &got); err != nil || len(out) > 2048 {
		t.Fatalf("cut to %d bytes: %s (%v)", len(out), out, err)
	}
	want := cutState{opens: true, opensMin: true, rows: true, tail: true}.note()
	if !slices.Equal(got.Notes, []string{"a note of the capture", want}) {
		t.Errorf("notes %q, want the capture's and %q", got.Notes, want)
	}
	for _, part := range []string{cutOpenMinPart, cutRowsPart, cutTailPart, cutOpenAdvice, cutDiffAdvice} {
		if !strings.Contains(want, part) {
			t.Errorf("the cut note %q does not say %q", want, part)
		}
	}
	if strings.Contains(want, cutOpenPart) {
		t.Errorf("the cut note %q says what a later cut overtook", want)
	}
}
