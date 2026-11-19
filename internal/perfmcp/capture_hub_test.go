package perfmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// newHubEnv is a capture-enabled server with the live page, as the hub
// runs it; its sessions are answered through the per-message API.
func newHubEnv(t *testing.T, o jobOpts) *jobEnv {
	t.Helper()
	o.live = true
	return newJobEnv(t, o)
}

// hubArgs are capture's arguments for the workload sleeping, for d.
func hubArgs(t *testing.T, d string) map[string]any {
	return map[string]any{"command": workloadCmd(t), "duration": d}
}

// hubCapture calls capture from c, which must accept it, and returns the
// job id.
func hubCapture(t *testing.T, c *mcpSession, args map[string]any) string {
	t.Helper()
	var a acceptedAnswer
	c.callOK("capture", args, &a)
	if a.Status != string(jobAccepted) || a.Job == "" {
		t.Fatalf("capture answered %+v", a)
	}
	return a.Job
}

// hubWaitEnd calls capture_wait from c until id ends: its result, or its
// error.
func hubWaitEnd(t *testing.T, c *mcpSession, id string) (string, bool) {
	t.Helper()
	var text string
	for end := time.Now().Add(90 * time.Second); time.Now().Before(end); {
		var isErr bool
		text, isErr = c.call(t.Context(), "capture_wait", map[string]any{"id": id, "timeout": "10s"})
		if isErr || isResult(t, text) {
			return text, isErr
		}
	}
	t.Fatalf("%s never ended: %s", id, text)
	return "", false
}

// hubResult is the result of id, which must end done.
func hubResult(t *testing.T, c *mcpSession, id string) captureResult {
	t.Helper()
	text, isErr := hubWaitEnd(t, c, id)
	if isErr {
		t.Fatalf("%s failed: %s", id, text)
	}
	var r captureResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("result %s: %v", text, err)
	}
	return r
}

// hubWaitState calls capture_wait from c with short timeouts until id's
// state passes ok.
func hubWaitState(t *testing.T, c *mcpSession, id string, ok func(jobView) bool) jobView {
	t.Helper()
	var v jobView
	for end := time.Now().Add(60 * time.Second); time.Now().Before(end); {
		text, isErr := c.call(t.Context(), "capture_wait", map[string]any{"id": id, "timeout": "200ms"})
		if isErr || isResult(t, text) {
			t.Fatalf("%s ended: %s", id, text)
		}
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		if ok(v) {
			return v
		}
	}
	t.Fatalf("%s never reached the state: %+v", id, v)
	return v
}

// pubIDs are the kept publications in l.pubs order, least recently shown
// first. Only a test that runs no tool meanwhile may read them.
func pubIDs(s *Server) []string {
	var ids []string
	for _, p := range s.live.pubs {
		ids = append(ids, p.id)
	}
	return ids
}

// §D2.4: capture admits in memory, so it is accepted at once while another
// session's tool holds the engine; the job then waits for the engine
// before it writes anything of its series.
func TestHubAcceptsWhileAnotherToolHoldsTheEngine(t *testing.T) {
	h := newEngineHold()
	e := newHubEnv(t, jobOpts{setup: func(s *Server) { s.holdEngine = h.hold }})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	a, b := newMCPSession(t, e.s, "A"), newMCPSession(t, e.s, "B")
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	done := holdTop(t, h, a, o)

	start := time.Now()
	job := hubCapture(t, b, hubArgs(t, "1s"))
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("capture took %v while the engine was held, want under 50ms", took)
	}
	hubWaitState(t, b, job, func(v jobView) bool { return v.Phase == "retention" })
	series := filepath.Join(e.profiles, defaultStdioCaptures, defaultSeries)
	for range 5 {
		if _, err := os.Lstat(series); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the series appeared while the engine was held: %v", err)
		}
		if hd, ok := e.s.engine.holding(); !ok || hd.tool != "top" {
			t.Fatalf("the engine is held by %+v, want top", hd)
		}
		time.Sleep(100 * time.Millisecond)
	}
	close(h.release)
	if text := <-done; strings.HasPrefix(text, "isError") {
		t.Fatalf("top: %s", text)
	}
	if r := hubResult(t, b, job); r.Derived != derivedReady || r.Publication == "" {
		t.Errorf("the job ended %q (%s), publication %q", r.Derived, r.DeriveError, r.Publication)
	}
}

// MaxEngineWait bounds tool calls only (§D2.4). A job's
// derivation waits behind a tool held three times that long, while
// another session's tool is told who holds the engine, and still ends
// ready.
func TestAJobsEngineStepsWaitBehindToolsAndComplete(t *testing.T) {
	const wait = 200 * time.Millisecond
	h := newEngineHold()
	reached, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e := newHubEnv(t, jobOpts{
		limits: func(l *Limits) { l.MaxEngineWait = wait },
		setup: func(s *Server) {
			s.holdEngine = h.hold
			s.beforeRelease = func(*job) {
				once.Do(func() { close(reached) })
				<-proceed
			}
		},
	})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	a, b, c := newMCPSession(t, e.s, "A"), newMCPSession(t, e.s, "B"), newMCPSession(t, e.s, "C")
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)

	job := hubCapture(t, b, hubArgs(t, "1s"))
	select {
	case <-reached:
	case <-time.After(60 * time.Second):
		t.Fatal("the job never committed")
	}
	done := holdTop(t, h, a, o)
	close(proceed)
	hubWaitState(t, b, job, func(v jobView) bool { return v.Phase == "deriving" })
	held := time.Now()

	start := time.Now()
	text, isErr := c.call(t.Context(), "top", map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "functions"})
	if took := time.Since(start); !isErr || !strings.Contains(text, `is busy with top from "A"`) || took < wait || took > wait+2*time.Second {
		t.Errorf("C's top after %v: %q (isError %v), want the busy text naming A's top", took, text, isErr)
	}
	time.Sleep(time.Until(held.Add(3 * wait)))
	if v := hubWaitState(t, b, job, func(jobView) bool { return true }); v.Status != string(jobDone) || v.Derived != derivedPending {
		t.Errorf("after 3×MaxEngineWait the job is %+v, want done and still deriving", v)
	}
	close(h.release)
	if text := <-done; strings.HasPrefix(text, "isError") {
		t.Fatalf("top: %s", text)
	}
	if r := hubResult(t, b, job); r.Derived != derivedReady || r.Publication == "" {
		t.Errorf("the job ended %q (%s), publication %q", r.Derived, r.DeriveError, r.Publication)
	}
}

