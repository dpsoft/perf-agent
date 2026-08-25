package gpu

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixtures for the module join
// ---------------------------------------------------------------------------

// pcJoinFixture is a Timeline wired to a module store holding one real cubin,
// plus the symbol index that cubin's kernel actually occupies. The index is
// read out of the fixture rather than hard-coded: whether CUPTI's
// functionIndex IS the .symtab index is measured on hardware, and these tests
// assert only that the join uses whatever the store uses, consistently.
type pcJoinFixture struct {
	tl      *Timeline
	store   *ModuleStore
	crc     uint64
	fnIndex uint32
	kernel  string
}

const pcJoinCRC = 0xC0FFEE

func newPCJoinFixture(t *testing.T, cfg TimelineConfig) *pcJoinFixture {
	t.Helper()
	return newPCJoinFixtureFrom(t, cfg, "single_lineinfo.cubin", "addOne")
}

func newPCJoinFixtureFrom(t *testing.T, cfg TimelineConfig, cubinName, kernel string) *pcJoinFixture {
	t.Helper()
	b := fixture(t, cubinName)
	store := NewModuleStore(ModuleStoreConfig{})
	require.NoError(t, store.Put(pcJoinCRC, b))

	cfg.Modules = store
	return &pcJoinFixture{
		tl:      NewTimeline(cfg),
		store:   store,
		crc:     pcJoinCRC,
		fnIndex: symIndexOf(t, b, kernel),
		kernel:  kernel,
	}
}

// sample emits one Tier B PC sample for the fixture's kernel.
func (f *pcJoinFixture) sample(t *testing.T, pid uint32, timeNs uint64) {
	t.Helper()
	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, f.crc, f.fnIndex, timeNs)))
}

// pcExec is an execution of a named kernel on a named device, in a process,
// with no vendor correlation value - the Tier B shape, where the exec's
// correlation is not what the PC samples join on.
func pcExec(pid uint32, kernel, device string, startNs, endNs uint64) GPUKernelExec {
	return GPUKernelExec{
		Execution:   GPUExecutionRef{Backend: BackendCUPTI, DeviceID: device},
		Correlation: CorrelationID{Backend: BackendCUPTI, PID: pid},
		Queue:       GPUQueueRef{Backend: BackendCUPTI, QueueID: "s0"},
		KernelName:  kernel,
		StartNs:     startNs,
		EndNs:       endNs,
	}
}

// ---------------------------------------------------------------------------
// The join works
// ---------------------------------------------------------------------------

// TestTierBSampleReachesItsExecution is the deliverable: a continuous-mode PC
// sample, which carries no correlation at all, arrives at the execution of the
// kernel it was sampled in - through the module and nothing else.
//
// Nothing in the inputs connects the sample to the execution directly. The
// sample knows a cubin CRC and a function index; the execution knows a kernel
// name. The module is the only thing that turns the first into the second.
func TestTierBSampleReachesItsExecution(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	f.sample(t, pid, 11)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]

	require.Len(t, view.PCSamples, 2, "both samples must reach the execution of their kernel")
	assert.Equal(t, PCAttribKernel, view.PCAttrib,
		"one execution of this kernel was in the horizon, so the attribution is not an inference about which invocation")
	assert.Equal(t, uint64(2), snap.AttributedPCSamples)
	assert.Equal(t, uint64(2), snap.PCJoin.AttributedKernel)
	assert.Zero(t, snap.PCJoin.AttributedExact, "no sample here carried a correlation")
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsJoined)
	assert.Zero(t, snap.PendingModuleSamples, "a joined group is consumed, not left behind")
	assert.Zero(t, snap.PendingModuleGroups)
	assert.Zero(t, snap.Dropped.EvictedPendingModuleSamples)

	assertPCJoinIdentities(t, snap)
}

// TestTierBJoinIsConsumingLikeTheExactPath pins the lifecycle: a group handed
// to an execution is gone, so a second Snapshot cannot hand the same samples
// to a second execution of the same kernel. Re-reporting them would
// double-count stall time across snapshots exactly as the pre-fix exec bug
// double-counted GPU time.
func TestTierBJoinIsConsumingLikeTheExactPath(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))
	first := f.tl.Snapshot()
	require.Len(t, first.Executions[0].PCSamples, 1)

	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 40, 50)))
	second := f.tl.Snapshot()
	require.Len(t, second.Executions, 1)
	assert.Empty(t, second.Executions[0].PCSamples,
		"the group was consumed by the first snapshot; the second must not receive it again")
	assert.Empty(t, string(second.Executions[0].PCAttrib),
		"an execution with no PC samples has nothing for gpu_pc_attrib to describe")
	assert.Zero(t, second.AttributedPCSamples)
}

