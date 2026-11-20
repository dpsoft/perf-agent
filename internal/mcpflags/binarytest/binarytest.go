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

// Targets are the GOOS/GOARCH pairs the binaries are checked for: what
// a package links can differ by target, through a file for one target
// only.
var Targets = []string{"linux/amd64", "linux/arm64", "darwin/arm64"}

// TargetEnv is the environment that makes go list list target's
// packages: its GOOS and GOARCH, with cgo enabled, so files that need
// cgo are listed too.
func TargetEnv(target string) []string {
	goos, goarch, _ := strings.Cut(target, "/")
	return []string{"GOOS=" + goos, "GOARCH=" + goarch, "CGO_ENABLED=1"}
}

// forbidden are the packages of the profiler itself, which no MCP binary
// links.
var forbidden = []string{"github.com/dpsoft/perf-agent/perfagent", "github.com/cilium/ebpf", "github.com/libbpf/blazesym/go"}

// allowed are the only modules besides the standard library an MCP binary
// links.
var allowed = []string{"github.com/dpsoft/perf-agent", "github.com/google/pprof"}

// LinksNoProfiler fails t when the main package in dir links the profiler,
// or any module outside allowed, for any of Targets.
func LinksNoProfiler(t testing.TB, dir string) {
	t.Helper()
	for _, target := range Targets {
		linksNoProfiler(t, dir, target)
	}
}

func linksNoProfiler(t testing.TB, dir, target string) {
	t.Helper()
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{.ImportPath}} {{.Standard}}", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), TargetEnv(target)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list in %s for %s: %v", dir, target, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "github.com/dpsoft/perf-agent/internal/perfmcp ") }) {
		t.Fatalf("go list in %s lists %d packages without perfmcp: not an MCP server's dependencies", dir, len(lines))
	}
	for _, line := range lines {
		path, std, _ := strings.Cut(line, " ")
		if slices.Contains(forbidden, path) {
			t.Errorf("the server links %s on %s", path, target)
		}
		if std == "true" {
			continue
		}
		if !slices.ContainsFunc(allowed, func(m string) bool { return path == m || strings.HasPrefix(path, m+"/") }) {
			t.Errorf("the server links %s on %s, outside %v", path, target, allowed)
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