// A committed capture is published facts-only: the first mode's view
// with no explanation, by the session that started the job, made current,
// under the title of the capture, its target, mode and profiled time. It
// is the server's page, not an attempt.
func TestADoneCaptureIsPublishedFactsOnly(t *testing.T) {
	e := newHubEnv(t, jobOpts{})
	a := newMCPSession(t, e.s, "claude-code")
	r := hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	if r.Publication != "p1" || r.DiffPublication != "" || r.Derived != derivedReady {
		t.Fatalf("publication %q, diff %q, derived %q (%s)", r.Publication, r.DiffPublication, r.Derived, r.DeriveError)
	}
	st := stateBy(t, e.s)
	if st.Current != "p1" || st.By != "claude-code" || len(st.Publications) != 1 || st.Publications[0].By != "claude-code" {
		t.Errorf("state %+v", st)
	}
	snap := e.s.live.snapshot()
	p := snap.pubs["p1"]
	if r.Capture.Target.Comm != "capworkload" || r.Capture.Target.PID <= 0 || r.Capture.ProfiledDuration == "" {
		t.Fatalf("target %+v, profiled %q", r.Capture.Target, r.Capture.ProfiledDuration)
	}
	want := fmt.Sprintf("adhoc/001-run · capworkload (%d) · cpu · %s", r.Capture.Target.PID, r.Capture.ProfiledDuration)
	if p.title != want || p.kind != pubExplained || len(p.result.Checked) != 0 || p.view != r.Profiles[0].Open.DefaultView || p.attempt != "" {
		t.Errorf("p1: title %q (want %q), kind %s, %d explained, view %s, attempt %q", p.title, want, p.kind, len(p.result.Checked), p.view, p.attempt)
	}
	if snap.selected != "" || snap.focus != "" {
		t.Errorf("a facts page selects %q and focuses %q", snap.selected, snap.focus)
	}
	if len(p.files) == 0 {
		t.Error("the publication records no file")
	}
}

// show false lists the publication and steers nothing: what the page shows
// stays, and the publication goes before the current one, so it is dropped
// before it. The page tools still steer, to it too.
func TestShowFalseOnlyLists(t *testing.T) {
	e := newHubEnv(t, jobOpts{})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	a, b := newMCPSession(t, e.s, "A"), newMCPSession(t, e.s, "B")
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	a.callOK("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}}, nil)
	var top struct {
		Rows []struct {
			Node string `json:"node"`
		} `json:"rows"`
	}
	a.callOK("top", map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "nodes"}, &top)
	if len(top.Rows) == 0 {
		t.Fatal("top found no node")
	}
	node := top.Rows[0].Node
	a.callOK("page_show", map[string]any{"node": node, "focus": true}, nil)
	before := e.s.live.snapshot()
	if before.current != "p1" || before.selected != node || before.focus != node || before.by != "A" {
		t.Fatalf("before the capture: current %q selected %q focus %q by %q", before.current, before.selected, before.focus, before.by)
	}

	args := hubArgs(t, "1s")
	args["show"] = false
	r := hubResult(t, b, hubCapture(t, b, args))
	if r.Publication != "p2" {
		t.Fatalf("publication %q (%s)", r.Publication, r.DeriveError)
	}
	after := e.s.live.snapshot()
	if after.current != before.current || after.selected != before.selected || after.focus != before.focus || after.by != before.by {
		t.Errorf("show false moved the page: current %q selected %q focus %q by %q", after.current, after.selected, after.focus, after.by)
	}
	if !slices.Equal(after.order, []string{"p1", "p2"}) || after.pubs["p2"].by != "B" {
		t.Errorf("listed %q, p2 by %q", after.order, after.pubs["p2"].by)
	}
	if ids := pubIDs(e.s); !slices.Equal(ids, []string{"p2", "p1"}) {
		t.Errorf("kept in the order %q, want p2 before the current p1", ids)
	}

	b.callOK("page_show", map[string]any{"publication": "p2"}, nil)
	if st := stateBy(t, e.s); st.Current != "p2" || st.By != "B" {
		t.Errorf("page_show p2: %+v", st)
	}
	a.callOK("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}}, nil)
	if st := stateBy(t, e.s); st.Current != "p3" || st.By != "A" {
		t.Errorf("page_explain after the capture: %+v", st)
	}
}

// A capture compared with a baseline adds a diff publication of the first
// mode's views, which show makes current; both are the starter's. With
// show false the pair is only listed.
func TestACompareAddsADiffPublication(t *testing.T) {
	e := newHubEnv(t, jobOpts{})
	a := newMCPSession(t, e.s, "A")
	if r := hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s"))); r.Publication != "p1" {
		t.Fatalf("001: publication %q (%s)", r.Publication, r.DeriveError)
	}
	r := hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "label": "cand", "compare_with": "001"}))
	if r.Publication != "p2" || r.DiffPublication != "p3" || r.Derived != derivedReady {
		t.Fatalf("002: publication %q, diff %q, derived %q (%s)", r.Publication, r.DiffPublication, r.Derived, r.DeriveError)
	}
	st := stateBy(t, e.s)
	if st.Current != "p3" || st.By != "A" {
		t.Errorf("state %+v, want the diff current, by A", st)
	}
	snap := e.s.live.snapshot()
	facts, diff := snap.pubs["p2"], snap.pubs["p3"]
	if facts.kind != pubExplained || facts.by != "A" || diff.kind != pubDiff || diff.by != "A" {
		t.Errorf("p2 %s by %q, p3 %s by %q", facts.kind, facts.by, diff.kind, diff.by)
	}
	if want := "adhoc/002-cand vs 001-run · cpu"; diff.title != want {
		t.Errorf("diff title %q, want %q", diff.title, want)
	}
	base, _ := e.manifestOf("adhoc", "001")
	cand, _ := e.manifestOf("adhoc", "002")
	for _, f := range slices.Concat(base.Files, cand.Files) {
		if !slices.Contains(diff.files, fileKey{f.Dev, f.Ino}) {
			t.Errorf("the diff does not record %s", f.Name)
		}
	}
	for _, f := range cand.Files {
		if !slices.Contains(facts.files, fileKey{f.Dev, f.Ino}) {
			t.Errorf("the facts page does not record %s", f.Name)
		}
	}

	r = hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "label": "cand2", "compare_with": "001", "show": false}))
	if r.Publication != "p4" || r.DiffPublication != "p5" {
		t.Fatalf("003: publication %q, diff %q (%s)", r.Publication, r.DiffPublication, r.DeriveError)
	}
	if st := stateBy(t, e.s); st.Current != "p3" {
		t.Errorf("show false moved the page to %q", st.Current)
	}
	if ids := pubIDs(e.s); ids[len(ids)-1] != "p3" {
		t.Errorf("kept in the order %q, want the current p3 last", ids)
	}
}

