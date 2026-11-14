package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func TestRunNeedsAProfilesRoot(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(nil, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "--profiles") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunRefusesUnknownLimits(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"--profiles", t.TempDir(), "--limit", "Bogus=1"}, strings.NewReader(""), &out, &errb)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "Model.MaxRawBytes") || !strings.Contains(errb.String(), "Bogus") {
		t.Errorf("stderr = %q", errb.String())
	}
}

func TestRunAppliesLimits(t *testing.T) {
	var out, errb bytes.Buffer
	args := []string{"--profiles", t.TempDir(), "--limit", "Model.MaxRawBytes=1073741824", "--limit", "Source.GitTimeout=2s", "--limit", "MaxRows=7"}
	if code := run(args, strings.NewReader(""), &out, &errb); code != 0 {
		t.Errorf("exit %d, stderr %q", code, errb.String())
	}
	if code := run([]string{"--profiles", t.TempDir(), "--limit", "MaxRows=x"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("non-integer limit: exit %d, want 2", code)
	}
}

// session runs initialize, tools/list, open and render against a profiles
// root and an output root and returns stdout and stderr.
func session(t *testing.T) (stdout, stderr string, profiles, output string) {
	t.Helper()
	profiles, output = t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(profiles, "cpu.pb.gz"), pmfixture.CPUBasic(), 0o644); err != nil {
		t.Fatal(err)
	}
	msg := func(id int, method string, params any) string {
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}
	in := msg(1, "initialize", map[string]any{"protocolVersion": "2025-11-25"}) +
		msg(2, "tools/list", map[string]any{}) +
		msg(3, "tools/call", map[string]any{"name": "open", "arguments": map[string]any{"path": "cpu.pb.gz"}})
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", profiles, "--output", output}, strings.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	return out.String(), errb.String(), profiles, output
}

func TestRunServesOverStdio(t *testing.T) {
	stdout, _, profiles, output := session(t)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d responses, want 3:\n%s", len(lines), stdout)
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	want := []string{"open", "top", "path", "sandwich", "gpu_kernels_for", "facts", "diff", "source", "render_flamegraph"}
	slices.Sort(names)
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}

	var opened struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &opened); err != nil || opened.Result.IsError {
		t.Fatalf("open: %s (%v)", lines[2], err)
	}
	var o struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(opened.Result.Content[0].Text), &o); err != nil || o.Handle == "" {
		t.Fatalf("open result: %v", err)
	}

	// Render over a second run against the same roots: the page lands in
	// --output and nowhere else.
	req, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": "render_flamegraph",
		"arguments": map[string]any{
			"handle": o.Handle, "view": o.DefaultView, "out": "page.html", "explanations": []any{},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	in := `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}` + "\n" +
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"open","arguments":{"path":"cpu.pb.gz"}}}` + "\n" + string(req) + "\n"
	var out, errb bytes.Buffer
	if code := run([]string{"--profiles", profiles, "--output", output}, strings.NewReader(in), &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(output, "page.html")); err != nil {
		t.Errorf("page not written: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(profiles, "page.html")); err == nil {
		t.Error("page written outside --output")
	}
}

func TestStdoutCarriesOnlyJSONRPC(t *testing.T) {
	stdout, _, _, _ := session(t)
	for line := range strings.SplitSeq(strings.TrimSpace(stdout), "\n") {
		var m struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil || m.JSONRPC != "2.0" {
			t.Errorf("stdout line is not JSON-RPC 2.0: %q (%v)", line, err)
		}
	}
}

func TestTheServerLinksNoNetworkingOrProfiler(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".").Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	forbidden := []string{"net", "net/http", "crypto/tls", "github.com/dpsoft/perf-agent/perfagent", "github.com/cilium/ebpf", "github.com/libbpf/blazesym/go"}
	allowed := []string{"github.com/dpsoft/perf-agent", "github.com/google/pprof"}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		path, std, _ := strings.Cut(line, " ")
		if slices.Contains(forbidden, path) {
			t.Errorf("the server links %s", path)
		}
		if std == "true" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(m string) bool { return path == m || strings.HasPrefix(path, m+"/") }) {
			t.Errorf("the server links %s, outside %v", path, allowed)
		}
	}
}

func TestTheServerIsNotShipped(t *testing.T) {
	for _, f := range []string{"Makefile", "Dockerfile.agent", "Dockerfile.shim", ".github/workflows/release.yml", "scripts/third-party-notices.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if bytes.Contains(b, []byte("perf-agent-mcp")) {
			t.Errorf("%s mentions perf-agent-mcp: the server is source-only", f)
		}
	}
}
