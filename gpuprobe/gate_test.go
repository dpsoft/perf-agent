package gpuprobe_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
	"github.com/dpsoft/perf-agent/symbolize"
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
	built := filepath.Join("..", "shim", "perfagent-gpu-stub")
	requireBuilt(t, built)
	// Attach to a private copy, not to the shared build output. Uprobes key
	// on the binary's *inode*, and this consumer must attach system-wide
	// (Config.PID is zero) because the stub process does not exist yet — the
	// stub only emits once the semaphore says someone is listening, so it has
	// to be launched after Attach. A system-wide attach on the shared inode
	// therefore also collects records from *any other* process on the machine
	// running that same image: a concurrent gate run in CI, a second
	// developer, a stray perfagent-gpu-stub. That is not hypothetical — a run
	// failed with "Records: expected 1000, actual 1128" for exactly this
	// reason. A per-run copy has an inode nobody else can execute, which is
	// what makes the exact count below deterministic.
	stub := privateStubCopy(t, built)

	// A real symbolizer, not nil: without one every capture counts as
	// SymbolizeFailed and no stack ever resolves, failing the gate for a
	// reason that has nothing to do with the transport under test. This is
	// the same construction profile/ uses.
	sym, err := symbolize.NewLocalSymbolizer()
	require.NoError(t, err)
	defer func() { _ = sym.Close() }()

	timeline := gpu.NewTimeline(gpu.TimelineConfig{})
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath:   stub,
		Backend:    gpu.GPUBackendID("stub"),
		Sink:       timeline,
		Symbolizer: sym,
	})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()

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
	// sample_period=8 explicit (it also happens to be the stub's default):
	// with 500 launches the sampler is deterministic and yields exactly 63
	// sampled launches (n%8==0 for n in 0..499, i.e. ceil(500/8)).
	cmd := exec.Command(stub, "500", "200", "8")
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

	// Parse the stub's own sampler accounting so it can be checked against
	// what the consumer counted: producer and consumer must not disagree
	// about how many stacks were taken.
	stubLine := regexp.MustCompile(`observed=(\d+) sampled=(\d+) period=(\d+)`).FindStringSubmatch(stubErr)
	require.Len(t, stubLine, 4, "stub stderr did not match the expected format: %s", stubErr)
	stubObserved, err := strconv.ParseUint(stubLine[1], 10, 64)
	require.NoError(t, err)
	stubSampled, err := strconv.ParseUint(stubLine[2], 10, 64)
	require.NoError(t, err)
	stubPeriod, err := strconv.ParseUint(stubLine[3], 10, 64)
	require.NoError(t, err)
	assert.Equal(t, uint64(500), stubObserved, "the sampler must see every one of the 500 launches")
	assert.Equal(t, uint64(8), stubPeriod, "the stub was invoked with sample_period=8")
	assert.Equal(t, uint64(63), stubSampled, "ceil(500/8): the sampler is deterministic at period 8 over 500 launches")

	stats := c.Stats()
	assert.Zero(t, stats.SequenceGaps, "no batch may be lost silently")
	// Records == 1000 (500 launches + 500 execs) is no longer a safe
	// assertion: kernel-name records also flow now, and their count is
	// non-deterministic - it depends on when the consumer attaches relative
	// to the producer's late-attach replay tick. Assert on the launches and
	// executions specifically (via the deterministic join-count and
	// sampled-launch handles below) instead of a total that can legitimately
	// vary.
	assert.Equal(t, stubSampled, stats.SampledLaunches,
		"the producer's own sampled= count and the consumer's SampledLaunches must not disagree")
	assert.Equal(t, uint64(63), stats.SampledLaunches,
		"ceil(500/8) sampled launches, deterministic because the sampler is")
	// Records is incremented *before* the sink call, so it alone does not
	// prove the timeline accepted anything — every other loss counter has to
	// be zero for the count above to mean what it looks like it means.
	assert.Zero(t, stats.SinkRejected,
		"Records counts before the sink call, so a rejection here means the timeline never took the event")
	assert.Zero(t, stats.Malformed,
		"a ringbuf sample that did not decode: a short header, or a payload shorter than the header claims")
	assert.Zero(t, stats.Undecoded,
		"the stub emits only launches, execs, sampled launches and kernel names, all of which this phase normalizes; an undecoded record means a kind arrived that it does not")
	assert.Zero(t, stats.KernelDropped,
		"records the BPF program could not deliver: an oversized batch, a full ringbuf, or a faulting read of the producer's buffer")
	assert.Zero(t, stats.ZeroCorrelation,
		"the stub never emits correlation 0, so no record may have been demoted to the heuristic join")
	assert.Zero(t, stats.StacksMissing,
		"the stub is built with frame pointers; a missing capture means bpf_get_stackid failed on a binary that should always succeed")
	assert.Zero(t, stats.StacksEvicted,
		"the parked-stack side table must never overflow at this launch rate and capacity")
	assert.Zero(t, stats.StackLookupFailed,
		"every resolved stack's stackmap entry must be readable back exactly once")
	assert.Zero(t, stats.StackDeleteFailed, "every stackmap entry read must also be deletable")
	assert.Zero(t, stats.StacksUncorrelated,
		"the stub never emits correlation 0 on a sampled launch")
	assert.Zero(t, stats.SymbolizeFailed,
		"a real Symbolizer is configured; every captured stack must resolve to at least one frame")
	assert.Zero(t, stats.KernelNamesUnresolved,
		"the stub interns both kernel names before any launch or exec references them; every event must carry a name")

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
	assert.Len(t, snap.Executions, 500, "500 launches + 500 execs, exactly, none lost")

	// Every execution must carry a kernel name resolved through interning -
	// KernelNamesUnresolved == 0 above is the aggregate count, this is the
	// same fact checked per event.
	for _, view := range snap.Executions {
		assert.NotEmpty(t, view.Exec.KernelName,
			"execution for correlation %s has no kernel name", view.Exec.Correlation.Value)
	}

	// Sampled-stack accounting: exactly 63 of the 500 launches reached the
	// timeline with a non-empty CPUStack, and every one of those stacks
	// names the stub's own launch function. The stub is built with frame
	// pointers, so a failure here is the capture or symbolization path, not
	// the workload — see resolveStackLocked in consumer.go.
	var sampledLaunches int
	for _, view := range snap.Executions {
		require.NotNil(t, view.Launch,
			"every execution joined its launch exactly (asserted above), so Launch must never be nil here")
		if len(view.Launch.Launch.CPUStack) == 0 {
			continue
		}
		sampledLaunches++
		var sawStubFrame bool
		for _, f := range view.Launch.Launch.CPUStack {
			if strings.Contains(f.Name, "perfagent_stub_run") {
				sawStubFrame = true
				break
			}
		}
		assert.True(t, sawStubFrame,
			"sampled launch's CPUStack does not contain a frame from the stub's own launch function: %+v",
			view.Launch.Launch.CPUStack)
	}
	assert.Equal(t, 63, sampledLaunches,
		"exactly 63 launches on the timeline must carry a non-empty CPUStack, matching Stats.SampledLaunches")

	// The two projected populations - stack-attributed and unattributed -
	// must sum to the exact total GPU duration, and no sample's value may
	// exceed its own execution's measured duration. That is what proves
	// nothing was scaled: sampling controls attribution only, never
	// duration - every execution is measured and joined regardless of
	// whether its launch was sampled.
	require.Len(t, samples, len(snap.Executions),
		"the stub emits no PC samples, so ProjectExecutions must emit exactly one sample per execution")
	var totalExecNs, totalSampleValue, attributedValue, unattributedValue uint64
	for i, view := range snap.Executions {
		require.Greater(t, view.Exec.EndNs, view.Exec.StartNs, "the stub's own execs must have a positive duration")
		dur := view.Exec.EndNs - view.Exec.StartNs
		totalExecNs += dur

		val := samples[i].Value
		totalSampleValue += val
		assert.LessOrEqualf(t, val, dur, "sample %d's value exceeds its own execution's measured duration - GPU time was scaled", i)

		var attributed bool
		for _, fr := range samples[i].Stack {
			if fr.Name == gpu.FrameLaunch {
				attributed = true
				break
			}
		}
		if attributed {
			attributedValue += val
		} else {
			unattributedValue += val
		}
	}
	assert.Equal(t, totalExecNs, totalSampleValue,
		"the projected samples must sum to exactly the measured GPU total, never a scaled estimate")
	assert.Equal(t, totalExecNs, attributedValue+unattributedValue,
		"the attributed and unattributed populations must sum to the exact total GPU duration")
	assert.Positive(t, attributedValue, "the stack-attributed population must be non-empty at sample_period=8")
	assert.Positive(t, unattributedValue, "the unattributed population must be non-empty at sample_period=8")
}

