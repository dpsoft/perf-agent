package main

// The offline half of Task 12.
//
// The measurement needs an RTX 3090 and none of it can be run here. What CAN
// be run here is everything between the measurement and the decision: the
// parsers that turn a producer's log line and a workload's result line into
// evidence, the medians, the ratio, and the four pre-committed threshold
// clauses. Those are the parts that decide the outcome, and a harness whose
// decision logic is only exercised on hardware is a harness whose decision
// logic has never been tested at all.
//
// Every threshold test states the arm table it feeds in, so a reader can see
// the thresholds are being applied to the numbers the plan named rather than
// to numbers chosen to produce a preferred answer.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
	"github.com/dpsoft/perf-agent/gpu"
)

// armsWith builds an arm table: a baseline, a Tier B arm at tierBCost percent,
// and one Tier A arm per (duty, cost) pair. The ratio is derived, never
// supplied, so a test cannot accidentally assert against a ratio that does not
// follow from the cost it set.
func armsWith(tierBCost float64, duties, costs []float64) []schema.GPUPCArm {
	if len(duties) != len(costs) {
		panic("duties and costs must be the same length")
	}
	base := 1000.0
	arms := []schema.GPUPCArm{
		{Name: "baseline", Tier: gpu.PCSamplingNameOff, MedianWallMs: base},
		{Name: "tier B", Tier: gpu.PCSamplingNameContinuous,
			MedianWallMs: base * (1 + tierBCost/100), CostPercent: tierBCost},
	}
	for i, duty := range duties {
		c := costs[i]
		arms = append(arms, schema.GPUPCArm{
			Name:           fmt.Sprintf("tier A %g%% duty", duty*100),
			Tier:           gpu.PCSamplingNameSerialized,
			DutyConfigured: duty,
			MedianWallMs:   base * (1 + c/100),
			CostPercent:    c,
			CostOverDuty:   (c / 100) / duty,
		})
	}
	return arms
}

// planDuties is the plan's own arm table: 10%, 5%, 2.5%.
var planDuties = []float64{0.10, 0.05, 0.025}

// armsFor is armsWith over the plan's three duties.
func armsFor(tierBCost float64, tierACosts [3]float64) []schema.GPUPCArm {
	return armsWith(tierBCost, planDuties, tierACosts[:])
}

func decide(arms []schema.GPUPCArm) schema.GPUPCDecision {
	return decideGPUPC(arms, gpuPCMaxWallPercent, gpuPCMaxCostOverDuty)
}

// ---------------------------------------------------------------------------
// The four pre-committed clauses.
// ---------------------------------------------------------------------------

// Tier B over 5% wall-clock: Tier B does not ship as always-on.
func TestTierBOverFivePercentStopsBeingAlwaysOn(t *testing.T) {
	d := decide(armsFor(7.5, [3]float64{1.0, 0.5, 0.25}))
	assert.Equal(t, verdictTierBExplicitOnly, d.TierB)
	assert.Contains(t, d.Fired, clauseTierBOverBudget)
	assert.Contains(t, strings.Join(d.Lines, "\n"), "does not ship as always-on")
}

// The mirror, and it matters as much: a Tier B that is within budget must not
// be quietly demoted, or the clause would be unfalsifiable.
func TestTierBWithinFivePercentStaysAnAlwaysOnCandidate(t *testing.T) {
	d := decide(armsFor(1.2, [3]float64{1.0, 0.5, 0.25}))
	assert.Equal(t, verdictTierBAlwaysOnCandidate, d.TierB)
	assert.NotContains(t, d.Fired, clauseTierBOverBudget)
}

// Exactly at the bar is within it: the plan says "> 5%" fires, so 5.0 does
// not. Pinned because a strict-versus-non-strict slip is the classic way a
// pre-committed threshold quietly becomes a different threshold.
func TestTierBExactlyAtTheBarDoesNotFire(t *testing.T) {
	d := decide(armsFor(gpuPCMaxWallPercent, [3]float64{1.0, 0.5, 0.25}))
	assert.Equal(t, verdictTierBAlwaysOnCandidate, d.TierB)
}

// Tier A at 10% duty costs <= 5% AND cost/duty <= 2: ships as an opt-in tier.
// 4% cost at 10% duty is a ratio of 0.4.
func TestTierAWithinBothBarsShipsOptIn(t *testing.T) {
	d := decide(armsFor(1.0, [3]float64{4.0, 2.0, 1.0}))
	assert.Equal(t, verdictTierAOptIn, d.TierA)
	assert.Contains(t, d.Fired, clauseTierAHeadlineWithin)
	assert.Contains(t, strings.Join(d.Lines, "\n"), "ships as an opt-in tier")
}

