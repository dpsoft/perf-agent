package gpu

// This suite replaces the earlier prototype's 53 checked-in replay goldens.
// A golden pinned the exact JSON shape of the normalized model, so a single
// added field regenerated all 53 and produced an unreviewable diff. Here we
// instead assert invariants any producer's output must satisfy - replay, HIP
// and CUPTI backends will all run the same table.
//
// Six invariants, each mapped to the scenario(s) below that can actually
// violate it (not just pass vacuously):
//
//  1. Exact joins are never reported heuristic.
//     Stressed by: all-exact (30 real JoinExact views) and correlation-gaps'
//     exact group. Mutation caught: JoinExact path setting Heuristic=true, or
//     the Join/Heuristic fields disagreeing.
//  2. Heuristic joins are always marked, and only ever attach to an exec
//     that never supplied a correlation ID at all (review Critical 2: an
//     exec whose correlation missed the cache has told us its launch aged
//     out, not that no correlation was ever available, and guessing there
//     risks mis-attributing to a different, still-live launch - spec §13
//     requires degrading to unattributed instead).
//     Stressed by: correlation-gaps' "gap-heur" group, which forces a real
//     heuristic join (matching correlation deliberately absent; only kernel
//     name + queue + causal timing can attach the launch), and
//     driveEvictedLaunchDegradesToUnattributed, the named spec §13
//     reproduction (see TestConformanceEvictedLaunchDegradesToUnattributed).
//     Mutation caught: findLaunchHeuristic's caller setting view.Launch
//     without also setting Join/Heuristic (e.g. Heuristic=true dropped from
//     the heuristic branch), or the exec.Correlation zero-value gate in
//     Snapshot being removed (assertHeuristicOnlyForCorrelationlessExecs).
//  3. No launch is fabricated.
//     Stressed by: correlation-gaps' "gap-orphan" group (no launch was ever
//     emitted for those execs) and the clock-domain test's rejected launches
//     (never even reached the cache). Mutation caught: a miss path attaching
//     a zero-value or otherwise invented *GPUKernelLaunch instead of leaving
//     Launch nil.
//  4. Timestamps are monotonic within the declared domain.
//     Stressed two ways: all-exact emits two PC samples per execution with
//     increasing TimeNs (catches a reordering bug in the pending-sample
//     path), and TestConformanceRejectedClockDomainsNeverSurface emits
//     genuinely unsupported domains (GPUDevice/Synced) alongside valid and
//     zero-value ones (catches CountingSink's domain check being weakened or
//     removed).
//  5. Losses are accounted.
//     Stressed by: overflow, which puts real drops (SinkStats.DroppedFull)
//     and real capacity evictions (LaunchCacheStats.EvictedCapacity) in the
//     same run at very different magnitudes, AND
//     horizon-and-domain-losses (see driveHorizonAndDomainLosses), which
//     exists specifically because overflow's harness never sets HorizonNs
//     and never attempts an unsupported clock domain - EvictedHorizon and
//     DroppedInvalid are structurally 0 everywhere else the reconciliation
//     formula runs, so those two of its four terms would be undetectable
//     dead weight without it. See assertLaunchLossesAccounted's doc comment
//     for why the check is scoped to launch-only, non-reused-ID scenarios
//     rather than applied universally.
//  6. Bounded memory.
//     Stressed by: overflow (100k launches into a capacity-64 cache).
//     Mutation caught: capacity eviction being skipped or the ring/cache
//     silently growing.

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// conformanceCacheCapacity matches invariant 6's literal wording ("a
	// cache of capacity 64") and is small enough that the overflow scenario's
	// 100k launches genuinely evict almost everything.
	conformanceCacheCapacity = 64

	// conformanceSinkBurst is deliberately much larger than the low-volume
	// correctness scenarios (a few dozen events each), so they never
	// spuriously hit ErrSinkFull, and deliberately much smaller than the
	// overflow scenario's 100k attempts, so that scenario still exhausts the
	// token bucket for real. Sharing one harness constructor across every
	// scenario means this number has to satisfy both jobs at once.
	conformanceSinkBurst = 4096
)

// attemptSink wraps an EventSink and records ground truth about what a
// producer actually attempted, independent of the wrapped sink's own
// bookkeeping (SinkStats/LaunchCacheStats) - the whole point of this suite is
// to check that bookkeeping, so "what was emitted" must not be derived from
// the same counters under test.
type attemptSink struct {
	inner EventSink

	launchAttempts uint64
	// launchCarried holds the correlation ID of every launch the wrapped
	// sink actually forwarded successfully (err == nil) - the only launches
	// that could possibly reach a view's Launch pointer.
	launchCarried  map[CorrelationID]struct{}
	execAttempts   uint64
	pcAttempts     uint64
	moduleAttempts uint64
	eventAttempts  uint64
}

func newAttemptSink(inner EventSink) *attemptSink {
	return &attemptSink{inner: inner, launchCarried: make(map[CorrelationID]struct{})}
}

func (a *attemptSink) EmitLaunch(l GPUKernelLaunch) error {
	a.launchAttempts++
	err := a.inner.EmitLaunch(l)
	if err == nil {
		a.launchCarried[l.Correlation] = struct{}{}
	}
	return err
}

func (a *attemptSink) EmitExec(e GPUKernelExec) error {
	a.execAttempts++
	return a.inner.EmitExec(e)
}

func (a *attemptSink) EmitPCSample(p GPUPCSample) error {
	a.pcAttempts++
	return a.inner.EmitPCSample(p)
}

func (a *attemptSink) EmitModule(m GPUModule) error {
	a.moduleAttempts++
	return a.inner.EmitModule(m)
}

