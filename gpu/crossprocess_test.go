package gpu

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// The probes are attached against the shim *file*, so every process that maps
// it feeds the same Timeline, and system-wide (Config.PID == 0) is the
// documented default. Vendor correlation counters restart from a low value in
// every process, so two profiled processes collide on correlation within the
// first handful of launches. These tests pin the consequence: an execution
// must join the launch its OWN process produced, never a namesake from
// another one.
//
// The stakes are the launch's symbolized CPU stack (Phase 4a), resolved
// against /proc/<pid>/maps of the process that produced it. A mis-join does
// not merely swap metadata; it attributes one process's measured GPU time to
// a call path in a different process's address space - a flame graph that is
// confidently, specifically wrong. Spec §4 exists to forbid exactly that.
//
// Every assertion below is stated in terms the OLD, pid-blind types could
// express too - a launch's LaunchContext.PID and an execution's own StartNs -
// so the tests compile unchanged against the pre-fix keying and fail on the
// join, not on a missing field.

// corrFor builds the correlation that a process's launch and its later
// execution both carry. The pid is part of the identity, not context the
// caller may leave off: it is what makes "correlation 7" in pid 4242 a
// different key from "correlation 7" in pid 5353.
func corrFor(pid uint32, value string) CorrelationID {
	return CorrelationID{Backend: BackendCUPTI, PID: pid, Value: value}
}

// launchIn is deliberately identical across processes except for pid and the
// captured stack: same kernel name, same queue, adjacent timestamps. Only the
// process identity can tell the two joins apart.
func launchIn(pid uint32, value string, timeNs uint64, fn string) GPUKernelLaunch {
	return GPUKernelLaunch{
		Correlation: corrFor(pid, value),
		KernelName:  "hot_kernel",
		TimeNs:      timeNs,
		Launch: LaunchContext{
			PID:      pid,
			TID:      pid,
			TimeNs:   timeNs,
			CPUStack: pp.FramesFromNames([]string{fn}),
		},
	}
}

func execIn(pid uint32, value string, startNs, endNs uint64) GPUKernelExec {
	return GPUKernelExec{
		Correlation: corrFor(pid, value),
		KernelName:  "hot_kernel",
		StartNs:     startNs,
		EndNs:       endNs,
	}
}

func stackNames(l *GPUKernelLaunch) []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.Launch.CPUStack))
	for _, f := range l.Launch.CPUStack {
		out = append(out, f.Name)
	}
	return out
}

// TestLaunchCacheKeepsTwoProcessesSameCorrelationApart is the storage half:
// the cache must hold both launches, not treat the second as a replacement
// of the first.
func TestLaunchCacheKeepsTwoProcessesSameCorrelationApart(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 8})
	c.Put(launchIn(4242, "7", 10, "a_work"))
	c.Put(launchIn(5353, "7", 11, "b_work"))

	require.Equal(t, 2, c.Len(), "two processes, two launches: one must not overwrite the other")
	assert.Zero(t, c.Stats().Replaced, "neither launch is a replacement of the other")

	got, ok := c.Get(corrFor(4242, "7"))
	require.True(t, ok, "pid 4242's launch must still be retrievable")
	assert.Equal(t, uint32(4242), got.Launch.PID)
	assert.Equal(t, []string{"a_work"}, stackNames(&got))

	got, ok = c.Get(corrFor(5353, "7"))
	require.True(t, ok, "pid 5353's launch must still be retrievable")
	assert.Equal(t, uint32(5353), got.Launch.PID)
	assert.Equal(t, []string{"b_work"}, stackNames(&got))
}

