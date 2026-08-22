package gpu

import (
	"strconv"
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