// ---------------------------------------------------------------------------
// kernel-ambiguous, and the de-overloading it exists for
// ---------------------------------------------------------------------------

// TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous is the assertion that
// pins the whole reason gpu_pc_attrib is a separate label.
//
// Two executions of one kernel are in the horizon, so which invocation the
// samples came from is an inference. That inference is marked - and
// ExecutionView.Ambiguous, which means "the heuristic LAUNCH join picked one
// of several candidate launches" and feeds AmbiguousHeuristicMatchCount, is
// left alone. Reusing it would emit gpu_ambiguous="true" on this sample
// alongside whatever gpu_join says, putting two unrelated facts on one boolean
// and making AmbiguousHeuristicMatchCount stop counting heuristic launch
// joins.
//
// The projection is asserted directly rather than only the struct field,
// because the struct field is not what a consumer reads.
func TestTierBAmbiguityIsMarkedWithoutTouchingAmbiguous(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	f.sample(t, pid, 11)
	f.sample(t, pid, 12)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 40, 50)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 2)

	var carrying int
	for _, view := range snap.Executions {
		labels := projectionLabels(view)
		_, hasAmbiguous := labels["gpu_ambiguous"]
		assert.False(t, hasAmbiguous,
			"gpu_ambiguous means a heuristic LAUNCH join chose between candidate launches; "+
				"PC ambiguity must never set it, or one boolean carries two unrelated facts")
		assert.False(t, view.Ambiguous, "and the field it comes from must be untouched too")

		if len(view.PCSamples) == 0 {
			continue
		}
		carrying++
		assert.Equal(t, PCAttribKernelAmbiguous, view.PCAttrib,
			"two executions of this kernel were in the horizon, so which invocation is an inference and must say so")
	}
	assert.Equal(t, 1, carrying,
		"the group goes to one execution whole; splitting it would manufacture a distribution the data does not contain")

	assert.Equal(t, uint64(1), snap.PCJoin.AmbiguousAttributions)
	assert.Zero(t, snap.JoinStats.AmbiguousHeuristicMatchCount,
		"AmbiguousHeuristicMatchCount counts heuristic launch joins and there were none; "+
			"a PC-ambiguity increment here would corrupt what that counter's name promises")
	assert.Equal(t, uint64(3), snap.PCJoin.AttributedKernel)
	assertPCJoinIdentities(t, snap)
}

// TestTierBAmbiguousGoesToTheEarliestExecution pins which invocation receives
// the samples, so the choice is a stated rule rather than whatever order the
// producer's drain happened to deliver executions in.
func TestTierBAmbiguousGoesToTheEarliestExecution(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	// Emitted late-first: ring order and StartNs order disagree deliberately.
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 900, 950)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 100, 150)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 2)
	require.Equal(t, uint64(900), snap.Executions[0].Exec.StartNs, "ring order is emission order")

	assert.Empty(t, snap.Executions[0].PCSamples)
	require.Len(t, snap.Executions[1].PCSamples, 1,
		"the earliest execution by StartNs takes the group, whatever order the producer drained them in")
	assert.Equal(t, PCAttribKernelAmbiguous, snap.Executions[1].PCAttrib)
}

// ---------------------------------------------------------------------------
// kernel-multidevice
// ---------------------------------------------------------------------------

// TestTierBMultiDeviceProcessIsMarked drives the out-of-scope condition the
// design names: gpu_pc_sample_batch_v1 carries no device id, and one binary on
// two devices has one cubin CRC, so their samples are indistinguishable on the
// wire. There is no way to make this join right; there is only a way to stop
// it looking right.
func TestTierBMultiDeviceProcessIsMarked(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, "other_kernel", "1", 25, 35)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 2)
	require.Len(t, snap.Executions[0].PCSamples, 1)

	assert.Equal(t, PCAttribKernelMultiDevice, snap.Executions[0].PCAttrib,
		"only one execution of this kernel was in the horizon, but the process ran on two devices, "+
			"so the samples cannot be shown to have come from this device at all")
	assert.Equal(t, uint64(1), snap.PCJoin.MultiDeviceProcesses)
	assert.Equal(t, uint64(1), snap.PCJoin.MultiDeviceAttributions)
	assert.Zero(t, snap.PCJoin.AmbiguousAttributions,
		"multidevice is the reported caveat; it must not also be counted as ambiguity")
	assertPCJoinIdentities(t, snap)
}