// TestTimelineJoinsExecutionToItsOwnProcessLaunch is the join half, and the
// one that matters: both executions must take the exact path, and each must
// carry the stack its own process captured.
//
// Executions are told apart by StartNs rather than by anything on the
// correlation, so the assertion is about the join and nothing else.
func TestTimelineJoinsExecutionToItsOwnProcessLaunch(t *testing.T) {
	wantPID := map[uint64]uint32{20: 4242, 21: 5353}
	wantStack := map[uint64]string{20: "a_work", 21: "b_work"}

	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launchIn(4242, "7", 10, "a_work")))
	require.NoError(t, tl.EmitLaunch(launchIn(5353, "7", 11, "b_work")))
	require.NoError(t, tl.EmitExec(execIn(4242, "7", 20, 30)))
	require.NoError(t, tl.EmitExec(execIn(5353, "7", 21, 31)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 2)
	for _, view := range snap.Executions {
		pid := wantPID[view.Exec.StartNs]
		require.NotNilf(t, view.Launch, "pid %d's execution lost its launch entirely", pid)
		assert.Equalf(t, JoinExact, view.Join,
			"pid %d supplied a correlation that exists in its own process; the join must be exact", pid)
		assert.Equalf(t, pid, view.Launch.Launch.PID,
			"pid %d's execution joined a launch from pid %d", pid, view.Launch.Launch.PID)
		assert.Equalf(t, []string{wantStack[view.Exec.StartNs]}, stackNames(view.Launch),
			"pid %d's GPU time must be attributed to the call path pid %d ran", pid, pid)
	}

	assert.Equal(t, uint64(2), snap.JoinStats.ExactExecutionJoinCount)
	assert.Zero(t, snap.JoinStats.UnmatchedExecutionCount)
	assert.Zero(t, snap.JoinStats.HeuristicExecutionJoinCount)
	assert.Equal(t, uint64(2), snap.JoinStats.MatchedLaunchCount,
		"two distinct launches were matched, not one launch matched twice")
	assert.Zero(t, snap.JoinStats.UnmatchedLaunchCount)
}

// TestTimelinePendingPCSamplesStayWithTheirProcess covers Timeline.pending,
// the second table keyed on correlation alone. A PC sample carrying pid
// 5353's correlation must never be handed to pid 4242's execution.
func TestTimelinePendingPCSamplesStayWithTheirProcess(t *testing.T) {
	wantPC := map[uint64]uint64{20: 0xAAA, 21: 0xBBB}

	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launchIn(4242, "7", 10, "a_work")))
	require.NoError(t, tl.EmitLaunch(launchIn(5353, "7", 11, "b_work")))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: corrFor(4242, "7"), PCOffset: 0xAAA, Count: 1, TimeNs: 25,
	}))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: corrFor(5353, "7"), PCOffset: 0xBBB, Count: 1, TimeNs: 26,
	}))
	require.NoError(t, tl.EmitExec(execIn(4242, "7", 20, 30)))
	require.NoError(t, tl.EmitExec(execIn(5353, "7", 21, 31)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 2)
	for _, view := range snap.Executions {
		require.Lenf(t, view.PCSamples, 1,
			"the execution starting at %d must get exactly its own process's sample", view.Exec.StartNs)
		assert.Equalf(t, wantPC[view.Exec.StartNs], view.PCSamples[0].PCOffset,
			"the execution starting at %d was handed another process's PC sample", view.Exec.StartNs)
	}
	assert.Equal(t, uint64(2), snap.AttributedPCSamples)
	assert.Zero(t, snap.PendingSamples, "both samples were claimed by their own execution")
}

// TestSingleProcessJoinsAreUnaffected is the non-regression half. With
// Config.PID != 0 only one process ever reaches the timeline, so
// process-qualifying the key must be a no-op there: every correlation still
// joins exactly, nothing is unmatched, nothing degrades to the heuristic, and
// no launch is treated as a replacement of another.
func TestSingleProcessJoinsAreUnaffected(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 512}})
	for i := range 200 {
		v := strconv.Itoa(i)
		require.NoError(t, tl.EmitLaunch(launchIn(pid, v, uint64(i*10)+1, "work_"+v)))
	}
	for i := range 200 {
		v := strconv.Itoa(i)
		require.NoError(t, tl.EmitExec(execIn(pid, v, uint64(i*10)+2, uint64(i*10)+3)))
	}

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 200)
	for _, view := range snap.Executions {
		require.NotNil(t, view.Launch)
		assert.Equal(t, JoinExact, view.Join)
		assert.Equal(t, view.Exec.Correlation.Value, view.Launch.Correlation.Value,
			"a single-process join must still pair an execution with its own correlation")
		assert.Equal(t, []string{"work_" + view.Exec.Correlation.Value}, stackNames(view.Launch))
	}
	assert.Equal(t, uint64(200), snap.JoinStats.ExactExecutionJoinCount)
	assert.Zero(t, snap.JoinStats.UnmatchedExecutionCount)
	assert.Zero(t, snap.JoinStats.HeuristicExecutionJoinCount)
	assert.Zero(t, snap.JoinStats.UnmatchedLaunchCount)
	assert.Zero(t, snap.LaunchCache.Replaced, "no correlation repeats within one process here")
}

