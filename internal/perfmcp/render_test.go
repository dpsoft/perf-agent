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
	a := newAttempts(2)
	a.add("h", "v", "", outcomeRejected, nil)
	a.add("h", "v", "a1", outcomeRejected, nil)
	a.add("h", "v", "a2", outcomeRejected, nil)
	a.add("h", "w", "", outcomeRejected, nil)
	rej, dropped := a.rejected("h", "v")
	if len(rej) != 2 || rej[0].Attempt != "a2" || rej[1].Attempt != "a3" || rej[1].RetryOf != "a2" || dropped != 1 {
		t.Errorf("rejected = %+v, dropped %d", rej, dropped)
	}
	if _, err := a.lookup("a1", "h", "v"); err == nil {
		t.Error("a dropped attempt is still known")
	}
	if _, err := a.lookup("a4", "h", "v"); err == nil || !strings.Contains(err.Error(), "w") {
		t.Errorf("a4 with view v: %v", err)
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

func TestThePageCountsRejectedAttemptsTheServerDropped(t *testing.T) {
	o := renderFixture(t, func(cfg *Config) { cfg.Limits.MaxAttemptsPerView = 2 })
	b := o.nodeID(t, "b")
	for _, retry := range []string{"", "a1"} {
		args := map[string]any{"explanations": shareClaim(b, "0.62", "{c1}"), "out": "x.html"}
		if retry != "" {
			args["attempt"] = retry
		}
		if _, text, isErr := o.render(t, args); !isErr {
			t.Fatalf("passed: %s", text)
		}
	}
	out, text, isErr := o.render(t, map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "x.html", "attempt": "a2"})
	if isErr || out.Attempt != "a3" {
		t.Fatalf("a3: %s", text)
	}
	sec := string(readFile(t, out.Path))
	sec = sec[strings.Index(sec, "<h5>Rejected at render"):]
	if !strings.Contains(sec, "a2") || !strings.Contains(sec, "1 earlier rejected attempt is not listed") || !strings.Contains(sec, "MaxAttemptsPerView") {
		t.Errorf("rejected section: %.2000s", sec)
	}
}
