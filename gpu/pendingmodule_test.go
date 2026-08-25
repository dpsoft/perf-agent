package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tierBSample builds a Tier B (CONTINUOUS-mode) PC sample: no correlation
// value, the producing PID set, and the module identity that has to carry
// the attribution instead. CUPTI populates correlationId only in
// KERNEL_SERIALIZED collection; in continuous mode it is always zero, which
// is what Correlation.Present() == false means here.
func tierBSample(pid uint32, crc uint64, fnIndex uint32, timeNs uint64) GPUPCSample {
	return GPUPCSample{
		Correlation:   CorrelationID{Backend: BackendCUPTI, PID: pid},
		Module:        ModuleRef{Backend: BackendCUPTI, CRC: crc},
		FunctionIndex: fnIndex,
		ClockDomain:   ClockDomainCPUMonotonic,
		TimeNs:        timeNs,
		PCOffset:      uint64(fnIndex)<<32 | timeNs,
		Count:         1,
	}
}

// TestTierBPCSamplesDoNotCollapseOntoOneKey is the anti-collapse regression.
//
// Timeline.pending keys on the whole CorrelationID. A Tier B sample has
// Correlation.Value == "" with Correlation.PID set, so every Tier B sample
// from one process hashes to the single key {BackendCUPTI, pid, ""}.
// pendingSampleCap (4,096 by default) then bounds an entire process's PC
// samples to that one entry and evicts everything past it into
// EvictedPendingSamples - and Snapshot matches none of them anyway, because
// no execution ever carries an empty correlation value. That is the
// pathology spec §6.1 describes for correlation-less launches, and it
// presents as "PC sampling produces almost nothing" with an eviction counter
// as the only clue.
//
// 10,000 samples across 50 distinct (crc, functionIndex) pairs is 200
// samples per pair - two orders of magnitude below any per-group bound - so
// with a correctly-keyed index nothing is lost. Against the single-key store
// 5,904 of them are evicted.
func TestTierBPCSamplesDoNotCollapseOntoOneKey(t *testing.T) {
	const (
		pid          = 4242
		crcCount     = 10
		fnCount      = 5
		distinctKeys = crcCount * fnCount // 50
		samples      = 10_000
	)

	tl := NewTimeline(TimelineConfig{})
	for i := range samples {
		crc := uint64(0xC0DE0000 + i%crcCount)
		fnIndex := uint32((i / crcCount) % fnCount)
		require.NoError(t, tl.EmitPCSample(tierBSample(pid, crc, fnIndex, uint64(i+1))))
	}

	snap := tl.Snapshot()

	assert.Zero(t, snap.Dropped.EvictedPendingSamples,
		"a Tier B sample must never reach the correlation-keyed pending store, "+
			"whose single {backend, pid, \"\"} key collapses a whole process's PC samples onto one entry")
	assert.Zero(t, snap.Dropped.EvictedPendingModuleSamples,
		"%d samples over %d distinct (crc, functionIndex) groups is far below every bound; "+
			"nothing should have been evicted", samples, distinctKeys)
	assert.Equal(t, samples, snap.PendingModuleSamples,
		"every Tier B sample must still be held, waiting for Task 8b's join")
	assert.Equal(t, distinctKeys, snap.PendingModuleGroups,
		"the samples must be spread over one group per (crc, functionIndex) pair, not collapsed")
	assert.Zero(t, snap.PendingSamples,
		"the correlation-keyed store must hold nothing at all in a pure Tier B run")
	assert.Zero(t, snap.PendingCorrelations)
}

// TestTierBHorizonEvictionCountsSeparately pins the counter split: a Tier B
// eviction storm must be distinguishable from a Tier A one, so an operator
// reading joinhealth can tell "the module index is too small / the horizon is
// too short" apart from "executions are not arriving for their correlations".
// Sharing one counter would make the two diagnoses indistinguishable at
// exactly the moment they matter.
func TestTierBHorizonEvictionCountsSeparately(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{PendingSampleHorizonNs: 100})

	// Three samples in one group, all near the anchor's start.
	for i := range 3 {
		require.NoError(t, tl.EmitPCSample(tierBSample(pid, 0xAAAA, 1, uint64(i+1))))
	}
	require.Len(t, tl.pendingModule, 1)

	// A later group drags the shared anchor far past the horizon, aging the
	// first group out.
	require.NoError(t, tl.EmitPCSample(tierBSample(pid, 0xBBBB, 2, 5_000)))

	snap := tl.Snapshot()
	assert.Equal(t, uint64(3), snap.Dropped.EvictedPendingModuleSamples,
		"the aged-out group's three samples must be counted as Tier B evictions")
	assert.Zero(t, snap.Dropped.EvictedPendingSamples,
		"a Tier B eviction must never be charged to the Tier A counter")
	assert.Equal(t, 1, snap.PendingModuleGroups, "only the newest group survives the horizon")
	assert.Equal(t, 1, snap.PendingModuleSamples)
}

