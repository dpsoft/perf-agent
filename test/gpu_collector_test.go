//go:build linux

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

// The collector's load-bearing claim is that ONE uprobe link with PID: 0
// covers processes created after the link exists. Everything else in the
// design follows from it: no pod watch, no link per pod, no race against
// cuInit.
//
// TWO conditions make these tests mean anything, and each is worthless
// without the other:
//
//   - Two workloads. With one, a pass cannot distinguish PID: 0 from a
//     lucky single-target attach.
//   - Rediscovery OFF. At the 5s default, the second workload appearing
//     proves nothing -- a rescan could have found and registered it, and
//     the output is identical either way.
//
// Both were run by hand on an RTX 3090 on 2026-09-15 before the design was
// written: A 296,364 samples / B 120,000 with B never discovered; C
// 298,844 / decoy absent entirely.

const (
	collectorBinary = "../gpu-cuda-profile"
	shimBinary      = "../shim/libperfagent-gpu-nvidia.so"
	workloadBinary  = "../shim/nvidia/testdata/cuda_workload"
)

func requireGPUCollector(t *testing.T) string {
	t.Helper()
	for _, p := range []string{collectorBinary, shimBinary, workloadBinary} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing %s: build with `go build -o gpu-cuda-profile ./cmd/gpu-cuda-profile` "+
				"and `make -C shim nvidia`", p)
		}
	}
	if err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Run(); err != nil {
		t.Skipf("no usable NVIDIA GPU (nvidia-smi: %v)", err)
	}
	abs, err := filepath.Abs(collectorBinary)
	if err != nil {
		t.Fatal(err)
	}
	// requireBPFRunnable lives in integration_test.go; with a non-empty
	// path it accepts file capabilities on the binary we exec, which is
	// what a setcap'd agent has.
	requireBPFRunnable(t, abs)
	return abs
}

