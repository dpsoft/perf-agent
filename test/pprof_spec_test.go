package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestGoToolPprofPutsFlatTimeOnTheHotLeaf is the regression test for #163.
//
// perf-agent stored Sample.Location root-first while the pprof proto
// specifies the leaf at index 0, so go tool pprof read every profile inside
// out: 99.29% of flat time landed on runtime.goexit -- a trampoline that
// executes no user work -- and the genuinely hot leaf showed zero.
//
// No in-tree test could catch that. internal/foldedstacks' fixtures were
// written to match the encoder, so they proved only that the encoder
// matched the fixtures; one of them was even named
// "...RootFirstIsTheDefaultBecauseThatIsWhatPerfAgentWrites". The only
// authority on what the proto means is a reader that implements it, so this
// asserts against go tool pprof and nothing else.
//
// The workload spins in math.Sin, so flat time belongs to math.sin. If the
// order regresses it lands on runtime.goexit instead, and the failure names
// both.
func TestGoToolPprofPutsFlatTimeOnTheHotLeaf(t *testing.T) {
	requireBPFRunnable(t, getAgentPath(t))

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	cmd, cleanup := spawnCPUBoundWorkload(t)
	defer cleanup()

	out := filepath.Join(t.TempDir(), "cpu.pb.gz")
	agent := exec.Command(getAgentPath(t),
		"--profile",
		"--pid", strconv.Itoa(cmd.Process.Pid),
		"--duration", "8s",
		"--profile-output", out,
	)
	agent.Stdout, agent.Stderr = os.Stdout, os.Stderr
	agent.Env = append(os.Environ(), "DEBUGINFOD_URLS=")
	if err := agent.Run(); err != nil {
		t.Fatalf("perf-agent: %v", err)
	}

	top, err := exec.Command("go", "tool", "pprof", "-top", "-nodecount=10", out).CombinedOutput()
	if err != nil {
		t.Fatalf("go tool pprof -top: %v\n%s", err, top)
	}
	text := string(top)
	t.Logf("go tool pprof -top:\n%s", strings.TrimSpace(text))

	// Rows look like: "  4780ms 79.80% 79.80%  5140ms 85.81%  math.sin"
	row := regexp.MustCompile(`(?m)^\s*(\S+)\s+([\d.]+)%\s+[\d.]+%\s+\S+\s+[\d.]+%\s+(\S+)\s*$`)
	var topFlat string
	var topFlatPct float64
	for _, m := range row.FindAllStringSubmatch(text, -1) {
		pct, convErr := strconv.ParseFloat(m[2], 64)
		if convErr != nil {
			continue
		}
		if pct > topFlatPct {
			topFlatPct, topFlat = pct, m[3]
		}
	}
	if topFlat == "" {
		t.Fatalf("could not parse any row out of go tool pprof -top:\n%s", text)
	}
	t.Logf("highest flat: %s at %.2f%%", topFlat, topFlatPct)

	// Frames that can only be outermost. Flat time here means the stack is
	// stored the wrong way round.
	for _, rootOnly := range []string{
		"runtime.goexit", "runtime.mstart", "runtime.rt0_go",
		"_start", "__libc_start_main", "__GI___clone3", "start_thread",
	} {
		if topFlat == rootOnly {
			t.Fatalf("go tool pprof attributes the most flat time (%.2f%%) to %s, which "+
				"executes no user work. Sample.Location is stored root-first while the "+
				"pprof proto specifies leaf-first (#163).\n%s", topFlatPct, topFlat, text)
		}
	}
}

// spawnCPUBoundWorkload starts the Go cpu_bound workload and returns it with
// a cleanup. It burns CPU in math.Sin, so the hot leaf is known in advance,
// which is what lets the test above assert on WHERE flat time lands rather
// than merely that some landed somewhere.
func spawnCPUBoundWorkload(t *testing.T) (*exec.Cmd, func()) {
	t.Helper()
	bin := "./workloads/go/cpu_bound"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("go cpu_bound workload not built: %v", err)
	}
	cmd := exec.Command(bin, "-duration", "40s", "-threads", "2")
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cpu_bound: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	return cmd, func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
}
