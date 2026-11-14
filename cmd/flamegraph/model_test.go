package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/profilemodel"
	"github.com/google/pprof/profile"
)

// sample is a stack written root-first for readability; writeProfile stores
// it leaf-first. A frame "name@/path.so" sits in that module.
type sample struct {
	stack []string
	value int64
}

func writeProfile(t *testing.T, dir, name string, samples ...sample) string {
	t.Helper()
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     1,
	}
	mappings := map[string]*profile.Mapping{}
	locs := map[string]*profile.Location{}
	loc := func(frame string) *profile.Location {
		if l, ok := locs[frame]; ok {
			return l
		}
		fnName, mod, _ := strings.Cut(frame, "@")
		var m *profile.Mapping
		if mod != "" {
			if m = mappings[mod]; m == nil {
				m = &profile.Mapping{ID: uint64(len(p.Mapping) + 1), File: mod, Start: 0x1000, Limit: 0x2000}
				p.Mapping = append(p.Mapping, m)
				mappings[mod] = m
			}
		}
		f := &profile.Function{ID: uint64(len(p.Function) + 1), Name: fnName, SystemName: fnName}
		p.Function = append(p.Function, f)
		l := &profile.Location{ID: uint64(len(p.Location) + 1), Mapping: m, Line: []profile.Line{{Function: f}}}
		if m != nil {
			l.Address = 0x1100
		}
		p.Location = append(p.Location, l)
		locs[frame] = l
		return l
	}
	for _, s := range samples {
		var ls []*profile.Location
		for i := len(s.stack) - 1; i >= 0; i-- {
			ls = append(ls, loc(s.stack[i]))
		}
		p.Sample = append(p.Sample, &profile.Sample{Location: ls, Value: []int64{s.value}})
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func exec(args ...string) (code int, stdout, stderr string) {
	var o, e bytes.Buffer
	code = run(args, &o, &e)
	return code, o.String(), e.String()
}

func basicProfile(t *testing.T) string {
	return writeProfile(t, t.TempDir(), "p.pb.gz",
		sample{[]string{"main", "work", "inner"}, 30},
		sample{[]string{"main", "work"}, 10},
		sample{[]string{"main", "idle"}, 5},
	)
}

func nodeIDFor(t *testing.T, path string, names ...string) profilemodel.NodeID {
	t.Helper()
	p, err := profilemodel.Load(path, "", profilemodel.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(profilemodel.ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	n := v.Root
	for _, name := range names {
		var next *profilemodel.Node
		for _, c := range n.Children {
			if c.Function.Name == name {
				next = c
			}
		}
		if next == nil {
			t.Fatalf("no %q under %q", name, n.Function.Name)
		}
		n = next
	}
	return n.ID
}

func TestFactsByNodeID(t *testing.T) {
	path := basicProfile(t)
	id := nodeIDFor(t, path, "main", "work")
	code, out, errOut := exec("-facts", string(id), path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got["node"] != string(id) {
		t.Errorf("node = %v, want %s", got["node"], id)
	}
}

func TestFactsByNamePath(t *testing.T) {
	path := basicProfile(t)
	id := nodeIDFor(t, path, "main", "work", "inner")
	code, out, errOut := exec("-facts", "main;work;inner", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var got struct {
		Node string `json:"node"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Node != string(id) {
		t.Errorf("node = %s, want %s", got.Node, id)
	}
}

func TestFactsNoMatch(t *testing.T) {
	code, _, errOut := exec("-facts", "main;nothing", basicProfile(t))
	if code != 1 || errOut == "" {
		t.Fatalf("exit %d, stderr %q; want 1 with a message", code, errOut)
	}
}

func TestFactsAmbiguousNamePathListsCandidates(t *testing.T) {
	path := writeProfile(t, t.TempDir(), "amb.pb.gz",
		sample{[]string{"main", "foo@/usr/lib/libx.so"}, 5},
		sample{[]string{"main", "foo@/usr/lib/liby.so"}, 7},
	)
	code, out, errOut := exec("-facts", "main;foo", path)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if out != "" {
		t.Errorf("stdout should be empty, got %q", out)
	}
	for _, mod := range []string{"libx.so", "liby.so"} {
		if !strings.Contains(errOut, mod) {
			t.Errorf("stderr should name candidate in %s: %q", mod, errOut)
		}
	}
	p, err := profilemodel.Load(path, "", profilemodel.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(profilemodel.ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	main := v.Root.Children[0]
	if len(main.Children) != 2 {
		t.Fatalf("want 2 foo nodes, got %d", len(main.Children))
	}
	for _, c := range main.Children {
		if !strings.Contains(errOut, string(c.ID)) {
			t.Errorf("stderr should list candidate ID %s: %q", c.ID, errOut)
		}
	}
}

func TestListNodes(t *testing.T) {
	path := basicProfile(t)
	code, out, errOut := exec("-list-nodes", "3", path)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	type row struct {
		ID   string `json:"id"`
		Path string `json:"path"`
		Cum  int64  `json:"cum"`
	}
	var rows []row
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		var r row
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		rows = append(rows, r)
	}
	want := []struct {
		path string
		cum  int64
	}{{"main", 45}, {"main;work", 40}, {"main;work;inner", 30}}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows: %s", len(rows), out)
	}
	for i, w := range want {
		if rows[i].Path != w.path || rows[i].Cum != w.cum || rows[i].ID == "" {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], w)
		}
	}
}

func TestDiffWritesPageAndJSON(t *testing.T) {
	dir := t.TempDir()
	base := writeProfile(t, dir, "base.pb.gz", sample{[]string{"main", "work"}, 100}, sample{[]string{"main", "io"}, 20})
	newer := writeProfile(t, dir, "new.pb.gz", sample{[]string{"main", "work"}, 150}, sample{[]string{"main", "io"}, 10})
	code, _, errOut := exec("-diff", base, newer)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	page, err := os.ReadFile(newer + ".diff.html")
	if err != nil || !strings.Contains(string(page), "<html") {
		t.Fatalf("page missing or not HTML: %v", err)
	}
	raw, err := os.ReadFile(newer + ".diff.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		BaseTotal int64 `json:"base_total"`
		NewTotal  int64 `json:"new_total"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.BaseTotal != 120 || d.NewTotal != 160 {
		t.Errorf("totals = %d/%d, want 120/160", d.BaseTotal, d.NewTotal)
	}
}

// The diff JSON is a deep tree: indenting it multiplies its size.
func TestDiffJSONIsCompact(t *testing.T) {
	dir := t.TempDir()
	base := writeProfile(t, dir, "base.pb.gz", sample{[]string{"main", "work", "inner"}, 100})
	newer := writeProfile(t, dir, "new.pb.gz", sample{[]string{"main", "work", "inner"}, 150})
	if code, _, e := exec("-diff", base, newer); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	raw, err := os.ReadFile(newer + ".diff.json")
	if err != nil {
		t.Fatal(err)
	}
	body, ok := bytes.CutSuffix(raw, []byte("\n"))
	if !ok || bytes.ContainsAny(body, "\n\t") {
		t.Fatalf("diff JSON is not compact:\n%s", raw)
	}
	if bytes.Contains(raw, []byte(`"delta_per_unit"`)) || !bytes.Contains(raw, []byte(`"per_unit":{"ok":false,"reason":"no work count"}`)) {
		t.Fatalf("per-unit availability should be stated once, not per node:\n%s", raw)
	}
}

func TestDiffExplicitOutputPath(t *testing.T) {
	dir := t.TempDir()
	base := writeProfile(t, dir, "base.pb.gz", sample{[]string{"main"}, 1})
	newer := writeProfile(t, dir, "new.pb.gz", sample{[]string{"main"}, 2})
	out := filepath.Join(dir, "d.html")
	if code, _, e := exec("-diff", base, "-o", out, newer); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, p := range []string{out, filepath.Join(dir, "d.json")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
}

func TestDiffRefusesToOverwriteInputs(t *testing.T) {
	dir := t.TempDir()
	base := writeProfile(t, dir, "base.pb.gz", sample{[]string{"main"}, 1})
	newer := writeProfile(t, dir, "new.pb.gz", sample{[]string{"main"}, 2})
	before, _ := os.ReadFile(newer)
	for _, o := range []string{newer, base} {
		if code, _, _ := exec("-diff", base, "-o", o, newer); code == 0 {
			t.Errorf("-o %s was accepted", o)
		}
	}
	// The JSON twin of a ".json" -o would collide with the page itself.
	if code, _, _ := exec("-diff", base, "-o", filepath.Join(dir, "x.json"), newer); code == 0 {
		t.Error("-o x.json accepted")
	}
	after, _ := os.ReadFile(newer)
	if !bytes.Equal(before, after) {
		t.Error("input profile was modified")
	}
}

func TestDiffRefusesLinksToInputs(t *testing.T) {
	dir := t.TempDir()
	base := writeProfile(t, dir, "base.pb.gz", sample{[]string{"main"}, 1})
	newer := writeProfile(t, dir, "new.pb.gz", sample{[]string{"main"}, 2})
	exNew := filepath.Join(dir, "new.exec")
	exBase := filepath.Join(dir, "base.exec")
	for _, f := range []string{exNew, exBase} {
		if err := os.WriteFile(f, []byte("sidecar "+f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, name string) string {
		l := filepath.Join(dir, name)
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	tracked := []string{base, newer, exNew, exBase}
	snap := func() [][]byte {
		var out [][]byte
		for _, f := range tracked {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
		return out
	}
	before := snap()

	pageLink := link(newer, "link.html")
	if code, _, e := exec("-diff", base, "-o", pageLink, newer); code != 1 || !strings.Contains(e, "refusing to overwrite") {
		t.Errorf("page symlink: exit %d, stderr %q", code, e)
	}
	jsonNew := link(exNew, "x.json")
	if code, _, e := exec("-diff", base, "-o", filepath.Join(dir, "x.html"), "-execlog", exNew, newer); code != 1 || !strings.Contains(e, "refusing to overwrite") {
		t.Errorf("json symlink to execlog: exit %d, stderr %q", code, e)
	}
	_ = os.Remove(jsonNew)
	link(exBase, "x.json")
	if code, _, e := exec("-diff", base, "-o", filepath.Join(dir, "x.html"), "-execlog-base", exBase, newer); code != 1 || !strings.Contains(e, "refusing to overwrite") {
		t.Errorf("json symlink to execlog-base: exit %d, stderr %q", code, e)
	}
	after := snap()
	for i := range tracked {
		if !bytes.Equal(before[i], after[i]) {
			t.Errorf("%s changed", tracked[i])
		}
	}
}

func TestModelFlagsRejectFolded(t *testing.T) {
	path := basicProfile(t)
	for _, args := range [][]string{
		{"-folded", "-list-nodes", "2", path},
		{"-folded", "-facts", "main", path},
		{"-folded", "-diff", path, path},
		{"-fuse", "-list-nodes", "2", "-in", path, "-in", path},
		{"-stack-order", "root-first", "-facts", "main", path},
		{"-list-nodes", "2", "-facts", "main", path},
		{"-execlog", "x.log", path},
		{"-execlog-base", "x.log", "-list-nodes", "1", path},
		{"-gpu-telemetry", "t.jsonl", "-list-nodes", "1", path},
	} {
		if code, _, e := exec(args...); code != 2 || e == "" {
			t.Errorf("%v: exit %d, stderr %q; want 2 with a message", args, code, e)
		}
	}
}

func TestLegacyFoldedStillWorks(t *testing.T) {
	code, out, _ := exec("-folded", basicProfile(t))
	if code != 0 || !strings.Contains(out, "main;work;inner 30") {
		t.Fatalf("exit %d, out %q", code, out)
	}
}
