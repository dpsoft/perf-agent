package gpuprobe_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
)

// hasBPFAndPerfmon mirrors perfagent/agent.go's hasCapSysPtrace: check
// Permitted as well as Effective, because a setcap'd binary has not promoted
// Permitted yet, and never gate on Getuid alone.
func hasBPFAndPerfmon() bool {
	if os.Geteuid() == 0 {
		return true
	}
	set := cap.GetProc()
	if set == nil {
		return false
	}
	for _, want := range []cap.Value{cap.BPF, cap.PERFMON} {
		ok := false
		for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
			if have, err := set.GetFlag(flag, want); err == nil && have {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// The Phase 3 gate: the stub drives the full pipeline to pprof samples on a
// machine with no GPU.
func TestStubDrivesThePipelineToPprofWithoutAGPU(t *testing.T) {
	if !hasBPFAndPerfmon() {
		t.Skip("needs CAP_BPF and CAP_PERFMON; setcap the test binary")
	}
	stub := filepath.Join("..", "shim", "perfagent-gpu-stub")
	requireBuilt(t, stub)

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: stub,
		Backend:  gpu.GPUBackendID("stub"),
		Sink:     timeline,
	})
	require.NoError(t, err)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The stub only emits once the semaphore says someone is listening, so it
	// must start after Attach.
	//
	// stderr is captured separately from stdout because the stub reports its
	// own producer-side drop counters there: with a consumer attached the
	// semaphore is non-zero for the whole run, so nothing may be dropped
	// before it ever reaches a probe. A consumer-side counter cannot see that
	// loss — it happens in the producer, upstream of the ringbuf.
	cmd := exec.Command(stub, "500", "200")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	require.NoError(t, err, "stdout: %s stderr: %s", stdout.String(), stderr.String())
	stubErr := stderr.String()

	// perfagent_stub_run flushes both batches synchronously before the
	// child process exits, and cmd.Run above already blocked until
	// then — so every record is in the ringbuf by this point. The sleep is
	// for our own Run() goroutine to drain it, not for the stub.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done

	// Producer-side loss, from the stub's own accounting. With a consumer
	// attached the whole run, every add() and flush() saw an armed semaphore,
	// so both counters must read zero.
	assert.Contains(t, stubErr, "launch_dropped=0",
		"the stub dropped launches: its semaphore read zero while a consumer was attached")
	assert.Contains(t, stubErr, "exec_dropped=0",
		"the stub dropped execs: its semaphore read zero while a consumer was attached")

	stats := c.Stats()
	assert.Zero(t, stats.SequenceGaps, "no batch may be lost silently")
	// SequenceGaps == 0 means no batch was lost, so the record count is
	// exact, not a lower bound: 500 launches + 500 execs.
	assert.Equal(t, uint64(1000), stats.Records, "500 launches + 500 execs, none lost")
	// Records is incremented *before* the sink call, so it alone does not
	// prove the timeline accepted anything — every other loss counter has to
	// be zero for the count above to mean what it looks like it means.
	assert.Zero(t, stats.SinkRejected,
		"Records counts before the sink call, so a rejection here means the timeline never took the event")
	assert.Zero(t, stats.Malformed,
		"a ringbuf sample that did not decode: a short header, or a payload shorter than the header claims")
	assert.Zero(t, stats.Undecoded,
		"the stub emits only launches and execs; an undecoded record means a kind arrived that this phase does not normalize")
	assert.Zero(t, stats.KernelDropped,
		"records the BPF program could not deliver: an oversized batch, a full ringbuf, or a faulting read of the producer's buffer")
	assert.Zero(t, stats.ZeroCorrelation,
		"the stub never emits correlation 0, so no record may have been demoted to the heuristic join")

	snap := timeline.Snapshot()
	samples := gpu.ProjectExecutions(snap)
	require.NotEmpty(t, samples, "the gate is pprof samples, not counters")

	// ProjectExecutions emits one sample per execution regardless of join
	// status - a launch that aged out of the cache before its exec arrived
	// still produces a sample, just with no [gpu:launch]-attributed launch
	// underneath it. So sample count alone cannot distinguish "everything
	// joined" from "nothing joined": that has to come from JoinStats
	// directly.
	assert.Zero(t, snap.JoinStats.UnmatchedExecutionCount,
		"an unmatched execution means its launch aged out of the cache or never arrived - the stub emits both sides, so this must be zero")
	assert.Equal(t, uint64(len(snap.Executions)), snap.JoinStats.ExactExecutionJoinCount,
		"every execution must join its launch by exact correlation, not a weaker path")
	assert.Positive(t, snap.JoinStats.ExactExecutionJoinCount,
		"the exact-join and unmatched assertions above are vacuous on an empty snapshot; there must be at least one real join")
	// Every execution joined its launch exactly, because the stub emits a
	// correlation on both sides.
	assert.Zero(t, snap.JoinStats.HeuristicExecutionJoinCount,
		"the stub supplies correlations; no join should need the heuristic")
}

func requireBuilt(t *testing.T, path string) {
	t.Helper()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make unavailable")
	}
	// filepath.Dir(path), not a double Dir: path is ".../shim/perfagent-gpu-stub"
	// and the Makefile with the perfagent-gpu-stub target lives in
	// ".../shim", not one level above it.
	cmd := exec.Command("make", "-C", filepath.Dir(path), "perfagent-gpu-stub")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build stub: %s", out)
}