// TestExecutionWithNoLaunchInItsOwnProcessIsUnattributed is the honest-miss
// half. Once the key names the process, an execution whose correlation value
// exists only in ANOTHER process legitimately misses the cache - and the miss
// must degrade to unattributed and be counted, never be repaired by the
// heuristic into the other process's launch (spec §13, review Critical 2).
//
// The two processes share a kernel name and queue and the surviving launch
// precedes the execution, so the heuristic would find it a "plausible"
// candidate if it were ever allowed to run here.
func TestExecutionWithNoLaunchInItsOwnProcessIsUnattributed(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launchIn(4242, "7", 10, "a_work")))
	require.NoError(t, tl.EmitExec(execIn(5353, "7", 20, 30)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	assert.Nil(t, view.Launch,
		"pid 5353's execution must not borrow pid 4242's launch, or its call stack")
	assert.False(t, view.Heuristic,
		"a correlation was supplied and missed; guessing is not the remedy")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount,
		"the miss must be counted, not absorbed silently")
	assert.Zero(t, snap.JoinStats.ExactExecutionJoinCount,
		"a cross-process match must never be reported as vendor-provided truth")
	assert.Zero(t, snap.JoinStats.HeuristicExecutionJoinCount)
}

// A record that carried no vendor correlation is still tagged with the
// process that produced it, so "did this record carry a correlation" cannot
// be asked by comparing against the zero CorrelationID. Present is the test,
// and Timeline must use it: an exec with a pid and no value has to reach the
// heuristic path, not be looked up as if "" were a real id.
func TestCorrelationCarryingOnlyAProcessIsNotPresent(t *testing.T) {
	assert.False(t, CorrelationID{}.Present())
	assert.False(t, CorrelationID{Backend: BackendLinuxDRM, PID: 4242}.Present(),
		"a pid is not a correlation")
	assert.True(t, corrFor(4242, "7").Present())

	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(GPUKernelLaunch{
		KernelName: "hot_kernel",
		TimeNs:     10,
		Launch:     LaunchContext{PID: 4242, TimeNs: 10},
	}))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendLinuxDRM, PID: 4242},
		KernelName:  "hot_kernel",
		StartNs:     20,
		EndNs:       30,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Equal(t, JoinHeuristic, snap.Executions[0].Join,
		"an exec with a pid but no correlation value supplied no correlation at all")
	assert.True(t, snap.Executions[0].Heuristic, "a guess must always be marked as one")
}