// TestMultiDeviceOutranksAmbiguity pins the precedence: an execution whose
// attribution is doubtful on two axes reports the one that says the samples
// might be from another device entirely, which is the larger doubt.
func TestMultiDeviceOutranksAmbiguity(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "1", 40, 50)))

	snap := f.tl.Snapshot()
	var attribs []PCAttrib
	for _, view := range snap.Executions {
		if len(view.PCSamples) > 0 {
			attribs = append(attribs, view.PCAttrib)
		}
	}
	require.Len(t, attribs, 1)
	assert.Equal(t, PCAttribKernelMultiDevice, attribs[0])
}

// TestSingleDeviceProcessIsNotMarkedMultiDevice is the negative half: the
// guard must not fire on the ordinary case, or the mark it exists to raise
// means nothing.
func TestSingleDeviceProcessIsNotMarkedMultiDevice(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)
	for i := range 20 {
		require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", uint64(20+i), uint64(30+i))))
	}
	require.NoError(t, f.tl.EmitExec(pcExec(9999, "elsewhere", "1", 5, 6)))

	snap := f.tl.Snapshot()
	assert.Zero(t, snap.PCJoin.MultiDeviceProcesses,
		"two processes on one device each is not one process on two devices")
	assert.Zero(t, snap.PCJoin.MultiDeviceAttributions)
	assert.Zero(t, snap.PCJoin.DeviceTrackingCapped)
}

// TestDeviceTrackingCapIsCountedNotSilent drives the multi-device guard past
// its bound. Past the cap a new process is treated as single-device, which is
// the right guess and still a guess - so the refusal is counted and raised,
// rather than leaving "we found no multi-device process" and "we stopped
// looking" indistinguishable.
func TestDeviceTrackingCapIsCountedNotSilent(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	for i := range maxTrackedDeviceProcesses + 10 {
		//nolint:gosec // the loop bound is a small constant.
		require.NoError(t, tl.EmitExec(pcExec(uint32(i+1), "k", "0", 1, 2)))
	}
	snap := tl.Snapshot()
	assert.Equal(t, uint64(10), snap.PCJoin.DeviceTrackingCapped)
	assert.Contains(t, strings.Join(JoinHealth(snap), "\n"), "multi-device guard",
		"a guard that stopped looking must say so")
}

// ---------------------------------------------------------------------------
// Refusals: the four ways a group is left pending
// ---------------------------------------------------------------------------

// TestTierBUnknownModuleStaysPendingAndIsEvicted is the "when in doubt, leave
// it pending" case, end to end. The cubin for this CRC never reached the
// agent, so nothing can name the function; the samples are not attached to a
// nearby kernel, they wait, and when they age out the loss is counted where
// losses are counted.
func TestTierBUnknownModuleStaysPendingAndIsEvicted(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{PendingSampleHorizonNs: 1_000})

	// A CRC the store has never seen, against an execution whose kernel name
	// would have matched had the module been in hand.
	const unknownCRC = 0xDEADBEEF
	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, unknownCRC, f.fnIndex, 10)))
	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, unknownCRC, f.fnIndex, 11)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))

	pendingSnap := f.tl.Snapshot()
	require.Len(t, pendingSnap.Executions, 1)
	assert.Empty(t, pendingSnap.Executions[0].PCSamples,
		"an unnameable group must never be attached to an execution that merely looks plausible")
	assert.Empty(t, string(pendingSnap.Executions[0].PCAttrib))
	assert.Equal(t, uint64(1), pendingSnap.PCJoin.GroupsUnresolvedName)
	assert.Zero(t, pendingSnap.PCJoin.GroupsJoined)
	assert.Equal(t, 2, pendingSnap.PendingModuleSamples, "still pending, not lost")
	assert.Zero(t, pendingSnap.Dropped.EvictedPendingModuleSamples)
	assert.Contains(t, strings.Join(JoinHealth(pendingSnap), "\n"),
		"could not be resolved to a device function",
		"an operator must be told why these samples are not in the profile")
	assertPCJoinIdentities(t, pendingSnap)

	// Age it out. The horizon anchor advances on the next sample, which
	// belongs to a different group.
	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, f.crc, f.fnIndex, 500_000)))

	evictedSnap := f.tl.Snapshot()
	assert.Equal(t, uint64(2), evictedSnap.Dropped.EvictedPendingModuleSamples,
		"the loss is counted at eviction, which is where it actually happens")
	assert.Zero(t, evictedSnap.Dropped.EvictedPendingSamples,
		"and never on the correlation-keyed counter")
}

