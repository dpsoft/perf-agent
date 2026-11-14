package perfmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
	"github.com/dpsoft/perf-agent/internal/sourceroot"
)

type srcRow struct {
	Kind     string `json:"kind"`
	Function string `json:"function"`
	Filename string `json:"filename"`
	Line     int64  `json:"line"`
	Module   string `json:"module"`
	BuildID  string `json:"build_id"`
	Value    Fact   `json:"value"`

	Resolution struct {
		Status     string   `json:"status"`
		Reason     string   `json:"reason"`
		Path       string   `json:"path"`
		Candidates []string `json:"candidates"`
		SHA256     string   `json:"sha256"`
	} `json:"resolution"`
	Lines []struct {
		N    int64  `json:"n"`
		Text string `json:"text"`
	} `json:"lines"`
}

type sourceOut struct {
	Handle      string   `json:"handle"`
	View        string   `json:"view"`
	Node        string   `json:"node"`
	SourceRoots []string `json:"source_roots"`
	Notes       []string `json:"notes"`
	Rows        []srcRow `json:"rows"`
	pageInfo
}

// numbered is a file of n lines "line K".
func numbered(n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return []byte(b.String())
}

func mkfile(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func openWith(t *testing.T, cfg Config, name string, raw []byte) opened {
	t.Helper()
	writeFixture(t, cfg.ProfilesDir, name, raw)
	c := newTestServer(t, cfg)
	var out openOut
	c.callOK("open", map[string]any{"path": name}, &out)
	return opened{c: c, handle: out.Handle, view: out.DefaultView}
}

func TestSourceShowsLinesWithTheirStatus(t *testing.T) {
	src := t.TempDir()
	mkfile(t, src, "src/app.c", numbered(60))
	cfg := testConfig(t)
	cfg.SourceDirs = []string{src}
	o := openWith(t, cfg, "cpu.pb.gz", pmfixture.CPUBasic())

	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "b"), "context": 2}), &out)
	if len(out.SourceRoots) != 1 || out.SourceRoots[0] != src || len(out.Rows) != 1 {
		t.Fatalf("out = %+v", out)
	}
	r := out.Rows[0]
	if r.Kind != "call_site" || r.Function != "b" || r.Line != 30 || r.Value.Value != "411" || r.BuildID != "b1" || r.Module != "/opt/app/bin/app" {
		t.Errorf("row = %+v", r)
	}
	if r.Resolution.Status != "source_unverified" || r.Resolution.Path != "src/app.c" || r.Resolution.SHA256 != digestOf(numbered(60)) {
		t.Errorf("resolution = %+v", r.Resolution)
	}
	if len(r.Lines) != 5 || r.Lines[0].N != 28 || r.Lines[2].Text != "line 30" || r.Lines[4].N != 32 {
		t.Errorf("lines = %+v", r.Lines)
	}
	if out.Notes != nil || out.OmittedWeightBasis != "value" || out.TotalRows != "1" {
		t.Errorf("page = %+v", out.pageInfo)
	}
}

func TestSourceGPUBasenameAmbiguity(t *testing.T) {
	src := t.TempDir()
	mkfile(t, src, "a/conv.cu", numbered(50))
	mkfile(t, src, "b/conv.cu", numbered(50))
	cfg := testConfig(t)
	cfg.SourceDirs = []string{src}
	writeFixture(t, cfg.ProfilesDir, "x.execlog", pmfixture.GPUBasicExeclog(t))
	o := openWith(t, cfg, "gpu.pb.gz", pmfixture.GPUBasic())

	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "conv")}), &out)
	if len(out.Rows) != 1 {
		t.Fatalf("rows = %+v", out.Rows)
	}
	r := out.Rows[0]
	if r.Kind != "gpu_pc" || r.Resolution.Status != "ambiguous" || len(r.Resolution.Candidates) != 2 || len(r.Lines) != 0 {
		t.Errorf("row = %+v", r)
	}
	if len(out.Notes) == 0 {
		t.Error("a GPU view's response carries no notes")
	}
}

func TestSourceWithoutRootsSaysHowToAddThem(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "b")}), &out)
	if len(out.Rows) != 1 || out.Rows[0].Resolution.Status != "unresolved" ||
		out.Rows[0].Resolution.Reason != "no source roots: start perf-agent-mcp with --source DIR" || len(out.Rows[0].Lines) != 0 {
		t.Errorf("rows = %+v", out.Rows)
	}
	if out.SourceRoots == nil {
		t.Error("source_roots is null, want []")
	}
}

func TestSourceBuildVerifiedThroughAManifest(t *testing.T) {
	src := t.TempDir()
	body := numbered(60)
	mkfile(t, src, "src/conv.cu", body)
	m, err := json.Marshal(sourceroot.Manifest{
		Format: sourceroot.ManifestFormat, CaptureIDs: []string{"cap-1"},
		Files: map[string]string{"src/conv.cu": digestOf(body)},
	})
	if err != nil {
		t.Fatal(err)
	}
	mf := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(mf, m, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.SourceDirs, cfg.SourceManifests = []string{src}, []string{mf}
	o := openWith(t, cfg, "gpu.pb.gz", pmfixture.GPUBasic())

	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "conv"), "context": 1}), &out)
	r := out.Rows[0]
	if r.Resolution.Status != "build_verified" || len(r.Lines) != 3 || r.Lines[1].Text != "line 42" {
		t.Errorf("row = %+v", r)
	}
}

func TestSourceArgumentsAreBounded(t *testing.T) {
	o := openFixture(t, "cpu.pb.gz", pmfixture.CPUBasic(), nil)
	b := o.nodeID(t, "b")
	for _, extra := range []map[string]any{{"context": -1}, {"context": 51}, {"limit": 0}, {"limit": 51}} {
		extra["node"] = b
		if text, isErr := o.c.call("source", o.args(extra)); !isErr {
			t.Errorf("%v accepted: %s", extra, text)
		}
	}
}

func TestSourceFilesCountAsInputsForRender(t *testing.T) {
	src := t.TempDir()
	mkfile(t, src, "src/app.c", numbered(60))
	if err := os.Link(filepath.Join(src, "src/app.c"), filepath.Join(src, "app.html")); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.SourceDirs, cfg.OutputDir = []string{src}, src
	o := openWith(t, cfg, "cpu.pb.gz", pmfixture.CPUBasic())
	b := o.nodeID(t, "b")
	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": b}), &out)

	text, isErr := o.c.call("render_flamegraph", o.args(map[string]any{"explanations": shareClaim(b, "0.411", "{c1}"), "out": "app.html"}))
	if !isErr || !strings.Contains(text, "input") {
		t.Errorf("render over a source file: isError=%v %s", isErr, text)
	}
}