func (a *attemptSink) EmitEvent(e GPUTimelineEvent) error {
	a.eventAttempts++
	return a.inner.EmitEvent(e)
}

// conformanceHarness is a fresh Timeline + CountingSink pair, wired the way a
// real backend is: producer -> CountingSink (admission control) ->
// Timeline (join point). The clock is frozen (never advanced) so the token
// bucket never refills mid-scenario: admission becomes a pure function of
// attempt count instead of racing wall-clock time, which is what makes
// invariant 5's reconciliation exact instead of flaky. reuses fakeClock from
// sink_test.go.
type conformanceHarness struct {
	tl      *Timeline
	sink    *CountingSink
	attempt *attemptSink

	// cacheCapacity is the launch-cache capacity this harness was built
	// with, kept alongside tl so assertBoundedMemory can check a scenario's
	// actual configured bound instead of assuming every harness shares
	// conformanceCacheCapacity - not true once
	// newConformanceHarnessWithConfig is used with a different capacity.
	cacheCapacity int
}

// newConformanceHarnessWithConfig is newConformanceHarness generalized over
// the launch-cache config, for the one scenario (horizon-and-domain-losses)
// that needs a HorizonNs the shared default harness deliberately doesn't
// set. The clock is always frozen, for the same reason newConformanceHarness
// freezes it.
func newConformanceHarnessWithConfig(cacheCfg LaunchCacheConfig, sinkBurst int) *conformanceHarness {
	tl := NewTimeline(TimelineConfig{LaunchCache: cacheCfg})
	clock := newFakeClock(time.Unix(0, 0))
	sink := NewCountingSinkWithRate(tl, sinkBurst, 0, clock.Now)
	return &conformanceHarness{tl: tl, sink: sink, attempt: newAttemptSink(sink), cacheCapacity: cacheCfg.Capacity}
}

func newConformanceHarness() *conformanceHarness {
	return newConformanceHarnessWithConfig(LaunchCacheConfig{Capacity: conformanceCacheCapacity}, conformanceSinkBurst)
}

// runConformance drives one producer scenario through a fresh, default-
// configured harness and checks every invariant a producer's output must
// satisfy. This is the helper later vendor backends (replay, HIP, CUPTI)
// call with their own drive functions. It returns the harness and the
// snapshot checked against, for the rare scenario (e.g.
// TestConformanceRejectedClockDomainsNeverSurface) that needs an additional,
// scenario-specific assertion beyond the standard six - callers that don't
// need that can simply ignore both.
func runConformance(t *testing.T, name string, drive func(EventSink) error) (*conformanceHarness, Snapshot) {
	t.Helper()
	h := newConformanceHarness()
	snap := runConformanceWithHarness(t, name, h, drive)
	return h, snap
}

// runConformanceWithHarness is runConformance generalized over the harness,
// for scenarios that need non-default configuration (a launch-cache
// horizon, a different sink burst) to make a particular invariant term
// reachable at all. It shares the exact same invariant checks as
// runConformance - a scenario using this entry point still runs the
// identical suite, just paired with a harness it built for itself.
func runConformanceWithHarness(t *testing.T, name string, h *conformanceHarness, drive func(EventSink) error) Snapshot {
	t.Helper()
	var snap Snapshot
	t.Run(name, func(t *testing.T) {
		require.NoError(t, drive(h.attempt))
		snap = assertConformanceInvariants(t, h)
	})
	return snap
}

// assertConformanceInvariants snapshots the harness exactly once (Snapshot
// consumes matched PC samples - a second call would see none) and checks all
// six invariants against that snapshot.
func assertConformanceInvariants(t *testing.T, h *conformanceHarness) Snapshot {
	t.Helper()
	snap := h.tl.Snapshot()
	sinkStats := h.sink.Stats()

	assertNoFabricatedLaunch(t, snap, h.attempt.launchCarried)
	assertExactNeverHeuristic(t, snap)
	assertHeuristicAlwaysMarked(t, snap)
	assertHeuristicOnlyForCorrelationlessExecs(t, snap)
	assertTimestampsMonotonic(t, snap)
	assertBoundedMemory(t, snap, h.cacheCapacity)

	// Invariant 5's literal formula - SinkStats.Launches.DroppedFull +
	// DroppedInvalid plus LaunchCacheStats.EvictedCapacity + EvictedHorizon
	// equals emitted minus retained - used to be exact only when the sink
	// handled launches exclusively, because DroppedFull/DroppedInvalid/
	// DroppedDownstream were aggregate counters shared by all five event
	// kinds: a scenario that also emitted execs/pc-samples/modules/events
	// would fold their drops into the same numbers and break the identity.
	// Review Important 2 broke SinkStats down per event kind
	// (sinkStats.Launches is now exclusively about launches regardless of
	// what else a scenario emits), which removes that restriction entirely -
	// this now runs for every scenario in this file, not just the
	// launch-only ones. The one restriction that survives is a reused
	// correlation ID, which adds a third loss category
	// (LaunchCacheStats.Replaced) the literal formula still doesn't mention;
	// assertLaunchLossesAccounted itself requires Replaced == 0.
	assertLaunchLossesAccounted(t, snap, sinkStats, h.attempt.launchAttempts)

	// Extends invariant 5 to PC samples - review Important 2: the PC-sample
	// path had no reconciliation at all (nothing counted attributed vs
	// still-pending), so a sample could vanish between ingestion and the
	// snapshot with nothing to notice. Holds for every scenario because
	// Snapshot is only ever called once per harness here (see this
	// function's own doc comment) - AttributedPCSamples/PendingSamples are
	// this-call gauges, not cumulative, so a second Snapshot call would need
	// its own reconciliation, not this one.
	assertPCSampleLossesAccounted(t, snap, sinkStats, h.attempt.pcAttempts)

	return snap
}

