//go:build integration

package test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	pprofpb "github.com/google/pprof/profile"
)

// fixture describes one debuginfod-integration workload: how to build the
// binary, where its unstripped artifact lives (for upload.sh), where the
// stripped/release artifact to spawn lives, and which function names we
// expect to see in the resolved pprof.
type fixture struct {
	name        string
	build       func(t *testing.T, fixtureDir string)
	unstripped  string // path under fixtureDir; passed to upload.sh
	spawn       string // path under fixtureDir; the process we profile
	wantAnyFunc []string
}

// cFixture is the original C workload from the PoC archive. The Makefile
// produces hello (stripped), hello.full (unstripped), hello.debug.
var cFixture = fixture{
	name:        "c",
	build:       func(t *testing.T, dir string) { mustMake(t, dir+"/sample", "all") },
	unstripped:  "sample/hello.full",
	spawn:       "sample/hello",
	wantAnyFunc: []string{"deep_function", "middle_function", "main"},
}

// rustFixture is the Rust workload built with `[profile.release]` mirroring
// realistic production settings: opt-level=3, lto, codegen-units=1,
// panic=abort, debug="line-tables-only", strip=none, frame pointers
// forced on. Validates that perf-agent + debuginfod can recover function
// names from a release-profile DWARF subset (line tables only).
var rustFixture = fixture{
	name: "rust_release_line_tables_only",
	build: func(t *testing.T, dir string) {
		if _, err := exec.LookPath("cargo"); err != nil {
			t.Skipf("cargo not available: %v", err)
		}
		cmd := exec.Command("cargo", "build", "--release")
		cmd.Dir = dir + "/rust-sample"
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("cargo build: %v", err)
		}
	},
	// Cargo's release profile keeps the binary unstripped (strip = "none"),
	// so the same binary serves both as the upload source and the spawned
	// process. upload.sh extracts .debug from it; we then spawn it directly.
	unstripped:  "rust-sample/target/release/rust_sample",
	spawn:       "rust-sample/target/release/rust_sample",
	wantAnyFunc: []string{"deep_function", "middle_function", "rust_sample::main", "main"},
}

func TestSymbolizeViaDebuginfod(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if !hasDocker(t) {
		t.Skip("docker not available")
	}

	fixtureDir := absFixtureDir(t)

	// Per-fixture: build first so we fail fast on missing toolchain.
	// We do this inside each subtest so missing cargo only skips Rust,
	// and missing gcc only skips C.
	fixtures := []fixture{cFixture, rustFixture}

	// Reset the debuginfo-store before bringing the server up. A previous
	// docker run may have left it root-owned.
	storeDir := fixtureDir + "/debuginfo-store"
	if err := os.RemoveAll(storeDir); err != nil {
		t.Fatalf("clean debuginfo-store: %v", err)
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("create debuginfo-store: %v", err)
	}

	// Build + upload each fixture's debuginfo BEFORE compose up. Skipped
	// fixtures simply contribute nothing to the store; the parent test
	// continues so other fixtures can still run.
	uploaded := []fixture{}
	for _, fx := range fixtures {
		fx := fx
		t.Run(fx.name+"/build_upload", func(t *testing.T) {
			fx.build(t, fixtureDir) // may t.Skip
			if err := runScriptEnv(
				fixtureDir+"/upload.sh",
				[]string{"STORE=" + storeDir},
				filepath.Join(fixtureDir, fx.unstripped),
			); err != nil {
				t.Fatalf("upload %s: %v", fx.name, err)
			}
		})
		// If build_upload skipped, we won't have a spawn target — drop it.
		if _, err := os.Stat(filepath.Join(fixtureDir, fx.spawn)); err == nil {
			uploaded = append(uploaded, fx)
		}
	}
	if len(uploaded) == 0 {
		t.Skip("no fixtures available (no gcc + no cargo)")
	}

	startCompose(t, fixtureDir)
	t.Cleanup(func() { stopCompose(t, fixtureDir) })

	waitForServer(t, "http://localhost:8002", 90*time.Second)

	for _, fx := range uploaded {
		fx := fx
		t.Run(fx.name+"/symbolize", func(t *testing.T) {
			profileAndAssert(t, fixtureDir, fx)
		})
	}
}

// profileAndAssert spawns the fixture, runs perf-agent against it pointed
// at the local debuginfod, and asserts at least one of the fixture's
// expected function names appears in the resulting pprof.
func profileAndAssert(t *testing.T, fixtureDir string, fx fixture) {
	t.Helper()
	bin := agentBinary(t)

	cmd := exec.Command(filepath.Join(fixtureDir, fx.spawn))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", fx.name, err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	agent := exec.Command(bin,
		"--profile",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "3s",
		"--profile-output", out,
		"--debuginfod-url", "http://localhost:8002",
		"--symbol-cache-dir", t.TempDir(),
	)
	agent.Stdout = os.Stdout
	agent.Stderr = os.Stderr
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent run for %s: %v", fx.name, err)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer func() { _ = gr.Close() }()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read inflated: %v", err)
	}

	p, err := pprofpb.ParseUncompressed(raw)
	if err != nil {
		t.Fatalf("parse pprof: %v", err)
	}

	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
		// Rust mangling preserves the unmangled name in DWARF; blazesym
		// demangles to a Rust-style "crate::module::func" form. Match the
		// suffix in case we got the mangled symbol.
		for _, want := range fx.wantAnyFunc {
			if fn.Name == want {
				return
			}
		}
	}
	// Also scan as fuzzy contains for Rust-mangled-name fallback.
	for _, fn := range p.Function {
		for _, want := range fx.wantAnyFunc {
			if want != "" && contains(fn.Name, want) {
				return
			}
		}
	}
	names := make([]string, 0, len(got))
	for k := range got {
		names = append(names, k)
	}
	slices.Sort(names)
	t.Fatalf("no expected fixture function for %s; wanted any of %v; got: %v",
		fx.name, fx.wantAnyFunc, names)
}

func contains(s, sub string) bool {
	if sub == "" {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func hasDocker(t *testing.T) bool {
	t.Helper()
	return exec.Command("docker", "info").Run() == nil
}

func absFixtureDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return filepath.Join(wd, "debuginfod")
}

func startCompose(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", dir+"/docker-compose.yml", "up", "-d", "debuginfod")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("compose up: %v", err)
	}
}

func stopCompose(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("docker", "compose", "-f", dir+"/docker-compose.yml", "down", "-v")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func mustMake(t *testing.T, dir, target string) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not available: %v", err)
	}
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skipf("gcc not available: %v", err)
	}
	cmd := exec.Command("make", "-C", dir, target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("make %s: %v", target, err)
	}
}

// runScriptEnv runs a script with additional environment variables appended to
// the current process environment, plus any positional args.
func runScriptEnv(path string, env []string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func waitForServer(t *testing.T, base string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		ctx, cancel := context.WithTimeout(t.Context(), 1*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+"/metrics", nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("debuginfod server at %s did not become ready in %v", base, deadline)
}

func agentBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("PERF_AGENT_BIN")
	if bin == "" {
		t.Skip("PERF_AGENT_BIN not set; skipping integration test")
	}
	return bin
}