// cost/duty > 2 at every duty tested: deep-dive mode only.
//
// Reaching this verdict at all takes a duty BELOW 2.5%, and the reason is
// arithmetic rather than a quirk of this table: cost/duty > 2 is the same
// statement as cost > 200 x duty percent, which at 2.5% duty is exactly
// "cost > 5%" — the wall-clock bar. See
// TestTheTwoHarshClausesCoincideAtThePlansLowestDuty. So the duties here are
// 10%/5%/1%, and the costs (25%, 12%, 3%) put every ratio above 2 while
// leaving the lowest duty inside the 5% wall bar.
func TestTierARatioAboveTwoAtEveryDutyIsDeepDiveOnly(t *testing.T) {
	d := decide(armsWith(1.0, []float64{0.10, 0.05, 0.01}, []float64{25.0, 12.0, 3.0}))
	assert.Equal(t, verdictTierADeepDiveOnly, d.TierA)
	assert.Contains(t, d.Fired, clauseTierARatioOverAtAll)
	assert.NotContains(t, d.Fired, clauseTierASmallestOverBud)
	joined := strings.Join(d.Lines, "\n")
	assert.Contains(t, joined, "deliberate deep-dive mode")
	assert.Contains(t, joined, "no suggestion that it suits continuous use")
}

// The finding, pinned so it cannot be un-noticed: on the plan's own arm table
// the deep-dive-only clause is UNREACHABLE, because at 2.5% duty it is the
// same condition as the unshippable clause and the unshippable one outranks
// it. A harness that reported deep-dive-only from these duties would be
// reporting a verdict its own thresholds cannot produce.
func TestTheTwoHarshClausesCoincideAtThePlansLowestDuty(t *testing.T) {
	// 2.5% duty, cost 6%: over the 5% wall bar, and ratio 2.4 > 2. The
	// higher duties are pushed over the ratio bar too (25% at 10% duty is
	// 2.5; 12% at 5% is 2.4), so the every-duty clause genuinely fires.
	d := decide(armsFor(1.0, [3]float64{25.0, 12.0, 6.0}))
	assert.Contains(t, d.Fired, clauseTierARatioOverAtAll)
	assert.Contains(t, d.Fired, clauseTierASmallestOverBud)
	assert.Equal(t, verdictTierAUnshippable, d.TierA,
		"the strictly harsher verdict must win")
	assert.Contains(t, strings.Join(d.Lines, "\n"),
		"they are the SAME condition",
		"the coincidence must be reported, not left for the reader to notice")
}

// One duty with a ratio at or below 2 is enough to stop the every-duty clause
// firing. Asserted because "at every duty tested" is a universal, and a
// harness that treated it as "at the headline duty" would reach a harsher
// verdict than the plan committed to.
func TestTierARatioClauseIsUniversalNotHeadline(t *testing.T) {
	// 25% at 10% duty is a ratio of 2.5, over the bar; 12% at 5% is 2.4,
	// over it; 4% at 2.5% is 1.6, WITHIN it. One arm within the bar is
	// enough to stop a clause that says "at every duty tested".
	d := decide(armsFor(1.0, [3]float64{25.0, 12.0, 4.0}))
	assert.NotContains(t, d.Fired, clauseTierARatioOverAtAll)
	assert.NotEqual(t, verdictTierADeepDiveOnly, d.TierA)
}

// Tier A at 2.5% duty still over 5% wall-clock: unshippable in this phase.
// This clause outranks every other verdict, so the table below is deliberately
// one in which the ratio clause ALSO fires — the harsher answer must win.
func TestTierASmallestDutyOverBudgetIsUnshippableAndOutranksEverything(t *testing.T) {
	d := decide(armsFor(1.0, [3]float64{30.0, 18.0, 9.0}))
	assert.Equal(t, verdictTierAUnshippable, d.TierA)
	assert.Contains(t, d.Fired, clauseTierASmallestOverBud)
	assert.Contains(t, d.Fired, clauseTierARatioOverAtAll)
	assert.Contains(t, strings.Join(d.Lines, "\n"), "unshippable in this phase")
}