// A publication that cannot be made is a derivation failure: the capture
// stays committed with its files, and its recipe publishes it again.
func TestAPublicationFailureIsDerivedFailedAndKeepsTheFiles(t *testing.T) {
	e := newHubEnv(t, jobOpts{limits: func(l *Limits) { l.MaxPageBytes = 512 }})
	a := newMCPSession(t, e.s, "A")
	r := hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	if r.Derived != derivedFailed || !strings.HasPrefix(r.DeriveError, "publication: ") || !strings.Contains(r.DeriveError, "MaxPageBytes=512") {
		t.Fatalf("derived %q, derive_error %q", r.Derived, r.DeriveError)
	}
	if r.Publication != "" || len(e.s.live.snapshot().order) != 0 {
		t.Errorf("publication %q, listed %q", r.Publication, e.s.live.snapshot().order)
	}
	if m, ok := e.manifestOf("adhoc", "001"); !ok || m.Outcome != captureOK {
		t.Errorf("manifest %+v (found %v)", m, ok)
	}
	if names := e.seriesNames("adhoc"); !slices.Contains(names, "001-run-cpu.pb.gz") || !slices.Contains(names, "001-run.capture.json") {
		t.Errorf("series holds %q", names)
	}
	if r.Recompute == nil || !slices.ContainsFunc(r.Recompute.Steps, func(c toolCall) bool { return c.Tool == "page_explain" }) {
		t.Errorf("recompute %+v", r.Recompute)
	}
	if d := e.s.registry.list(); len(d) != 1 || d[0].view().Derived != derivedFailed {
		t.Errorf("the job is not derived failed in the registry")
	}
}

// schemaOf is each tool's input schema, from tools/list.
func schemaOf(t *testing.T, c *mcpSession) map[string]json.RawMessage {
	t.Helper()
	var tl struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(c.handle(t.Context(), "tools/list", map[string]any{}), &tl); err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	for _, tool := range tl.Result.Tools {
		out[tool.Name] = tool.InputSchema
	}
	return out
}

// checkAgainstSchema fails the test for args that schema does not
// describe: an unknown name, a required one missing, or a value of
// another JSON type.
func checkAgainstSchema(t *testing.T, tool string, schema json.RawMessage, args map[string]any) {
	t.Helper()
	var sc struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(schema, &sc); err != nil {
		t.Fatalf("%s's schema: %v", tool, err)
	}
	for _, r := range sc.Required {
		if _, ok := args[r]; !ok {
			t.Errorf("%s: required %s is missing from %v", tool, r, args)
		}
	}
	for k, v := range args {
		p, ok := sc.Properties[k]
		if !ok {
			t.Errorf("%s takes no argument %s", tool, k)
			continue
		}
		var fits bool
		switch p.Type {
		case "string":
			_, fits = v.(string)
		case "integer":
			f, ok := v.(float64)
			fits = ok && f == float64(int64(f))
		case "array":
			_, fits = v.([]any)
		case "":
			fits = true
		}
		if !fits {
			t.Errorf("%s: argument %s is %T, its schema says %s", tool, k, v, p.Type)
		}
	}
}

// bindArgs is args with every "${<bind>.<field>}" replaced by that field
// of the result bound by that name.
func bindArgs(t *testing.T, args map[string]any, bound map[string]map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, v := range args {
		if s, ok := v.(string); ok {
			if ref, ok := strings.CutPrefix(s, "${"); ok && strings.HasSuffix(ref, "}") {
				name, field, _ := strings.Cut(strings.TrimSuffix(ref, "}"), ".")
				got, ok := bound[name][field]
				if !ok {
					t.Fatalf("%s binds %s, which no earlier step made", k, s)
				}
				v = got
			}
		}
		out[k] = v
	}
	return out
}

// withoutAttempt is a live model's JSON without its attempt, which only
// an agent's page_explain records.
func withoutAttempt(t *testing.T, model []byte) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(model, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "attempt")
	return mustJSON(t, m)
}

// After the registry forgot a compare, a wait by its capture id answers
// with the recipe; run as it is, step by step through the bindings, every
// call fits its tool's schema and the page shows the capture again: its
// facts page, then its diff, current, each the same page as the capture's
// own (the recipe's facts page is an agent's attempt, the capture's is
// not).
func TestRecomputeInTheHubRepublishes(t *testing.T) {
	e := newHubEnv(t, jobOpts{limits: func(l *Limits) { l.Capture.MaxJobs = 1 }})
	a := newMCPSession(t, e.s, "A")
	hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	e.fakeProfile(pmfixture.CPUBasicNew())
	own := hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "label": "cand", "compare_with": "001"}))
	e.fakeProfile(thirdProfile())
	hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))

	r := hubResult(t, a, "adhoc/002")
	if r.Derived != derivedRecompute || r.Recompute == nil || r.Publication != "" {
		t.Fatalf("derived %q, recompute %+v, publication %q", r.Derived, r.Recompute, r.Publication)
	}
	var tools []string
	for _, st := range r.Recompute.Steps {
		tools = append(tools, st.Tool)
	}
	if want := []string{"open", "open", "diff", "page_explain", "page_diff"}; !slices.Equal(tools, want) {
		t.Fatalf("steps %q, want %q", tools, want)
	}
	schemas := schemaOf(t, a)
	before := e.s.live.snapshot()
	bound := map[string]map[string]any{}
	for _, st := range r.Recompute.Steps {
		args := bindArgs(t, st.Arguments, bound)
		checkAgainstSchema(t, st.Tool, schemas[st.Tool], args)
		text := a.callOK(st.Tool, args, nil)
		if st.Bind != "" {
			var res map[string]any
			if err := json.Unmarshal([]byte(text), &res); err != nil {
				t.Fatal(err)
			}
			bound[st.Bind] = res
		}
	}
	after := e.s.live.snapshot()
	if len(after.order) != len(before.order)+2 {
		t.Fatalf("listed %q before and %q after", before.order, after.order)
	}
	facts, diff := after.pubs[after.order[len(after.order)-2]], after.pubs[after.order[len(after.order)-1]]
	if facts.kind != pubExplained || diff.kind != pubDiff || after.current != diff.id || facts.view != bound["new_cpu"]["default_view"] {
		t.Errorf("republished %s (%s, view %s) and %s (%s), current %s", facts.id, facts.kind, facts.view, diff.id, diff.kind, after.current)
	}
	ownFacts, ownDiff := after.pubs[own.Publication], after.pubs[own.DiffPublication]
	if ownFacts == nil || ownDiff == nil {
		t.Fatalf("the capture's own publications %s and %s are gone: %q", own.Publication, own.DiffPublication, after.order)
	}
	if facts.title != ownFacts.title || diff.title != ownDiff.title {
		t.Errorf("titles %q and %q, the capture's %q and %q", facts.title, diff.title, ownFacts.title, ownDiff.title)
	}
	if facts.attempt == "" || ownFacts.attempt != "" {
		t.Errorf("the recipe's page has attempt %q, the capture's %q", facts.attempt, ownFacts.attempt)
	}
	if got, want := withoutAttempt(t, facts.model), withoutAttempt(t, ownFacts.model); got != want {
		t.Errorf("the republished facts page differs:\n got %s\nwant %s", got, want)
	}
	if string(diff.model) != string(ownDiff.model) {
		t.Errorf("the republished diff differs:\n got %s\nwant %s", diff.model, ownDiff.model)
	}
}

