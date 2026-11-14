package perfmcp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// openOut is the open result as a client reads it.
type openOut struct {
	Handle        string `json:"handle"`
	ProfileDigest string `json:"profile_digest"`
	ProfileName   string `json:"profile_name"`
	CaptureID     string `json:"capture_id"`
	Views         []struct {
		View       string `json:"view"`
		SampleType string `json:"sample_type"`
		Unit       string `json:"unit"`
		Index      string `json:"index"`
	} `json:"views"`
	DefaultView  string `json:"default_view"`
	Capabilities struct {
		Executions struct {
			OK     bool   `json:"ok"`
			Reason string `json:"reason"`
		} `json:"executions"`
		PCSampling  bool     `json:"pc_sampling"`
		SourceRoots []string `json:"source_roots"`
		Render      struct {
			Enabled    bool   `json:"enabled"`
			OutputRoot string `json:"output_root"`
		} `json:"render"`
	} `json:"capabilities"`
	Quality map[string]any `json:"quality"`
	Notes   []string       `json:"notes"`
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestOpenReturnsAHandleViewsAndCapabilities(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)

	var out openOut
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &out)
	want := digestOf(pmfixture.CPUBasic())
	if out.Handle != want || out.ProfileDigest != want {
		t.Errorf("handle %q digest %q, want both %q", out.Handle, out.ProfileDigest, want)
	}
	if out.ProfileName != "cpu.pb.gz" {
		t.Errorf("profile_name = %q", out.ProfileName)
	}
	if len(out.Views) != 1 || out.Views[0].View != "cpu/nanoseconds#0" || out.Views[0].SampleType != "cpu" ||
		out.Views[0].Unit != "nanoseconds" || out.Views[0].Index != "0" {
		t.Errorf("views = %+v", out.Views)
	}
	if out.DefaultView != "cpu/nanoseconds#0" {
		t.Errorf("default_view = %q", out.DefaultView)
	}
	ex := out.Capabilities.Executions
	if ex.OK || ex.Reason != "no sidecar" {
		t.Errorf("executions = %+v", ex)
	}
	if out.Capabilities.PCSampling {
		t.Error("pc_sampling true on a CPU profile")
	}
	if out.Capabilities.Render.Enabled || out.Capabilities.Render.OutputRoot != "" {
		t.Errorf("render = %+v without OutputDir", out.Capabilities.Render)
	}
	if out.Capabilities.SourceRoots == nil {
		t.Error("source_roots is null, want []")
	}
	if out.Notes == nil {
		t.Error("notes is null, want []")
	}
	unresolved, _ := out.Quality["unresolved"].(map[string]any)
	if unresolved["value"] != "0.000" || unresolved["status"] != "zero" {
		t.Errorf("quality.unresolved = %v", unresolved)
	}
}

func TestOpenReportsRenderAndSourceRoots(t *testing.T) {
	cfg := testConfig(t)
	cfg.OutputDir = t.TempDir()
	src := t.TempDir()
	cfg.SourceDirs = []string{src}
	writeFixture(t, cfg.ProfilesDir, "gpu.pb.gz", pmfixture.GPUBasic())
	c := newTestServer(t, cfg)

	var out openOut
	c.callOK("open", map[string]any{"path": "gpu.pb.gz"}, &out)
	if !out.Capabilities.Render.Enabled || out.Capabilities.Render.OutputRoot != cfg.OutputDir {
		t.Errorf("render = %+v, want enabled at %s", out.Capabilities.Render, cfg.OutputDir)
	}
	if len(out.Capabilities.SourceRoots) != 1 || out.Capabilities.SourceRoots[0] != src {
		t.Errorf("source_roots = %v", out.Capabilities.SourceRoots)
	}
	if !out.Capabilities.PCSampling {
		t.Error("pc_sampling false on a profile with gpu_src_status")
	}
	if out.CaptureID != "cap-1" {
		t.Errorf("capture_id = %q", out.CaptureID)
	}
}

func TestOpenWithAnExeclogGivesADifferentHandle(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "gpu.pb.gz", pmfixture.GPUBasic())
	writeFixture(t, cfg.ProfilesDir, "gpu.execlog", pmfixture.GPUBasicExeclog(t))
	c := newTestServer(t, cfg)

	var bare, withLog openOut
	c.callOK("open", map[string]any{"path": "gpu.pb.gz"}, &bare)
	c.callOK("open", map[string]any{"path": "gpu.pb.gz", "execlog": "gpu.execlog"}, &withLog)
	if bare.Handle == withLog.Handle {
		t.Errorf("same handle %q with and without the log", bare.Handle)
	}
	if bare.ProfileDigest != withLog.ProfileDigest {
		t.Errorf("digests differ: %q vs %q", bare.ProfileDigest, withLog.ProfileDigest)
	}
	if !strings.HasPrefix(withLog.Handle, withLog.ProfileDigest+"+") {
		t.Errorf("handle %q does not extend the digest", withLog.Handle)
	}
	if !withLog.Capabilities.Executions.OK {
		t.Errorf("executions = %+v, want ok", withLog.Capabilities.Executions)
	}
	if bare.Capabilities.Executions.OK || bare.Capabilities.Executions.Reason != "no sidecar" {
		t.Errorf("bare executions = %+v", bare.Capabilities.Executions)
	}
	if len(withLog.Notes) == 0 {
		t.Error("a GPU view has no sampling notes")
	}
	if withLog.DefaultView != "gpu/nanoseconds#0" {
		t.Errorf("default_view = %q", withLog.DefaultView)
	}
}

