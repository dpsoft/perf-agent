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
	"strconv"
	"testing"
	"time"

	pprofpb "github.com/google/pprof/profile"
)

func TestSymbolizeViaDebuginfod(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if !hasDocker(t) {
		t.Skip("docker not available")
	}

	fixtureDir := absFixtureDir(t)

	// Build the fixture binary before touching Docker so we fail fast on
	// missing toolchain rather than after burning time on compose up.
	if err := runMake(fixtureDir+"/sample", "all"); err != nil {
		t.Fatalf("build fixture: %v", err)
	}

	// Ensure the debuginfo-store directory is writable by the current user.
	// A previous docker run may have created it owned by root. Remove and
	// recreate so upload.sh can populate it.
	storeDir := fixtureDir + "/debuginfo-store"
	if err := os.RemoveAll(storeDir); err != nil {
		t.Fatalf("clean debuginfo-store: %v", err)
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatalf("create debuginfo-store: %v", err)
	}

	// Populate the store BEFORE compose up — the container mounts it :ro and
	// rescans every 10s, so pre-population is fastest.
	// upload.sh uses STORE env var; set it to the absolute path.
	if err := runScriptEnv(fixtureDir+"/upload.sh", []string{"STORE=" + storeDir}, fixtureDir+"/sample/hello"); err != nil {
		t.Fatalf("upload: %v", err)
	}

	startCompose(t, fixtureDir)
	t.Cleanup(func() { stopCompose(t, fixtureDir) })

	waitForServer(t, "http://localhost:8002", 90*time.Second)

	cmd := exec.Command(fixtureDir + "/sample/hello")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hello: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	out := filepath.Join(t.TempDir(), "profile.pb.gz")
	bin := agentBinary(t)
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
		t.Fatalf("perf-agent run: %v", err)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	defer gr.Close()
	raw, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("read inflated: %v", err)
	}

	p, err := pprofpb.ParseUncompressed(raw)
	if err != nil {
		t.Fatalf("parse pprof: %v", err)
	}

	wantAny := []string{"deep_function", "middle_function", "main"}
	got := map[string]bool{}
	for _, fn := range p.Function {
		got[fn.Name] = true
	}
	var matched bool
	for _, w := range wantAny {
		if got[w] {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("no expected fixture function in profile; have: %+v", got)
	}
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

func runMake(dir, target string) error {
	cmd := exec.Command("make", "-C", dir, target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runScript(path string, args ...string) error {
	cmd := exec.Command(path, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
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