// thirdProfile is a cpu profile unlike CPUBasic and CPUBasicNew.
func thirdProfile() []byte {
	return pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("third")}, Value: 1000})
}

// fakeProfile makes e's fake perf-agent write b as its cpu profile from
// its next capture on.
func (e *jobEnv) fakeProfile(b []byte) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(filepath.Dir(e.fake), "fixtures", "cpu.pb.gz"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// Retention keeps a capture the page keeps, by its files, whatever the
// store has open; once its publication is dropped, the next capture's
// retention removes it.
func TestRetentionSparesPublishedCaptures(t *testing.T) {
	e := newHubEnv(t, jobOpts{limits: func(l *Limits) {
		l.Capture.KeepPerSeries = 1
		l.MaxOpenHandles = 1
		l.MaxPublications = 2
	}})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	a := newMCPSession(t, e.s, "A")
	if r := hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s"))); r.Publication != "p1" {
		t.Fatalf("001: %q (%s)", r.Publication, r.DeriveError)
	}
	// 002's open evicts 001's handle: only p1 keeps 001's files now. Its
	// profile differs, so it has an entry of its own.
	e.fakeProfile(pmfixture.CPUBasicNew())
	if r := hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s"))); r.Publication != "p2" {
		t.Fatalf("002: %q (%s)", r.Publication, r.DeriveError)
	}
	if names := e.s.store.openNames(); len(names) != 1 {
		t.Fatalf("the store has %v open", names)
	}
	e.s.maintenance(true)
	if _, ok := e.manifestOf("adhoc", "001"); !ok || !slices.Contains(e.seriesNames("adhoc"), "001-run-cpu.pb.gz") {
		t.Fatalf("retention removed the published 001: %q", e.seriesNames("adhoc"))
	}

	// A third publication drops p1, the least recently shown.
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	a.callOK("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}}, nil)
	if snap := e.s.live.snapshot(); snap.pubs["p1"] != nil {
		t.Fatalf("p1 is still kept: %q", snap.order)
	}
	e.fakeProfile(thirdProfile())
	r := hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	if !slices.Contains(r.Evicted, "adhoc/001-run") {
		t.Errorf("evicted %q, want adhoc/001-run", r.Evicted)
	}
	if _, ok := e.manifestOf("adhoc", "001"); ok {
		t.Errorf("001 is still there: %q", e.seriesNames("adhoc"))
	}
	if _, ok := e.manifestOf("adhoc", "002"); !ok {
		t.Errorf("002, the newest before 003, was removed: %q", e.seriesNames("adhoc"))
	}
}

// Without the live page capture takes no show (refused as an unknown
// argument, which the message no longer lists), and a result names no
// publication; the hub's schema has show.
func TestStdioHasNoShowAndNoPublication(t *testing.T) {
	_, c := newCapEnv(t, jobOpts{})
	args := map[string]any{"command": workloadCmd(t), "duration": "1s", "show": false}
	text, isErr := c.call("capture", args)
	w := toolErrorOf(t, text)
	if !isErr || w.Code != string(capture.CodeInvalidArgument) || !strings.Contains(w.Message, `unknown argument "show"`) || strings.Contains(w.Message, "show,") || strings.HasSuffix(w.Message, "show") {
		t.Errorf("capture with show: %s", text)
	}
	var list struct {
		Tools []struct {
			Name        string `json:"name"`
			InputSchema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	for _, tool := range list.Tools {
		if _, ok := tool.InputSchema.Properties["show"]; ok && tool.Name == "capture" {
			t.Errorf("stdio's capture schema has show")
		}
	}
	delete(args, "show")
	_, raw := waitResult(t, c, startCapture(t, c, args))
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"publication", "diff_publication", "recompute"} {
		if _, ok := keys[k]; ok {
			t.Errorf("stdio's result has %s: %s", k, raw)
		}
	}
	hub := newHubEnv(t, jobOpts{})
	var show struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemaOf(t, newMCPSession(t, hub.s, "A"))["capture"], &show); err != nil || show.Properties["show"].Type != "boolean" {
		t.Errorf("the hub's capture schema: show %+v (%v)", show.Properties["show"], err)
	}
}