// assertNoFabricatedLaunch is invariant 3: every non-nil view.Launch must
// carry a Correlation that some emitted launch actually carried, never a
// zero-value or otherwise invented value.
func assertNoFabricatedLaunch(t *testing.T, snap Snapshot, carried map[CorrelationID]struct{}) {
	t.Helper()
	for _, view := range snap.Executions {
		if view.Launch == nil {
			continue
		}
		_, ok := carried[view.Launch.Correlation]
		assert.True(t, ok, "execution %+v was attached a launch (correlation %+v) that was never actually emitted",
			view.Exec.Correlation, view.Launch.Correlation)
	}
}

// assertExactNeverHeuristic is invariant 1.
func assertExactNeverHeuristic(t *testing.T, snap Snapshot) {
	t.Helper()
	for _, view := range snap.Executions {
		if view.Join == JoinExact {
			assert.False(t, view.Heuristic, "an exact join (correlation %+v) must never be marked heuristic", view.Exec.Correlation)
		}
	}
}

// assertHeuristicAlwaysMarked is invariant 2.
func assertHeuristicAlwaysMarked(t *testing.T, snap Snapshot) {
	t.Helper()
	for _, view := range snap.Executions {
		if view.Join == JoinHeuristic {
			assert.True(t, view.Heuristic, "a heuristic join (correlation %+v) must always be marked Heuristic", view.Exec.Correlation)
		}
	}
}

// assertHeuristicOnlyForCorrelationlessExecs is the general form of review
// Critical 2's fix, checked against every scenario in this file: a
// GPUKernelExec that carried a non-zero Correlation and still ended up
// heuristically joined would mean an exec whose exact lookup missed (launch
// evicted, or never arrived) got guessed-attached anyway, instead of
// degrading to unattributed per spec §13. Only an exec with the zero-value
// Correlation (a backend, like DRM, that never supplies one) may take the
// heuristic path at all. See TestConformanceEvictedLaunchDegradesToUnattributed
// below for the specific reproduction this generalizes.
//
// The test is Present(), not equality with the zero CorrelationID: since
// issue #36 a correlation also carries the producing process, so a record
// with a pid and no vendor value is not the zero value yet still supplied no
// correlation.
func assertHeuristicOnlyForCorrelationlessExecs(t *testing.T, snap Snapshot) {
	t.Helper()
	for _, view := range snap.Executions {
		if view.Join == JoinHeuristic {
			assert.False(t, view.Exec.Correlation.Present(),
				"a heuristic join must only ever attach to an exec that never supplied a correlation ID at all, got %+v",
				view.Exec.Correlation)
		}
	}
}

// assertTimestampsMonotonic is invariant 4, scoped exactly as the brief
// states it: non-decreasing per correlation (not globally across unrelated
// correlations, which real producers have no reason to keep ordered against
// each other), plus every accepted event's ClockDomain normalizing to
// ClockDomainCPUMonotonic.
func assertTimestampsMonotonic(t *testing.T, snap Snapshot) {
	t.Helper()
	for _, view := range snap.Executions {
		assert.LessOrEqual(t, view.Exec.StartNs, view.Exec.EndNs,
			"execution %+v ends before it starts", view.Exec.Correlation)
		assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(view.Exec.ClockDomain),
			"an accepted exec must normalize to the CPU-monotonic domain")

		if view.Launch != nil {
			assert.LessOrEqual(t, view.Launch.TimeNs, view.Exec.StartNs,
				"launch %+v is timestamped after the execution it was joined to", view.Launch.Correlation)
			assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(view.Launch.ClockDomain),
				"an accepted launch must normalize to the CPU-monotonic domain")
		}

		var lastPC uint64
		for i, pcs := range view.PCSamples {
			if i > 0 {
				assert.LessOrEqual(t, lastPC, pcs.TimeNs,
					"PC samples attached to execution %+v are out of order", view.Exec.Correlation)
			}
			lastPC = pcs.TimeNs
			assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(pcs.ClockDomain),
				"an accepted PC sample must normalize to the CPU-monotonic domain")
		}
	}
}

// assertBoundedMemory is invariant 6. capacity is the launch-cache capacity
// the scenario's own harness was configured with (conformanceHarness.
// cacheCapacity), not necessarily conformanceCacheCapacity - the
// horizon-and-domain-losses scenario deliberately uses a different one.
func assertBoundedMemory(t *testing.T, snap Snapshot, capacity int) {
	t.Helper()
	assert.LessOrEqual(t, snap.LaunchCache.Live, capacity,
		"the launch cache must never hold more than its configured capacity")
}

// assertLaunchLossesAccounted is invariant 5, restricted (see
// assertConformanceInvariants) to launch-only, non-reused-correlation-ID
// scenarios, which is exactly where the brief's literal formula is exact -
// and, since the scenario is launch-only, exactly where reading
// sinkStats.Launches specifically (rather than some cross-kind aggregate;
// SinkStats is now broken down per event kind - review Important 2) is
// unambiguously correct.
func assertLaunchLossesAccounted(t *testing.T, snap Snapshot, sinkStats SinkStats, launchAttempts uint64) {
	t.Helper()
	require.Equal(t, uint64(0), snap.LaunchCache.Replaced,
		"this reconciliation assumes no correlation-ID reuse; a launch-only scenario that reuses IDs needs a different formula")
	require.Equal(t, uint64(0), sinkStats.Launches.DroppedDownstream,
		"the harness's inner sink is a Timeline, which never rejects a delivered event")

	retained := uint64(snap.LaunchCache.Live)
	require.GreaterOrEqual(t, launchAttempts, retained, "cannot have retained more launches than were attempted")
	lost := launchAttempts - retained
	accounted := sinkStats.Launches.DroppedFull + sinkStats.Launches.DroppedInvalid + snap.LaunchCache.EvictedCapacity + snap.LaunchCache.EvictedHorizon
	assert.Equal(t, lost, accounted,
		"every launch that isn't currently retained must be accounted for by a sink drop or a cache eviction")
}