// TestProjectionLabelsNameTheProducingProcess is issue #53's projection half.
// The join (above) already keeps the two processes apart internally; this
// pins that the separation is VISIBLE in the profile. Both executions carry
// the identical gpu_correlation string - that is the ambiguity #53 names, and
// it is deliberately left in place - so gpu_pid must be the thing that tells
// them apart.
func TestProjectionLabelsNameTheProducingProcess(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launchIn(4242, "7", 10, "a_work")))
	require.NoError(t, tl.EmitLaunch(launchIn(5353, "7", 11, "b_work")))
	require.NoError(t, tl.EmitExec(execIn(4242, "7", 20, 30)))
	require.NoError(t, tl.EmitExec(execIn(5353, "7", 21, 31)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 2)

	byPID := map[string]pp.ProfileSample{}
	for _, s := range samples {
		byPID[s.Labels["gpu_pid"]] = s
	}
	require.Contains(t, byPID, "4242", "no sample named pid 4242 as its producer")
	require.Contains(t, byPID, "5353", "no sample named pid 5353 as its producer")

	assert.Equal(t, "cupti:7", byPID["4242"].Labels["gpu_correlation"])
	assert.Equal(t, "cupti:7", byPID["5353"].Labels["gpu_correlation"],
		"gpu_correlation deliberately keeps its backend:value format, so the two "+
			"processes still share one string - gpu_pid is what disambiguates them")

	// gpu_pid must agree with the pid the frames were symbolized against;
	// a label naming one process over another's call path is worse than none.
	assert.Equal(t, uint32(4242), byPID["4242"].Pid)
	assert.Equal(t, uint32(5353), byPID["5353"].Pid)
	assert.Contains(t, frameNames(byPID["4242"].Stack), "a_work")
	assert.Contains(t, frameNames(byPID["5353"].Stack), "b_work")
}

// TestProjectionNamesTheProcessOfAnUnmatchedExecution covers the population
// that keeps gpu_correlation without a launch: the correlation was supplied
// but missed the cache (its launch aged out), so there is no stack and
// ProfileSample.Pid is honestly 0 - no address space to symbolize against -
// yet the execution's own correlation still names the process that produced
// it. Without this, the exact samples that keep the ambiguous label would
// keep the ambiguity too.
func TestProjectionNamesTheProcessOfAnUnmatchedExecution(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(execIn(4242, "7", 20, 30)))

	snap := tl.Snapshot()
	require.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 1)
	assert.Equal(t, "unmatched", samples[0].Labels["gpu_join"])
	assert.Equal(t, "cupti:7", samples[0].Labels["gpu_correlation"])
	assert.Equal(t, "4242", samples[0].Labels["gpu_pid"],
		"an unmatched execution still knows which process produced it")
	assert.Equal(t, uint32(0), samples[0].Pid,
		"with no launch there is no address space, so the sample's pid stays 0")
}

// TestProjectionOmitsPidWhenNoProcessIsKnown is the honesty half: an
// execution that carried no correlation at all and matched no launch has no
// process to name. Emitting gpu_pid="0" would name pid 0 - the kernel - as
// the producer; the label is omitted instead, and gpu_join says why.
func TestProjectionOmitsPidWhenNoProcessIsKnown(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(GPUKernelExec{KernelName: "orphan", StartNs: 20, EndNs: 30}))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, "unmatched", samples[0].Labels["gpu_join"])
	assert.NotContains(t, samples[0].Labels, "gpu_pid",
		"no process is known, so no process may be named")
}

// TestProjectionEmitsPidInSingleProcessMode pins the deliberate choice to
// emit gpu_pid even when every sample carries the same value (Config.PID !=
// 0). A consumer cannot distinguish "skipped because single-process" from
// "GPU labels absent entirely", so the label is always present; its cost is
// one string-table value for the whole profile.
func TestProjectionEmitsPidInSingleProcessMode(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	for i := range 3 {
		v := strconv.Itoa(i)
		require.NoError(t, tl.EmitLaunch(launchIn(4242, v, uint64(i*10)+1, "work")))
		require.NoError(t, tl.EmitExec(execIn(4242, v, uint64(i*10)+2, uint64(i*10)+3)))
	}

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 3)
	for _, s := range samples {
		assert.Equal(t, "4242", s.Labels["gpu_pid"])
	}
}

// ---------------------------------------------------------------------------
// Issue #52: the heuristic join path.
//
// #36 process-qualified the EXACT join, and the tests above pin it. The
// heuristic path was left process-blind: it runs only for an execution that
// supplied no correlation, and the reasoning at the time was that such an
// execution has no process to group by. That reasoning was wrong.
// CorrelationID.Present() tests Value alone, so PID and Value are independent
// and a record can name its process while carrying no correlation - which is
// what TestCorrelationCarryingOnlyAProcessIsNotPresent above already
// constructs. The heuristic therefore groups by process too, and refuses to
// join an execution that names none.
//
// What was at stake is one step worse than #36's: a heuristic match takes the
// chosen launch's CPUStack AND its Launch.Tags, which carry pod_uid and
// container_id. A cross-process guess is a cross-CONTAINER billing error.
// ---------------------------------------------------------------------------

