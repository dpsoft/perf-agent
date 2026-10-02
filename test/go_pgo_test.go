package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestGoPGOAcceptsAndAppliesOurProfile is the regression test for #171.
//
// Go's PGO matches profile functions on (name, start_line) and refuses a
// profile that omits the field outright:
//
//	preprofile: error parsing profile: profile missing Function.start_line data
//
// perf-agent populated 0 of 22 functions, so `go build -pgo=` rejected every
// profile it wrote, while README.md claimed the opposite.
//
// The assertion is in two parts, and the second is the one that matters.
// Acceptance alone proves only that the field is PRESENT; a wrong value is
// accepted too, and then silently matches nothing -- which would be worse
// than the loud error it replaced. So this also requires the compiler to
// report a PGO decision, which it can only do after matching a function
// against its own records.
func TestGoPGOAcceptsAndAppliesOurProfile(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	src := "./workloads/go/cpu_bound.go"
	if _, err := os.Stat(src); err != nil {
		t.Skipf("go workload source not present: %v", err)
	}

	cmd, cleanup := spawnCPUBoundWorkload(t)
	defer cleanup()

	dir := t.TempDir()
	prof := filepath.Join(dir, "cpu.pb.gz")
	agent := exec.Command(getAgentPath(t),
		"--profile",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "8s",
		"--profile-output", prof,
	)
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	agent.Env = append(os.Environ(), "DEBUGINFOD_URLS=")
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent: %v", err)
	}

	// Part 1: the toolchain accepts it.
	build := exec.Command("go", "build",
		"-pgo="+prof,
		"-gcflags=-d=pgodebug=1",
		"-o", filepath.Join(dir, "app"),
		src,
	)
	out, err := build.CombinedOutput()
	text := string(out)
	t.Logf("go build -pgo output:\n%s", strings.TrimSpace(text))
	if strings.Contains(text, "missing Function.start_line") {
		t.Fatalf("go build -pgo= still rejects the profile for want of start_line (#171):\n%s", text)
	}
	if err != nil {
		t.Fatalf("go build -pgo=: %v\n%s", err, text)
	}

	// Part 2: it matched something. pgodebug prints a line per PGO decision,
	// and the compiler can only reach one by matching a profile function
	// against (name, start_line). A populated-but-wrong start_line produces
	// a clean build with no such line, which is the silent failure this
	// guards.
	if !strings.Contains(text, "hot-node") && !strings.Contains(text, "hot-callsite") {
		t.Fatalf("the profile was accepted but the compiler reported no PGO decision, so no "+
			"function matched. start_line is present but wrong (#171).\n%s", text)
	}
}