// assertPCSampleLossesAccounted is invariant 5 extended to PC samples -
// review Important 2. Every PC sample a producer attempted must land in
// exactly one bucket: rejected by the sink (DroppedFull/DroppedInvalid/
// DroppedDownstream, all per-kind since Important 2), attributed to an
// execution in this snapshot, still pending a future one, or evicted from
// the pending store (Timeline's own per-correlation cap, horizon or
// cardinality bound - review Critical 5). Holds for every scenario, unlike
// assertLaunchLossesAccounted: PC samples have no analogue of
// LaunchCacheStats.Replaced complicating the count (a correlation's samples
// simply accumulate in pending; there is no "replace" concept), and
// PendingSamples/AttributedPCSamples are already scoped to a single
// Snapshot call rather than needing a launch-only carve-out.
func assertPCSampleLossesAccounted(t *testing.T, snap Snapshot, sinkStats SinkStats, pcAttempts uint64) {
	t.Helper()
	rejected := sinkStats.PCSamples.DroppedFull + sinkStats.PCSamples.DroppedInvalid + sinkStats.PCSamples.DroppedDownstream
	accepted := sinkStats.PCSamples.Accepted
	require.Equal(t, pcAttempts, accepted+rejected,
		"every attempted PC sample must have been either accepted by the sink or rejected and counted")
	assert.Equal(t, accepted, snap.AttributedPCSamples+uint64(snap.PendingSamples)+snap.Dropped.EvictedPendingSamples,
		"every PC sample the sink accepted must be attributed, still pending, or evicted from the pending store - never unaccounted for")
}

// TestConformance runs the producer-scenario table against every invariant.
func TestConformance(t *testing.T) {
	scenarios := []struct {
		name  string
		drive func(EventSink) error
	}{
		{"all-exact", driveAllExact},
		{"correlation-gaps", driveCorrelationGaps},
		{"overflow", driveOverflow},
	}
	for _, sc := range scenarios {
		runConformance(t, sc.name, sc.drive)
	}
}

// TestConformanceCorrelationGapsActuallyExercisesHeuristic closes a gap the
// table-driven TestConformance above cannot: nothing in that loop ever reads
// JoinStats, so a driveCorrelationGaps edit that silently stopped exercising
// the heuristic join (e.g. the "gap-heur" group's exec correlation being
// left non-zero, which review Critical 2's gate would then route to
// UnmatchedExecutionCount instead) would still pass every invariant check -
// an unmatched exec is exactly as invariant-compliant as a heuristically
// matched one. This pins the counts the doc comment at the top of this file
// claims: 3 heuristic joins (the "gap-heur" group) and 2 unmatched
// executions (the "gap-orphan" group), independent of the harness's own
// bookkeeping being otherwise self-consistent.
func TestConformanceCorrelationGapsActuallyExercisesHeuristic(t *testing.T) {
	_, snap := runConformance(t, "correlation-gaps-heuristic-check", driveCorrelationGaps)
	assert.Equal(t, uint64(3), snap.JoinStats.HeuristicExecutionJoinCount,
		"the gap-heur group must actually take the heuristic path, not silently degrade to unmatched")
	assert.Equal(t, uint64(2), snap.JoinStats.UnmatchedExecutionCount,
		"the gap-orphan group must remain genuinely unmatched")
}

// driveAllExact is the happy-path scenario: every execution correlates
// exactly to the launch that produced it, and each carries two PC samples
// with increasing timestamps (stresses invariant 4's per-correlation
// ordering on the PC-sample path, not just a single-sample no-op case).
func driveAllExact(sink EventSink) error {
	const n = 30
	for i := 0; i < n; i++ {
		id := "exact-" + strconv.Itoa(i)
		base := uint64(i * 100)
		if err := sink.EmitLaunch(launch(id, base+1)); err != nil {
			return err
		}
		if err := sink.EmitExec(execFor(id, base+10, base+20)); err != nil {
			return err
		}
		corr := CorrelationID{Backend: BackendCUPTI, Value: id}
		if err := sink.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: base + 13, PCOffset: 0x10, Count: 1}); err != nil {
			return err
		}
		if err := sink.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: base + 17, PCOffset: 0x20, Count: 2}); err != nil {
			return err
		}
	}
	return nil
}