// The residual the plan's four clauses do not name: the headline duty is over
// the wall-clock bar, but a smaller duty is within BOTH bars. Duty-cycling
// works here — it just needs to be turned down — so the honest outcome is a
// named verdict that says which duty to ship, not a silent promotion to
// "opt-in" and not a demotion to "deep dive".
//
// 8% at 10% duty (ratio 0.8, over the wall bar); 4% at 5% (ratio 0.8, within
// both); 2% at 2.5% (ratio 0.8, within both).
func TestTierAOverBudgetAtTheHeadlineButTunableNamesTheDuty(t *testing.T) {
	d := decide(armsFor(1.0, [3]float64{8.0, 4.0, 2.0}))
	assert.Equal(t, verdictTierASmallerDuty, d.TierA)
	assert.NotContains(t, d.Fired, clauseTierAHeadlineWithin)
	assert.NotContains(t, d.Fired, clauseTierASmallestOverBud)
	joined := strings.Join(d.Lines, "\n")
	// The LARGEST qualifying duty, not just any: 5%, not 2.5%.
	assert.Contains(t, joined, "tier A 5% duty is within both bars")
}

// A table where nothing qualifies at any duty and neither harsh clause fires
// is not a pass. It means cost does not fall with duty the way serialization
// says it must, so the measurement itself is unsound — and it gets a verdict
// that says so rather than the friendliest neighbouring answer.
//
// Duties 10%/5%/1%; costs 8%, 6%, 3%. The two larger duties are over the wall
// bar (so nothing qualifies there); at 1% duty 3% is INSIDE the wall bar but
// its ratio is 3.0, outside the ratio bar — so nothing qualifies anywhere, the
// every-duty ratio clause does not fire (10% duty is at 0.8), and the
// lowest-duty wall clause does not fire either.
func TestTierAWithNothingQualifyingAnywhereIsIndeterminateNotAPass(t *testing.T) {
	d := decide(armsWith(1.0, []float64{0.10, 0.05, 0.01}, []float64{8.0, 6.0, 3.0}))
	assert.Empty(t, d.Fired, "no Tier A clause fires on this table")
	assert.Equal(t, verdictTierAIndeterminate, d.TierA)
	assert.Contains(t, strings.Join(d.Lines, "\n"), "do not read this as a pass")
}

// The counterpart, and the reassuring half: on the PLAN's own duties the
// decision is total. At 2.5% duty "within the wall bar" and "within the ratio
// bar" are the same condition, so the lowest-duty arm either qualifies for
// both — giving opt-in or a smaller duty — or fails both, giving unshippable.
// Indeterminate is unreachable there, which is why it exists as a guard for
// other duty tables rather than as an expected outcome.
func TestOnThePlansDutiesTheDecisionIsTotal(t *testing.T) {
	for _, costs := range [][3]float64{
		{1, 0.5, 0.25}, {4, 2, 1}, {8, 4, 2}, {12, 7, 4},
		{25, 12, 6}, {30, 18, 9}, {0.1, 0.1, 0.1}, {60, 30, 15},
	} {
		d := decide(armsFor(1.0, costs))
		assert.NotEqual(t, verdictTierAIndeterminate, d.TierA,
			"costs %v must reach a real verdict", costs)
		assert.NotEmpty(t, d.TierA)
	}
}

// A table with no serialized arm at all must not report a Tier A verdict.
func TestNoTierAArmIsIndeterminate(t *testing.T) {
	arms := armsFor(1.0, [3]float64{1, 1, 1})[:2]
	d := decide(arms)
	assert.Equal(t, verdictTierAIndeterminate, d.TierA)
	assert.Equal(t, verdictTierBAlwaysOnCandidate, d.TierB)
}

// The thresholds are echoed into the decision so the output file itself shows
// which numbers were applied. A run whose recorded bars differ from the plan's
// is a run whose verdict was taken against different thresholds.
func TestDecisionEchoesTheCommittedThresholds(t *testing.T) {
	d := decide(armsFor(1.0, [3]float64{1, 1, 1}))
	assert.InDelta(t, 5.0, d.MaxWallPercent, 1e-9)
	assert.InDelta(t, 2.0, d.MaxCostOverDut, 1e-9)
}

// ---------------------------------------------------------------------------
// The summary arithmetic.
// ---------------------------------------------------------------------------

// The cost is against the BASELINE arm — the shipping Phase 4 configuration
// with PC sampling off — and the ratio is against the CONFIGURED duty. Both
// are pinned here because both are choices, not arithmetic.
func TestSummarizeComputesCostAgainstBaselineAndRatioAgainstConfiguredDuty(t *testing.T) {
	arms := []schema.GPUPCArm{
		{Name: "baseline", Tier: gpu.PCSamplingNameOff,
			Runs: runsWithWall(100, 110, 90, 105, 95)},
		{Name: "tier A 10%", Tier: gpu.PCSamplingNameSerialized, DutyConfigured: 0.10,
			Runs: runsWithWall(120, 130, 110, 125, 115)},
	}
	summarizeGPUPCArms(arms)
	assert.InDelta(t, 100, arms[0].MedianWallMs, 1e-9, "median of 90,95,100,105,110")
	assert.InDelta(t, 120, arms[1].MedianWallMs, 1e-9, "median of 110,115,120,125,130")
	assert.InDelta(t, 20, arms[1].CostPercent, 1e-9)
	assert.InDelta(t, 2.0, arms[1].CostOverDuty, 1e-9, "0.20 cost over 0.10 duty")
	assert.Zero(t, arms[0].CostPercent, "the baseline is not a cost against itself")
}

