package capture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// goBuild builds a one-file main package whose go.mod names module into
// a new directory, inside a fresh git repository with one commit so the
// binary carries vcs.revision. It returns the binary and the commit.
func goBuild(t *testing.T, module string) (bin, revision string) {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go tool to build a fixture: %v", err)
	}
	gitTool, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("no git to give the fixture a revision: %v", err)
	}
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module "+module+"\n\ngo 1.26\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GOFLAGS=", "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0",
	)
	run := func(name string, args ...string) string {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", filepath.Base(name), strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(gitTool, "init", "-q")
	run(gitTool, "add", ".")
	run(gitTool, "commit", "-q", "-m", "fixture")
	revision = run(gitTool, "rev-parse", "HEAD")
	bin = filepath.Join(dir, "prog")
	run(goTool, "build", "-buildvcs=true", "-o", bin, ".")
	return bin, revision
}

// buildInfoOf runs checkBuildInfo on the file at path.
func buildInfoOf(t *testing.T, path string) (version, revision string, err error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	return checkBuildInfo(f, path)
}

// elfWarningsOf runs checkELF on the file at path.
func elfWarningsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	return checkELF(f, path)
}

func TestCheckBuildInfo(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildInfoOf(t, self); err == nil || !strings.Contains(err.Error(), "not perf-agent") || !strings.Contains(err.Error(), ".test") {
		t.Errorf("the test binary: %v, want refused as not perf-agent, naming its .test main package", err)
	}

	other, _ := goBuild(t, "example.com/x")
	if _, _, err := buildInfoOf(t, other); err == nil || !strings.Contains(err.Error(), "example.com/x") {
		t.Errorf("module example.com/x: %v, want refused naming it", err)
	}

	pa, rev := goBuild(t, PerfAgentModule)
	version, revision, err := buildInfoOf(t, pa)
	if err != nil {
		t.Fatalf("a %s main at the module root: %v", PerfAgentModule, err)
	}
	if version == "" {
		t.Error("no version")
	}
	if revision != rev {
		t.Errorf("revision %q, want %q", revision, rev)
	}

	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := buildInfoOf(t, plain); err == nil || !strings.Contains(err.Error(), "not perf-agent") {
		t.Errorf("a shell script: %v, want refused as not perf-agent", err)
	}
}

func TestCheckModule(t *testing.T) {
	bi := &debug.BuildInfo{
		Path: PerfAgentModule,
		Main: debug.Module{Path: PerfAgentModule, Version: "v0.9.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: "0123abcd"},
		},
	}
	version, revision, err := checkModule(bi)
	if err != nil || version != "v0.9.0" || revision != "0123abcd" {
		t.Fatalf("checkModule = %q, %q, %v; want v0.9.0, 0123abcd, nil", version, revision, err)
	}
	bi.Path = PerfAgentModule + "/cmd/flamegraph"
	if _, _, err := checkModule(bi); err == nil || !strings.Contains(err.Error(), "cmd/flamegraph") {
		t.Fatalf("another main package of the module: %v, want refused naming it", err)
	}

	// The paths come from the file, so what a refusal echoes is bounded.
	bi.Main.Path = "example.com/" + strings.Repeat("é", 400)
	_, _, err = checkModule(bi)
	if err == nil || len(err.Error()) > 2*maxEchoBytes || !strings.Contains(err.Error(), "…") {
		t.Fatalf("a long module path: %d bytes %q, want refused and bounded", len(err.Error()), err)
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("a long module path: %q is not valid UTF-8", err)
	}
	bi.Main.Path = PerfAgentModule
	bi.Path = PerfAgentModule
	bi.Main.Version = strings.Repeat("v", 1000)
	if version, _, err := checkModule(bi); err != nil || len(version) > maxEchoBytes+len("…") {
		t.Fatalf("a long version: %d bytes, %v; want it bounded", len(version), err)
	}
}

