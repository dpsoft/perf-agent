package perfmcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/explain"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// rendering is a server with CPUBasic opened and an output root.
type rendering struct {
	opened
	profiles, out string
}

func renderFixture(t *testing.T, edit func(*Config)) rendering {
	t.Helper()
	cfg := testConfig(t)
	cfg.OutputDir = t.TempDir()
	if edit != nil {
		edit(&cfg)
	}
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var op openOut
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &op)
	return rendering{opened: opened{c: c, handle: op.Handle, view: op.DefaultView}, profiles: cfg.ProfilesDir, out: cfg.OutputDir}
}

// shareClaim is one explanation of node with a share_of_total claim of
// value, written as JSON text, and prose that places it.
func shareClaim(node, value, prose string) []any {
	return []any{map[string]any{
		"node": node,
		"claims": []any{map[string]any{
			"id": "c1", "metric": "share_of_total", "denominator": "view_total", "unit": "ratio", "value": json.RawMessage(value),
		}},
		"prose": prose,
	}}
}

type renderOut struct {
	Attempt          string `json:"attempt"`
	Status           string `json:"status"`
	Written          *bool  `json:"written"`
	Path             string `json:"path"`
	Bytes            string `json:"bytes"`
	FiguresChecked   string `json:"figures_checked"`
	UncheckedNumbers string `json:"unchecked_numbers"`
	Failures         []struct {
		Claim  string `json:"claim"`
		Code   string `json:"code"`
		Agent  string `json:"agent"`
		Engine *struct {
			Value string `json:"value"`
		} `json:"engine"`
	} `json:"failures"`
	Notices []struct {
		Path    string `json:"path"`
		Message string `json:"message"`
	} `json:"notices"`
}

// render calls render_flamegraph with o's handle and view plus extra and
// decodes its JSON text, which a rejection also carries.
func (o rendering) render(t *testing.T, extra map[string]any) (renderOut, string, bool) {
	t.Helper()
	text, isErr := o.c.call("render_flamegraph", o.args(extra))
	var out renderOut
	if strings.HasPrefix(text, "{") {
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("decode %s: %v", text, err)
		}
	}
	return out, text, isErr
}

// entries are the names in dir, failing the test on a temporary left
// behind.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		if strings.Contains(de.Name(), ".tmp-") {
			t.Errorf("temporary %s left in %s", de.Name(), dir)
		}
		names = append(names, de.Name())
	}
	return names
}

func readFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRenderWritesAPageForCheckedClaims(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	out, text, isErr := o.render(t, map[string]any{
		"explanations": shareClaim(b, "0.411", "b takes {c1} of the time, maybe 3 calls"), "out": "cpu.html",
	})
	if isErr {
		t.Fatalf("render: %s", text)
	}
	want := filepath.Join(o.out, "cpu.html")
	if out.Attempt != "a1" || out.Status != "written" || out.Path != want || out.FiguresChecked != "1" || out.UncheckedNumbers != "1" {
		t.Errorf("result = %s", text)
	}
	page := readFile(t, want)
	if out.Bytes != fmt.Sprint(len(page)) {
		t.Errorf("bytes %s, page has %d", out.Bytes, len(page))
	}
	if !bytes.HasPrefix(page, []byte("<!doctype html>")) || !bytes.Contains(page, []byte("41.1%")) {
		t.Errorf("page does not look like the explained page: %.200s", page)
	}
	if got := entries(t, o.out); len(got) != 1 {
		t.Errorf("output root holds %v", got)
	}
	if out.Notices == nil {
		t.Errorf("notices must be a list: %s", text)
	}
}

func TestRenderRejectsAndWritesNothing(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "b takes {c1}"), "out": "cpu.html"})
	if !isErr || out.Status != "rejected" || out.Attempt != "a1" || out.Written == nil || *out.Written {
		t.Fatalf("isError=%v %s", isErr, text)
	}
	if len(out.Failures) != 1 || out.Failures[0].Engine == nil || out.Failures[0].Engine.Value != "0.411" || out.Failures[0].Agent != "0.62" {
		t.Errorf("failures: %s", text)
	}
	if !strings.Contains(text, `attempt \"a1\"`) && !strings.Contains(text, `"attempt":"a1"`) {
		t.Errorf("no retry advice: %s", text)
	}
	if got := entries(t, o.out); len(got) != 0 {
		t.Errorf("output root holds %v", got)
	}
}