// §D2.5: a job outlives the session that started it; two sessions wait
// on it at once, each to its own deadline, and one's cancel ends it for
// the other.
func TestMultiSessionWaitsAndCancel(t *testing.T) {
	e := newHubEnv(t, jobOpts{})
	a, b, c := newMCPSession(t, e.s, "A"), newMCPSession(t, e.s, "B"), newMCPSession(t, e.s, "C")
	job := hubCapture(t, a, hubArgs(t, "1m"))
	// A is gone from here on: nothing of its session is used again.
	hubWaitState(t, b, job, profiling)

	type waited struct {
		text  string
		isErr bool
		took  time.Duration
	}
	wait := func(c *mcpSession, timeout string, out *waited) {
		start := time.Now()
		out.text, out.isErr = c.call(t.Context(), "capture_wait", map[string]any{"id": job, "timeout": timeout})
		out.took = time.Since(start)
	}
	var bw, cw waited
	var wg sync.WaitGroup
	wg.Go(func() { wait(b, "1s", &bw) })
	wg.Go(func() { wait(c, "3s", &cw) })
	wg.Wait()
	for _, w := range []struct {
		name     string
		got      waited
		min, max time.Duration
	}{{"B", bw, time.Second, 2500 * time.Millisecond}, {"C", cw, 3 * time.Second, 4500 * time.Millisecond}} {
		var v jobView
		if w.got.isErr || json.Unmarshal([]byte(w.got.text), &v) != nil || v.Status != string(jobRunning) {
			t.Errorf("%s's wait: %s", w.name, w.got.text)
		}
		if w.got.took < w.min || w.got.took > w.max {
			t.Errorf("%s's wait took %v, want [%v, %v]", w.name, w.got.took, w.min, w.max)
		}
	}
	var ca cancelAnswer
	b.callOK("capture_cancel", map[string]any{"id": job}, &ca)
	if ca.Status != cancelAnswerCancelling {
		t.Errorf("B's cancel: %+v", ca)
	}
	text, isErr := hubWaitEnd(t, c, job)
	if w := toolErrorOf(t, text); !isErr || w.Code != string(capture.CodeCancelled) || w.Status != string(jobCancelled) {
		t.Errorf("C's wait after B's cancel: %s", text)
	}
}

// A wait whose call ends before the job settles (the client cancelled it,
// its session ended, or the server is closing) answers an error naming the
// job and its capture id, never the job's running state: a client that
// reads it after a restart must wait by the capture id.
func TestAWaitWhoseCallEndsNamesTheCaptureID(t *testing.T) {
	e := newHubEnv(t, jobOpts{})
	a := newMCPSession(t, e.s, "A")
	job := hubCapture(t, a, hubArgs(t, "1m"))
	v := hubWaitState(t, a, job, profiling)
	if v.ID == "" {
		t.Fatalf("a profiling job has no capture id: %+v", v)
	}
	waitEnded := func() (wireError, string) {
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		text, isErr := a.call(ctx, "capture_wait", map[string]any{"id": job, "timeout": "30s"})
		if !isErr {
			t.Fatalf("a wait whose call ended answered %s", text)
		}
		return toolErrorOf(t, text), text
	}

	w, text := waitEnded()
	if w.Code != string(capture.CodeCancelled) || w.Job != job || w.CaptureID != v.ID || w.Status != "" {
		t.Errorf("a cancelled wait: %s", text)
	}
	if !strings.Contains(w.Message, v.ID) || !strings.Contains(w.Message, "not cancelled") || !strings.Contains(w.Next, "capture_wait") || !strings.Contains(w.Next, v.ID) {
		t.Errorf("a cancelled wait says %q, next %q", w.Message, w.Next)
	}

	// As the hub stops: the captures are killed first, then every call's
	// context ends. The job is left running here so the wait sees it.
	e.s.killed.Store(true)
	w, text = waitEnded()
	if w.Code != string(capture.CodeClosing) || w.Job != job || w.CaptureID != v.ID || w.Status != "" {
		t.Errorf("a wait on a closing server: %s", text)
	}
	if !strings.Contains(w.Message, v.ID) || !strings.Contains(w.Next, "restart") || !strings.Contains(w.Next, "capture_wait") || !strings.Contains(w.Next, v.ID) {
		t.Errorf("a wait on a closing server says %q, next %q", w.Message, w.Next)
	}
	var ca cancelAnswer
	a.callOK("capture_cancel", map[string]any{"id": job}, &ca)
	hubWaitEnd(t, a, job)
}

// In the hub, with the registry evicted, waits by capture id read
// the manifest through the lookup workers. While that read is blocked
// each wait answers loading at its own deadline and starts no goroutine;
// then the answer is the recipe, with the page step, and nothing derived
// is written.
func TestHistoricalLookupsInTheHub(t *testing.T) {
	var blocked atomic.Pointer[chan struct{}]
	e := newHubEnv(t, jobOpts{
		limits: func(l *Limits) { l.Capture.MaxJobs = 1 },
		setup: func(s *Server) {
			s.lookupHook = func(k string) {
				if strings.HasPrefix(k, "list:") {
					return
				}
				if ch := blocked.Load(); ch != nil {
					<-*ch
				}
			}
		},
	})
	a := newMCPSession(t, e.s, "A")
	hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	names := e.seriesNames("adhoc")

	gate := make(chan struct{})
	blocked.Store(&gate)
	created := goroutinesCreated()
	for range 4 {
		start := time.Now()
		text, isErr := a.call(t.Context(), "capture_wait", map[string]any{"id": "adhoc/001", "timeout": "300ms"})
		took := time.Since(start)
		var v struct {
			Status string `json:"status"`
		}
		if isErr || json.Unmarshal([]byte(text), &v) != nil || v.Status != statusLoading {
			t.Fatalf("a wait while the read is blocked: %s", text)
		}
		if took < 300*time.Millisecond || took > 1500*time.Millisecond {
			t.Errorf("a loading wait took %v, want its 300ms deadline", took)
		}
	}
	if n := goroutinesCreated() - created; n > 4 {
		t.Errorf("%d goroutines were created by 4 waits", n)
	}
	blocked.Store(nil)
	close(gate)
	r := hubResult(t, a, "adhoc/001")
	if r.Derived != derivedRecompute || r.Recompute == nil || !slices.ContainsFunc(r.Recompute.Steps, func(c toolCall) bool { return c.Tool == "page_explain" }) {
		t.Errorf("derived %q, recompute %+v", r.Derived, r.Recompute)
	}
	if got := e.seriesNames("adhoc"); !slices.Equal(got, names) {
		t.Errorf("the series held %q and now %q", names, got)
	}
}

// The running job's row names the session that started it, and so does
// the engine while the job holds it.
func TestCaptureListShowsWhoStartedTheRunningJob(t *testing.T) {
	var (
		mu   sync.Mutex
		busy string
	)
	e := newHubEnv(t, jobOpts{setup: func(s *Server) {
		fault := s.captureFault
		s.captureFault = func(p faultPoint) error {
			if p == faultInEngine {
				mu.Lock()
				busy = s.busyText()
				mu.Unlock()
			}
			return fault(p)
		}
	}})
	codex, claude := newMCPSession(t, e.s, "codex"), newMCPSession(t, e.s, "claude-code")
	job := hubCapture(t, codex, hubArgs(t, "1m"))
	hubWaitState(t, claude, job, profiling)
	var l listing
	claude.callOK("capture_list", map[string]any{}, &l)
	if len(l.Jobs) != 1 || l.Jobs[0].Job != job || l.Jobs[0].By != "codex" {
		t.Errorf("jobs %+v, want %s by codex", l.Jobs, job)
	}
	mu.Lock()
	if want := `is busy with capture ` + job + ` from "codex"`; !strings.Contains(busy, want) {
		t.Errorf("while the job held the engine: %q, want %q", busy, want)
	}
	mu.Unlock()
	claude.callOK("capture_cancel", map[string]any{"id": job}, nil)
	hubWaitEnd(t, claude, job)
}

