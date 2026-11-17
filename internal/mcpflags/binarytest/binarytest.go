// Package binarytest holds the checks that perf-agent-mcp and
// perf-agent-hub share about their binaries: what they link and that no
// release ships them. Only test files import it.
package binarytest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// forbidden are the packages of the profiler itself, which no MCP binary
// links.
var forbidden = []string{"github.com/dpsoft/perf-agent/perfagent", "github.com/cilium/ebpf", "github.com/libbpf/blazesym/go"}

// allowed are the only modules besides the standard library an MCP binary
// links.
var allowed = []string{"github.com/dpsoft/perf-agent", "github.com/google/pprof"}

// LinksNoProfiler fails t when the main package in dir links the profiler,
// or any module outside allowed.
func LinksNoProfiler(t testing.TB, dir string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in %s: %v", dir, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "github.com/dpsoft/perf-agent/internal/perfmcp ") }) {
		t.Fatalf("go list in %s lists %d packages without perfmcp: not an MCP server's dependencies", dir, len(lines))
	}
	for _, line := range lines {
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

// shipping are the files, relative to the module root, that build or
// ship what a release contains.
var shipping = []string{"Makefile", "Dockerfile.agent", "Dockerfile.shim", ".github/workflows/release.yml", "scripts/third-party-notices.sh"}

// NotShipped fails t when a file that builds or ships a release names the
// binary called name: the MCP servers are source-only.
func NotShipped(t testing.TB, name string) {
	t.Helper()
	gomod, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(gomod)))
	for _, f := range shipping {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if bytes.Contains(b, []byte(name)) {
			t.Errorf("%s mentions %s: the server is source-only", f, name)
		}
	}
}