// driveCorrelationGaps deliberately breaks the exact-correlation path: some
// executions must fall back to the heuristic join (same queue and kernel
// name, launch precedes exec, but a different correlation ID), and some must
// find no candidate at all.
func driveCorrelationGaps(sink EventSink) error {
	// Exact group: 5 launch/exec pairs sharing a correlation.
	for i := 0; i < 5; i++ {
		id := "gap-exact-" + strconv.Itoa(i)
		base := uint64(i*100) + 1
		if err := sink.EmitLaunch(launch(id, base)); err != nil {
			return err
		}
		if err := sink.EmitExec(execFor(id, base+10, base+20)); err != nil {
			return err
		}
	}

	// Heuristic-recoverable group: launch and exec share a kernel name and
	// queue, and the exec's Correlation carries no value - review Critical 2
	// restricts the heuristic path to execs that never supplied a
	// correlation ID at all (the exact case this group models: a backend,
	// like DRM, with no correlation concept), so only the heuristic join
	// (kernel name + queue + causal timing) can attach a launch here.
	//
	// It does carry the producing process, matching launch()'s
	// LaunchContext.PID. Issue #52 made that mandatory: an execution that
	// names neither a correlation nor a process cannot be joined heuristically
	// at all, because nothing would stop the guess from reaching into another
	// process's launch - and its CPU stack and pod_uid/container_id tags. A
	// correlation-less backend that wants this group's behavior has to say
	// whose execution it is, which every probe-based producer can.
	for i := 0; i < 3; i++ {
		l := launch("gap-heur-"+strconv.Itoa(i), uint64(i*100)+1) // KernelName -> "k_gap-heur-<i>"
		if err := sink.EmitLaunch(l); err != nil {
			return err
		}
		e := GPUKernelExec{
			Correlation: CorrelationID{Backend: BackendCUPTI, PID: l.Launch.PID},
			KernelName:  l.KernelName,
			Queue:       l.Queue,
			StartNs:     uint64(i*100) + 10,
			EndNs:       uint64(i*100) + 20,
		}
		if err := sink.EmitExec(e); err != nil {
			return err
		}
	}

	// Orphaned group: correlation and kernel name that never had any
	// matching launch emitted at all.
	for i := 0; i < 2; i++ {
		e := execFor("gap-orphan-"+strconv.Itoa(i), uint64(9000+i*10), uint64(9000+i*10+5))
		if err := sink.EmitExec(e); err != nil {
			return err
		}
	}
	return nil
}

// driveOverflow drives 100k launches with unique correlation IDs through a
// capacity-64 cache behind a burst-4096 token bucket, deliberately
// overflowing both by very different margins (~96k sink drops, ~4k cache
// evictions - see conformanceSinkBurst's doc comment for why they differ).
// Every ID is unique (strconv.Itoa(i)) for the same reason
// BenchmarkSnapshotAtScale insists on it: a reused ID collapses the cache to
// one entry and "overflow" becomes an illusion. Launch-only by design - see
// assertConformanceInvariants for why mixing event types would break the
// exactness of invariant 5's reconciliation.
func driveOverflow(sink EventSink) error {
	for i := 0; i < 100_000; i++ {
		id := strconv.Itoa(i)
		err := sink.EmitLaunch(launch(id, uint64(i)+1))
		if err != nil && !errors.Is(err, ErrSinkFull) {
			return err
		}
	}
	return nil
}

// driveMixedClockDomains emits launches and execs in a mix of the supported
// domain (explicit and zero-value/defaulted) and two unsupported domains.
// Expressed as a plain drive func like every other scenario - a vendor
// backend wired through runConformance inherits this coverage too, not just
// this file's own tests - it deliberately discards each unsupported-domain
// call's error rather than asserting on it inline (the same way
// driveOverflow discards ErrSinkFull): rejection is still verified, just
// after driving completes, via SinkStats and the snapshot (see
// TestConformanceRejectedClockDomainsNeverSurface) rather than a per-call
// assertion the drive signature has no *testing.T to make.
func driveMixedClockDomains(sink EventSink) error {
	for i := 0; i < 3; i++ { // valid, explicit domain
		id := "good-" + strconv.Itoa(i)
		l := launch(id, uint64(i*10)+1)
		l.ClockDomain = ClockDomainCPUMonotonic
		if err := sink.EmitLaunch(l); err != nil {
			return err
		}
		e := execFor(id, uint64(i*10)+2, uint64(i*10)+3)
		e.ClockDomain = ClockDomainCPUMonotonic
		if err := sink.EmitExec(e); err != nil {
			return err
		}
	}
	for i := 0; i < 3; i++ { // zero-value domain must default, not reject
		id := "zero-" + strconv.Itoa(i)
		if err := sink.EmitLaunch(launch(id, uint64(1000+i))); err != nil {
			return err
		}
		if err := sink.EmitExec(execFor(id, uint64(2000+i), uint64(2000+i+1))); err != nil {
			return err
		}
	}
	badDomains := []ClockDomain{ClockDomainGPUDevice, ClockDomainSynced}
	for i, dom := range badDomains {
		id := "bad-" + strconv.Itoa(i)
		l := launch(id, uint64(5000+i))
		l.ClockDomain = dom
		_ = sink.EmitLaunch(l) // expected rejection; verified post-hoc below

		e := execFor(id, uint64(6000+i), uint64(6000+i+1))
		e.ClockDomain = dom
		_ = sink.EmitExec(e)
	}
	return nil
}