func runsWithWall(v ...float64) []schema.GPUPCRun {
	out := make([]schema.GPUPCRun, 0, len(v))
	for i, w := range v {
		out = append(out, schema.GPUPCRun{RunN: i + 1, WallMs: w})
	}
	return out
}

func TestMedianFloat(t *testing.T) {
	assert.Zero(t, medianFloat(nil))
	assert.InDelta(t, 3.0, medianFloat([]float64{5, 1, 3}), 1e-9)
	assert.InDelta(t, 2.5, medianFloat([]float64{1, 2, 3, 4}), 1e-9)
	// The input must not be reordered under the caller.
	in := []float64{3, 1, 2}
	_ = medianFloat(in)
	assert.Equal(t, []float64{3, 1, 2}, in)
}

// ---------------------------------------------------------------------------
// The evidence parsers. These are what stand between "the arm ran in the mode
// it claims" and "the arm reported a flatteringly small overhead".
// ---------------------------------------------------------------------------

func TestParseAdapterReportReadsATierARun(t *testing.T) {
	stderr := strings.Join([]string{
		"perfagent-cupti: pc_sampling=on tier=A/kernel-serialized",
		"perfagent-cupti: graph_execs=0 multi_device=0 devices=1",
		"perfagent-cupti: tier A bursts=41 burst_ns=2091000000 duty=0.1043 gap_ns=450000000 " +
			"windows=82 range_end_drains=41 start_failed=0 stop_failed=0 graph_refused=0 sampling_now=0",
		"perfagent-cupti: pc exit tier=serialized period=8(=256 cycles) stall_reasons=38 " +
			"ctx_seen=1 ctx_enabled=1 ctx_enable_failed=0 pc_records=1828 pcs=352 " +
			"graph_execs=0 multi_device=0 finalize_seen=0",
	}, "\n")
	ev := parseAdapterReport(stderr)
	assert.Equal(t, "serialized", ev.ProducerTier)
	assert.Equal(t, uint64(1828), ev.ProducerPCRecords)
	assert.Equal(t, uint64(41), ev.ProducerBursts)
	assert.Equal(t, uint64(82), ev.ProducerWindows)
	assert.InDelta(t, 0.1043, ev.ProducerDuty, 1e-9)
	assert.Zero(t, ev.ProducerStartFailed)
	assert.Zero(t, ev.ProducerStopFailed)
	assert.Zero(t, ev.ProducerGraphRefuse)
	assert.Zero(t, ev.ProducerGraphExecs)
}

// The startup line spells the tier "A/kernel-serialized" and the exit report
// spells it "serialized". The parser must take it from the exit report, which
// is the line that also carries what the run actually did — reading the
// startup spelling would report a tier the run merely intended.
func TestParseAdapterReportPrefersTheExitReportSpelling(t *testing.T) {
	ev := parseAdapterReport(strings.Join([]string{
		"perfagent-cupti: pc_sampling=on tier=B/continuous",
		"perfagent-cupti: pc exit tier=continuous period=8(=256 cycles) pc_records=903 " +
			"graph_execs=0 multi_device=0",
	}, "\n"))
	assert.Equal(t, "continuous", ev.ProducerTier)
	assert.Equal(t, uint64(903), ev.ProducerPCRecords)
	assert.Zero(t, ev.ProducerBursts, "Tier B does not burst")
}

func TestParseAdapterReportReadsTheOffRun(t *testing.T) {
	ev := parseAdapterReport(strings.Join([]string{
		"perfagent-cupti: pc_sampling=off tier=none",
		"perfagent-cupti: graph_execs=0 multi_device=0 devices=1",
		"perfagent-cupti: pc_sampling=off tier_refused=0 " +
			"(set PERFAGENT_GPU_PC_SAMPLING=continuous or =serialized)",
	}, "\n"))
	assert.Equal(t, "off", ev.ProducerTier)
	assert.Zero(t, ev.ProducerPCRecords)
	assert.Zero(t, ev.ProducerBursts)
}

