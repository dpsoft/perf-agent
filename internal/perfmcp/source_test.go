package perfmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		Total      string   `json:"candidates_total"`
		SHA256     string   `json:"sha256"`
		VerifiedBy string   `json:"verified_by"`
		Commit     string   `json:"commit"`
		TopLevel   string   `json:"git_toplevel"`
	} `json:"resolution"`
	LinesError string `json:"lines_error"`
	Lines      []struct {
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
	if !isErr || !strings.Contains(text, "refusing to overwrite input") {
		t.Errorf("render over a source file: isError=%v %s", isErr, text)
	}
}

func TestSourceCapsAmbiguousCandidates(t *testing.T) {
	src := t.TempDir()
	for i := range 20 {
		mkfile(t, src, fmt.Sprintf("d%02d/conv.cu", i), numbered(50))
	}
	cfg := testConfig(t)
	cfg.SourceDirs = []string{src}
	o := openWith(t, cfg, "gpu.pb.gz", pmfixture.GPUBasic())
	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "conv")}), &out)
	r := out.Rows[0].Resolution
	if r.Status != "ambiguous" || len(r.Candidates) != 16 || r.Total != "20" || !slices.IsSorted(r.Candidates) {
		t.Errorf("resolution = %+v", r)
	}
}

func TestSourceLinesErrorSaysWhatToDo(t *testing.T) {
	src := t.TempDir()
	mkfile(t, src, "src/app.c", numbered(10))
	cfg := testConfig(t)
	cfg.SourceDirs = []string{src}
	o := openWith(t, cfg, "cpu.pb.gz", pmfixture.CPUBasic())
	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "b")}), &out)
	r := out.Rows[0]
	if len(r.Lines) != 0 || !strings.Contains(r.LinesError, "line 30 is past the end of src/app.c (10 lines)") || strings.Contains(r.LinesError, "sourceroot:") {
		t.Errorf("lines_error = %q lines %v", r.LinesError, r.Lines)
	}
}

func TestSourceLimitIsClampedToMaxRows(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits = DefaultLimits()
	cfg.Limits.MaxRows = 3
	o := openWith(t, cfg, "cpu.pb.gz", pmfixture.CPUBasic())
	var b string
	for _, r := range o.top(t, map[string]any{"rows": "nodes", "limit": 3}).Rows {
		b = r.Node
	}
	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": b}), &out)
	text, isErr := o.c.call("source", o.args(map[string]any{"node": b, "limit": 4}))
	if !isErr || !strings.Contains(text, "this server's MaxRows") {
		t.Errorf("limit 4: isError=%v %s", isErr, text)
	}
}

// gitInDir runs git for test setup in dir.
func gitInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		"-c", "init.defaultBranch=main", "-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSourceGitSelectsTheTrackedFileAndReadsHeadOncePerCall(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	src := t.TempDir()
	mkfile(t, src, "src/app.c", numbered(60))
	gitInDir(t, src, "init", "-q")
	gitInDir(t, src, "add", ".")
	gitInDir(t, src, "commit", "-q", "-m", "init")
	commit := gitInDir(t, src, "rev-parse", "HEAD")

	// Three samples through main;a with different lines of app.c give a
	// page of three rows.
	file := "/build/app/src/app.c"
	frame := func(name string, line int64) pmfixture.Frame {
		return pmfixture.In("/opt/app/bin/app", "b1", uint64(line), pmfixture.L(name, file, line))
	}
	raw := pmfixture.Build("cpu", "nanoseconds", nil,
		pmfixture.Sample{Frames: []pmfixture.Frame{frame("main", 10), frame("a", 20)}, Value: 5},
		pmfixture.Sample{Frames: []pmfixture.Frame{frame("main", 10), frame("a", 21)}, Value: 4},
		pmfixture.Sample{Frames: []pmfixture.Frame{frame("main", 10), frame("a", 22)}, Value: 3},
	)

	log := filepath.Join(t.TempDir(), "heads.log")
	bin := t.TempDir()
	shim := fmt.Sprintf("#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in 'HEAD^{commit}') echo head >> %s;; esac; done\nexec %s \"$@\"\n", log, realGit)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := testConfig(t)
	cfg.SourceDirs, cfg.SourceGit = []string{src}, true
	o := openWith(t, cfg, "cpu.pb.gz", raw)
	var out sourceOut
	o.c.callOK("source", o.args(map[string]any{"node": o.nodeID(t, "a")}), &out)
	if len(out.Rows) != 3 {
		t.Fatalf("rows = %d, want 3: %+v", len(out.Rows), out.Rows)
	}
	for _, r := range out.Rows {
		res := r.Resolution
		if res.Status != "user_selected" || res.VerifiedBy != "git" || res.Commit != commit || res.TopLevel == "" || len(r.Lines) == 0 {
			t.Errorf("row %d: %+v", r.Line, r)
		}
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(got), "head"); n != 1 {
		t.Errorf("HEAD read %d times for one call, want 1", n)
	}
}