// TestConformanceRejectedClockDomainsNeverSurface targets invariant 4's
// second clause directly: "every accepted event has ClockDomainCPUMonotonic
// after normalization." The three table-driven scenarios in TestConformance
// only ever use the supported domain, so that clause holds for them
// vacuously - nothing else was ever possible. driveMixedClockDomains is the
// scenario that can actually fail: launches and execs in the unsupported
// ClockDomainGPUDevice/ClockDomainSynced domains, alongside valid explicit
// and valid zero-value (defaulted) ones. This test drives it through
// runConformance - the same entry point a vendor backend uses - then adds
// the two checks a generic invariant pass can't express: that the bad ones
// were actually rejected (SinkStats per-kind DroppedInvalid) and that none
// of them leak into the snapshot under any correlation.
//
// Mutation this catches: CountingSink.admit's ValidateSupportedClockDomain
// check being removed or weakened to accept a non-monotonic domain - the
// "bad-*" correlations would then surface in the snapshot and the leak
// assertion below would fail. Checking Launches and Execs separately (not a
// combined total) is itself a review Important 2 regression check: before
// SinkStats was broken down per event kind, a bug that rejected every
// launch but zero execs (or vice versa) could still sum to the same total
// this test used to check.
func TestConformanceRejectedClockDomainsNeverSurface(t *testing.T) {
	h, snap := runConformance(t, "mixed-clock-domains", driveMixedClockDomains)
	sinkStats := h.sink.Stats()

	assert.Equal(t, uint64(2), sinkStats.Launches.DroppedInvalid,
		"every unsupported-domain launch attempt (one per bad domain) must be rejected and counted")
	assert.Equal(t, uint64(2), sinkStats.Execs.DroppedInvalid,
		"every unsupported-domain exec attempt (one per bad domain) must be rejected and counted")

	for _, view := range snap.Executions {
		assert.NotContains(t, view.Exec.Correlation.Value, "bad-",
			"an execution rejected for an unsupported clock domain must never appear in the snapshot")
		if view.Launch != nil {
			assert.NotContains(t, view.Launch.Correlation.Value, "bad-",
				"a launch rejected for an unsupported clock domain must never surface via a join")
		}
	}
}

// driveHorizonAndDomainLosses is the scenario that keeps invariant 5's
// EvictedHorizon and DroppedInvalid terms honest. No other scenario in this
// file sets LaunchCacheConfig.HorizonNs, so EvictedHorizon is structurally 0
// everywhere else; driveMixedClockDomains does attempt unsupported clock
// domains, but that scenario mixes in execs, so
// assertConformanceInvariants's launch-only gate never runs the
// reconciliation formula against it. That means DroppedInvalid was 0 in
// every run that actually evaluated the sum, and EvictedHorizon was 0 in
// all of them: `0 + X == X` means deleting either term from
// assertLaunchLossesAccounted's sum would still pass the entire rest of the
// suite. This scenario forces both nonzero - and, as a consequence of how
// it's built, EvictedCapacity too - in one run where the reconciliation
// formula is actually checked.
//
// Two phases against a cache of capacity 1000 / horizon 50ns:
//   - Phase A: 200 launches spaced 10ns apart (TimeNs = 0, 10, 20, ...). The
//     50ns horizon only lets entries within 50ns of the newest timestamp
//     stay live, so as the anchor advances, older entries age out via
//     EvictedHorizon long before the cache is anywhere near its capacity of
//     1000.
//   - Phase B: 1500 more launches that all carry the same TimeNs (2000,
//     equal to Phase A's final horizon boundary), so the horizon anchor
//     only advances once, at the very start of this phase, and horizon
//     eviction cannot fire again after Phase A's last few survivors age
//     out. Once no more entries can be aged out by horizon, filling the
//     cache with 1500 more unique-correlation entries against a capacity of
//     1000 has nowhere to go but EvictedCapacity.
//   - 4 launches in unsupported clock domains, rejected outright
//     (DroppedInvalid), touching neither the cache nor the token bucket.
//
// Every correlation ID is unique and every launch is emitted exactly once,
// so LaunchCacheStats.Replaced stays 0 and assertLaunchLossesAccounted's
// preconditions hold.
func driveHorizonAndDomainLosses(sink EventSink) error {
	for i := 0; i < 200; i++ {
		if err := sink.EmitLaunch(launch("horizon-"+strconv.Itoa(i), uint64(i*10))); err != nil {
			return err
		}
	}
	for i := 0; i < 1500; i++ {
		if err := sink.EmitLaunch(launch("capacity-"+strconv.Itoa(i), 2000)); err != nil {
			return err
		}
	}
	badDomains := []ClockDomain{ClockDomainGPUDevice, ClockDomainSynced, ClockDomainGPUDevice, ClockDomainSynced}
	for i, dom := range badDomains {
		l := launch("horizon-bad-"+strconv.Itoa(i), 2000)
		l.ClockDomain = dom
		_ = sink.EmitLaunch(l) // expected rejection; verified post-hoc below
	}
	return nil
}

// TestConformanceLossAccountingCoversHorizonAndInvalidDomain drives
// driveHorizonAndDomainLosses through a harness with HorizonNs actually set
// (runConformance's shared default harness deliberately never sets it), then
// - beyond the standard six invariants runConformanceWithHarness already
// checks, including the reconciliation formula itself, since this scenario
// is launch-only and unique-ID by construction - asserts that the three
// previously-dead-or-unchecked terms are genuinely nonzero here. Without
// that extra guard, a future edit to this scenario's numbers could silently
// regress it back into vacuity while the reconciliation assertion kept
// passing for the wrong reason (both sides shrinking to 0 together).
//
// Mutation this catches: deleting "+ sinkStats.DroppedInvalid" or
// "+ snap.LaunchCache.EvictedHorizon" from assertLaunchLossesAccounted's sum
// - both now make the reconciliation check inside
// runConformanceWithHarness/assertConformanceInvariants fail here, where
// deleting either used to be invisible everywhere else in this file.
func TestConformanceLossAccountingCoversHorizonAndInvalidDomain(t *testing.T) {
	h := newConformanceHarnessWithConfig(LaunchCacheConfig{Capacity: 1000, HorizonNs: 50}, conformanceSinkBurst)
	snap := runConformanceWithHarness(t, "horizon-and-domain-losses", h, driveHorizonAndDomainLosses)
	sinkStats := h.sink.Stats()

	require.Greater(t, snap.LaunchCache.EvictedHorizon, uint64(0), "the scenario must force a real horizon eviction")
	require.Greater(t, sinkStats.Launches.DroppedInvalid, uint64(0), "the scenario must force a real clock-domain rejection")
	require.Greater(t, snap.LaunchCache.EvictedCapacity, uint64(0), "the scenario must force a real capacity eviction too")

	t.Logf("loss reconciliation: attempts=%d retained=%d droppedFull=%d droppedInvalid=%d evictedCapacity=%d evictedHorizon=%d replaced=%d",
		h.attempt.launchAttempts, snap.LaunchCache.Live, sinkStats.Launches.DroppedFull, sinkStats.Launches.DroppedInvalid,
		snap.LaunchCache.EvictedCapacity, snap.LaunchCache.EvictedHorizon, snap.LaunchCache.Replaced)
}