// heurExecIn is execIn for the heuristic path: it names the producing process
// but carries no vendor correlation value, so Present() is false and the
// execution takes the heuristic join. Everything else - queue, kernel name,
// timing - is identical to launchIn's, so only the process can separate them.
func heurExecIn(pid uint32, startNs, endNs uint64) GPUKernelExec {
	return GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, PID: pid},
		KernelName:  "hot_kernel",
		StartNs:     startNs,
		EndNs:       endNs,
	}
}

// taggedLaunchIn is launchIn plus the container attribution a real launch
// carries. These tags are the actual payload of the defect: a mis-joined
// execution inherits them and bills its GPU time to a container that never
// ran it.
func taggedLaunchIn(pid uint32, value string, timeNs uint64, fn, pod string) GPUKernelLaunch {
	l := launchIn(pid, value, timeNs, fn)
	l.Launch.Tags = map[string]string{"pod_uid": pod, "container_id": "c-" + pod}
	return l
}

// TestHeuristicJoinRefusesAnotherProcessLaunch is issue #52's motivating case.
// Only pid 4242 ever launched anything; pid 5353's correlation-less execution
// matches that launch on queue, kernel name and causal timing, which is
// exactly the "plausible candidate" the old, process-blind grouping would
// have handed over - along with a_work's call stack and pod-a's pod_uid.
//
// Mutation this catches: candidateGroupKey losing its pid field, or Snapshot
// looking up the process-blind index for a join rather than only for the
// blocked counter.
func TestHeuristicJoinRefusesAnotherProcessLaunch(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(taggedLaunchIn(4242, "7", 10, "a_work", "pod-a")))
	require.NoError(t, tl.EmitExec(heurExecIn(5353, 20, 30)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	assert.Nil(t, view.Launch,
		"pid 5353 has no launch of its own; pid 4242's must not stand in for it")
	assert.False(t, view.Heuristic, "a refused join is not a heuristic join")
	assert.NotEqual(t, JoinHeuristic, view.Join)

	js := snap.JoinStats
	assert.Equal(t, uint64(1), js.CorrelationlessExecutionCount,
		"entering the heuristic path at all must be counted, not just succeeding at it")
	assert.Equal(t, uint64(1), js.CrossProcessHeuristicBlockedCount,
		"a candidate did qualify on queue, kernel name and timing - the refusal is the process guard's, and must be visible")
	assert.Equal(t, uint64(1), js.UnmatchedExecutionCount,
		"the execution degrades to unattributed; it is not dropped")
	assert.Zero(t, js.HeuristicExecutionJoinCount)
	assert.Zero(t, js.ExactExecutionJoinCount)

	// The GPU time survives, and carries none of pid 4242's container.
	samples := ProjectExecutions(snap)
	require.Len(t, samples, 1)
	assert.Equal(t, "unmatched", samples[0].Labels["gpu_join"])
	assert.NotContains(t, samples[0].Labels, "pod_uid",
		"a refused join must not inherit the other container's attribution")
	assert.NotContains(t, samples[0].Labels, "container_id")
	assert.Equal(t, "5353", samples[0].Labels["gpu_pid"],
		"the execution still knows which process produced it")
	assert.NotContains(t, frameNames(samples[0].Stack), "a_work",
		"a refused join must not inherit the other process's call path either")
}

// TestHeuristicJoinStaysWithinItsOwnProcess is the other half: with both
// processes launching, each correlation-less execution must find its OWN
// process's launch. Refusing everything would be a safe but useless guard;
// this pins that the heuristic still works, process-scoped.
//
// The two launches are identical but for pid, stack and tags, and the
// timestamps are interleaved (10 and 11, execs at 20 and 21) so that the
// "most recent preceding launch" rule alone would pick pid 5353's for both.
func TestHeuristicJoinStaysWithinItsOwnProcess(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(taggedLaunchIn(4242, "7", 10, "a_work", "pod-a")))
	require.NoError(t, tl.EmitLaunch(taggedLaunchIn(5353, "7", 11, "b_work", "pod-b")))
	require.NoError(t, tl.EmitExec(heurExecIn(4242, 20, 30)))
	require.NoError(t, tl.EmitExec(heurExecIn(5353, 21, 31)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 2)

	wantStack := map[uint32]string{4242: "a_work", 5353: "b_work"}
	wantPod := map[uint32]string{4242: "pod-a", 5353: "pod-b"}
	for _, view := range snap.Executions {
		pid := view.Exec.Correlation.PID
		require.NotNilf(t, view.Launch, "pid %d's own launch was available and must have been used", pid)
		assert.Equalf(t, JoinHeuristic, view.Join, "pid %d: a guess must still be labelled a guess", pid)
		assert.Truef(t, view.Heuristic, "pid %d", pid)
		assert.Falsef(t, view.Ambiguous, "pid %d: one candidate per process, so nothing is ambiguous", pid)
		assert.Equalf(t, pid, view.Launch.Launch.PID, "pid %d joined pid %d's launch", pid, view.Launch.Launch.PID)
		assert.Equalf(t, []string{wantStack[pid]}, stackNames(view.Launch), "pid %d got the wrong call path", pid)
		assert.Equalf(t, wantPod[pid], view.Launch.Launch.Tags["pod_uid"], "pid %d got the wrong container", pid)
	}

	js := snap.JoinStats
	assert.Equal(t, uint64(2), js.HeuristicExecutionJoinCount)
	assert.Equal(t, uint64(2), js.CorrelationlessExecutionCount,
		"both executions entered the heuristic path, whether or not they succeeded")
	assert.Zero(t, js.CrossProcessHeuristicBlockedCount,
		"each process had its own candidate, so nothing was refused")
	assert.Zero(t, js.UnmatchedExecutionCount)

	// Every sample still says it is a guess. #52 must not be closed by
	// quietly promoting the survivors to something they are not.
	for _, s := range ProjectExecutions(snap) {
		assert.Equal(t, "heuristic", s.Labels["gpu_join"],
			"a process-scoped guess is still a guess")
	}
}

// TestHeuristicJoinRefusedWhenExecutionNamesNoProcess is the shape gpuprobe
// would actually produce if it ever violated spec §6: correlationOf(pid, 0)
// returns the whole zero CorrelationID, discarding the pid, so the execution
// names neither a correlation nor a process. There is nothing to group it by
// and nothing to check a candidate against, so it must be refused - and the
// refusal counted, because this is precisely the "reachable by accident" case
// the issue is about.
func TestHeuristicJoinRefusedWhenExecutionNamesNoProcess(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(taggedLaunchIn(4242, "7", 10, "a_work", "pod-a")))
	require.NoError(t, tl.EmitExec(GPUKernelExec{
		// Exactly gpuprobe's correlationOf(pid, 0) result: no value, no pid.
		KernelName: "hot_kernel",
		StartNs:    20, EndNs: 30,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch,
		"an execution that names no process cannot be shown to share one with any launch")

	js := snap.JoinStats
	assert.Equal(t, uint64(1), js.CorrelationlessExecutionCount)
	assert.Equal(t, uint64(1), js.CrossProcessHeuristicBlockedCount,
		"a candidate qualified on everything except a provable process, which is the whole point")
	assert.Equal(t, uint64(1), js.UnmatchedExecutionCount)
	assert.Zero(t, js.HeuristicExecutionJoinCount)

	samples := ProjectExecutions(snap)
	require.Len(t, samples, 1)
	assert.Equal(t, "unmatched", samples[0].Labels["gpu_join"])
	assert.NotContains(t, samples[0].Labels, "gpu_pid", "no process is known, so none may be named")
	assert.NotContains(t, samples[0].Labels, "pod_uid")
	assert.Equal(t, uint64(10), samples[0].Value,
		"the measured GPU time is kept: this is degrade-to-unattributed, not drop")
}

// TestCorrelationlessExecutionIsCountedWithNoCandidateToRefuse separates the
// two counters. Here the heuristic path is entered but no launch qualified at
// all, so nothing was refused on process grounds - the blocked counter must
// stay at zero while the path-entered counter still fires.
//
// This is the counter-honesty half of #52: if CrossProcessHeuristicBlockedCount
// counted every correlation-less miss, it would read high for reasons that
// have nothing to do with processes, and "N cross-container attributions
// prevented" would stop meaning that.
func TestCorrelationlessExecutionIsCountedWithNoCandidateToRefuse(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(heurExecIn(5353, 20, 30)))

	snap := tl.Snapshot()
	js := snap.JoinStats
	assert.Equal(t, uint64(1), js.CorrelationlessExecutionCount,
		"the heuristic path was entered, and entering it is what #52 wants visible")
	assert.Zero(t, js.CrossProcessHeuristicBlockedCount,
		"no launch existed at all, so no cross-process join was prevented")
	assert.Equal(t, uint64(1), js.UnmatchedExecutionCount)
}

// TestExactJoinsNeverEnterTheHeuristicCounters is the non-regression guard for
// #36's path, which this change must not touch: an execution that supplies a
// correlation takes the exact join (or an honest miss) and must not appear in
// either of #52's counters, whatever the process layout.
func TestExactJoinsNeverEnterTheHeuristicCounters(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launchIn(4242, "7", 10, "a_work")))
	require.NoError(t, tl.EmitLaunch(launchIn(5353, "7", 11, "b_work")))
	require.NoError(t, tl.EmitExec(execIn(4242, "7", 20, 30)))
	require.NoError(t, tl.EmitExec(execIn(5353, "7", 21, 31)))
	// A correlation that exists only in another process: an honest miss.
	require.NoError(t, tl.EmitExec(execIn(6464, "7", 22, 32)))

	js := tl.Snapshot().JoinStats
	assert.Equal(t, uint64(2), js.ExactExecutionJoinCount)
	assert.Equal(t, uint64(1), js.UnmatchedExecutionCount)
	assert.Zero(t, js.CorrelationlessExecutionCount,
		"every execution here supplied a correlation, so none entered the heuristic path")
	assert.Zero(t, js.CrossProcessHeuristicBlockedCount,
		"the exact path's cross-process miss is not the heuristic guard's doing, and must not be credited to it")
}