// A producer that printed nothing must leave the tier EMPTY, never "off".
// This is the difference between "the adapter was loaded and PC sampling was
// off" and "the adapter was never loaded at all", and the second one is an arm
// that measured nothing while looking like the cheapest arm in the table.
func TestParseAdapterReportOnSilenceLeavesTheTierUnset(t *testing.T) {
	ev := parseAdapterReport("some unrelated stderr from the workload\n")
	assert.Empty(t, ev.ProducerTier)
}

func TestParseConcurrentLine(t *testing.T) {
	r, err := parseConcurrentLine(
		"concurrent: iters=20000 warmup=64 streams=4 rounds=64000 blocks=16 threads=256 " +
			"sync_every=4 kernels=80000 elapsed_ms=24310.750 kernels_per_s=3290.7 " +
			"max_abs_err=0.000000000\n")
	require.NoError(t, err)
	assert.InDelta(t, 24310.750, r.WallMs, 1e-6)
	assert.InDelta(t, 3290.7, r.KernelsPerS, 1e-6)
	assert.Zero(t, r.MaxAbsErr)
}

func TestParseConcurrentLineRefusesSilence(t *testing.T) {
	_, err := parseConcurrentLine("concurrent: some other line\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no \"concurrent: iters=...\" result line")
}

// ---------------------------------------------------------------------------
// The arm-proof assertions. Every one of these can go red, which is the point:
// the failure they exist to catch reports a small, green overhead number.
// ---------------------------------------------------------------------------

func goodOffRun() schema.GPUPCRun {
	return schema.GPUPCRun{Evidence: schema.GPUPCEvidence{
		ProducerTier: gpu.PCSamplingNameOff, ExecutionsSeen: 4000,
		ExecutionsNotSerialized: 4000, SnapshotTier: gpu.PCSamplingNameOff,
	}}
}

func goodTierARun() schema.GPUPCRun {
	return schema.GPUPCRun{Evidence: schema.GPUPCEvidence{
		ProducerTier: gpu.PCSamplingNameSerialized, ProducerPCRecords: 1828,
		ProducerBursts: 41, ProducerWindows: 82, ProducerDuty: 0.104,
		PCSamplesDecoded: 1828, SamplingWindowsDecoded: 82,
		SamplingWindowsReceived: 82, ExecutionsSeen: 4000,
		ExecutionsSerialized: 380, ExecutionsNotSerialized: 3620,
		SnapshotTier: gpu.PCSamplingNameSerialized,
	}}
}

func testCfg() gpuPCConfig {
	return gpuPCConfig{MinConcurrency: 1.5, MinKernelUs: 50, MinBursts: 4}
}

func TestTheGoodArmsPass(t *testing.T) {
	cfg := testCfg()
	require.NoError(t, assertArmRanInItsMode(cfg, gpuPCArms[0], goodOffRun()))
	require.NoError(t, assertArmRanInItsMode(cfg, gpuPCArms[2], goodTierARun()))
}

// The single most important negative: an arm that ran nothing at all. It
// satisfies every "must be zero" clause perfectly and would be the fastest arm
// in the table.
func TestAnArmThatMeasuredNothingFails(t *testing.T) {
	r := goodOffRun()
	r.Evidence.ExecutionsSeen = 0
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[0], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "measured nothing")
}

// The baseline must be genuinely off. A baseline that was itself sampling
// understates every other arm's margin by exactly its own cost.
func TestABaselineThatWasSamplingFails(t *testing.T) {
	r := goodOffRun()
	r.Evidence.PCSamplesDecoded = 17
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[0], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "17 PC samples decoded with the tier off")
}

// A Tier A arm whose bursts overlapped no kernel measured the cost of starting
// and stopping CUPTI, not the cost of serialization — and would report a
// beautifully small number for it.
func TestATierAArmThatSerializedNothingFails(t *testing.T) {
	r := goodTierARun()
	r.Evidence.ExecutionsSerialized = 0
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[2], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not measure serialization")
}

// A Tier A arm that never bursted is the baseline under another name.
func TestATierAArmWithTooFewBurstsFails(t *testing.T) {
	r := goodTierARun()
	r.Evidence.ProducerBursts, r.Evidence.ProducerWindows = 2, 4
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[2], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "want at least 4")
}

// The window count must reconcile with the burst count: one open record and
// one closed record per burst, minus the close of a burst still open at exit.
func TestWindowsMustReconcileWithBursts(t *testing.T) {
	r := goodTierARun()
	r.Evidence.ProducerWindows = 60 // neither 82 nor 81
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[2], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "want 2N or 2N-1")

	r.Evidence.ProducerWindows = 81 // the hard-exit shape: 2N-1
	require.NoError(t, assertArmRanInItsMode(testCfg(), gpuPCArms[2], r))
}