// TestTierBKernelNameMismatchStaysPending is the rule the design states in so
// many words: matching the module's function name against the execution's
// KernelName is what links a PC group to an execution, and where they do not
// match the samples stay pending rather than being attached to a plausible
// neighbour.
//
// The comparison is exact. This test uses a name that differs by one
// character, in the same process, on the same queue, at a time that would have
// qualified under any windowed heuristic - everything a "close enough" match
// would have taken.
func TestTierBKernelNameMismatchStaysPending(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})
	require.Equal(t, "addOne", f.kernel)

	f.sample(t, pid, 10)
	require.NoError(t, f.tl.EmitExec(pcExec(pid, "addOnes", "0", 20, 30)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Empty(t, snap.Executions[0].PCSamples,
		"a one-character difference is a different kernel; the module, not the string, decides identity")
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsNoExecution)
	assert.Equal(t, 1, snap.PendingModuleSamples)
	assertPCJoinIdentities(t, snap)
}

// TestTierBJoinStaysWithinTheProcess is issue #52's discipline at the join.
// Two processes running the identical binary produce the identical cubin CRC
// and the identical function index - only the pid separates them, and it is a
// field of both keys rather than a check beside them.
func TestTierBJoinStaysWithinTheProcess(t *testing.T) {
	const (
		pidA = 4242
		pidB = 5353
	)
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pidA, 10)
	f.sample(t, pidB, 11)
	// Only process A has an execution.
	require.NoError(t, f.tl.EmitExec(pcExec(pidA, f.kernel, "0", 20, 30)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.Len(t, snap.Executions[0].PCSamples, 1,
		"process A's execution takes process A's group and nothing else")
	assert.Equal(t, uint32(pidA), snap.Executions[0].PCSamples[0].Correlation.PID)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsJoined)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsNoExecution,
		"process B's identical group finds no execution of its own and waits")
	assert.Equal(t, 1, snap.PendingModuleSamples)
	assertPCJoinIdentities(t, snap)
}

// TestTierBGroupWithNoProcessIsRefused covers the producer that names no
// process at all. Every such group from every process shares one key, so
// attaching one to an execution would be attributing one process's GPU samples
// to another's call stack on nothing but a kernel name.
func TestTierBGroupWithNoProcessIsRefused(t *testing.T) {
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, 0, 10)
	require.NoError(t, f.tl.EmitExec(pcExec(0, f.kernel, "0", 20, 30)))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Empty(t, snap.Executions[0].PCSamples)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsNoProcess)
	assert.Zero(t, snap.PCJoin.GroupsJoined)
	assert.Equal(t, 1, snap.PendingModuleSamples)
	assert.Contains(t, strings.Join(JoinHealth(snap), "\n"), "naming no process")
	assertPCJoinIdentities(t, snap)
}

// TestTierBWithoutAModuleStoreLeavesEverythingPending pins that a nil store is
// accounted for rather than skipped. A Timeline with no module store cannot
// name any group, which is the same fact for the profile as a cubin that never
// arrived - and it must read that way in the counters, not as a healthy run
// that happened to join nothing.
func TestTierBWithoutAModuleStoreLeavesEverythingPending(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{})

	require.NoError(t, tl.EmitPCSample(tierBSample(pid, pcJoinCRC, 3, 10)))
	require.NoError(t, tl.EmitExec(pcExec(pid, "addOne", "0", 20, 30)))

	snap := tl.Snapshot()
	assert.Empty(t, snap.Executions[0].PCSamples)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsUnresolvedName,
		"no store is not 'nothing to do'; it is 'every group is unnameable'")
	assert.Equal(t, 1, snap.PendingModuleSamples)
	assertPCJoinIdentities(t, snap)
}