// stageShimDir copies the built shim into its own directory, giving it an
// inode distinct from every other copy.
func stageShimDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body, err := os.ReadFile(shimBinary)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(dst, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

type workload struct {
	cmd *exec.Cmd
	pid int
}

// startWorkload launches cuda_workload under the given shim. iters sets how
// long it lives: 4000 iterations is roughly 1.1s.
func startWorkload(t *testing.T, shim string, iters int) *workload {
	t.Helper()
	abs, err := filepath.Abs(workloadBinary)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(abs, strconv.Itoa(iters), "200", "0")
	cmd.Env = append(os.Environ(),
		"CUDA_INJECTION64_PATH="+shim,
		"PERFAGENT_GPU_SAMPLE_PERIOD=1",
		"PERFAGENT_GPU_LOG=stderr",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start workload: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &workload{cmd: cmd, pid: cmd.Process.Pid}
}

type collectorRun struct {
	cmd  *exec.Cmd
	out  string
	logs *strings.Builder
}

// startCollector runs the agent attached to shim with rediscovery disabled.
//
// The hour-long rediscover interval is the point, not a detail: with the
// 5s default this test cannot tell the uprobe from a rescan.
func startCollector(t *testing.T, agent, shim string, dur time.Duration) *collectorRun {
	t.Helper()
	out := filepath.Join(t.TempDir(), "collector.pb.gz")
	logs := &strings.Builder{}
	cmd := exec.Command(agent,
		"-discover",
		"-shim", shim,
		"-wait-for-shim", "30s",
		"-rediscover-every", "1h",
		"-duration", dur.String(),
		"-out", out,
	)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start collector: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &collectorRun{cmd: cmd, out: out, logs: logs}
}

var discoveredRE = regexp.MustCompile(`discovered \d+ process\(es\) mapping [^:]+: \[([0-9 ]*)\]`)

// waitForAttach blocks until the agent logs its discovery line, which is
// emitted immediately after the uprobe link is created.
func (c *collectorRun) waitForAttach(t *testing.T) []int {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if m := discoveredRE.FindStringSubmatch(c.logs.String()); m != nil {
			var pids []int
			for _, f := range strings.Fields(m[1]) {
				n, err := strconv.Atoi(f)
				if err != nil {
					t.Fatalf("unparsable pid %q in discovery line", f)
				}
				pids = append(pids, n)
			}
			return pids
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("collector never logged a discovery line in 45s; output:\n%s", c.logs.String())
	return nil
}

func (c *collectorRun) waitAndCollect(t *testing.T) string {
	t.Helper()
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("collector exited badly: %v\noutput:\n%s", err, c.logs.String())
	}
	if _, err := os.Stat(c.out); err != nil {
		t.Fatalf("no profile written: %v\noutput:\n%s", err, c.logs.String())
	}
	return c.out
}

var gpuPIDRE = regexp.MustCompile(`gpu_pid:\s*(\d+)`)

// gpuPIDsIn reads the distinct gpu_pid labels out of a profile.
func gpuPIDsIn(t *testing.T, profile string) map[int]bool {
	t.Helper()
	out, err := exec.Command("go", "tool", "pprof", "-traces", profile).Output()
	if err != nil {
		t.Fatalf("go tool pprof -traces %s: %v", profile, err)
	}
	pids := map[int]bool{}
	for _, m := range gpuPIDRE.FindAllStringSubmatch(string(out), -1) {
		n, cerr := strconv.Atoi(m[1])
		if cerr != nil {
			continue
		}
		pids[n] = true
	}
	return pids
}

func TestPIDZeroCoversAProcessStartedAfterAttach(t *testing.T) {
	agent := requireGPUCollector(t)
	shim := stageShimDir(t)

	// A exists first only because -discover needs one target to attach in
	// agent mode. B is the process under test.
	a := startWorkload(t, shim, 200000)
	time.Sleep(4 * time.Second)

	c := startCollector(t, agent, shim, 40*time.Second)
	discovered := c.waitForAttach(t)

	b := startWorkload(t, shim, 60000)
	profile := c.waitAndCollect(t)

	pids := gpuPIDsIn(t, profile)
	if !pids[a.pid] {
		t.Fatalf("workload A (%d) is absent; the harness is broken, not the feature.\n%s",
			a.pid, c.logs.String())
	}
	if !pids[b.pid] {
		t.Fatalf("workload B (%d) is absent. It started AFTER the link was created and "+
			"rediscovery was disabled, so PID:0 does not cover processes created after "+
			"attach -- which is the whole collector design.\n%s", b.pid, c.logs.String())
	}
	for _, d := range discovered {
		if d == b.pid {
			t.Fatal("B appears in the discovery line, so it was found by a scan and this " +
				"run did not exercise PID:0 at all")
		}
	}
}

func TestADifferentInodeIsNotCovered(t *testing.T) {
	agent := requireGPUCollector(t)

	// Identical bytes, two directories, therefore two inodes -- which is
	// two attach targets to a uprobe.
	attached := stageShimDir(t)
	decoy := stageShimDir(t)

	c1 := startWorkload(t, attached, 200000)
	time.Sleep(4 * time.Second)

	c := startCollector(t, agent, attached, 40*time.Second)
	c.waitForAttach(t)

	d := startWorkload(t, decoy, 60000)
	profile := c.waitAndCollect(t)

	pids := gpuPIDsIn(t, profile)
	if !pids[c1.pid] {
		t.Fatalf("workload C (%d) is absent; the harness is broken.\n%s", c1.pid, c.logs.String())
	}
	if pids[d.pid] {
		t.Fatalf("workload D (%d) appears despite mapping a DIFFERENT inode holding "+
			"identical bytes. The (dev, ino) rule the whole hostPath layout rests on does "+
			"not hold.\n%s", d.pid, c.logs.String())
	}
	// Without this, the test above could pass simply because nothing is
	// ever covered.
	if len(pids) == 0 {
		t.Fatal("no gpu_pid labels at all; the run captured nothing and proves nothing")
	}
}

// startCollectorMode runs the agent in collector mode: it installs the
// carried shim into dir and attaches to every shim inode there that has a
// live mapper.
func startCollectorMode(t *testing.T, agent, carried, dir string, dur time.Duration) *collectorRun {
	t.Helper()
	out := filepath.Join(t.TempDir(), "collector.pb.gz")
	logs := &strings.Builder{}
	cmd := exec.Command(agent,
		"-mode", "collector",
		"-shim", carried,
		"-shim-dir", dir,
		"-wait-for-shim", "10s",
		"-rediscover-every", "1h",
		"-duration", dur.String(),
		"-out", out,
	)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start collector: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &collectorRun{cmd: cmd, out: out, logs: logs}
}

// TestAnOlderShimStillMappedIsAlsoAttached is the upgrade case, and it is
// the one most likely to be skipped because it passes trivially when
// nothing is running.
//
// Replacing the shim is a rename(2), which leaves the previous inode alive
// for every process that already mapped it -- that survival is exactly what
// makes the replace safe. An agent that attached only to the file it just
// installed would silently stop covering every workload already running,
// with no error and no clue beyond a thinner profile.
func TestAnOlderShimStillMappedIsAlsoAttached(t *testing.T) {
	agent := requireGPUCollector(t)

	dir := t.TempDir()
	body, err := os.ReadFile(shimBinary)
	if err != nil {
		t.Fatal(err)
	}
	// A previous version, already in the shim directory and already mapped
	// -- what a rename(2) upgrade leaves behind.
	older := filepath.Join(dir, "libperfagent-gpu-nvidia.so.previous")
	if err := os.WriteFile(older, body, 0o755); err != nil {
		t.Fatal(err)
	}
	carried := stageShimDir(t) // the copy the agent "ships with" and installs

	old := startWorkload(t, older, 200000)
	time.Sleep(4 * time.Second)

	c := startCollectorMode(t, agent, carried, dir, 40*time.Second)
	c.waitForAttach(t)

	// A workload on the newly installed shim, started after attach.
	fresh := startWorkload(t, filepath.Join(dir, "libperfagent-gpu-nvidia.so"), 60000)
	profile := c.waitAndCollect(t)

	pids := gpuPIDsIn(t, profile)
	if !pids[fresh.pid] {
		t.Fatalf("the workload on the INSTALLED shim (%d) is absent; collector mode is not "+
			"attaching to the file it installed.\n%s", fresh.pid, c.logs.String())
	}
	if !pids[old.pid] {
		t.Fatalf("the workload on the PREVIOUS shim (%d) is absent. An upgrade leaves the "+
			"old inode alive for its mappers, so attaching only to the current file drops "+
			"every workload that was already running -- silently.\n%s", old.pid, c.logs.String())
	}
	if !strings.Contains(c.logs.String(), "also attaching to") {
		t.Errorf("no log line about the additional inode; the second link may have been " +
			"created by accident rather than by the upgrade path")
	}
}