// driveEvictedLaunchDegradesToUnattributed is the spec §13 fixture named in
// the final whole-branch review's Critical 2 finding: "a sample whose launch
// has been evicted degrades to unattributed rather than mis-attributed."
// Reproduces the reviewer's exact scenario against a capacity-1 cache: two
// launches sharing one kernel name ("hot_kernel") so the surviving launch is
// a plausible-looking heuristic candidate for the evicted one's exec, then
// an exec whose Correlation names the evicted launch. Before the review
// Critical 2 fix, this exec would have been heuristically reattached to the
// survivor - a different launch's PID/Tags entirely. The caller (the
// dedicated test below, not the generic table) checks the specific outcome;
// this drive func only needs to produce the scenario.
func driveEvictedLaunchDegradesToUnattributed(sink EventSink) error {
	a := launch("a", 10)
	a.KernelName = "hot_kernel"
	if err := sink.EmitLaunch(a); err != nil {
		return err
	}
	b := launch("b", 20)
	b.KernelName = "hot_kernel" // shares a's kernel name and queue
	if err := sink.EmitLaunch(b); err != nil {
		return err // evicts "a" from a capacity-1 cache
	}
	exec := execFor("a", 30, 40) // Correlation "a": exact lookup misses because "a" was evicted
	exec.KernelName = "hot_kernel"
	return sink.EmitExec(exec)
}

// TestConformanceEvictedLaunchDegradesToUnattributed drives the scenario
// above through a dedicated capacity-1 harness and asserts the specific
// outcome spec §13 requires: the exec attached to correlation "a" must have
// no Launch at all, not the survivor "b". The general
// assertHeuristicOnlyForCorrelationlessExecs invariant (run for every
// scenario, including this one, via runConformanceWithHarness) already
// guards the mechanism; this test pins the concrete, named reproduction so
// a future change that broke the mechanism in a way the general invariant
// couldn't see would still be caught here.
func TestConformanceEvictedLaunchDegradesToUnattributed(t *testing.T) {
	h := newConformanceHarnessWithConfig(LaunchCacheConfig{Capacity: 1}, conformanceSinkBurst)
	snap := runConformanceWithHarness(t, "evicted-launch-degrades", h, driveEvictedLaunchDegradesToUnattributed)

	require.Len(t, snap.Executions, 1)
	view := snap.Executions[0]
	assert.Equal(t, "a", view.Exec.Correlation.Value)
	assert.Nil(t, view.Launch,
		"an execution whose launch was evicted must degrade to unattributed, never reattach to a different launch that merely shares its kernel name")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
}

// drivePCSampleMixedFate is the scenario that keeps
// assertPCSampleLossesAccounted's PendingSamples/AttributedPCSamples/
// EvictedPendingSamples terms honest. Every other PC-sample-emitting
// scenario in this file (currently just all-exact) matches every sample to
// an exec before the single Snapshot call, so PendingSamples and
// Dropped.EvictedPendingSamples are structurally 0 wherever the
// reconciliation actually runs - the same "dead term" trap invariant 5's
// launch formula had before driveHorizonAndDomainLosses was added. This
// scenario forces all three terms nonzero in one run: "attributed" (an
// exec arrives for its samples), "pending" (samples with no exec at
// Snapshot time), and "evicted" (two orphan correlations pushed out of
// Timeline's pending store entirely by the cardinality bound - the
// dedicated harness this scenario runs under sets the launch-cache
// capacity, and therefore pendingCap, to 2).
//
// Ordering matters here: "evict-a"/"evict-b" are inserted first (oldest),
// filling pendingCap exactly; "pending" then "attributed" each push a new
// distinct correlation past that cap, evicting the two "evict-*"
// correlations in FIFO order (oldest first) before either "pending" or
// "attributed" is ever at risk. By the time Snapshot runs, "attributed" and
// "pending" are the two live pending entries - Snapshot matches
// "attributed" to its exec and leaves "pending" untouched.
func drivePCSampleMixedFate(sink EventSink) error {
	for _, id := range []string{"evict-a", "evict-b"} {
		corr := CorrelationID{Backend: BackendCUPTI, Value: id}
		if err := sink.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: 0}); err != nil {
			return err
		}
	}

	// Pending: samples with no exec at all, so they remain in Timeline's
	// pending store when Snapshot runs. Inserted before "attributed" so
	// eviction pressure below lands on the two "evict-*" correlations, not
	// this one.
	pendingCorr := CorrelationID{Backend: BackendCUPTI, Value: "pending"}
	for i := 0; i < 3; i++ {
		if err := sink.EmitPCSample(GPUPCSample{Correlation: pendingCorr, TimeNs: uint64(i)}); err != nil {
			return err
		}
	}

	// Attributed: a launch, its exec, and 2 samples that will be matched at
	// Snapshot time.
	if err := sink.EmitLaunch(launch("attributed", 1)); err != nil {
		return err
	}
	if err := sink.EmitExec(execFor("attributed", 10, 20)); err != nil {
		return err
	}
	attributedCorr := CorrelationID{Backend: BackendCUPTI, Value: "attributed"}
	for i := 0; i < 2; i++ {
		if err := sink.EmitPCSample(GPUPCSample{Correlation: attributedCorr, TimeNs: uint64(11 + i)}); err != nil {
			return err
		}
	}
	return nil
}