// TestTierBUnknownFunctionIndexStaysPending is the other half of unnameable: the
// module is in hand, and the index is not one of its functions. The store
// refuses to pick a neighbouring function and the join refuses to invent an
// execution for it.
func TestTierBUnknownFunctionIndexStaysPending(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, f.crc, f.fnIndex+9999, 10)))
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))

	snap := f.tl.Snapshot()
	assert.Empty(t, snap.Executions[0].PCSamples)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsUnresolvedName)
	assertPCJoinIdentities(t, snap)
}

// ---------------------------------------------------------------------------
// The exact path is unchanged
// ---------------------------------------------------------------------------

// TestExactCorrelationJoinIsUnchangedAndLabelledExact is the non-regression
// half. Tier A keeps taking the correlation index, first, and the samples it
// delivers are labelled exact - vendor truth, not an inference - while the
// module join is confined to what that pass left unserved.
func TestExactCorrelationJoinIsUnchangedAndLabelledExact(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})
	corr := CorrelationID{Backend: BackendCUPTI, PID: pid, Value: "77"}

	require.NoError(t, f.tl.EmitPCSample(GPUPCSample{
		Correlation:   corr,
		Module:        ModuleRef{Backend: BackendCUPTI, CRC: f.crc},
		FunctionIndex: f.fnIndex,
		TimeNs:        10,
		PCOffset:      0x10,
		Count:         1,
	}))
	exec := pcExec(pid, f.kernel, "0", 20, 30)
	exec.Correlation = corr
	require.NoError(t, f.tl.EmitExec(exec))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	require.Len(t, view.PCSamples, 1)
	assert.Equal(t, PCAttribExact, view.PCAttrib)
	assert.Equal(t, uint64(1), snap.PCJoin.AttributedExact)
	assert.Zero(t, snap.PCJoin.AttributedKernel)
	assert.Zero(t, snap.PCJoin.GroupsExamined(), "the module store was never consulted; nothing was pending in it")
	assertPCJoinIdentities(t, snap)
}

// TestExactPathKeepsFirstClaim pins the ordering the design requires. An
// execution served by the correlation index is not a candidate for the module
// join, so a correlation-less group for the same kernel cannot pile onto it -
// it waits for an execution of its own.
func TestExactPathKeepsFirstClaim(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})
	corr := CorrelationID{Backend: BackendCUPTI, PID: pid, Value: "77"}

	require.NoError(t, f.tl.EmitPCSample(GPUPCSample{
		Correlation: corr, Module: ModuleRef{Backend: BackendCUPTI, CRC: f.crc},
		FunctionIndex: f.fnIndex, TimeNs: 10, Count: 1,
	}))
	f.sample(t, pid, 11)

	exec := pcExec(pid, f.kernel, "0", 20, 30)
	exec.Correlation = corr
	require.NoError(t, f.tl.EmitExec(exec))

	snap := f.tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.Len(t, snap.Executions[0].PCSamples, 1, "only the exact sample lands here")
	assert.Equal(t, PCAttribExact, snap.Executions[0].PCAttrib)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsNoExecution,
		"the only execution of this kernel was already served exactly, so the module group waits")
	assert.Equal(t, 1, snap.PendingModuleSamples)
	assertPCJoinIdentities(t, snap)
}

// ---------------------------------------------------------------------------
// Accounting
// ---------------------------------------------------------------------------

// TestTierBJoinAccountsForEveryGroup forces all four group outcomes in one
// run. A refusal path that returned without incrementing a counter - the
// easiest mistake here and an invisible one - breaks the partition.
func TestTierBJoinAccountsForEveryGroup(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{})

	f.sample(t, pid, 10)                                                          // joined
	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, 0xBAD, f.fnIndex, 11))) // unnameable
	f.sample(t, 5353, 12)                                                         // named, no execution (other process)
	f.sample(t, 0, 13)                                                            // no process
	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 20, 30)))

	snap := f.tl.Snapshot()
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsJoined)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsUnresolvedName)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsNoExecution)
	assert.Equal(t, uint64(1), snap.PCJoin.GroupsNoProcess)
	assert.Equal(t, uint64(4), snap.PCJoin.GroupsExamined())
	assert.Equal(t, 3, snap.PendingModuleGroups)
	assertPCJoinIdentities(t, snap)
}