func TestOpenAcceptsAnAbsolutePathInsideTheRoot(t *testing.T) {
	cfg := testConfig(t)
	if err := os.Mkdir(filepath.Join(cfg.ProfilesDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	writeFixture(t, cfg.ProfilesDir, "sub/cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)

	var out openOut
	c.callOK("open", map[string]any{"path": filepath.Join(cfg.ProfilesDir, "cpu.pb.gz")}, &out)
	if out.ProfileName != "cpu.pb.gz" {
		t.Errorf("profile_name = %q", out.ProfileName)
	}
	c.callOK("open", map[string]any{"path": filepath.Join(cfg.ProfilesDir, "sub", "cpu.pb.gz")}, &out)
	if out.ProfileName != "sub/cpu.pb.gz" {
		t.Errorf("profile_name = %q", out.ProfileName)
	}
}

func TestOpenAcceptsASymlinkInsideTheRoot(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	if err := os.Symlink("cpu.pb.gz", filepath.Join(cfg.ProfilesDir, "latest.pb.gz")); err != nil {
		t.Fatal(err)
	}
	c := newTestServer(t, cfg)
	var out openOut
	c.callOK("open", map[string]any{"path": "latest.pb.gz"}, &out)
	if out.ProfileDigest != digestOf(pmfixture.CPUBasic()) {
		t.Errorf("digest %q", out.ProfileDigest)
	}
}

func TestOpenRefusesPathsOutsideTheRoot(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	outside := t.TempDir()
	writeFixture(t, outside, "other.pb.gz", pmfixture.CPUBasic())
	if err := os.Symlink(filepath.Join(outside, "other.pb.gz"), filepath.Join(cfg.ProfilesDir, "link.pb.gz")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cfg.ProfilesDir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	c := newTestServer(t, cfg)

	for _, p := range []string{
		"../x",
		"../" + filepath.Base(outside) + "/other.pb.gz",
		"/etc/passwd",
		filepath.Join(outside, "other.pb.gz"),
		"link.pb.gz",
		"linkdir/other.pb.gz",
	} {
		text, isErr := c.call("open", map[string]any{"path": p})
		if !isErr {
			t.Errorf("open %q succeeded: %s", p, text)
			continue
		}
		if !strings.Contains(text, "profiles root "+cfg.ProfilesDir) {
			t.Errorf("open %q: error does not name the profiles root: %s", p, text)
		}
	}
	// An execlog is confined the same way.
	text, isErr := c.call("open", map[string]any{"path": "cpu.pb.gz", "execlog": "/etc/passwd"})
	if !isErr || !strings.Contains(text, "profiles root "+cfg.ProfilesDir) {
		t.Errorf("execlog outside the root: isError=%v %s", isErr, text)
	}
}

func TestOpenRefusesNonRegularFiles(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	if err := syscall.Mkfifo(filepath.Join(cfg.ProfilesDir, "pipe.pb.gz"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(cfg.ProfilesDir, "dir.pb.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTestServer(t, cfg)

	for _, args := range []map[string]any{
		{"path": "pipe.pb.gz"},
		{"path": "dir.pb.gz"},
		{"path": "cpu.pb.gz", "execlog": "pipe.pb.gz"},
	} {
		text, isErr := c.call("open", args)
		name := args["path"].(string)
		if e, ok := args["execlog"]; ok {
			name = e.(string)
		}
		if !isErr || !strings.Contains(text, name) || !strings.Contains(text, "not a regular file") {
			t.Errorf("open %v: isError=%v %s", args, isErr, text)
		}
	}
	// The server keeps serving.
	var out openOut
	c.callOK("open", map[string]any{"path": "cpu.pb.gz"}, &out)
}

func TestOpenRefusesAMissingExeclog(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "gpu.pb.gz", pmfixture.GPUBasic())
	c := newTestServer(t, cfg)

	text, isErr := c.call("open", map[string]any{"path": "gpu.pb.gz", "execlog": "gpu.execlog"})
	if !isErr || !strings.Contains(text, "not found") || !strings.Contains(text, `"gpu.execlog"`) ||
		!strings.Contains(text, cfg.ProfilesDir) {
		t.Fatalf("isError=%v %s", isErr, text)
	}
	if n := len(c.srv.store.entries); n != 0 {
		t.Errorf("store holds %d entries after a refused open", n)
	}
	var out openOut
	c.callOK("open", map[string]any{"path": "gpu.pb.gz"}, &out)
	if out.Capabilities.Executions.Reason != "no sidecar" {
		t.Errorf("executions = %+v", out.Capabilities.Executions)
	}
}

func TestOpenRefusesAMissingProfile(t *testing.T) {
	cfg := testConfig(t)
	c := newTestServer(t, cfg)
	text, isErr := c.call("open", map[string]any{"path": "nope.pb.gz"})
	if !isErr || !strings.Contains(text, "not found") || !strings.Contains(text, cfg.ProfilesDir) {
		t.Errorf("isError=%v %s", isErr, text)
	}
	text, isErr = c.call("open", map[string]any{})
	if !isErr || !strings.Contains(text, "path") {
		t.Errorf("no path: isError=%v %s", isErr, text)
	}
}

func TestOpenRefusesAProfileOverALimit(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.Model.MaxSamples = 1
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	text, isErr := c.call("open", map[string]any{"path": "cpu.pb.gz"})
	if !isErr || !strings.Contains(text, "MaxSamples") {
		t.Errorf("isError=%v %s", isErr, text)
	}
}

func TestUnknownArgumentsAreNamed(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	text, isErr := c.call("open", map[string]any{"path": "cpu.pb.gz", "color": "red"})
	if !isErr || !strings.Contains(text, "path, execlog") || !strings.Contains(text, "color") {
		t.Errorf("isError=%v %s", isErr, text)
	}
	text, isErr = c.call("open", map[string]any{"path": 7})
	if !isErr || !strings.Contains(text, "path, execlog") {
		t.Errorf("wrong type: isError=%v %s", isErr, text)
	}
}

func TestHandlesAreEvidenceNotPolicy(t *testing.T) {
	strict := testConfig(t)
	strict.Limits.Model.MaxSidecarRows = 1
	loose := testConfig(t)
	for _, cfg := range []Config{strict, loose} {
		writeFixture(t, cfg.ProfilesDir, "gpu.pb.gz", pmfixture.GPUBasic())
		writeFixture(t, cfg.ProfilesDir, "gpu.execlog", pmfixture.GPUBasicExeclog(t))
	}
	args := map[string]any{"path": "gpu.pb.gz", "execlog": "gpu.execlog"}
	var a, b openOut
	newTestServer(t, strict).callOK("open", args, &a)
	newTestServer(t, loose).callOK("open", args, &b)
	if a.Handle != b.Handle {
		t.Errorf("handles differ under different limits: %q vs %q", a.Handle, b.Handle)
	}
	if a.Capabilities.Executions.OK || a.Capabilities.Executions.Reason != "sidecar exceeds limits" {
		t.Errorf("strict executions = %+v", a.Capabilities.Executions)
	}
	if !b.Capabilities.Executions.OK {
		t.Errorf("loose executions = %+v", b.Capabilities.Executions)
	}
}

func TestLimitsAreCopiedAtNew(t *testing.T) {
	cfg := testConfig(t)
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	srv := must(New(cfg))(t)
	t.Cleanup(func() { _ = srv.Close() })
	cfg.Limits.Model.MaxSamples = 1
	if _, err := srv.store.open("cpu.pb.gz", ""); err != nil {
		t.Errorf("a later change to the caller's Config reached the server: %v", err)
	}
}

func TestNewRefusesAMissingProfilesRoot(t *testing.T) {
	cfg := testConfig(t)
	cfg.ProfilesDir = filepath.Join(cfg.ProfilesDir, "missing")
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), cfg.ProfilesDir) {
		t.Errorf("err = %v", err)
	}
	if _, err := New(Config{Limits: DefaultLimits()}); err == nil || !strings.Contains(err.Error(), "ProfilesDir") {
		t.Errorf("empty ProfilesDir: err = %v", err)
	}
	cfg = testConfig(t)
	cfg.OutputDir = filepath.Join(cfg.ProfilesDir, "missing")
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), cfg.OutputDir) {
		t.Errorf("missing OutputDir: err = %v", err)
	}
}

func TestOversizedResponsesSayHowToShrinkThem(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxResponseBytes = 64
	writeFixture(t, cfg.ProfilesDir, "cpu.pb.gz", pmfixture.CPUBasic())
	c := newTestServer(t, cfg)
	text, isErr := c.call("open", map[string]any{"path": "cpu.pb.gz"})
	if !isErr || !strings.Contains(text, "smaller limit") || !strings.Contains(text, "MaxResponseBytes=64") {
		t.Errorf("isError=%v %s", isErr, text)
	}
}

func TestToolsListOpen(t *testing.T) {
	c := newTestServer(t, testConfig(t))
	res := c.rpc("tools/list", map[string]any{})
	var out struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "open" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	tool := out.Tools[0]
	if tool.Annotations["readOnlyHint"] != true || tool.Annotations["idempotentHint"] != true ||
		tool.Annotations["destructiveHint"] != false || tool.Annotations["openWorldHint"] != false {
		t.Errorf("annotations = %v", tool.Annotations)
	}
	if tool.InputSchema["additionalProperties"] != false {
		t.Errorf("schema = %v", tool.InputSchema)
	}
}