// TestConformancePCSampleReconciliationCoversPendingAndEvicted drives
// drivePCSampleMixedFate through a harness whose Timeline has a small
// per-correlation sample cap, then - beyond the standard invariants
// runConformanceWithHarness already checks, including
// assertPCSampleLossesAccounted itself - asserts the three terms are
// genuinely nonzero, the same way
// TestConformanceLossAccountingCoversHorizonAndInvalidDomain does for the
// launch formula's dead terms.
//
// Mutation this catches: deleting "+ uint64(snap.PendingSamples)" or
// "+ snap.Dropped.EvictedPendingSamples" from assertPCSampleLossesAccounted's
// sum - both now make the reconciliation check fail here, where deleting
// either was invisible in every other scenario in this file.
func TestConformancePCSampleReconciliationCoversPendingAndEvicted(t *testing.T) {
	// LaunchCache capacity 2 -> pendingCap 2 (Timeline reuses the launch-
	// cache capacity for pending's cardinality bound by default): exactly
	// enough for "pending" and "attributed" to both survive, forcing
	// "evict-a"/"evict-b" out entirely. See drivePCSampleMixedFate's doc
	// comment for the exact ordering this depends on.
	h := newConformanceHarnessWithConfig(LaunchCacheConfig{Capacity: 2}, conformanceSinkBurst)
	snap := runConformanceWithHarness(t, "pc-sample-mixed-fate", h, drivePCSampleMixedFate)

	assert.Equal(t, uint64(2), snap.AttributedPCSamples, "the attributed correlation's 2 samples must be attached to its exec")
	assert.Equal(t, 3, snap.PendingSamples, "the pending correlation's 3 samples must remain, unmatched")
	assert.Equal(t, 1, snap.PendingCorrelations)
	assert.Equal(t, uint64(2), snap.Dropped.EvictedPendingSamples,
		"evict-a and evict-b (1 sample each) must have been evicted from pending entirely by the cardinality bound")
}

// BenchmarkSnapshotAtScale is the Phase 2 gate: a million launches through a
// bounded cache must snapshot in well under a second, with allocation
// reflecting the bounded cache rather than a million retained launches.
//
// PC samples were added to this benchmark for review Critical 5: before
// that fix, a single pending correlation's samples could accumulate without
// bound, and this benchmark - the phase's own memory/timing gate - emitted
// zero PC samples, so it could not see that growth at all. Each sampled
// exec (1 in 4, same as before) now also gets 3 PC samples under its own
// correlation, which exercises both the per-correlation cap and the
// proportional weight distribution's cost, not just the launch-cache path.
// newSnapshotScaleTimeline builds the fixture BenchmarkSnapshotAtScale
// measures: 1M launches with unique correlation IDs (see the comment
// inline), a quarter of them with a matched exec, each of those with 3 PC
// samples. Factored out of the benchmark so it can be rebuilt once per
// b.N iteration - see BenchmarkSnapshotAtScale's doc comment for why that
// matters.
func newSnapshotScaleTimeline() *Timeline {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 65536}})
	for i := 0; i < 1_000_000; i++ {
		// Every launch gets its own correlation ID. Reusing one would mean the
		// cache holds a single entry for the whole run, and this benchmark
		// would report a fast snapshot precisely because the structure under
		// test was empty - the phase gate would pass while measuring nothing.
		id := strconv.Itoa(i)
		_ = tl.EmitLaunch(launch(id, uint64(i)))
		if i%4 == 0 {
			_ = tl.EmitExec(execFor(id, uint64(i), uint64(i+10)))
			corr := CorrelationID{Backend: BackendCUPTI, Value: id}
			for j := 0; j < 3; j++ {
				_ = tl.EmitPCSample(GPUPCSample{Correlation: corr, TimeNs: uint64(i + j), PCOffset: uint64(j), Count: 1})
			}
		}
	}
	return tl
}

// BenchmarkSnapshotAtScale is the Phase 2 exit gate: a million launches
// through a bounded cache must snapshot in well under a second, with
// allocation reflecting the bounded cache rather than a million retained
// launches.
//
// The timeline is rebuilt inside the loop, with the timer stopped for the
// rebuild, on every iteration - not built once outside the loop and
// snapshotted b.N times. Review Critical 3 made Snapshot consume
// executions/events/modules/matched-PC-samples, so a timeline built once
// and snapshotted repeatedly is fully drained after iteration 1: every
// iteration after the first would measure an empty structure. That is
// exactly invisible under `-benchtime=1x` (the convention this project's
// other GPU benchmarks rely on and what this benchmark itself used to be
// measured with) since b.N is 1 there and the bug never gets a second
// iteration to hide in - but it is exactly what `go test -bench .`
// (b.N auto-tuned, almost always > 1, and what CI or a casual check
// actually runs) would hit, reporting a few milliseconds and a few dozen
// allocations for an empty timeline while believing it measured the real
// one. A `b.Skip` on b.N > 1, or a doc comment demanding a flag, would
// both still allow the benchmark to be run wrongly and lie; rebuilding the
// fixture per iteration is correct unconditionally, at any -benchtime.
func BenchmarkSnapshotAtScale(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tl := newSnapshotScaleTimeline()
		b.StartTimer()
		_ = tl.Snapshot()
	}
}