// cancelAnswers are what capture_cancel answers a job.
var cancelAnswers = []string{cancelAnswerCancelling, cancelAnswerCancelled, cancelAnswerTooLate, cancelAnswerEnded, cancelAnswerPending}

// waitStatuses are the statuses a capture_wait answer that is not a result
// may have.
var waitStatuses = []string{
	string(jobAccepted), string(jobPreparing), string(jobRunning), string(jobCommitting), string(jobDone),
	statusLoading, statusInProgress,
}

// activeJobs counts the jobs that hold the active slot by their state:
// not ended, not ending, and not an undecided commit that left it.
func activeJobs(s *Server) int {
	n := 0
	for _, j := range s.registry.list() {
		j.mu.Lock()
		if !j.status.terminal() && j.ending == "" && j.phase != phaseUndecided {
			n++
		}
		j.mu.Unlock()
	}
	return n
}

// Four sessions call the four capture tools at once, over and over, on
// short captures, while two more open profiles and steer the page as each
// capture is published. Every answer is one the state machine gives, one
// capture at most is active at any time, every capture that ends done is
// published, and the kept publications keep their order.
func TestFourSessionsRaceTheCaptureTools(t *testing.T) {
	const maxPubs = 3
	e := newHubEnv(t, jobOpts{limits: func(l *Limits) {
		l.Capture.WindowCaptures = 1000
		// The racing sessions finish many jobs; the steady one must stay
		// in the registry, or its wait answers from the manifest.
		l.Capture.MaxJobs = 4096
		l.MaxPublications = maxPubs
		l.MaxOpenHandles = 2
	}})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	w := workloadCmd(t)
	var (
		mu      sync.Mutex
		jobs    []string
		results []captureResult
	)
	// done notes a capture_wait answer that is a result.
	done := func(text string) {
		var r captureResult
		if json.Unmarshal([]byte(text), &r) == nil && r.Capture.ID != "" {
			mu.Lock()
			results = append(results, r)
			mu.Unlock()
		}
	}
	known := func(r *rand.Rand) string {
		mu.Lock()
		defer mu.Unlock()
		if len(jobs) == 0 || r.IntN(4) == 0 {
			return "adhoc/001"
		}
		return jobs[r.IntN(len(jobs))]
	}
	codeOK := func(tool, text string) {
		var out struct {
			Error *wireError `json:"error"`
		}
		if err := json.Unmarshal([]byte(text), &out); err != nil || out.Error == nil {
			t.Errorf("%s: an error that is not one {\"error\": …} object: %s", tool, text)
			return
		}
		if _, ok := capture.DefaultRetryable[capture.Code(out.Error.Code)]; !ok {
			t.Errorf("%s: undocumented code %q: %s", tool, out.Error.Code, text)
		}
	}
	end := time.Now().Add(3 * time.Second)
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	var most atomic.Int32
	sampler.Go(func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Millisecond):
			}
			if n := int32(activeJobs(e.s)); n > most.Load() {
				most.Store(n)
			}
			e.noteRecord()
		}
	})
	// One capture in a series of its own, which no session cancels, ends
	// done while the others race.
	steady := newMCPSession(t, e.s, "steady")
	steadyJob := hubCapture(t, steady, map[string]any{"command": w, "duration": "1s", "series": "steady"})
	var wg sync.WaitGroup
	for i := range 4 {
		c := newMCPSession(t, e.s, fmt.Sprintf("session-%d", i))
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(i)))
			for time.Now().Before(end) {
				switch r.IntN(4) {
				case 0:
					text, isErr := c.call(t.Context(), "capture", map[string]any{"command": w, "duration": "1s"})
					if isErr {
						codeOK("capture", text)
						continue
					}
					var a acceptedAnswer
					if json.Unmarshal([]byte(text), &a) != nil || a.Status != string(jobAccepted) || a.Job == "" {
						t.Errorf("capture: %s", text)
						continue
					}
					mu.Lock()
					jobs = append(jobs, a.Job)
					mu.Unlock()
				case 1:
					text, isErr := c.call(t.Context(), "capture_wait", map[string]any{"id": known(r), "timeout": "200ms"})
					if isErr {
						codeOK("capture_wait", text)
						continue
					}
					var v struct {
						Capture json.RawMessage `json:"capture"`
						Status  string          `json:"status"`
					}
					if json.Unmarshal([]byte(text), &v) != nil || v.Capture == nil && !slices.Contains(waitStatuses, v.Status) {
						t.Errorf("capture_wait: %s", text)
					}
					done(text)
				case 2:
					text, isErr := c.call(t.Context(), "capture_cancel", map[string]any{"id": known(r)})
					if isErr {
						codeOK("capture_cancel", text)
						continue
					}
					var ca cancelAnswer
					if json.Unmarshal([]byte(text), &ca) != nil || !slices.Contains(cancelAnswers, ca.Status) {
						t.Errorf("capture_cancel: %s", text)
					}
				case 3:
					text, isErr := c.call(t.Context(), "capture_list", map[string]any{})
					var l listing
					if isErr || json.Unmarshal([]byte(text), &l) != nil {
						t.Errorf("capture_list: %s", text)
					}
				}
			}
		})
	}
	// Two more sessions open profiles and steer the page while captures
	// are published.
	for i := range 2 {
		c := newMCPSession(t, e.s, fmt.Sprintf("page-%d", i))
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), uint64(10+i)))
			for time.Now().Before(end) {
				var o openedAs
				text, isErr := c.call(t.Context(), "open", map[string]any{"path": []string{"cpu.pb.gz", "cpu-new.pb.gz"}[r.IntN(2)]})
				if isErr || json.Unmarshal([]byte(text), &o) != nil {
					t.Errorf("open: %s", text)
					continue
				}
				switch r.IntN(3) {
				case 0:
					text, isErr = c.call(t.Context(), "page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}})
				case 1:
					text, isErr = c.call(t.Context(), "page_state", map[string]any{})
				case 2:
					if order := e.s.live.snapshot().order; len(order) > 0 {
						text, isErr = c.call(t.Context(), "page_show", map[string]any{"publication": order[r.IntN(len(order))]})
					}
				}
				// A handle another call evicted, or a publication dropped
				// since the snapshot, is an answer the tool gives.
				if isErr && !strings.Contains(text, "is not open") && !strings.Contains(text, "is not kept") {
					t.Errorf("page tool: %s", text)
				}
			}
		})
	}
	wg.Wait()
	mu.Lock()
	started := slices.Clone(jobs)
	mu.Unlock()
	if text, isErr := hubWaitEnd(t, steady, steadyJob); isErr {
		t.Errorf("the steady capture: %s", text)
	} else {
		done(text)
	}
	last := newMCPSession(t, e.s, "cleanup")
	for _, j := range started {
		last.callOK("capture_cancel", map[string]any{"id": j}, nil)
		if text, isErr := hubWaitEnd(t, last, j); !isErr {
			done(text)
		}
	}
	close(stop)
	sampler.Wait()
	if n := most.Load(); n > 1 {
		t.Errorf("%d jobs were active at once", n)
	}
	if len(started) == 0 {
		t.Error("no capture was accepted in 3s")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Error("no capture ended done")
	}
	for _, r := range results {
		if r.Derived != derivedReady || r.Publication == "" {
			t.Errorf("capture %s: derived %q (%s), publication %q", r.Capture.Capture, r.Derived, r.DeriveError, r.Publication)
		}
	}
	// The kept publications: at most MaxPublications, each once, the
	// current one last, each keeping its files, and as the snapshot lists
	// them.
	l, snap := e.s.live, e.s.live.snapshot()
	ids := pubIDs(e.s)
	if len(ids) > maxPubs || len(slices.Compact(slices.Sorted(slices.Values(ids)))) != len(ids) {
		t.Errorf("kept %q", ids)
	}
	if l.current != "" && ids[len(ids)-1] != l.current {
		t.Errorf("current %q is not last in %q", l.current, ids)
	}
	if !slices.Equal(slices.Sorted(slices.Values(ids)), slices.Sorted(slices.Values(snap.order))) || snap.current != l.current {
		t.Errorf("kept %q current %q, the snapshot lists %q current %q", ids, l.current, snap.order, snap.current)
	}
	for _, p := range l.pubs {
		if len(p.files) == 0 {
			t.Errorf("%s records no file", p.id)
		}
	}
}