func TestRejectedAttemptsComeFromTheServersRecord(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	if _, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "b takes {c1}"), "out": "cpu.html"}); !isErr {
		t.Fatalf("a1 passed: %s", text)
	}
	out, text, isErr := o.render(t, map[string]any{
		"explanations": shareClaim(b, "0.411", "b takes {c1}; nothing was rejected"), "out": "cpu.html", "attempt": "a1",
	})
	if isErr || out.Attempt != "a2" {
		t.Fatalf("a2: %s", text)
	}
	page := string(readFile(t, out.Path))
	i := strings.Index(page, "Rejected at render")
	if i < 0 || !strings.Contains(page, "1 claim rejected at render") {
		t.Fatalf("page has no rejected section")
	}
	if sec := page[i:]; !strings.Contains(sec, "a1") || !strings.Contains(sec, "<s>0.62</s>") {
		t.Errorf("a1's failure is not listed")
	}
}

func TestAttemptsAreBoundToHandleAndView(t *testing.T) {
	o := renderFixture(t, nil)
	writeFixture(t, o.profiles, "new.pb.gz", pmfixture.CPUBasicNew())
	var other openOut
	o.c.callOK("open", map[string]any{"path": "new.pb.gz"}, &other)
	b := o.nodeID(t, "b")
	if _, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "{c1}"), "out": "x.html"}); !isErr {
		t.Fatalf("a1 passed: %s", text)
	}
	// a1 belongs to the first handle; nothing is recorded for the refusal.
	text, isErr := o.c.call("render_flamegraph", map[string]any{
		"handle": other.Handle, "view": other.DefaultView, "explanations": []any{}, "out": "x.html", "attempt": "a1",
	})
	if !isErr || !strings.Contains(text, "a1") || !strings.Contains(text, string(o.handle)) {
		t.Errorf("foreign attempt: isError=%v %s", isErr, text)
	}
	if _, text, isErr := o.render(t, map[string]any{"explanations": []any{}, "out": "x.html", "attempt": "a99"}); !isErr || !strings.Contains(text, "a99") {
		t.Errorf("unknown attempt: isError=%v %s", isErr, text)
	}
	// An explanations array of the wrong shape is recorded as invalid and
	// takes the next number.
	out, text, isErr := o.render(t, map[string]any{"explanations": map[string]any{}, "out": "x.html"})
	if !isErr || !strings.Contains(text, "a2") {
		t.Errorf("invalid shape: isError=%v %s", isErr, text)
	}
	if out.Attempt != "" && out.Attempt != "a2" {
		t.Errorf("attempt %q", out.Attempt)
	}
	out, text, isErr = o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html", "attempt": "a2"})
	if isErr || out.Attempt != "a3" {
		t.Errorf("retry of a2: %s", text)
	}
	if got := entries(t, o.out); len(got) != 1 {
		t.Errorf("output root holds %v", got)
	}
}

func TestAttemptsKeepAtMostMaxAttemptsPerView(t *testing.T) {
	a := newAttempts(2, 256)
	a.add("h", "v", "", outcomeRejected, nil)
	a.add("h", "v", "a1", outcomeRejected, nil)
	a.add("h", "v", "a2", outcomeRejected, nil)
	// Other outcomes are bounded apart and never evict a rejection.
	for range 3 {
		a.add("h", "v", "", outcomeInvalid, nil)
	}
	a.add("h", "w", "", outcomeRejected, nil)
	rej, dropped, _ := a.rejected("h", "v")
	if len(rej) != 2 || rej[0].Attempt != "a2" || rej[1].Attempt != "a3" || rej[1].RetryOf != "a2" || dropped != 1 {
		t.Errorf("rejected = %+v, dropped %d", rej, dropped)
	}
	for _, id := range []string{"a1", "a4"} {
		if _, err := a.lookup(id, "h", "v"); err == nil {
			t.Errorf("dropped attempt %s is still known", id)
		}
	}
	if _, err := a.lookup("a5", "h", "v"); err != nil {
		t.Errorf("a5: %v", err)
	}
	if _, err := a.lookup("a7", "h", "v"); err == nil || !strings.Contains(err.Error(), "w") {
		t.Errorf("a7 with view v: %v", err)
	}
}