// privateStubCopy copies the built stub into this test's own temp directory
// and returns the copy's path. The copy is what the test attaches to and what
// it runs, so the uprobe can only ever match this run's executions.
//
// t.TempDir() is removed when the test ends, taking the copy with it. It lives
// under TMPDIR (/tmp here, mounted rw,nosuid,nodev — not noexec, so the copy
// is runnable); nosuid is irrelevant because the stub is an ordinary
// unprivileged producer and carries no file capabilities. If TMPDIR were ever
// noexec the exec below fails loudly with ENOEXEC/EACCES rather than
// silently degrading.
func privateStubCopy(t *testing.T, src string) string {
	t.Helper()
	info, err := os.Stat(src)
	require.NoError(t, err)
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	dst := filepath.Join(t.TempDir(), filepath.Base(src))
	// Preserve the executable bit from the source rather than assuming 0755.
	require.NoError(t, os.WriteFile(dst, data, info.Mode().Perm()))
	require.NotZero(t, info.Mode().Perm()&0o100, "built stub is not executable")

	// A distinct inode is the entire point; assert it rather than trusting the
	// copy. Two paths sharing an inode (a hard link, or a copy that silently
	// became a link) would put the shared image back under the uprobe.
	srcSys, dstInfo := info.Sys(), mustStat(t, dst)
	require.NotEqual(t, inodeOf(t, srcSys), inodeOf(t, dstInfo.Sys()),
		"the copy must have its own inode, or the attach is not private to this run")
	return dst
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

func inodeOf(t *testing.T, sys any) uint64 {
	t.Helper()
	st, ok := sys.(*syscall.Stat_t)
	require.True(t, ok, "stat did not yield a *syscall.Stat_t")
	return st.Ino
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