// A job cancelled while it waits for the engine before its capture gives
// back everything it took: the pins and the windows' reserved duration
// are where they were, before the tool lets go of the engine.
func TestPinsAndReservationsReturnAfterAnEngineWaitCancel(t *testing.T) {
	h := newEngineHold()
	e := newHubEnv(t, jobOpts{setup: func(s *Server) { s.holdEngine = h.hold }})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	a, b := newMCPSession(t, e.s, "A"), newMCPSession(t, e.s, "B")
	var o openedAs
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &o)
	reserved := func() (time.Duration, int) {
		e.s.registry.mu.Lock()
		defer e.s.registry.mu.Unlock()
		return used(e.s.registry.win.rs), len(e.s.registry.res)
	}
	pinTotal, pinHolders := e.s.pins.total, len(e.s.pins.held)
	usedBefore, resBefore := reserved()

	done := holdTop(t, h, a, o)
	job := hubCapture(t, b, hubArgs(t, "5s"))
	hubWaitState(t, b, job, func(v jobView) bool { return v.Phase == "retention" })
	if d, n := reserved(); d != usedBefore+5*time.Second || n != resBefore+1 {
		t.Errorf("admission reserved %v in %d reservations", d, n)
	}
	var ca cancelAnswer
	b.callOK("capture_cancel", map[string]any{"id": job}, &ca)
	if ca.Status != cancelAnswerCancelling {
		t.Errorf("cancel: %+v", ca)
	}
	text, isErr := hubWaitEnd(t, b, job)
	if w := toolErrorOf(t, text); !isErr || w.Code != string(capture.CodeCancelled) {
		t.Fatalf("the job ended %s", text)
	}
	if hd, ok := e.s.engine.holding(); !ok || hd.tool != "top" {
		t.Errorf("the engine is held by %+v; the job ended before top let go", hd)
	}
	if d, n := reserved(); d != usedBefore || n != resBefore {
		t.Errorf("the windows hold %v in %d reservations, %v in %d before", d, n, usedBefore, resBefore)
	}
	if e.s.pins.total != pinTotal || len(e.s.pins.held) != pinHolders {
		t.Errorf("the pins hold %d bytes for %d holders, %d for %d before", e.s.pins.total, len(e.s.pins.held), pinTotal, pinHolders)
	}
	close(h.release)
	if text := <-done; strings.HasPrefix(text, "isError") {
		t.Fatalf("top: %s", text)
	}
}

// An unsteered publication keeps the page where it is; but when making
// room drops the current one, the page shows the new one rather than a
// publication it no longer keeps. With nothing shown yet it is listed
// and nothing is current.
func TestAnUnsteeredPublicationStandsInForADroppedCurrent(t *testing.T) {
	s := newLiveServer(t, func(c *Config) { c.Limits.MaxPublications = 2 }, map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()})
	o := openLive(t, s, "cpu.pb.gz")
	r := o.check(t, s, nil)
	publish := func(steer bool) string {
		t.Helper()
		p, err := s.publishExplained(o.e, o.v, o.view, r, "", "", "A", steer)
		if err != nil {
			t.Fatal(err)
		}
		return p.id
	}
	if id := publish(false); s.live.current != "" || !slices.Equal(pubIDs(s), []string{id}) {
		t.Errorf("unsteered first: current %q, kept %q", s.live.current, pubIDs(s))
	}
	publish(true) // p2, current
	publish(false)
	if s.live.current != "p2" || !slices.Equal(pubIDs(s), []string{"p3", "p2"}) {
		t.Errorf("unsteered beside a current one: current %q, kept %q", s.live.current, pubIDs(s))
	}
	s.lim.MaxPublications = 1
	publish(false)
	if s.live.current != "p4" || !slices.Equal(pubIDs(s), []string{"p4"}) || s.live.snapshot().current != "p4" {
		t.Errorf("the current one dropped: current %q, kept %q", s.live.current, pubIDs(s))
	}
}