// TestTierBSamplesStayWithTheirProcess is issue #52's discipline applied to
// the module index: the PID is IN the key, so a sample from one process
// cannot land in another's group. There is no cross-process check to forget,
// because there is no check - two processes running the identical binary
// produce the identical cubin CRC and the identical function index, and only
// the PID separates them.
func TestTierBSamplesStayWithTheirProcess(t *testing.T) {
	const (
		pidA = 4242
		pidB = 5353
	)
	tl := NewTimeline(TimelineConfig{})

	// Same CRC, same function index, different processes: the worst case for
	// a store that keyed on the module alone.
	require.NoError(t, tl.EmitPCSample(tierBSample(pidA, 0xF00D, 7, 10)))
	require.NoError(t, tl.EmitPCSample(tierBSample(pidA, 0xF00D, 7, 11)))
	require.NoError(t, tl.EmitPCSample(tierBSample(pidB, 0xF00D, 7, 12)))

	require.Len(t, tl.pendingModule, 2,
		"identical (crc, functionIndex) from two processes must be two groups, not one")

	keyA := pendingModuleKey{Backend: BackendCUPTI, PID: pidA, CubinCRC: 0xF00D, FunctionIndex: 7}
	keyB := pendingModuleKey{Backend: BackendCUPTI, PID: pidB, CubinCRC: 0xF00D, FunctionIndex: 7}

	groupA, ok := tl.pendingModule[keyA]
	require.True(t, ok)
	groupB, ok := tl.pendingModule[keyB]
	require.True(t, ok)

	require.Len(t, groupA.samples, 2)
	require.Len(t, groupB.samples, 1)
	for _, s := range groupA.samples {
		assert.Equal(t, uint32(pidA), s.Correlation.PID,
			"pid %d's group holds a sample from pid %d", pidA, s.Correlation.PID)
	}
	assert.Equal(t, uint32(pidB), groupB.samples[0].Correlation.PID)

	snap := tl.Snapshot()
	assert.Equal(t, 2, snap.PendingModuleGroups)
	assert.Equal(t, 3, snap.PendingModuleSamples)
	assert.Zero(t, snap.Dropped.EvictedPendingModuleSamples)
	assert.Zero(t, snap.Dropped.EvictedPendingSamples)
}

// TestTierBDistinctFunctionIndexSplitsGroups isolates the field this task
// adds. Same process, same cubin, different device functions: without
// FunctionIndex on GPUPCSample the two are one key and Task 8b could never
// resolve them to different kernels.
func TestTierBDistinctFunctionIndexSplitsGroups(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitPCSample(tierBSample(pid, 0x1234, 0, 10)))
	require.NoError(t, tl.EmitPCSample(tierBSample(pid, 0x1234, 1, 11)))

	snap := tl.Snapshot()
	assert.Equal(t, 2, snap.PendingModuleGroups,
		"two device functions in one cubin must key apart")
	assert.Equal(t, 2, snap.PendingModuleSamples)
}

// TestTierBCardinalityEvictionCountsSeparately drives the module index's own
// cardinality cap - the bound that reads non-zero when a JIT- or
// template-explosion workload loads more distinct device functions than the
// index can hold - and pins that it too stays off the Tier A counter.
func TestTierBCardinalityEvictionCountsSeparately(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{MaxPendingModuleGroups: 4})

	for i := range 6 {
		require.NoError(t, tl.EmitPCSample(tierBSample(pid, uint64(i), uint32(i), uint64(i+1))))
	}

	snap := tl.Snapshot()
	assert.Equal(t, 4, snap.PendingModuleGroups, "the cap bounds distinct groups")
	assert.Equal(t, uint64(2), snap.Dropped.EvictedPendingModuleSamples,
		"the two oldest groups' samples were evicted, oldest first")
	assert.Zero(t, snap.Dropped.EvictedPendingSamples)
	assert.Equal(t, 4, snap.PendingModuleSamples)
}