func TestAttemptViewsAreBoundedAndKeepTheirRejectedCount(t *testing.T) {
	a := newAttempts(2, 1)
	a.add("h", "v", "", outcomeRejected, nil)
	a.add("h", "v", "", outcomeRejected, nil)
	a.add("h", "w", "", outcomeRejected, nil) // evicts v's record
	if _, err := a.lookup("a1", "h", "v"); err == nil {
		t.Error("an attempt of an evicted view is still known")
	}
	if len(a.views) != 1 || len(a.byID) != 1 {
		t.Errorf("%d views, %d attempts kept", len(a.views), len(a.byID))
	}
	a.add("h", "v", "", outcomeWritten, nil) // evicts w's record
	rej, dropped, incomplete := a.rejected("h", "v")
	if len(rej) != 0 || dropped != 2 || incomplete {
		t.Errorf("v: %+v dropped %d incomplete %v", rej, dropped, incomplete)
	}
	// The counters hold 4 views; one more rejected view evicted loses count.
	for i := range 6 {
		a.add("h", fmt.Sprint("x", i), "", outcomeRejected, nil)
	}
	if len(a.counts) > 4 {
		t.Errorf("%d counters", len(a.counts))
	}
	a.add("h", "fresh", "", outcomeWritten, nil)
	if _, _, incomplete := a.rejected("h", "fresh"); !incomplete {
		t.Error("a view without a record after the counters overflowed reads as complete")
	}
}

