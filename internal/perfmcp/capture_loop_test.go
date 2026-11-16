package perfmcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// rowsOf decodes the rows of a diff result, or of a compare view.
func rowsOf(t *testing.T, raw []byte) any {
	t.Helper()
	var m struct {
		Rows any `json:"rows"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Rows == nil {
		t.Fatalf("no rows in %s: %v", raw, err)
	}
	return m.Rows
}

// compareRows are the rows of each compare view of a result text, by view.
func compareRows(t *testing.T, text string) map[string]any {
	t.Helper()
	var r struct {
		Compare struct {
			Views []json.RawMessage `json:"views"`
		} `json:"compare"`
	}
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, v := range r.Compare.Views {
		var name struct {
			View string `json:"view"`
		}
		if err := json.Unmarshal(v, &name); err != nil {
			t.Fatal(err)
		}
		out[name.View] = rowsOf(t, v)
	}
	return out
}

// A loop: a kept base, then three candidates compared with it. Each gets
// the next id and a compare, and each view's next call replayed against
// diff gives the same rows.
func TestALoopOfABaseAndThreeCandidates(t *testing.T) {
	e, c := newCapEnv(t, jobOpts{})
	cmd := workloadCmd(t)
	base, _ := waitResult(t, c, startCapture(t, c, map[string]any{
		"command": cmd, "series": "loop", "label": "base", "duration": "1s", "keep": true,
	}))
	if base.Capture.ID != "001" || !base.Capture.Kept || len(base.Profiles) != 1 || base.Profiles[0].Open == nil || base.Derived != derivedReady {
		t.Fatalf("base %+v", base)
	}
	for i := 1; i <= 3; i++ {
		with := "001"
		if i == 2 {
			with = "loop/001"
		}
		r, text := waitResult(t, c, startCapture(t, c, map[string]any{
			"command": cmd, "series": "loop", "label": fmt.Sprintf("cand%d", i), "compare_with": with,
		}))
		if want := fmt.Sprintf("%03d", i+1); r.Capture.ID != want || r.Capture.CompareWith != "001" {
			t.Fatalf("candidate %d: id %s compare_with %q", i, r.Capture.ID, r.Capture.CompareWith)
		}
		if r.Capture.Settings.Duration != "1s" || r.Capture.StoppedBy != "duration" || r.Capture.Target.Exit != "" {
			t.Errorf("candidate %d: settings %+v, target %+v", i, r.Capture.Settings, r.Capture.Target)
		}
		if r.Compare == nil || r.Compare.Base.ID != "001" || r.Compare.Comparability["settings"] != "equal" || len(r.Compare.Views) == 0 {
			t.Fatalf("candidate %d: compare %+v", i, r.Compare)
		}
		live := compareRows(t, text)
		for _, v := range r.Compare.Views {
			if v.Next.Tool != "diff" {
				t.Fatalf("next %+v", v.Next)
			}
			args := v.Next.Arguments
			args["limit"] = e.s.lim.Capture.CompareRows
			got := c.callOK("diff", args, nil)
			if !reflect.DeepEqual(rowsOf(t, []byte(got)), live[v.View]) {
				t.Errorf("candidate %d: the replayed diff of %s differs", i, v.View)
			}
		}
	}
}

// A new server, as after a restart, compares with a baseline it never
// captured: it reopens it from its manifest, and answers a wait for it
// from the disk.
func TestCompareWithAcrossAServerRestart(t *testing.T) {
	first, c1 := newCapEnv(t, jobOpts{})
	waitResult(t, c1, startCapture(t, c1, map[string]any{"command": workloadCmd(t), "series": "rs", "label": "base", "duration": "1s"}))
	first.noteRecord()
	if err := c1.w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.s.Close(); err != nil {
		t.Fatal(err)
	}

	second, c2 := newCapEnv(t, jobOpts{profiles: first.profiles})
	r, _ := waitResult(t, c2, startCapture(t, c2, map[string]any{"command": workloadCmd(t), "series": "rs", "label": "cand", "compare_with": "001"}))
	if r.Capture.ID != "002" || r.Compare == nil || r.Compare.Base.ID != "001" || r.Compare.Base.Handle == "" || len(r.Compare.Views) == 0 {
		t.Fatalf("result %+v", r)
	}
	old, _ := waitResult(t, c2, "rs/001")
	if old.Derived != derivedRecompute || old.Handles != "reopen" || old.Recompute == nil || old.Recompute.Baseline != "none" {
		t.Errorf("the baseline's answer %+v", old)
	}
	second.noteRecord()
}

// substitute replaces each "${bind.field}" argument with that field of
// the result bound by that name.
func substitute(t *testing.T, args map[string]any, binds map[string]map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, v := range args {
		s, ok := v.(string)
		if ref, isRef := strings.CutPrefix(s, "${"); ok && isRef {
			name, field, _ := strings.Cut(strings.TrimSuffix(ref, "}"), ".")
			got, found := binds[name][field]
			if !found {
				t.Fatalf("%s is not bound", s)
			}
			out[k] = got
			continue
		}
		out[k] = v
	}
	return out
}

// replay runs a recompute recipe through the tools, binding results by
// name, and returns the text of each step.
func replay(t *testing.T, c *testClient, rc *recompute) []string {
	t.Helper()
	binds := map[string]map[string]any{}
	var texts []string
	for _, step := range rc.Steps {
		text := c.callOK(step.Tool, substitute(t, step.Arguments, binds), nil)
		texts = append(texts, text)
		if step.Bind != "" {
			var m map[string]any
			if err := json.Unmarshal([]byte(text), &m); err != nil {
				t.Fatal(err)
			}
			binds[step.Bind] = m
		}
	}
	return texts
}

// Once a job has left the registry, a wait by its capture id answers from
// the manifest with a recipe; every step of it, run with its bindings,
// gives the live job's compare rows exactly.
func TestRecomputeReplaysExactly(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.MaxJobs = 1 }})
	cmd := workloadCmd(t)
	waitResult(t, c, startCapture(t, c, map[string]any{"command": cmd, "series": "rp", "label": "base", "duration": "1s"}))
	live, liveText := waitResult(t, c, startCapture(t, c, map[string]any{"command": cmd, "series": "rp", "label": "cand", "compare_with": "001"}))
	if live.Derived != derivedReady {
		t.Fatalf("live %+v", live)
	}
	waitResult(t, c, startCapture(t, c, map[string]any{"command": cmd, "series": "other", "duration": "1s"}))

	old, _ := waitResult(t, c, "rp/002")
	if old.Derived != derivedRecompute || old.Handles != "reopen" || old.Recompute == nil || old.Recompute.Baseline != "present" {
		t.Fatalf("historical %+v", old)
	}
	texts := replay(t, c, old.Recompute)
	want := compareRows(t, liveText)
	var diffs int
	for i, step := range old.Recompute.Steps {
		if step.Tool != "diff" {
			continue
		}
		diffs++
		var got struct {
			View string `json:"view"`
		}
		if err := json.Unmarshal([]byte(texts[i]), &got); err != nil {
			t.Fatal(err)
		}
		if rows, ok := want[got.View]; !ok || !reflect.DeepEqual(rowsOf(t, []byte(texts[i])), rows) {
			t.Errorf("step %d: the replayed rows differ from the live compare's", i)
		}
	}
	if diffs != len(want) || diffs == 0 {
		t.Errorf("%d diffs for %d views", diffs, len(want))
	}
}

// A capture whose baseline retention has since removed is answered with
// a recipe that says so: it reopens the capture and has no diff.
func TestRecomputeReportsARetainedAwayBaseline(t *testing.T) {
	first := newJobEnv(t, jobOpts{})
	st := capture.Settings{Modes: cpuOnly, Duration: time.Second, SampleRate: 99, Unwind: "auto"}
	first.commitSynthetic("r", "base", st, manifest{})
	first.commitSynthetic("r", "cand", st, manifest{CompareWith: "001", Kept: true})
	first.commitSynthetic("r", "base2", st, manifest{})
	first.commitSynthetic("r", "cand2", st, manifest{CompareWith: "003"})
	if err := first.s.Close(); err != nil {
		t.Fatal(err)
	}

	// A new server keeps two unkept captures: its start-up retention
	// removes 001, which no newer capture compares with.
	second, c := newCapEnv(t, jobOpts{profiles: first.profiles, limits: func(l *Limits) { l.Capture.KeepPerSeries = 2 }})
	if !eventuallyTrue(20*time.Second, func() bool { _, ok := second.manifestOf("r", "001"); return !ok }) {
		t.Fatal("retention kept 001")
	}
	r, _ := waitResult(t, c, "r/002")
	if r.Recompute == nil || !strings.Contains(r.Recompute.Baseline, "baseline 001 is gone") {
		t.Fatalf("recompute %+v", r.Recompute)
	}
	for _, step := range r.Recompute.Steps {
		if step.Tool != "open" || strings.Contains(fmt.Sprint(step.Arguments), "001-") {
			t.Errorf("step %+v", step)
		}
	}
	replay(t, c, r.Recompute)
}