// An arm that ran at a duty nobody configured has a meaningless ratio, because
// the ratio's denominator is the configured duty.
func TestATierAArmRunningAtTheWrongDutyFails(t *testing.T) {
	r := goodTierARun()
	r.Evidence.ProducerDuty = 0.31 // asked for ~0.10
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[2], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "achieved duty 0.3100")

	// The burst timer ticks at burst/5, so a 50 ms burst really runs
	// 50..60 ms and the achieved duty legitimately overshoots the
	// configured one. That must NOT fail.
	r.Evidence.ProducerDuty = 0.118
	require.NoError(t, assertArmRanInItsMode(testCfg(), gpuPCArms[2], r))
}

// CUDA graphs make Tier A refuse to burst. An arm in such a process ran Tier A
// for part of its length and baseline for the rest, which is not an arm.
func TestATierAArmInAGraphProcessFails(t *testing.T) {
	r := goodTierARun()
	r.Evidence.ProducerGraphExecs = 3
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[2], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "CUDA graph executions observed")
}

// Tier B must emit no window at all: a CONTINUOUS producer announcing one
// would be claiming a perturbation it did not cause.
func TestATierBArmThatEmittedAWindowFails(t *testing.T) {
	r := schema.GPUPCRun{Evidence: schema.GPUPCEvidence{
		ProducerTier: gpu.PCSamplingNameContinuous, ProducerPCRecords: 900,
		PCSamplesDecoded: 900, SamplingWindowsDecoded: 2, ExecutionsSeen: 4000,
		SnapshotTier: gpu.PCSamplingNameContinuous,
	}}
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[1], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "it must do neither")
}

// A Tier B arm that decoded no PC sample is the baseline with a different
// name, and would report ~0% overhead for a tier that did nothing.
func TestATierBArmThatSampledNothingFails(t *testing.T) {
	r := schema.GPUPCRun{Evidence: schema.GPUPCEvidence{
		ProducerTier: gpu.PCSamplingNameContinuous, ExecutionsSeen: 4000,
		SnapshotTier: gpu.PCSamplingNameContinuous,
	}}
	err := assertArmRanInItsMode(testCfg(), gpuPCArms[1], r)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "baseline with a different name")
}

// ---------------------------------------------------------------------------
// The workload guards and the cross-arm proof.
// ---------------------------------------------------------------------------

// A serial workload produces a small, green and meaningless Tier A cost — the
// exact defect the plan rules the 393k launches/s ceiling out for.
func TestASerialBaselineFailsAsAMicrobenchmark(t *testing.T) {
	err := assertBaselineIsRealistic(testCfg(), schema.GPUPCArm{
		MedianConcurrency: 1.02, MedianKernelUs: 300})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "almost no concurrency for serialization to destroy")
}

func TestTrivialKernelsFailAsAMicrobenchmark(t *testing.T) {
	err := assertBaselineIsRealistic(testCfg(), schema.GPUPCArm{
		MedianConcurrency: 3.4, MedianKernelUs: 3})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "launch-rate microbenchmark")
}

func TestARealisticBaselinePasses(t *testing.T) {
	require.NoError(t, assertBaselineIsRealistic(testCfg(), schema.GPUPCArm{
		MedianConcurrency: 3.4, MedianKernelUs: 300}))
}

// If the duty environment did not take, the three Tier A arms are one arm
// under three names — and their three different ratios are pure fiction.
// Burst count over fixed work is inversely proportional to burst+gap, so a
// lower duty must open strictly fewer bursts.
func TestThreeTierAArmsWithTheSameBurstCountFail(t *testing.T) {
	arms := []schema.GPUPCArm{
		{Name: "baseline", Tier: gpu.PCSamplingNameOff},
		{Name: "10%", Tier: gpu.PCSamplingNameSerialized, DutyConfigured: 0.100,
			Runs: runsWithBursts(41, 41, 41)},
		{Name: "5%", Tier: gpu.PCSamplingNameSerialized, DutyConfigured: 0.050,
			Runs: runsWithBursts(41, 41, 41)},
	}
	err := assertDutyKnobDidSomething(arms)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "one arm under three names")
}

func TestDecreasingBurstCountsAcrossDutiesPass(t *testing.T) {
	arms := []schema.GPUPCArm{
		{Name: "baseline", Tier: gpu.PCSamplingNameOff},
		{Name: "10%", Tier: gpu.PCSamplingNameSerialized, DutyConfigured: 0.100,
			Runs: runsWithBursts(48, 49, 48)},
		{Name: "5%", Tier: gpu.PCSamplingNameSerialized, DutyConfigured: 0.050,
			Runs: runsWithBursts(24, 25, 24)},
		{Name: "2.5%", Tier: gpu.PCSamplingNameSerialized, DutyConfigured: 0.025,
			Runs: runsWithBursts(12, 12, 13)},
	}
	require.NoError(t, assertDutyKnobDidSomething(arms))
}