// TestTierBPerGroupSampleCapCountsSeparately covers the third way a Tier B
// sample can be lost: one group accumulating past MaxPendingSamplesPerCorrelation
// because its execution never arrives. All three losses land on
// EvictedPendingModuleSamples and none on EvictedPendingSamples.
func TestTierBPerGroupSampleCapCountsSeparately(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{MaxPendingSamplesPerCorrelation: 3})

	for i := range 5 {
		require.NoError(t, tl.EmitPCSample(tierBSample(pid, 0xABC, 0, uint64(i+1))))
	}

	snap := tl.Snapshot()
	assert.Equal(t, 1, snap.PendingModuleGroups)
	assert.Equal(t, 3, snap.PendingModuleSamples)
	assert.Equal(t, uint64(2), snap.Dropped.EvictedPendingModuleSamples)
	assert.Zero(t, snap.Dropped.EvictedPendingSamples)
}

// TestTierBSamplesReconcile is the accounting invariant for the new store,
// with every one of its terms forced non-zero in a single run: some samples
// still pending, some evicted by the cardinality cap, some by the horizon,
// some by the per-group sample cap. If any bound stopped counting, or counted
// into the wrong store, the sum stops matching what was emitted.
func TestTierBSamplesReconcile(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{
		MaxPendingModuleGroups:          3,
		MaxPendingSamplesPerCorrelation: 2,
		PendingSampleHorizonNs:          1_000,
	})

	emitted := uint64(0)
	emit := func(crc uint64, fn uint32, timeNs uint64) {
		t.Helper()
		require.NoError(t, tl.EmitPCSample(tierBSample(pid, crc, fn, timeNs)))
		emitted++
	}

	// Per-group sample cap: 4 samples into a group that holds 2.
	for i := range 4 {
		emit(0x10, 0, uint64(i+1))
	}
	// Cardinality: five more distinct groups against a cap of 3.
	for i := range 5 {
		emit(uint64(0x20+i), uint32(i), uint64(10+i))
	}
	// Horizon: a jump far past 1,000 ns ages everything before it out.
	emit(0x99, 9, 500_000)

	snap := tl.Snapshot()

	assert.Equal(t, emitted,
		snap.AttributedPCSamples+
			uint64(snap.PendingSamples)+snap.Dropped.EvictedPendingSamples+
			uint64(snap.PendingModuleSamples)+snap.Dropped.EvictedPendingModuleSamples,
		"every emitted sample must be attributed, pending, or evicted - never unaccounted for")

	assert.Zero(t, snap.AttributedPCSamples, "no execution was ever emitted")
	assert.Zero(t, snap.PendingSamples, "not one of these carried a correlation value")
	assert.Zero(t, snap.Dropped.EvictedPendingSamples,
		"the Tier A counter must stay assertably zero through every Tier B loss")
	assert.Positive(t, snap.Dropped.EvictedPendingModuleSamples,
		"all three bounds fired; a zero here means at least one stopped counting")
	assert.Equal(t, 1, snap.PendingModuleGroups, "only the post-horizon group survives")
	assert.Equal(t, 1, snap.PendingModuleSamples)
}

// TestTierAPCSamplesStillUseTheCorrelationIndex is the non-regression half:
// a sample that DOES carry a correlation value must keep taking the exact
// path untouched, and must never appear in the module index. Task 8a routes
// on Correlation.Present() and nothing else.
func TestTierAPCSamplesStillUseTheCorrelationIndex(t *testing.T) {
	const pid = 4242
	tl := NewTimeline(TimelineConfig{})
	corr := CorrelationID{Backend: BackendCUPTI, PID: pid, Value: "77"}

	require.NoError(t, tl.EmitLaunch(launchIn(pid, "77", 10, "a_work")))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation:   corr,
		Module:        ModuleRef{Backend: BackendCUPTI, CRC: 0xF00D},
		FunctionIndex: 7,
		TimeNs:        25,
		PCOffset:      0xAAA,
		Count:         1,
	}))
	require.NoError(t, tl.EmitExec(execIn(pid, "77", 20, 30)))

	assert.Empty(t, tl.pendingModule, "a correlation-bearing sample must never enter the module index")

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.Len(t, snap.Executions[0].PCSamples, 1)
	assert.Equal(t, uint32(7), snap.Executions[0].PCSamples[0].FunctionIndex,
		"FunctionIndex must survive the exact-correlation path unchanged")
	assert.Equal(t, uint64(1), snap.AttributedPCSamples)
	assert.Zero(t, snap.PendingModuleSamples)
	assert.Zero(t, snap.PendingModuleGroups)
}