func TestRenderIsDisabledWithoutAnOutputRoot(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			Annotations map[string]any `json:"annotations"`
			InputSchema struct {
				Properties map[string]any `json:"properties"`
			} `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(c.rpc("tools/list", map[string]any{}).Result, &list); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range list.Tools {
		if tool.Name != "render_flamegraph" {
			continue
		}
		found = true
		if !strings.HasPrefix(tool.Description, "DISABLED: start perf-agent-mcp with --output DIR to enable rendering.") {
			t.Errorf("description = %q", tool.Description)
		}
		if tool.Annotations["readOnlyHint"] != false || tool.Annotations["destructiveHint"] != false ||
			tool.Annotations["idempotentHint"] != false || tool.Annotations["openWorldHint"] != false {
			t.Errorf("annotations = %v", tool.Annotations)
		}
		if _, ok := tool.InputSchema.Properties["overwrite"]; ok {
			t.Error("the schema offers overwrite")
		}
	}
	if !found {
		t.Fatal("render_flamegraph is not listed")
	}
	var op openOut
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &op)
	text, isErr := c.call("render_flamegraph", map[string]any{"handle": op.Handle, "view": op.DefaultView, "explanations": []any{}, "out": "x.html"})
	if !isErr || !strings.Contains(text, "--output") {
		t.Errorf("isError=%v %s", isErr, text)
	}
}

func TestRenderRefusesToLeaveTheOutputRoot(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(o.out, "link")); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{"../x.html", filepath.Join(outside, "x.html"), "link/x.html", "missing/x.html", "x.txt", "", "."} {
		_, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": out})
		if !isErr {
			t.Errorf("out %q written: %s", out, text)
		}
	}
	if got := entries(t, outside); len(got) != 0 {
		t.Errorf("outside holds %v", got)
	}
	if got := entries(t, o.out); len(got) != 1 {
		t.Errorf("output root holds %v", got)
	}
	// An absolute path inside the root works.
	if _, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": filepath.Join(o.out, "in.html")}); isErr {
		t.Errorf("absolute path inside: %s", text)
	}
}

func TestRenderNeverOverwritesAnInput(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.OutputDir = cfg.ProfilesDir })
	b := o.nodeID(t, "b")
	input := filepath.Join(o.profiles, "cpu.pb.gz")
	if err := os.Link(input, filepath.Join(o.profiles, "hard.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cpu.pb.gz", filepath.Join(o.profiles, "sym.html")); err != nil {
		t.Fatal(err)
	}
	exps := shareClaim(b, "0.411", "{c1}")
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "hard.html"}); !isErr || !strings.Contains(text, "input") {
		t.Errorf("hard link: isError=%v %s", isErr, text)
	}
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "sym.html"}); !isErr || !strings.Contains(text, "symbolic link") {
		t.Errorf("symlink: isError=%v %s", isErr, text)
	}
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "cpu.pb.gz.html"}); isErr {
		t.Errorf("new name beside the input: %s", text)
	}
	if !bytes.Equal(readFile(t, input), pmfixture.CPUBasic()) {
		t.Error("the input changed")
	}
	entries(t, o.profiles)
}

func TestRenderRefusesAnExistingFile(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	exps := shareClaim(b, "0.411", "{c1}")
	theirs := filepath.Join(o.out, "x.html")
	writeFixture(t, o.out, "x.html", []byte("theirs"))
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "x.html"}); !isErr || !strings.Contains(text, "new file name") {
		t.Errorf("existing file: isError=%v %s", isErr, text)
	}
	if got := readFile(t, theirs); string(got) != "theirs" {
		t.Errorf("x.html = %q", got)
	}
	// A page this server wrote is not replaced either.
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "y.html"}); isErr {
		t.Fatalf("y.html: %s", text)
	}
	first := readFile(t, filepath.Join(o.out, "y.html"))
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "y.html"}); !isErr || !strings.Contains(text, "new file name") {
		t.Errorf("second y.html: isError=%v %s", isErr, text)
	}
	if !bytes.Equal(readFile(t, filepath.Join(o.out, "y.html")), first) {
		t.Error("y.html changed")
	}
	// overwrite is not an argument.
	if _, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "y.html", "overwrite": true}); !isErr || !strings.Contains(text, "overwrite") {
		t.Errorf("overwrite: isError=%v %s", isErr, text)
	}
	entries(t, o.out)
}

func TestRenderKeepsAFileCreatedWhileItWrites(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	input := filepath.Join(o.profiles, "cpu.pb.gz")
	for _, tc := range []struct {
		name   string
		create func(string) error
		check  func(string) error
	}{
		{"file", func(p string) error { return os.WriteFile(p, []byte("theirs"), 0o644) }, func(p string) error {
			if got, err := os.ReadFile(p); err != nil || string(got) != "theirs" {
				return fmt.Errorf("%q, %v", got, err)
			}
			return nil
		}},
		{"symlink", func(p string) error { return os.Symlink(input, p) }, func(p string) error {
			if got, err := os.Readlink(p); err != nil || got != input {
				return fmt.Errorf("%q, %v", got, err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.name + ".html"
			dest := filepath.Join(o.out, out)
			called := false
			o.c.srv.beforePublish = func(name string) {
				called = true
				if name != out {
					t.Errorf("hook got %q", name)
				}
				if err := tc.create(dest); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() { o.c.srv.beforePublish = nil })
			_, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": out})
			if !called || !isErr || !strings.Contains(text, "new file name") {
				t.Errorf("called=%v isError=%v %s", called, isErr, text)
			}
			if err := tc.check(dest); err != nil {
				t.Errorf("the entry created meanwhile changed: %v", err)
			}
			entries(t, o.out)
		})
	}
	if !bytes.Equal(readFile(t, input), pmfixture.CPUBasic()) {
		t.Error("the input changed")
	}
}

func TestRenderFailsClosedWithoutHardLinks(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	o.c.srv.link = func(_ *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	_, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html"})
	if !isErr || !strings.Contains(text, "hard link") || !strings.Contains(text, "--output") {
		t.Errorf("isError=%v %s", isErr, text)
	}
	if got := entries(t, o.out); len(got) != 0 {
		t.Errorf("output root holds %v", got)
	}
}

func TestRenderIgnoresAgentTolerance(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	out, text, isErr := o.render(t, map[string]any{
		"explanations": shareClaim(b, "0.62", "{c1}"), "out": "x.html", "tolerance": 0.5, "rounding": "1 ratio",
	})
	if !isErr || out.Status != "rejected" {
		t.Fatalf("isError=%v %s", isErr, text)
	}
	var paths []string
	for _, n := range out.Notices {
		paths = append(paths, n.Path)
	}
	if strings.Join(paths, ",") != "rounding,tolerance" || !strings.Contains(out.Notices[1].Message, "engine") {
		t.Errorf("notices = %+v", out.Notices)
	}
}

func TestRenderPageSizeLimit(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.Limits.MaxPageBytes = 1024 })
	b := o.nodeID(t, "b")
	_, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html"})
	if !isErr || !strings.Contains(text, "MaxPageBytes") {
		t.Errorf("isError=%v %s", isErr, text)
	}
	if got := entries(t, o.out); len(got) != 0 {
		t.Errorf("output root holds %v", got)
	}
}

// rejectedSection is the page's rejected pill and section, failing the
// test without one.
func rejectedSection(t *testing.T, path string) string {
	t.Helper()
	page := string(readFile(t, path))
	i := strings.Index(page, "rejected at render")
	if i < 0 {
		i = strings.Index(page, "not kept</span>")
	}
	if i < 0 {
		t.Fatal("the page lists no rejected attempt")
	}
	if strings.Contains(page, "exceeded at render") {
		t.Error("the page shows a limit pill")
	}
	return page[max(i-200, 0):]
}

func TestThePageCountsRejectedAttemptsTheServerDropped(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.Limits.MaxAttemptsPerView = 2 })
	b := o.nodeID(t, "b")
	for _, retry := range []string{"", "a1", "a2"} {
		args := map[string]any{"explanations": shareClaim(b, "0.62", "{c1}"), "out": "x.html"}
		if retry != "" {
			args["attempt"] = retry
		}
		if _, text, isErr := o.render(t, args); !isErr {
			t.Fatalf("passed: %s", text)
		}
	}
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html", "attempt": "a3"})
	if isErr || out.Attempt != "a4" {
		t.Fatalf("a4: %s", text)
	}
	sec := rejectedSection(t, out.Path)
	if !strings.Contains(sec, "2 claims rejected at render in 2 attempts, plus 1 earlier rejected attempt not kept</span>") {
		t.Errorf("rejected section: %.1500s", sec)
	}
}

func TestRejectionsOutliveCheapAttempts(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.Limits.MaxAttemptsPerView = 2 })
	b := o.nodeID(t, "b")
	if _, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "{c1}"), "out": "x.html"}); !isErr {
		t.Fatalf("passed: %s", text)
	}
	for range 2 {
		if _, text, isErr := o.render(t, map[string]any{"explanations": map[string]any{}, "out": "x.html"}); !isErr {
			t.Fatalf("invalid passed: %s", text)
		}
	}
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html"})
	if isErr {
		t.Fatalf("write: %s", text)
	}
	sec := rejectedSection(t, out.Path)
	if !strings.Contains(sec, "1 claim rejected at render</span>") || !strings.Contains(sec, "<s>0.62</s>") || strings.Contains(sec, "not kept") {
		t.Errorf("rejected section: %.1500s", sec)
	}
}

func TestRejectionsSurviveHandleEviction(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.Limits.MaxOpenHandles = 1 })
	writeFixture(t, o.profiles, "new.pb.gz", pmfixture.CPUBasicNew())
	b := o.nodeID(t, "b")
	if _, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "{c1}"), "out": "x.html"}); !isErr {
		t.Fatalf("passed: %s", text)
	}
	o.c.callOK("open", map[string]any{"path": "new.pb.gz"}, nil)
	o.c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, nil)
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html"})
	if isErr {
		t.Fatalf("write: %s", text)
	}
	sec := rejectedSection(t, out.Path)
	if !strings.Contains(sec, "1 claim rejected at render</span>") || !strings.Contains(sec, "<s>0.62</s>") {
		t.Errorf("rejected section: %.1500s", sec)
	}
}

// rejectBadNode records a rejected attempt on o's handle and view: an
// explanation of a node that does not exist.
func rejectBadNode(t *testing.T, c *testClient, handle, view string) {
	t.Helper()
	text, isErr := c.call("render_flamegraph", map[string]any{
		"handle": handle, "view": view, "out": "x.html",
		"explanations": []any{map[string]any{"node": "0000000000000000", "prose": "x"}},
	})
	if !isErr || !strings.Contains(text, `"status":"rejected"`) {
		t.Fatalf("not rejected: %s", text)
	}
}

func TestEvictedAttemptViewsKeepTheirRejectedCount(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.Limits.MaxAttemptViews = 1 })
	writeFixture(t, o.profiles, "new.pb.gz", pmfixture.CPUBasicNew())
	var other openOut
	o.c.callOK("open", map[string]any{"path": "new.pb.gz"}, &other)
	rejectBadNode(t, o.c, o.handle, o.view)
	rejectBadNode(t, o.c, o.handle, o.view)
	rejectBadNode(t, o.c, other.Handle, other.DefaultView) // evicts the first view's record
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(o.nodeID(t, "b"), "0.411", "{c1}"), "out": "y.html"})
	if isErr {
		t.Fatalf("write: %s", text)
	}
	if sec := rejectedSection(t, out.Path); !strings.Contains(sec, "2 earlier rejected attempts not kept</span>") {
		t.Errorf("rejected section: %.1500s", sec)
	}
}

func TestPagesSayWhenAttemptHistoryMayBeIncomplete(t *testing.T) {
	cfg := testConfig(t)
	cfg.OutputDir = t.TempDir()
	cfg.Limits.MaxAttemptViews = 1 // so 4 counters
	c := newTestServer(t, cfg)
	var ops []openOut
	for i := range 7 {
		name := fmt.Sprintf("p%d.pb.gz", i)
		writeFixture(t, cfg.ProfilesDir, name, pmfixture.Build("cpu", "nanoseconds", nil,
			pmfixture.Sample{Frames: []pmfixture.Frame{pmfixture.Fn("main"), pmfixture.Fn("work")}, Value: int64(i + 1)}))
		var op openOut
		c.callOK("open", map[string]any{"path": name}, &op)
		ops = append(ops, op)
	}
	// Six views rejected in turn: four counts are kept, the fifth is lost.
	for _, op := range ops[:6] {
		rejectBadNode(t, c, op.Handle, op.DefaultView)
	}
	last := opened{c: c, handle: ops[6].Handle, view: ops[6].DefaultView}
	var res renderOut
	text := c.callOK("render_flamegraph", last.args(map[string]any{
		"out": "z.html", "explanations": []any{map[string]any{"node": last.nodeID(t, "work"), "prose": "work"}},
	}), &res)
	page := string(readFile(t, res.Path))
	if !strings.Contains(page, "earlier attempt history for this view may be incomplete</span>") {
		t.Errorf("no incomplete-history note: %s", text)
	}
	// A view whose count was kept is complete.
	first := opened{c: c, handle: ops[1].Handle, view: ops[1].DefaultView}
	c.callOK("render_flamegraph", first.args(map[string]any{
		"out": "w.html", "explanations": []any{map[string]any{"node": first.nodeID(t, "work"), "prose": "work"}},
	}), &res)
	if page := string(readFile(t, res.Path)); strings.Contains(page, "may be incomplete") || !strings.Contains(page, "1 earlier rejected attempt not kept</span>") {
		t.Error("a view whose count was kept reads as incomplete or lost its count")
	}
}

func TestRenderRefusesBadPageNames(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	writeFixture(t, o.out, "file", []byte("x"))
	if err := os.Mkdir(filepath.Join(o.out, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	long := strings.Repeat("x", 300)
	for _, tc := range []struct{ out, want string }{
		{strings.Repeat("p", 255-22-len(".html")+1) + ".html", "shorter"},
		{".html", "name"},
		{"sub/.html", "name"},
		{"file/x.html", "a regular file"},
		{"sub/../x.html", `".."`},
		{"../x.html", `".."`},
		{filepath.Join(outside, long+".html"), "outside"},
		{long + "/x.html", "cannot open"},
	} {
		// A failing claim: a refused name is reported before the check.
		_, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.62", "{c1}"), "out": tc.out})
		if !isErr || !strings.Contains(text, tc.want) || strings.Contains(text, "rejected") {
			t.Errorf("out %.40q: isError=%v %s", tc.out, isErr, text)
		}
		if strings.Contains(text, long[:100]) || strings.Contains(text, strings.Repeat("p", 100)) {
			t.Errorf("out %.40q: the message repeats the whole path: %s", tc.out, text)
		}
	}
	// The longest allowed name works, and no attempt was recorded before it.
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": strings.Repeat("p", 255-22-len(".html")) + ".html"})
	if isErr || out.Attempt != "a1" {
		t.Errorf("longest name: %s", text)
	}
	entries(t, o.out)
}

func TestAttemptLimitDefaults(t *testing.T) {
	d := DefaultLimits()
	if d.MaxAttemptsPerView != 20 || d.MaxAttemptViews != 64 || maxStoredFailures != 32 {
		t.Errorf("MaxAttemptsPerView %d, MaxAttemptViews %d, maxStoredFailures %d", d.MaxAttemptsPerView, d.MaxAttemptViews, maxStoredFailures)
	}
	if l := (Limits{MaxMessageBytes: 1, MaxResponseBytes: 1, MaxOpenHandles: 1}).withDefaults(); l.MaxAttemptsPerView != 20 || l.MaxAttemptViews != 64 {
		t.Errorf("filled in: %d %d", l.MaxAttemptsPerView, l.MaxAttemptViews)
	}
}

func TestStoredFailuresKeepClaimsFirst(t *testing.T) {
	var fs []explain.Failure
	for i := range 10 {
		fs = append(fs, explain.Failure{Explanation: -1, Code: explain.CodeLimitExceeded, Ref: fmt.Sprint("L", i)})
	}
	for i := range 10 {
		fs = append(fs, explain.Failure{Code: explain.CodeUnresolvedNodeRef, Ref: fmt.Sprint("r", i)})
	}
	for i := range 30 {
		fs = append(fs, explain.Failure{Code: explain.CodeValueMismatch, Claim: fmt.Sprint("c", i)})
	}
	kept, omitted := storedFailures(fs)
	if len(kept) != 32 || omitted != 18 {
		t.Fatalf("kept %d omitted %d", len(kept), omitted)
	}
	if kept[0].Claim != "c0" || kept[29].Claim != "c29" || kept[30].Ref != "r0" || kept[31].Ref != "r1" {
		t.Errorf("kept = %+v", kept)
	}
	if kept, omitted := storedFailures(fs[:3]); len(kept) != 3 || omitted != 0 {
		t.Errorf("short list: %d %d", len(kept), omitted)
	}
}

func TestARejectedAttemptKeepsAtMost32Failures(t *testing.T) {
	o := renderFixture(t, nil)
	b := o.nodeID(t, "b")
	var exps []any
	for e := range 2 {
		var claims []any
		for i := range 20 {
			claims = append(claims, map[string]any{"id": fmt.Sprintf("c%d", e*20+i+1), "metric": "share_of_total",
				"denominator": "view_total", "unit": "ratio", "value": json.RawMessage("0.62")})
		}
		exps = append(exps, map[string]any{"node": b, "claims": claims, "prose": "x"})
	}
	out, text, isErr := o.render(t, map[string]any{"explanations": exps, "out": "x.html"})
	if !isErr || len(out.Failures) != 40 {
		t.Fatalf("the rejection lists %d failures: isError=%v %.300s", len(out.Failures), isErr, text)
	}
	if !strings.Contains(text, "and 8 more failures") {
		t.Errorf("the result does not say what the record leaves out: %.300s", text)
	}
	page, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html", "attempt": "a1"})
	if isErr {
		t.Fatalf("write: %s", text)
	}
	sec := rejectedSection(t, page.Path)
	if !strings.Contains(sec, "32 claims rejected at render") || !strings.Contains(sec, "and 8 more failures") {
		t.Errorf("rejected section: %.600s", sec)
	}
	if n := strings.Count(sec, "<s>0.62</s>"); n != 32 {
		t.Errorf("%d claim rows", n)
	}
}