// TestJoinHealthSurfacesTheHeuristicProcessGuard closes the loop #51 opens:
// a counter nobody prints is a counter nobody reads. Both of #52's counters
// must reach the operator-facing lines, and the healthy case must stay silent
// about them.
func TestJoinHealthSurfacesTheHeuristicProcessGuard(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(taggedLaunchIn(4242, "7", 10, "a_work", "pod-a")))
	require.NoError(t, tl.EmitExec(heurExecIn(5353, 20, 30)))

	lines := JoinHealth(tl.Snapshot())
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "arrived with no vendor correlation",
		"entering the heuristic path is a producer contract violation and must be said out loud")
	assert.Contains(t, joined, "could not be shown to come from the same process",
		"the prevented cross-container attribution must be reported, not merely counted")
	assert.Contains(t, lines[0], "anomalies", "the summary must agree that something is wrong")

	// Healthy run: exact joins only, and neither line appears.
	clean := NewTimeline(TimelineConfig{})
	require.NoError(t, clean.EmitLaunch(launchIn(4242, "7", 10, "a_work")))
	require.NoError(t, clean.EmitExec(execIn(4242, "7", 20, 30)))
	cleanLines := JoinHealth(clean.Snapshot())
	require.Len(t, cleanLines, 1, "a healthy snapshot must stay one line: %v", cleanLines)
	assert.Contains(t, cleanLines[0], "no anomalies")
}