func runsWithBursts(v ...uint64) []schema.GPUPCRun {
	out := make([]schema.GPUPCRun, 0, len(v))
	for i, b := range v {
		out = append(out, schema.GPUPCRun{RunN: i + 1,
			Evidence: schema.GPUPCEvidence{ProducerBursts: b}})
	}
	return out
}

// ---------------------------------------------------------------------------
// The arm table itself, and the environment it produces.
// ---------------------------------------------------------------------------

// The three Tier A gaps are not free numbers: they are exactly what the
// adapter's own duty ceiling produces for a 50 ms burst, which is why setting
// the ceiling pins the gap. If either side ever changes, this fails.
func TestTheArmTableIsThePlansAndItsDutiesAreExact(t *testing.T) {
	require.Len(t, gpuPCArms, 5)
	assert.Equal(t, gpu.PCSamplingOff, gpuPCArms[0].Tier)
	assert.Equal(t, gpu.PCSamplingContinuous, gpuPCArms[1].Tier)
	for i, want := range []float64{0.10, 0.05, 0.025} {
		a := gpuPCArms[2+i]
		assert.Equal(t, gpu.PCSamplingSerialized, a.Tier)
		assert.Equal(t, 50, a.BurstMs, "the plan's burst length")
		assert.InDelta(t, want, a.dutyConfigured(), 1e-9, "arm %q", a.Name)
		// min_gap = burst * (1/max_duty - 1), the adapter's own formula.
		assert.InDelta(t, float64(a.GapMs), float64(a.BurstMs)*(1/want-1), 1e-9)
	}
	assert.Zero(t, gpuPCArms[0].dutyConfigured(), "the off arm serializes nothing")
	assert.Zero(t, gpuPCArms[1].dutyConfigured(), "tier B serializes nothing")
}

// The tier is written to the producer's environment EXPLICITLY on every arm
// including the off one. An exported PERFAGENT_GPU_PC_SAMPLING in the
// operator's shell must not turn the baseline into a serializing arm, which
// would make every other arm look free.
func TestEveryArmWritesItsTierExplicitly(t *testing.T) {
	cfg := gpuPCConfig{ShimPath: "/tmp/adapter.so"}
	for _, spec := range gpuPCArms {
		env := gpuPCArmEnv(cfg, spec)
		assert.Contains(t, env, gpu.PCSamplingEnvVar+"="+spec.Tier.EnvValue(),
			"arm %q must name its tier", spec.Name)
		assert.Contains(t, env, "CUDA_INJECTION64_PATH=/tmp/adapter.so")
	}
}

// Both duty knobs, and that is the point: the ceiling sets the burst
// controller's MINIMUM gap and the max-gap sets its maximum, so the interval
// the loop clamps into collapses to a point and the arm runs at exactly the
// duty it claims. With only the ceiling set, a high pair rate would have the
// loop lengthen the gap and the arm would run at a duty nobody asked for.
func TestTierAArmsPinTheGapFromBothSides(t *testing.T) {
	cfg := gpuPCConfig{ShimPath: "x"}
	for i, permille := range []int{100, 50, 25} {
		env := gpuPCArmEnv(cfg, gpuPCArms[2+i])
		joined := strings.Join(env, " ")
		assert.Contains(t, joined, "PERFAGENT_GPU_PC_BURST_MS=50")
		assert.Contains(t, joined,
			"PERFAGENT_GPU_PC_MAX_DUTY_PERMILLE="+strconv.Itoa(permille))
		assert.Contains(t, joined,
			"PERFAGENT_GPU_PC_MAX_GAP_MS="+strconv.Itoa(gpuPCArms[2+i].GapMs))
	}
}

// The two non-Tier-A arms must set no burst knob at all: a baseline carrying
// Tier A's environment would be a baseline that bursts.
func TestNonTierAArmsCarryNoBurstEnvironment(t *testing.T) {
	for _, spec := range gpuPCArms[:2] {
		joined := strings.Join(gpuPCArmEnv(gpuPCConfig{ShimPath: "x"}, spec), " ")
		assert.NotContains(t, joined, "PERFAGENT_GPU_PC_BURST_MS")
		assert.NotContains(t, joined, "PERFAGENT_GPU_PC_MAX_DUTY_PERMILLE")
		assert.NotContains(t, joined, "PERFAGENT_GPU_PC_MAX_GAP_MS")
	}
}