// The disk refusal names the captures the page keeps from retention, with
// the live page only.
func TestTheDiskRefusalNamesPublishedCaptures(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprintf("live=%v", live), func(t *testing.T) {
			e := newJobEnv(t, jobOpts{live: live, limits: func(l *Limits) {
				l.Capture.MaxProfileBytes, l.Capture.MaxExeclogBytes = 1, 1
				l.Capture.MaxStoredBytes = 3 + 1 + storeHeadroom
			}})
			// The first capture makes the directory (its profile is over
			// MaxProfileBytes); then a file no capture owns fills it.
			e.run(e.request(time.Second))
			junk := filepath.Join(e.profiles, defaultStdioCaptures, "junk")
			if err := os.WriteFile(junk, make([]byte, 2*storeHeadroom), 0o600); err != nil {
				t.Fatal(err)
			}
			j, v := e.run(e.request(time.Second))
			ce := jobErr(j)
			if v.Status != string(jobFailed) || ce == nil || ce.Code != capture.CodeDisk || !strings.Contains(ce.Message, "Capture.MaxStoredBytes") {
				t.Fatalf("view %+v, err %v", v, ce)
			}
			if got := strings.Contains(ce.Message, "retention keeps every capture the live page shows"); got != live {
				t.Errorf("the message names the page's captures: %v, want %v: %s", got, live, ce.Message)
			}
		})
	}
}

// A capture is published whatever the store kept open: derive's own opens
// evict the handles it opened first when MaxOpenHandles is below their
// number, and the pages are made from what the job pinned. Every limit
// from 1 to 5, with two modes, alone and compared.
func TestAPublicationNeedsNoOpenHandle(t *testing.T) {
	modes := []any{"cpu", "offcpu"}
	for handles := 1; handles <= 5; handles++ {
		t.Run(fmt.Sprintf("MaxOpenHandles=%d", handles), func(t *testing.T) {
			e := newHubEnv(t, jobOpts{limits: func(l *Limits) { l.MaxOpenHandles = handles }})
			a := newMCPSession(t, e.s, "A")
			r := hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "duration": "1s", "modes": modes}))
			if r.Derived != derivedReady || r.Publication == "" {
				t.Fatalf("two modes: derived %q (%s), publication %q", r.Derived, r.DeriveError, r.Publication)
			}
			e.fakeProfile(pmfixture.CPUBasicNew())
			r = hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "label": "cand", "compare_with": "001"}))
			if r.Derived != derivedReady || r.Publication == "" || r.DiffPublication == "" {
				t.Fatalf("compared: derived %q (%s), publication %q, diff %q", r.Derived, r.DeriveError, r.Publication, r.DiffPublication)
			}
			if r.Compare == nil || len(r.Compare.Views) != 2 {
				t.Errorf("compare %+v, want a view per mode", r.Compare)
			}
			if st := stateBy(t, e.s); st.Current != r.DiffPublication {
				t.Errorf("current %q, want the diff %q", st.Current, r.DiffPublication)
			}
		})
	}
}

// When the facts page is published and the diff is not, the facts page
// stays and, with show, is shown; the error says so, and the recipe makes
// only the diff again.
func TestAFailedDiffKeepsAndShowsTheFactsPage(t *testing.T) {
	var fail atomic.Bool
	e := newHubEnv(t, jobOpts{setup: func(s *Server) {
		s.beforeCaptureDiff = func() error {
			if fail.Load() {
				return errors.New("injected")
			}
			return nil
		}
	}})
	a := newMCPSession(t, e.s, "A")
	hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	fail.Store(true)
	r := hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "label": "cand", "compare_with": "001"}))
	if r.Derived != derivedFailed || r.Publication != "p2" || r.DiffPublication != "" {
		t.Fatalf("derived %q, publication %q, diff %q", r.Derived, r.Publication, r.DiffPublication)
	}
	if want := "publication: the facts page p2 is kept and shown, but the diff was not published: injected"; r.DeriveError != want {
		t.Errorf("derive_error %q, want %q", r.DeriveError, want)
	}
	if st := stateBy(t, e.s); st.Current != "p2" || st.By != "A" {
		t.Errorf("state %+v, want the facts page p2 shown", st)
	}
	if r.Recompute == nil || slices.ContainsFunc(r.Recompute.Steps, func(c toolCall) bool { return c.Tool == "page_explain" }) ||
		!slices.ContainsFunc(r.Recompute.Steps, func(c toolCall) bool { return c.Tool == "page_diff" }) {
		t.Errorf("recompute %+v, want the diff and not the facts page", r.Recompute)
	}
	if m, ok := e.manifestOf("adhoc", "002"); !ok || m.Outcome != captureOK {
		t.Errorf("manifest %+v (found %v)", m, ok)
	}

	args := map[string]any{"command": workloadCmd(t), "label": "cand2", "compare_with": "001", "show": false}
	r = hubResult(t, a, hubCapture(t, a, args))
	if want := "publication: the facts page p3 is kept, but the diff was not published: injected"; r.DeriveError != want {
		t.Errorf("show false: derive_error %q, want %q", r.DeriveError, want)
	}
	if st := stateBy(t, e.s); st.Current != "p2" {
		t.Errorf("show false moved the page to %q", st.Current)
	}
}

// A diff keeps both captures' own files, even when the store read the
// baseline's bytes from another file first: here an equal profile opened
// before the baseline was captured.
func TestADiffKeepsTheBaselinesOwnFiles(t *testing.T) {
	e := newHubEnv(t, jobOpts{})
	writeCPUFixtures(t, Config{ProfilesDir: e.profiles})
	a := newMCPSession(t, e.s, "A")
	// cpu.pb.gz holds the bytes the fake writes, so 001 shares its entry.
	a.callOK("open", map[string]any{"path": "cpu.pb.gz"}, nil)
	hubResult(t, a, hubCapture(t, a, hubArgs(t, "1s")))
	e.fakeProfile(pmfixture.CPUBasicNew())
	r := hubResult(t, a, hubCapture(t, a, map[string]any{"command": workloadCmd(t), "label": "cand", "compare_with": "001"}))
	if r.DiffPublication == "" {
		t.Fatalf("no diff: %q", r.DeriveError)
	}
	diff := e.s.live.snapshot().pubs[r.DiffPublication]
	base, _ := e.manifestOf("adhoc", "001")
	cand, _ := e.manifestOf("adhoc", "002")
	for _, f := range slices.Concat(base.Files, cand.Files) {
		if !slices.Contains(diff.files, fileKey{f.Dev, f.Ino}) {
			t.Errorf("the diff does not keep %s", f.Name)
		}
	}
}