// TestTierBReconciliationCoversAttributedAndPending is the reconciliation
// identity with its new bucket forced non-zero: some samples attributed
// through the module, some still pending, some evicted. Every sample emitted
// lands in exactly one.
func TestTierBReconciliationCoversAttributedAndPending(t *testing.T) {
	const pid = 4242
	f := newPCJoinFixture(t, TimelineConfig{
		MaxPendingSamplesPerCorrelation: 2,
	})

	emitted := uint64(0)
	// Four into the joinable group, which holds two: two attributed, two
	// evicted by the per-group cap.
	for i := range 4 {
		f.sample(t, pid, uint64(10+i))
		emitted++
	}
	// One into a group nothing can name: still pending at the end.
	require.NoError(t, f.tl.EmitPCSample(tierBSample(pid, 0xBAD, f.fnIndex, 20)))
	emitted++

	require.NoError(t, f.tl.EmitExec(pcExec(pid, f.kernel, "0", 30, 40)))

	snap := f.tl.Snapshot()

	assert.Equal(t, emitted,
		snap.AttributedPCSamples+
			uint64(snap.PendingSamples)+snap.Dropped.EvictedPendingSamples+
			uint64(snap.PendingModuleSamples)+snap.Dropped.EvictedPendingModuleSamples,
		"every emitted sample must be attributed, pending, or evicted - never unaccounted for")

	assert.Equal(t, uint64(2), snap.AttributedPCSamples)
	assert.Equal(t, uint64(2), snap.PCJoin.AttributedKernel)
	assert.Equal(t, uint64(2), snap.Dropped.EvictedPendingModuleSamples)
	assert.Equal(t, 1, snap.PendingModuleSamples)
	assert.Zero(t, snap.Dropped.EvictedPendingSamples,
		"the Tier A counter stays assertably zero through every Tier B outcome")
	assertPCJoinIdentities(t, snap)
}

// assertPCJoinIdentities checks the two sums the PC join must satisfy in every
// scenario, in the same shape the conformance suite checks them: the attributed
// breakdown accounts for every attributed sample, and the four group outcomes
// account for every group examined.
func assertPCJoinIdentities(t *testing.T, snap Snapshot) {
	t.Helper()
	assert.Equal(t, snap.AttributedPCSamples, snap.PCJoin.AttributedTotal(),
		"AttributedExact + AttributedKernel must account for every attributed sample: %+v", snap.PCJoin)
	assert.Equal(t, snap.PCJoin.GroupsExamined(),
		snap.PCJoin.GroupsJoined+uint64(snap.PendingModuleGroups),
		"every group examined must be joined or left pending for one counted reason: %+v", snap.PCJoin)
	assertPCAttribAccompaniesSamples(t, snap)
}

// ---------------------------------------------------------------------------
// The enum itself
// ---------------------------------------------------------------------------

// TestPCAttribsAreExhaustiveAndStable pins the four wire strings. They are the
// gpu_pc_attrib label values; rewording one silently changes the profile.
func TestPCAttribsAreExhaustiveAndStable(t *testing.T) {
	assert.Equal(t,
		[]PCAttrib{"exact", "kernel", "kernel-ambiguous", "kernel-multidevice"},
		PCAttribs())

	PCAttribs()[0] = "mutated"
	assert.Equal(t, PCAttribExact, PCAttribs()[0], "PCAttribs must return a copy")
}

// TestPCAttribRefusesValuesNobodyDecided is the SrcStatus discipline applied
// here: a value the join did not choose - including the empty one, which is
// what an ExecutionView carrying samples and no attribution would hold - must
// fail at the serialization boundary rather than ship as a string a consumer
// would read as meaningful.
func TestPCAttribRefusesValuesNobodyDecided(t *testing.T) {
	for _, a := range PCAttribs() {
		b, err := json.Marshal(a)
		require.NoError(t, err)
		assert.JSONEq(t, `"`+string(a)+`"`, string(b))
	}
	for _, bad := range []PCAttrib{"", "kernel-probably", "heuristic"} {
		_, err := json.Marshal(bad)
		assert.Error(t, err, "gpu_pc_attrib %q is not one of the four and must not serialize", bad)
	}

	// omitempty is what keeps a view with no PC samples off that path.
	b, err := json.Marshal(ExecutionView{})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "pc_attrib")
}