func TestCheckELFWarnsOnSharedBlazesym(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if w := elfWarningsOf(t, self); len(w) != 0 {
		t.Errorf("the test binary: warnings %q, want none", w)
	}
	if w := blazesymWarnings([]string{"libc.so.6", "libblazesym_c.so"}); len(w) != 1 || !strings.Contains(w[0], "libblazesym_c.so") {
		t.Errorf("a shared blazesym: warnings %q, want one naming it", w)
	}
	if w := blazesymWarnings([]string{"libc.so.6", "libm.so.6"}); len(w) != 0 {
		t.Errorf("no blazesym: warnings %q, want none", w)
	}
	script := filepath.Join(t.TempDir(), "script")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if w := elfWarningsOf(t, script); len(w) != 1 {
		t.Errorf("a file that is not ELF: warnings %q, want one", w)
	}
}

func TestCheckIdentity(t *testing.T) {
	was := Identity{Dev: 1, Ino: 2, Size: 3, Mtime: 4, Ctime: 5, Xattr: [32]byte{6}}
	if e := checkIdentity(was, was); e != nil {
		t.Fatalf("equal identities: %v", e)
	}
	for _, tc := range []struct {
		field  string
		change func(*Identity)
	}{
		{"dev", func(i *Identity) { i.Dev++ }},
		{"ino", func(i *Identity) { i.Ino++ }},
		{"size", func(i *Identity) { i.Size++ }},
		{"mtime", func(i *Identity) { i.Mtime++ }},
		{"ctime", func(i *Identity) { i.Ctime++ }},
		{"xattr", func(i *Identity) { i.Xattr[31] = 1 }},
	} {
		now := was
		tc.change(&now)
		e := checkIdentity(was, now)
		if e == nil {
			t.Errorf("%s differs: nil", tc.field)
			continue
		}
		if e.Code != CodeBinaryChanged || e.Retryable {
			t.Errorf("%s differs: code %s retryable %v, want binary_changed, not retryable", tc.field, e.Code, e.Retryable)
		}
		if got := e.Error(); !strings.Contains(got, "perf-agent changed since start-up; restart perf-agent-mcp") {
			t.Errorf("%s differs: %q", tc.field, got)
		}
		if e.Changed != tc.field {
			t.Errorf("Changed = %q, want %q", e.Changed, tc.field)
		}
	}
}

// TestEveryCodeHasADocumentedRetryable reads the Code constants from
// errors.go, so a new code cannot be added without its §4.4 retryable
// value.
func TestEveryCodeHasADocumentedRetryable(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "errors.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var codes []Code
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			v := s.(*ast.ValueSpec)
			if id, ok := v.Type.(*ast.Ident); !ok || id.Name != "Code" {
				continue
			}
			for _, val := range v.Values {
				lit, ok := val.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a Code constant is not a string literal: %#v", val)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				codes = append(codes, Code(s))
			}
		}
	}
	if len(codes) != 26 {
		t.Fatalf("%d Code constants in errors.go, want the 26 rows of §4.4", len(codes))
	}
	retryable := []Code{
		CodeBusy, CodeBudget, CodeTargetChanged, CodeTargetExited, CodePIDReused,
		CodeStartTimeout, CodeFinishTimeout, CodeNoOutput, CodePerfAgentFailed,
	}
	for _, c := range codes {
		r, ok := DefaultRetryable[c]
		if !ok {
			t.Errorf("%s has no DefaultRetryable entry", c)
			continue
		}
		if want := slices.Contains(retryable, c); r != want {
			t.Errorf("DefaultRetryable[%s] = %v, want %v", c, r, want)
		}
	}
	if len(DefaultRetryable) != len(codes) {
		t.Errorf("DefaultRetryable has keys that are not constants: %q", slices.Sorted(maps.Keys(DefaultRetryable)))
	}
	if e := Errorf(CodeBusy, "wait", "held by %d", 7); !e.Retryable || e.Message != "held by 7" || e.Next != "wait" {
		t.Errorf("Errorf(busy) = %+v", e)
	}
	if e := Errorf(CodeDisabled, "", "off"); e.Retryable || e.Error() != "disabled: off" {
		t.Errorf("Errorf(disabled) = %+v, Error() %q", e, e.Error())
	}
}