// ---------------------------------------------------------------------------
// The concurrency measurement, which is what proves the workload is the
// realistic one rather than a microbenchmark wearing its name.
// ---------------------------------------------------------------------------

// Four kernels of 100 us each, all overlapping over a 100 us span: the
// concurrency is 4 and the mean duration is 100 us. This is the shape
// cuda_concurrent.cu is built to produce and the shape Tier A destroys.
func TestConcurrencyOfFullyOverlappingKernels(t *testing.T) {
	snap := gpu.Snapshot{Executions: []gpu.ExecutionView{
		execAt(1000, 101000), execAt(1000, 101000),
		execAt(1000, 101000), execAt(1000, 101000),
	}}
	c, us := concurrencyOf(snap)
	assert.InDelta(t, 4.0, c, 1e-9)
	assert.InDelta(t, 100.0, us, 1e-9)
}

// The same four kernels run back to back instead: concurrency 1. This is the
// value the baseline guard refuses, and it is refused precisely because a
// serial workload gives Tier A nothing to destroy.
func TestConcurrencyOfSerialKernelsIsOne(t *testing.T) {
	snap := gpu.Snapshot{Executions: []gpu.ExecutionView{
		execAt(0, 100000), execAt(100000, 200000),
		execAt(200000, 300000), execAt(300000, 400000),
	}}
	c, us := concurrencyOf(snap)
	assert.InDelta(t, 1.0, c, 1e-9)
	assert.InDelta(t, 100.0, us, 1e-9)
}

// A snapshot with no executions must report zero rather than dividing by
// zero: a run that produced nothing has no concurrency, and NaN would sail
// straight past the floor comparison.
func TestConcurrencyOfNothingIsZeroNotNaN(t *testing.T) {
	c, us := concurrencyOf(gpu.Snapshot{})
	assert.Zero(t, c)
	assert.Zero(t, us)
}

// An inverted or zero-length interval carries no duration and must not be
// counted as one, nor drag the span backwards.
func TestConcurrencyIgnoresDegenerateIntervals(t *testing.T) {
	snap := gpu.Snapshot{Executions: []gpu.ExecutionView{
		execAt(1000, 101000),
		execAt(5000, 5000), // zero length
		execAt(9000, 8000), // inverted
	}}
	c, us := concurrencyOf(snap)
	assert.InDelta(t, 1.0, c, 1e-9)
	assert.InDelta(t, 100.0, us, 1e-9)
}

func execAt(start, end uint64) gpu.ExecutionView {
	return gpu.ExecutionView{Exec: gpu.GPUKernelExec{StartNs: start, EndNs: end}}
}

// ---------------------------------------------------------------------------
// The skip path — the only thing this scenario can prove without hardware, and
// therefore the one thing here that must be proven exhaustively.
// ---------------------------------------------------------------------------

// All four branches, in order, each reachable. On the implementer's machine
// only the first can ever fire; a skip path whose other branches have never
// been executed is a skip path nobody has read.
func TestSkipReasonsAreReachableAndOrdered(t *testing.T) {
	// Two paths that exist, so the last two branches are about the
	// arguments and not about the filesystem.
	present := t.TempDir() + "/exists"
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o600))
	missing := t.TempDir() + "/absent"

	full := gpuPCConfig{ShimPath: present, WorkloadPath: present}

	assert.Contains(t, gpuPCSkipReasonWith(false, false, full),
		"missing required capabilities")
	assert.Contains(t, gpuPCSkipReasonWith(false, false, full), "CAP_BPF",
		"the message must name gpuprobe's own set, not the larger one the other scenarios need")
	assert.NotContains(t, gpuPCSkipReasonWith(false, false, full), "CAP_SYS_ADMIN",
		"checking for CAP_SYS_ADMIN here would skip on a correctly-capped machine")

	assert.Contains(t, gpuPCSkipReasonWith(true, false, full), "no NVIDIA GPU")

	assert.Contains(t, gpuPCSkipReasonWith(true, true,
		gpuPCConfig{ShimPath: missing, WorkloadPath: present}), "make -C shim nvidia")

	assert.Contains(t, gpuPCSkipReasonWith(true, true,
		gpuPCConfig{ShimPath: present, WorkloadPath: missing}), "make -C shim nvidia-concurrent")

	assert.Empty(t, gpuPCSkipReasonWith(true, true, full),
		"with caps, a GPU and both binaries present, nothing may skip — otherwise the "+
			"scenario would skip on the one machine it exists to run on")
}
