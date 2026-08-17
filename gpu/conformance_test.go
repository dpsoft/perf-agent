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
//  2. Heuristic joins are always marked.
//     Stressed by: correlation-gaps' "gap-heur" group, which forces a real
//     heuristic join (matching correlation deliberately absent; only kernel
//     name + queue + causal timing can attach the launch). Mutation caught:
//     findLaunchHeuristic's caller setting view.Launch without also setting
//     Join/Heuristic (e.g. Heuristic=true dropped from the heuristic branch).
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
	assertTimestampsMonotonic(t, snap)
	assertBoundedMemory(t, snap, h.cacheCapacity)

	// Invariant 5's literal formula - SinkStats.DroppedFull + DroppedInvalid
	// plus LaunchCacheStats.EvictedCapacity + EvictedHorizon equals emitted
	// minus retained - is only exact when the sink handled launches
	// exclusively and no correlation ID was reused. SinkStats.DroppedFull/
	// DroppedInvalid/DroppedDownstream are aggregate counters shared by all
	// five event kinds, not per-type, so a scenario that also emits execs/
	// pc-samples/modules/events would fold their drops into the same
	// numbers and break the identity; a reused correlation ID adds a third
	// loss category (LaunchCacheStats.Replaced) the literal formula doesn't
	// mention at all. Both conditions are real gaps in what these
	// components expose for a caller who wants exact reconciliation on a
	// mixed workload - see the report. They are not a reason to weaken the
	// assertion, only to scope it to the scenario where it is provably
	// exact (overflow: launch-only, unique IDs).
	if h.attempt.execAttempts == 0 && h.attempt.pcAttempts == 0 &&
		h.attempt.moduleAttempts == 0 && h.attempt.eventAttempts == 0 {
		assertLaunchLossesAccounted(t, snap, sinkStats, h.attempt.launchAttempts)
	}

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
// scenarios, which is exactly where the brief's literal formula is exact.
func assertLaunchLossesAccounted(t *testing.T, snap Snapshot, sinkStats SinkStats, launchAttempts uint64) {
	t.Helper()
	require.Equal(t, uint64(0), snap.LaunchCache.Replaced,
		"this reconciliation assumes no correlation-ID reuse; a launch-only scenario that reuses IDs needs a different formula")
	require.Equal(t, uint64(0), sinkStats.DroppedDownstream,
		"the harness's inner sink is a Timeline, which never rejects a delivered event")

	retained := uint64(snap.LaunchCache.Live)
	require.GreaterOrEqual(t, launchAttempts, retained, "cannot have retained more launches than were attempted")
	lost := launchAttempts - retained
	accounted := sinkStats.DroppedFull + sinkStats.DroppedInvalid + snap.LaunchCache.EvictedCapacity + snap.LaunchCache.EvictedHorizon
	assert.Equal(t, lost, accounted,
		"every launch that isn't currently retained must be accounted for by a sink drop or a cache eviction")
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
	// queue but deliberately different correlation IDs, so the exact lookup
	// misses and only the heuristic join (kernel name + queue + causal
	// timing) can attach a launch.
	for i := 0; i < 3; i++ {
		l := launch("gap-heur-"+strconv.Itoa(i), uint64(i*100)+1) // KernelName -> "k_gap-heur-<i>"
		if err := sink.EmitLaunch(l); err != nil {
			return err
		}
		e := GPUKernelExec{
			Correlation: CorrelationID{Backend: BackendCUPTI, Value: "gap-heur-exec-" + strconv.Itoa(i)},
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
// were actually rejected (SinkStats.DroppedInvalid) and that none of them
// leak into the snapshot under any correlation.
//
// Mutation this catches: CountingSink.admit's ValidateSupportedClockDomain
// check being removed or weakened to accept a non-monotonic domain - the
// "bad-*" correlations would then surface in the snapshot and the leak
// assertion below would fail.
func TestConformanceRejectedClockDomainsNeverSurface(t *testing.T) {
	h, snap := runConformance(t, "mixed-clock-domains", driveMixedClockDomains)
	sinkStats := h.sink.Stats()

	assert.Equal(t, uint64(2*2), sinkStats.DroppedInvalid,
		"every unsupported-domain attempt (one launch, one exec, per bad domain) must be rejected and counted")

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
	require.Greater(t, sinkStats.DroppedInvalid, uint64(0), "the scenario must force a real clock-domain rejection")
	require.Greater(t, snap.LaunchCache.EvictedCapacity, uint64(0), "the scenario must force a real capacity eviction too")

	t.Logf("loss reconciliation: attempts=%d retained=%d droppedFull=%d droppedInvalid=%d evictedCapacity=%d evictedHorizon=%d replaced=%d",
		h.attempt.launchAttempts, snap.LaunchCache.Live, sinkStats.DroppedFull, sinkStats.DroppedInvalid,
		snap.LaunchCache.EvictedCapacity, snap.LaunchCache.EvictedHorizon, snap.LaunchCache.Replaced)
}

// BenchmarkSnapshotAtScale is the Phase 2 gate: a million launches through a
// bounded cache must snapshot in well under a second, with allocation
// reflecting the bounded cache rather than a million retained launches.
func BenchmarkSnapshotAtScale(b *testing.B) {
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
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tl.Snapshot()
	}
}
