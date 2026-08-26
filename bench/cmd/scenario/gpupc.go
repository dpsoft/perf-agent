package main

// The "gpu-pc-overhead" scenario: the marginal cost of GPU PC sampling, and
// the pre-committed thresholds that turn it into a decision.
//
// This file is the harness for plan Task 12. It cannot be completed on a
// machine with no GPU: what it produces here is the instrument, and what it
// produces on an RTX 3090 is the number that decides whether Tier A ships.
//
// What is measured, and against what
// ----------------------------------
// The baseline arm is NOT "no injection". Spec §9.1 already measured
// injection at 0.0% and the activity path at −10.0%, and those costs are paid
// whether or not PC sampling is on. The question here is strictly the
// MARGINAL cost of PC sampling, so the baseline is the shipping Phase 4
// configuration -- shim injected, RUNTIME + RESOURCE callbacks,
// CONCURRENT_KERNEL activity, drain at 100 ms, consumer attached -- with PC
// sampling OFF. An uninjected run is taken too, before the arms, but it is
// labelled "calibration" and is never used as the baseline.
//
// Why a concurrent workload and not the 393k launches/s ceiling
// -------------------------------------------------------------
// §9.1's ceiling is the wrong instrument for this question. It exaggerates
// per-launch costs and UNDERSTATES serialization costs, because serialization
// hurts in proportion to the concurrency it destroys and a stream of trivial
// kernels has almost none. shim/nvidia/testdata/cuda_concurrent.cu exists for
// this measurement: several streams, non-trivial kernel durations, genuine
// overlap. See its header for why it is a second workload rather than a
// change to cuda_workload.cu.
//
// The concurrency is not assumed. It is measured out of the profile
// (sum of exec durations over the span they cover) and the run FAILS if the
// baseline arm's value is near 1 -- a workload that turned out to be serial
// would produce a small, green, meaningless Tier A cost, which is the same
// defect as a counter reading green when things are worst.
//
// Why every arm has to prove which mode it ran in
// -----------------------------------------------
// The benchmark-shaped instance of this project's standing defect is an arm
// that did not actually enable the tier it claims. Such an arm reports a
// wonderfully small overhead. So every arm asserts, from BOTH ends
// independently -- the producer's own report line on stderr and the
// consumer's counters -- that it ran in the mode it says, and a mismatch
// fails the whole run rather than contributing a number.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"kernel.org/pub/linux/libs/security/libcap/cap"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/gpuprobe"
)

// ---------------------------------------------------------------------------
// The pre-committed thresholds and the verdicts they produce.
//
// These numbers are from the plan and were fixed before any data existed. A
// threshold decided after seeing the data is not a threshold, which is why
// they are constants here and are echoed into the output JSON: a reader can
// see they were not adjusted to fit.
// ---------------------------------------------------------------------------

const (
	// gpuPCMaxWallPercent: "> 5% wall-clock on a realistic workload".
	gpuPCMaxWallPercent = 5.0
	// gpuPCMaxCostOverDuty: "cost ÷ duty ≤ 2".
	gpuPCMaxCostOverDuty = 2.0
)

// The verdicts, as stable identifiers rather than prose, so the outcome is a
// decision a script can read and not a paragraph a reader can reinterpret.
const (
	verdictTierBAlwaysOnCandidate = "TIER_B_ALWAYS_ON_CANDIDATE"
	verdictTierBExplicitOnly      = "TIER_B_NOT_ALWAYS_ON"

	verdictTierAOptIn        = "TIER_A_SHIPS_OPT_IN"
	verdictTierASmallerDuty  = "TIER_A_SHIPS_AT_A_SMALLER_DUTY"
	verdictTierADeepDiveOnly = "TIER_A_DEEP_DIVE_ONLY"
	verdictTierAUnshippable  = "TIER_A_UNSHIPPABLE"
	// verdictTierAIndeterminate is reachable only from a shape the plan's
	// four clauses do not cover -- for instance a cost that does not
	// increase with duty, which would mean the measurement itself is
	// unsound. It is a named outcome rather than a silent fallthrough to
	// the friendliest verdict, for the same reason gpu_serialized has an
	// "unknown" that must never degrade to "false".
	verdictTierAIndeterminate = "TIER_A_INDETERMINATE"
)

// The threshold clause identifiers, in the plan's own order.
const (
	clauseTierBOverBudget     = "tier-b-cost-over-5pct"
	clauseTierAHeadlineWithin = "tier-a-10pct-duty-within-5pct-and-ratio-within-2"
	clauseTierARatioOverAtAll = "tier-a-cost-over-duty-above-2-at-every-duty"
	// The plan spells its fourth clause "Tier A at 2.5% duty > 5%
	// wall-clock" because 2.5% was the lowest duty its arm table tested,
	// and the reason it gives is that "duty-cycling has no remaining
	// lever". The lever the clause names is the lowest duty on the table.
	// With a 1% arm on the table that is no longer 2.5%, so the clause is
	// evaluated at the lowest duty tested and the identifier says so
	// rather than naming a duty that is not the one being tested.
	//
	// This makes the clause STRICTLY WEAKER than its literal 2.5%
	// wording: a table where the 2.5% arm is over the bar and the 1% arm
	// is not now escapes the unshippable verdict, where before it could
	// not. That is precisely what makes TIER_A_DEEP_DIVE_ONLY reachable,
	// and it is exactly the sort of loosening that must not happen
	// quietly — so decideGPUPC prints a NOTE naming the 2.5% arm's cost
	// whenever the plan's literal wording would have fired and the
	// evaluated clause did not.
	clauseTierASmallestOverBud = "tier-a-lowest-duty-cost-over-5pct"
)

// gpuPCPlanNamedLowestDuty is the duty the plan's fourth clause names in
// prose. It is not a threshold and nothing is decided from it; it exists so
// the harness can tell the reader when the clause's evaluation point has moved
// below the duty the plan wrote down.
const gpuPCPlanNamedLowestDuty = 0.025

// gpuPCArmSpec is one arm's configuration. The four Tier A arms differ only
// in their gap, which is what makes the duty the single moving variable.
type gpuPCArmSpec struct {
	Name    string
	Tier    gpu.PCSamplingTier
	BurstMs int
	GapMs   int
}

// gpuPCArms is the arm table, in the order it runs them: baseline first
// (everything else is measured against it), then Tier B, then Tier A at
// decreasing duty.
//
// The Tier A gaps are not free parameters. The adapter derives its minimum gap
// from the duty ceiling -- min_gap = burst * (1/max_duty - 1) -- so 450 / 950
// / 1950 / 4950 ms after a 50 ms burst ARE 10% / 5% / 2.5% / 1%, and setting
// the maximum gap to the same value pins the burst controller's closed loop to
// exactly that gap instead of letting it tune. See gpuPCArmEnv.
//
// WHY THERE IS A 1% ARM, which the plan's original table did not have.
//
// "cost / duty > 2" is the same statement as "cost% > 200 x duty". At 2.5%
// duty that is exactly "cost% > 5%", which is the wall-clock bar. So on the
// plan's original three duties the every-duty ratio clause STRICTLY IMPLIES
// the lowest-duty wall clause, the harsher verdict outranks it, and
// TIER_A_DEEP_DIVE_ONLY could not be reached whatever the numbers were. The
// two clauses separate only BELOW 2.5% duty.
//
// That mattered because the two verdicts mean materially different things.
// "The tier does not ship" and "the tier works, but only as a deliberate
// deep-dive tool" are different answers, and a harness that can only ever
// produce the first is not applying four clauses, it is applying three.
//
// At 1% duty "ratio > 2" means "cost > 2%", comfortably inside the 5% bar, so
// a tier that is consistently inefficient but genuinely cheap at low duty
// lands in deep-dive instead of being killed. The thresholds are untouched;
// only the input space is now wide enough for all four clauses to be told
// apart. The cost is run length -- see gpuPCMinFixedWorkSec.
var gpuPCArms = []gpuPCArmSpec{
	{Name: "baseline (pc sampling off)", Tier: gpu.PCSamplingOff},
	{Name: "tier B continuous", Tier: gpu.PCSamplingContinuous},
	{Name: "tier A 50ms/450ms (10% duty)", Tier: gpu.PCSamplingSerialized, BurstMs: 50, GapMs: 450},
	{Name: "tier A 50ms/950ms (5% duty)", Tier: gpu.PCSamplingSerialized, BurstMs: 50, GapMs: 950},
	{Name: "tier A 50ms/1950ms (2.5% duty)", Tier: gpu.PCSamplingSerialized, BurstMs: 50, GapMs: 1950},
	{Name: "tier A 50ms/4950ms (1% duty)", Tier: gpu.PCSamplingSerialized, BurstMs: 50, GapMs: 4950},
}

// gpuPCLongestTierACycleMs is the longest burst+gap cycle in the arm table:
// the 1%-duty arm's 5 s. Derived from the table rather than written down, so
// adding or removing an arm cannot leave it stale.
func gpuPCLongestTierACycleMs() int {
	longest := 0
	for _, a := range gpuPCArms {
		if a.Tier != gpu.PCSamplingSerialized {
			continue
		}
		if c := a.BurstMs + a.GapMs; c > longest {
			longest = c
		}
	}
	return longest
}

// lowestConfiguredDuty is the smallest duty the arm table asks for, which is
// the arm the fixed-work floor and the plan's fourth clause both key on.
func lowestConfiguredDuty() float64 {
	lowest := 0.0
	for _, a := range gpuPCArms {
		if d := a.dutyConfigured(); d > 0 && (lowest == 0 || d < lowest) {
			lowest = d
		}
	}
	return lowest
}

// gpuPCMinFixedWorkSec is the shortest fixed work that lets EVERY Tier A arm
// clear its own "bursts >= MinBursts" floor. It is what the 1% arm costs: not
// five more runs, but a longer run.
//
// One burst opens per burst+gap cycle, so an arm whose cycle is C seconds
// opens roughly T/C bursts over T seconds of work. At 2.5% duty C is 2 s and
// the default floor of four bursts needs 8 s; at 1% duty C is 5 s and the same
// four bursts need 20 s. The extra cycle of slack is not a fudge factor: the
// count is of COMPLETED cycles, the first burst does not open at t=0, and a
// floor met only exactly would turn ordinary jitter into a failed run of the
// entire benchmark after twenty minutes of GPU time.
//
// Two things make this conservative in the right direction rather than the
// flattering one. It is checked against the UNINJECTED calibration run, which
// is the fastest run the benchmark takes -- every arm is slower and therefore
// opens MORE bursts. And it is checked against the workload's fixed-work
// elapsed_ms, while the adapter's burst timer runs for the whole process
// including CUDA init, warm-up and teardown.
func gpuPCMinFixedWorkSec(minBursts uint64) float64 {
	return float64(minBursts+1) * float64(gpuPCLongestTierACycleMs()) / 1000
}

// dutyConfigured is burst/(burst+gap): the fraction of wall-clock the arm asks
// to spend with kernels serialized. Zero for the two non-Tier-A arms.
func (a gpuPCArmSpec) dutyConfigured() float64 {
	if a.Tier != gpu.PCSamplingSerialized || a.BurstMs+a.GapMs == 0 {
		return 0
	}
	return float64(a.BurstMs) / float64(a.BurstMs+a.GapMs)
}

// gpuPCConfig is everything the scenario takes from the command line.
type gpuPCConfig struct {
	ShimPath     string
	WorkloadPath string
	Runs         int

	Iters     int
	Warmup    int
	Streams   int
	Rounds    int
	Blocks    int
	Threads   int
	SyncEvery int

	// MinConcurrency and MinKernelUs are the two guards that keep this
	// from silently measuring a microbenchmark. Both are asserted against
	// the BASELINE arm, which is the only arm whose concurrency is
	// supposed to be undisturbed.
	MinConcurrency float64
	MinKernelUs    float64
	// MinBursts is how many bursts the lowest-duty Tier A arm must have
	// opened for its duty to mean anything. At 2.5% the cycle is 2 s, so
	// this is also the real floor on how long the fixed work must take.
	MinBursts uint64
	// MinCalibrationSec / MaxCalibrationSec bound the uninjected run, so
	// a workload sized far too small or far too large announces itself
	// with the retuning advice instead of producing noise.
	MinCalibrationSec float64
	MaxCalibrationSec float64
}

// ---------------------------------------------------------------------------
// The skip path: the only thing this scenario can prove without hardware.
// ---------------------------------------------------------------------------

// gpuPCSkipReason returns the reason this scenario cannot run here, or "" if
// it can. Every reason is a SKIP and not a failure: a machine with no GPU is
// not a broken machine, and a benchmark that failed there would be turned off
// in CI and would then never run anywhere.
//
// The capability set checked is gpuprobe's own -- CAP_BPF, CAP_PERFMON,
// CAP_CHECKPOINT_RESTORE -- and deliberately NOT the larger set
// bench/cmd/scenario's other scenarios need. Checking for CAP_SYS_ADMIN here
// would skip on a correctly-capped machine, and a skip for the wrong reason is
// indistinguishable from a skip for the right one.
func gpuPCSkipReason(cfg gpuPCConfig) string {
	return gpuPCSkipReasonWith(hasGPUCaps(), hasNVIDIADevice(), cfg)
}

// gpuPCSkipReasonWith is gpuPCSkipReason with the two environment probes
// injected, so all four skip branches are reachable from a unit test on a
// machine that can only ever produce the first. A skip path that cannot be
// exercised is a skip path nobody has read.
func gpuPCSkipReasonWith(caps, gpuPresent bool, cfg gpuPCConfig) string {
	if !caps {
		return "missing required capabilities (CAP_BPF, CAP_PERFMON, CAP_CHECKPOINT_RESTORE); " +
			"run: sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario " +
			"(not from /tmp: it is nosuid and file caps do not survive exec)"
	}
	if !gpuPresent {
		return "no NVIDIA GPU on this machine (/dev/nvidiactl absent); " +
			"this scenario measures CUPTI PC sampling and has nothing to measure without one"
	}
	if _, err := os.Stat(cfg.ShimPath); err != nil {
		return fmt.Sprintf("CUPTI adapter not built at %s (%v); build it with: make -C shim nvidia",
			cfg.ShimPath, err)
	}
	if _, err := os.Stat(cfg.WorkloadPath); err != nil {
		return fmt.Sprintf("concurrent CUDA workload not built at %s (%v); "+
			"build it with: make -C shim nvidia-concurrent", cfg.WorkloadPath, err)
	}
	return ""
}

// hasGPUCaps mirrors gpuprobe/gate_test.go's hasCaps: Permitted as well as
// Effective, because a setcap'd binary has not promoted Permitted yet, and
// never a bare Geteuid check.
func hasGPUCaps() bool {
	if os.Geteuid() == 0 {
		return true
	}
	set := cap.GetProc()
	if set == nil {
		return false
	}
	for _, w := range []cap.Value{cap.BPF, cap.PERFMON, cap.CHECKPOINT_RESTORE} {
		ok := false
		for _, flag := range []cap.Flag{cap.Permitted, cap.Effective} {
			if have, err := set.GetFlag(flag, w); err == nil && have {
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

// hasNVIDIADevice is the cheapest honest test for "a CUDA context can be
// created here". It does not shell out to nvidia-smi: this runs on a machine
// where the answer is no, and a missing binary and a missing GPU must not be
// reported as the same thing.
func hasNVIDIADevice() bool {
	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		return true
	}
	if ents, err := os.ReadDir("/proc/driver/nvidia/gpus"); err == nil && len(ents) > 0 {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Running the arms.
// ---------------------------------------------------------------------------

// runGPUPCOverhead runs the calibration pass and then the six arms,
// interleaved, five runs each, and fills doc.GPUPC. It returns false when any
// assertion failed; main exits non-zero on that, because a benchmark that
// could not prove what it measured must not look like one that did.
func runGPUPCOverhead(doc *schema.Document, cfg gpuPCConfig, out io.Writer) bool {
	kernels := cfg.Iters * cfg.Streams
	res := &schema.GPUPCOverhead{
		Workload: schema.GPUPCWorkload{
			Path: cfg.WorkloadPath, Iters: cfg.Iters, Warmup: cfg.Warmup,
			Streams: cfg.Streams, Rounds: cfg.Rounds, Blocks: cfg.Blocks,
			Threads: cfg.Threads, SyncEvery: cfg.SyncEvery, KernelsRun: kernels,
		},
	}
	doc.GPUPC = res
	doc.Config.Runs = cfg.Runs

	// How long the fixed work must take for the LOWEST-duty arm to clear
	// its own bursts floor. Derived from the arm table, announced before
	// anything runs, and checked before the twenty minutes of GPU time
	// rather than after them.
	minSec := cfg.MinCalibrationSec
	derived := gpuPCMinFixedWorkSec(cfg.MinBursts)
	if derived > minSec {
		minSec = derived
	}
	_, _ = fmt.Fprintf(out, "gpu-pc-overhead: %d arms x %d interleaved runs + 1 calibration = "+
		"%d fixed-work runs; the fixed work must take %.0f-%.0f s (the %.0f s floor is "+
		"derived: the lowest-duty arm's %.1f s cycle x %d bursts + one cycle of slack)\n",
		len(gpuPCArms), cfg.Runs, len(gpuPCArms)*cfg.Runs+1, minSec, cfg.MaxCalibrationSec,
		derived, float64(gpuPCLongestTierACycleMs())/1000, cfg.MinBursts)
	if minSec > cfg.MaxCalibrationSec {
		_, _ = fmt.Fprintf(out, "gpu-pc-overhead: FAILED: the lowest-duty arm needs at least "+
			"%.0f s of fixed work to open %d bursts, but --gpu-max-calibration-sec is "+
			"%.0f s. No sizing can satisfy both, so this configuration could only ever "+
			"produce a Tier A arm whose duty means nothing. Raise "+
			"--gpu-max-calibration-sec, or lower --gpu-min-bursts and accept a coarser "+
			"duty measurement.\n", derived, cfg.MinBursts, cfg.MaxCalibrationSec)
		return false
	}

	// The calibration pass: the same fixed work with NO adapter injected.
	// It warms the device clocks before the first arm and it proves the
	// workload is sized sanely. It is not an arm and is never the
	// baseline -- see this file's header.
	calRun, err := runConcurrentWorkload(cfg, nil)
	if err != nil {
		_, _ = fmt.Fprintf(out, "gpu-pc-overhead: calibration run failed: %v\n", err)
		return false
	}
	calRun.RunN = 0
	res.Calibration = schema.GPUPCArm{
		Name: "calibration (uninjected; NOT the baseline)", Tier: "none",
		Runs: []schema.GPUPCRun{calRun}, MedianWallMs: calRun.WallMs,
		MedianKernelsPerS: calRun.KernelsPerS,
	}
	_, _ = fmt.Fprintf(out, "gpu-pc-overhead: calibration (uninjected, not an arm): "+
		"%.1f ms for %d kernels, %.0f kernels/s\n", calRun.WallMs, kernels, calRun.KernelsPerS)
	if sec := calRun.WallMs / 1000; sec < minSec || sec > cfg.MaxCalibrationSec {
		want := minSec * 1.2
		_, _ = fmt.Fprintf(out, "gpu-pc-overhead: FAILED: the fixed work takes %.1f s uninjected, "+
			"outside the sane window [%.0f s, %.0f s]. Retune with --gpu-iters (how many "+
			"kernels) or --gpu-rounds (how long each one takes), then re-run. Too short "+
			"and the %.0f%%-duty arm cannot open %d bursts for its duty to mean anything; "+
			"too long and the %d interleaved runs take longer than the operator will "+
			"wait. At this sizing, --gpu-iters %d would land near %.0f s.\n",
			sec, minSec, cfg.MaxCalibrationSec,
			lowestConfiguredDuty()*100, cfg.MinBursts, len(gpuPCArms)*cfg.Runs+1,
			int(float64(cfg.Iters)*want/max(sec, 0.001)), want)
		return false
	}

	arms := make([]schema.GPUPCArm, len(gpuPCArms))
	for i, spec := range gpuPCArms {
		arms[i] = schema.GPUPCArm{
			Name: spec.Name, Tier: spec.Tier.String(),
			BurstMs: spec.BurstMs, GapMs: spec.GapMs,
			DutyConfigured: spec.dutyConfigured(),
		}
	}

	// INTERLEAVED, per §9.1's method: every arm once per round, rather
	// than five of one arm then five of the next. Thermal drift, clock
	// boost state and any other slow drift then fall on all six arms
	// alike instead of on whichever ran last.
	ok := true
	for run := 1; run <= cfg.Runs; run++ {
		for i, spec := range gpuPCArms {
			r, err := measureGPUPCArm(cfg, spec)
			if err != nil {
				_, _ = fmt.Fprintf(out, "gpu-pc-overhead: FAILED: arm %q run %d: %v\n", spec.Name, run, err)
				return false
			}
			r.RunN = run
			if err := assertArmRanInItsMode(cfg, spec, r); err != nil {
				_, _ = fmt.Fprintf(out, "gpu-pc-overhead: FAILED: arm %q run %d did not prove it "+
					"ran in the mode it claims: %v\n", spec.Name, run, err)
				_, _ = fmt.Fprintf(out, "  evidence: %+v\n", r.Evidence)
				ok = false
			}
			arms[i].Runs = append(arms[i].Runs, r)
			_, _ = fmt.Fprintf(out, "gpu-pc-overhead: run %d/%d %-32s %8.1f ms  %7.0f kern/s  "+
				"conc %.2f  kern %.0f us\n", run, cfg.Runs, spec.Name, r.WallMs,
				r.KernelsPerS, r.Concurrency, r.MeanKernelUs)
		}
		// Fail after a COMPLETE round rather than at the first bad arm:
		// one round gives every arm's evidence side by side, which is
		// the useful diagnostic, and four more rounds of a
		// known-unsound configuration is ten minutes of GPU time that
		// tells nobody anything.
		if !ok {
			break
		}
	}

	// Recorded whether or not the assertions held. The arms are the
	// evidence of what went wrong; what is withheld on failure is the
	// DECISION, which stays at its zero value so nothing in the file can
	// be read as a verdict.
	summarizeGPUPCArms(arms)
	res.Arms = arms
	if !ok {
		return false
	}

	if err := assertBaselineIsRealistic(cfg, arms[0]); err != nil {
		_, _ = fmt.Fprintf(out, "gpu-pc-overhead: FAILED: %v\n", err)
		return false
	}
	if err := assertDutyKnobDidSomething(arms); err != nil {
		_, _ = fmt.Fprintf(out, "gpu-pc-overhead: FAILED: %v\n", err)
		return false
	}
	res.Decision = decideGPUPC(arms, gpuPCMaxWallPercent, gpuPCMaxCostOverDuty)

	renderGPUPCTable(out, res)
	for _, line := range res.Decision.Lines {
		_, _ = fmt.Fprintln(out, line)
	}
	return true
}

// measureGPUPCArm attaches a consumer configured for the arm's tier, runs the
// fixed work under the adapter with the arm's environment, and collects both
// the timing and the evidence.
//
// A fresh attach per arm-run is deliberate. The tier is a property of the
// Timeline as well as of the producer, so one long-lived consumer could not
// carry five tiers; and per-run counters that start at zero are what make the
// evidence assertions exact rather than differential.
func measureGPUPCArm(cfg gpuPCConfig, spec gpuPCArmSpec) (schema.GPUPCRun, error) {
	timeline := gpu.NewTimeline(gpu.TimelineConfig{PCSampling: spec.Tier})

	// No symbolizer, on purpose, and identically on every arm. Resolving a
	// sampled launch's stack is agent-side work that costs the WORKLOAD
	// nothing, and it cannot succeed anyway once the workload has exited
	// (its /proc/<pid>/maps is gone). Leaving it out removes a variance
	// source without removing any cost the arms differ in: the producer
	// still samples launches and still captures stacks in BPF, on every
	// arm, exactly as the shipping configuration does.
	c, err := gpuprobe.Attach(gpuprobe.Config{
		ShimPath: cfg.ShimPath,
		PID:      0, // the process that will map the adapter does not exist yet
		Backend:  gpu.BackendCUPTI,
		Sink:     timeline,
	})
	if err != nil {
		return schema.GPUPCRun{}, fmt.Errorf("attach: %w", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		if err := c.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			runErr = err
		}
	}()

	run, err := runConcurrentWorkload(cfg, gpuPCArmEnv(cfg, spec))
	if err != nil {
		cancel()
		<-done
		return schema.GPUPCRun{}, err
	}

	// The adapter's atexit handler has already run cuptiActivityFlushAll
	// and flushed both batches, so everything is in the ringbuf; this is
	// for the consumer goroutine to drain the tail.
	time.Sleep(500 * time.Millisecond)
	cancel()
	<-done
	if runErr != nil {
		return schema.GPUPCRun{}, fmt.Errorf("consumer: %w", runErr)
	}
	c.Flush()

	snap := timeline.Snapshot()
	st := c.Stats()
	run.Concurrency, run.MeanKernelUs = concurrencyOf(snap)
	run.Evidence.PCSamplesDecoded = st.PCSamplesDecoded
	run.Evidence.SamplingWindowsDecoded = st.SamplingWindowsDecoded
	run.Evidence.SamplingWindowsReceived = snap.SamplingWindowsReceived
	run.Evidence.ExecutionsSeen = len(snap.Executions)
	run.Evidence.ExecutionsSerialized = snap.ExecutionsSerialized
	run.Evidence.ExecutionsNotSerialized = snap.ExecutionsNotSerialized
	run.Evidence.ExecutionsUnknown = snap.ExecutionsSerializationUnknown
	run.Evidence.SnapshotTier = snap.PCSampling.String()
	return run, nil
}

// gpuPCArmEnv builds the producer environment for an arm.
//
// The Tier A gap is pinned rather than tuned, and that is the whole reason
// both duty knobs are set. The burst controller clamps its gap into
// [min_gap, max_gap] where min_gap = burst * (1/max_duty - 1). Setting
// max_duty so that min_gap is the arm's gap AND setting max_gap to the same
// value collapses the interval to a point: whatever the closed loop computes,
// the gap it returns is the arm's gap. Without the second knob a workload
// producing a high pair rate would have the loop lengthen the gap, the arm
// would run at a duty nobody asked for, and the cost-over-duty ratio would be
// computed against a denominator that never happened.
func gpuPCArmEnv(cfg gpuPCConfig, spec gpuPCArmSpec) []string {
	env := []string{
		"CUDA_INJECTION64_PATH=" + cfg.ShimPath,
		"PERFAGENT_GPU_LOG=stderr",
		// Set EXPLICITLY on every arm including the off one, never left
		// to be inherited: an exported PERFAGENT_GPU_PC_SAMPLING in the
		// operator's shell must not turn the baseline arm into a
		// serializing one, which would make every other arm look free.
		gpu.PCSamplingEnvVar + "=" + spec.Tier.EnvValue(),
	}
	if spec.Tier != gpu.PCSamplingSerialized {
		return env
	}
	// burst * (1/duty - 1) == gap  =>  duty == burst / (burst + gap).
	permille := int(math.Round(1000 * float64(spec.BurstMs) / float64(spec.BurstMs+spec.GapMs)))
	return append(env,
		fmt.Sprintf("PERFAGENT_GPU_PC_BURST_MS=%d", spec.BurstMs),
		fmt.Sprintf("PERFAGENT_GPU_PC_MAX_DUTY_PERMILLE=%d", permille),
		fmt.Sprintf("PERFAGENT_GPU_PC_MAX_GAP_MS=%d", spec.GapMs),
	)
}

// runConcurrentWorkload runs one fixed-work pass. extraEnv nil means an
// UNINJECTED run (the calibration pass): no CUDA_INJECTION64_PATH, so the
// adapter is not loaded at all.
func runConcurrentWorkload(cfg gpuPCConfig, extraEnv []string) (schema.GPUPCRun, error) {
	cmd := exec.Command(cfg.WorkloadPath,
		fmt.Sprintf("--iters=%d", cfg.Iters),
		fmt.Sprintf("--warmup=%d", cfg.Warmup),
		fmt.Sprintf("--streams=%d", cfg.Streams),
		fmt.Sprintf("--rounds=%d", cfg.Rounds),
		fmt.Sprintf("--blocks=%d", cfg.Blocks),
		fmt.Sprintf("--threads=%d", cfg.Threads),
		fmt.Sprintf("--sync-every=%d", cfg.SyncEvery),
		"--linger-ms=0",
	)
	// os.Environ() carries whatever the operator exported; every variable
	// this scenario cares about is appended AFTER it, and os/exec keeps
	// the last occurrence of a duplicate key.
	cmd.Env = append(os.Environ(), extraEnv...)
	if extraEnv == nil {
		// The calibration pass must be genuinely uninjected even if the
		// operator has CUDA_INJECTION64_PATH exported.
		cmd.Env = append(cmd.Env, "CUDA_INJECTION64_PATH=")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	t0 := time.Now()
	err := cmd.Run()
	processMs := float64(time.Since(t0).Microseconds()) / 1000.0
	if err != nil {
		return schema.GPUPCRun{}, fmt.Errorf("workload: %w\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	run, err := parseConcurrentLine(stdout.String())
	if err != nil {
		return schema.GPUPCRun{}, fmt.Errorf("%w\nstdout: %s", err, stdout.String())
	}
	run.ProcessMs = processMs
	// Exactly zero, not "small": every kernel is the identity on its
	// buffer. A non-zero value means this arm perturbed the computation
	// and its timing must not be used.
	if run.MaxAbsErr != 0 {
		return schema.GPUPCRun{}, fmt.Errorf(
			"workload reported max_abs_err=%g, want exactly 0: the computation was "+
				"corrupted, so this run's timing means nothing", run.MaxAbsErr)
	}
	if extraEnv != nil {
		run.Evidence = parseAdapterReport(stderr.String())
	}
	return run, nil
}

// parseConcurrentLine reads cuda_concurrent's single result line.
func parseConcurrentLine(stdout string) (schema.GPUPCRun, error) {
	var line string
	sc := bufio.NewScanner(strings.NewReader(stdout))
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "concurrent: iters=") {
			line = sc.Text()
		}
	}
	if line == "" {
		return schema.GPUPCRun{}, errors.New("workload printed no \"concurrent: iters=...\" result line")
	}
	kv := newKeyValues(line)
	elapsed, ok := kv.float("elapsed_ms")
	if !ok {
		return schema.GPUPCRun{}, fmt.Errorf("no elapsed_ms in %q", line)
	}
	kps, _ := kv.float("kernels_per_s")
	errv, ok := kv.float("max_abs_err")
	if !ok {
		return schema.GPUPCRun{}, fmt.Errorf("no max_abs_err in %q", line)
	}
	return schema.GPUPCRun{WallMs: elapsed, KernelsPerS: kps, MaxAbsErr: errv}, nil
}

// parseAdapterReport reads the producer's own account of the run off stderr.
//
// This is the producer half of the evidence and it is deliberately NOT
// derived from the consumer's counters: the two ends can disagree, and the gap
// between them is the loss. An arm proved only by the consumer would look
// identical whether the producer ran in the right mode and lost records, or
// ran in the wrong mode and had nothing to send.
func parseAdapterReport(stderr string) schema.GPUPCEvidence {
	var ev schema.GPUPCEvidence
	sc := bufio.NewScanner(strings.NewReader(stderr))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		// "perfagent-cupti: pc exit tier=serialized ... pc_records=N ..."
		case strings.Contains(line, "perfagent-cupti: pc ") && strings.Contains(line, " tier=") &&
			strings.Contains(line, " pc_records="):
			kv := newKeyValues(line)
			if s, ok := kv.str("tier"); ok {
				ev.ProducerTier = s
			}
			ev.ProducerPCRecords, _ = kv.uint("pc_records")
			ev.ProducerGraphExecs, _ = kv.uint("graph_execs")
		// "perfagent-cupti: pc_sampling=off tier_refused=N ..."
		case strings.Contains(line, "perfagent-cupti: pc_sampling=off"):
			ev.ProducerTier = gpu.PCSamplingNameOff
		// "perfagent-cupti: tier A bursts=N burst_ns=N duty=F ..."
		case strings.Contains(line, "perfagent-cupti: tier A bursts="):
			kv := newKeyValues(line)
			ev.ProducerBursts, _ = kv.uint("bursts")
			ev.ProducerWindows, _ = kv.uint("windows")
			ev.ProducerDuty, _ = kv.float("duty")
			ev.ProducerStartFailed, _ = kv.uint("start_failed")
			ev.ProducerStopFailed, _ = kv.uint("stop_failed")
			ev.ProducerGraphRefuse, _ = kv.uint("graph_refused")
		}
	}
	return ev
}

// keyValues splits a log line into its key=value tokens. Later occurrences of
// a key win, which matches the log's own "the last word is the final state"
// shape.
type keyValues map[string]string

func (kv keyValues) str(k string) (string, bool) { v, ok := kv[k]; return v, ok }

func (kv keyValues) float(k string) (float64, bool) {
	s, ok := kv[k]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

func (kv keyValues) uint(k string) (uint64, bool) {
	s, ok := kv[k]
	if !ok {
		return 0, false
	}
	u, err := strconv.ParseUint(s, 10, 64)
	return u, err == nil
}

func newKeyValues(line string) keyValues {
	kv := keyValues{}
	for _, tok := range strings.Fields(line) {
		if i := strings.IndexByte(tok, '='); i > 0 {
			kv[tok[:i]] = tok[i+1:]
		}
	}
	return kv
}

// concurrencyOf turns a snapshot into the two properties that decide whether
// this workload is the realistic one the plan requires:
//
//	concurrency  = sum(exec duration) / (max end - min start)
//	meanKernelUs = sum(exec duration) / count
//
// Both are computed over the executions the profile RETAINED, which may be a
// suffix of the run if the ring evicted. That is fine and is why the span is
// taken from the retained set rather than from the workload's own clock: both
// numerator and denominator then describe the same interval.
func concurrencyOf(snap gpu.Snapshot) (concurrency, meanKernelUs float64) {
	var busy uint64
	var lo, hi uint64
	n := 0
	for i := range snap.Executions {
		e := snap.Executions[i].Exec
		if e.EndNs <= e.StartNs {
			continue // a zero or inverted interval carries no duration
		}
		busy += e.EndNs - e.StartNs
		if n == 0 || e.StartNs < lo {
			lo = e.StartNs
		}
		if e.EndNs > hi {
			hi = e.EndNs
		}
		n++
	}
	if n == 0 {
		return 0, 0
	}
	meanKernelUs = float64(busy) / float64(n) / 1000.0
	if hi > lo {
		concurrency = float64(busy) / float64(hi-lo)
	}
	return concurrency, meanKernelUs
}

// ---------------------------------------------------------------------------
// The assertions. Each arm proves it ran in the mode it says it ran in.
// ---------------------------------------------------------------------------

// assertArmRanInItsMode is the answer to "a benchmark that silently measures
// the wrong thing". Every clause below can go red, and the red ones are the
// ones that would otherwise report a flatteringly small overhead.
func assertArmRanInItsMode(cfg gpuPCConfig, spec gpuPCArmSpec, r schema.GPUPCRun) error {
	var problems []string
	fail := func(format string, a ...any) { problems = append(problems, fmt.Sprintf(format, a...)) }

	// Non-vacuity FIRST, and on every arm. An arm where the pipeline
	// never ran at all satisfies every negative assertion below
	// perfectly, and would be the cheapest arm in the table.
	if r.Evidence.ExecutionsSeen == 0 {
		fail("no GPU executions reached the timeline: the adapter was not injected, " +
			"the probes did not attach, or the workload ran no kernels — this arm " +
			"measured nothing")
	}
	if r.Evidence.ProducerTier == "" {
		fail("the adapter printed no report line on stderr: PERFAGENT_GPU_LOG did not " +
			"take, or the adapter was never loaded into the workload")
	}

	switch spec.Tier {
	case gpu.PCSamplingOff:
		if r.Evidence.ProducerTier != gpu.PCSamplingNameOff {
			fail("producer reports tier %q, want %q", r.Evidence.ProducerTier, gpu.PCSamplingNameOff)
		}
		// Off means off. If any of these moved, the baseline is itself
		// paying a PC-sampling cost and every other arm's margin is
		// understated by exactly that amount.
		if r.Evidence.PCSamplesDecoded != 0 {
			fail("%d PC samples decoded with the tier off", r.Evidence.PCSamplesDecoded)
		}
		if r.Evidence.SamplingWindowsDecoded != 0 || r.Evidence.SamplingWindowsReceived != 0 {
			fail("%d sampling windows decoded / %d received with the tier off",
				r.Evidence.SamplingWindowsDecoded, r.Evidence.SamplingWindowsReceived)
		}
		if r.Evidence.ProducerBursts != 0 {
			fail("producer opened %d bursts with the tier off", r.Evidence.ProducerBursts)
		}
		if r.Evidence.ExecutionsSerialized != 0 {
			fail("%d executions marked serialized with the tier off", r.Evidence.ExecutionsSerialized)
		}

	case gpu.PCSamplingContinuous:
		if r.Evidence.ProducerTier != gpu.PCSamplingNameContinuous {
			fail("producer reports tier %q, want %q", r.Evidence.ProducerTier, gpu.PCSamplingNameContinuous)
		}
		if r.Evidence.ProducerPCRecords == 0 {
			fail("producer drained 0 PC records: Tier B was selected but never collected")
		}
		if r.Evidence.PCSamplesDecoded == 0 {
			fail("0 PC samples reached the consumer: this arm is baseline with a different name")
		}
		// A CONTINUOUS producer announcing a window would be claiming a
		// perturbation it did not cause.
		if r.Evidence.SamplingWindowsDecoded != 0 || r.Evidence.ProducerBursts != 0 {
			fail("Tier B emitted %d windows / opened %d bursts; it must do neither",
				r.Evidence.SamplingWindowsDecoded, r.Evidence.ProducerBursts)
		}

	case gpu.PCSamplingSerialized:
		if r.Evidence.ProducerTier != gpu.PCSamplingNameSerialized {
			fail("producer reports tier %q, want %q", r.Evidence.ProducerTier, gpu.PCSamplingNameSerialized)
		}
		if r.Evidence.ProducerBursts < cfg.MinBursts {
			fail("producer opened %d bursts, want at least %d: too few for the duty "+
				"fraction to mean anything over this run",
				r.Evidence.ProducerBursts, cfg.MinBursts)
		}
		// One open record and one closed record per burst, minus the
		// close of a burst still open when the process exited.
		if w, b := r.Evidence.ProducerWindows, r.Evidence.ProducerBursts; w != 2*b && w != 2*b-1 {
			fail("producer emitted %d windows for %d bursts, want 2N or 2N-1", w, b)
		}
		if r.Evidence.ProducerStartFailed != 0 || r.Evidence.ProducerStopFailed != 0 {
			fail("cuptiPCSamplingStart failed %d times, Stop %d times: some bursts did "+
				"not happen and the achieved duty is not the configured one",
				r.Evidence.ProducerStartFailed, r.Evidence.ProducerStopFailed)
		}
		if r.Evidence.ProducerGraphRefuse != 0 || r.Evidence.ProducerGraphExecs != 0 {
			fail("CUDA graph executions observed (%d) / Tier A refusals (%d): Tier A "+
				"stops bursting in such a process, so this arm did not run Tier A "+
				"for its whole length",
				r.Evidence.ProducerGraphExecs, r.Evidence.ProducerGraphRefuse)
		}
		if r.Evidence.PCSamplesDecoded == 0 {
			fail("0 PC samples reached the consumer despite %d bursts", r.Evidence.ProducerBursts)
		}
		if r.Evidence.SamplingWindowsReceived == 0 {
			fail("no sampling window reached the agent: nothing in the resulting profile " +
				"could say which executions ran perturbed")
		}
		// The load-bearing one. Bursts that overlapped no execution
		// serialized nothing, so an arm with zero serialized executions
		// measured the cost of starting and stopping CUPTI and not the
		// cost of serialization.
		if r.Evidence.ExecutionsSerialized == 0 {
			fail("not one execution was marked gpu_serialized=\"true\": the bursts did " +
				"not overlap any kernel, so this arm did not measure serialization")
		}
		if r.Evidence.SnapshotTier != gpu.PCSamplingNameSerialized {
			fail("the agent's own snapshot reports tier %q", r.Evidence.SnapshotTier)
		}
		// The achieved duty against what the arm asked for. The upper
		// bound is DERIVED, not a magic tolerance: the burst timer ticks
		// at burst/5 by default, so a burst runs 50..60 ms, and two
		// ticks of slack covers scheduling overshoot on a loaded
		// machine. Anything above that and the arm ran at a duty nobody
		// configured, which would make its ratio meaningless.
		want := spec.dutyConfigured()
		tick := float64(spec.BurstMs) / 5
		hi := (float64(spec.BurstMs) + 2*tick) / (float64(spec.BurstMs) + 2*tick + float64(spec.GapMs))
		if d := r.Evidence.ProducerDuty; d > hi || d < want/2 {
			fail("producer achieved duty %.4f, outside [%.4f, %.4f] for a configured %.4f",
				d, want/2, hi, want)
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// assertBaselineIsRealistic is the guard against measuring a microbenchmark.
//
// The plan rules the saturating launch-rate ceiling out as an instrument
// precisely because it understates serialization costs. A workload that turned
// out to be effectively serial, or whose kernels turned out to be
// microseconds long, would reproduce that error while looking like a proper
// measurement — so the two properties the plan requires are asserted rather
// than asserted-in-a-comment.
func assertBaselineIsRealistic(cfg gpuPCConfig, base schema.GPUPCArm) error {
	if base.MedianConcurrency < cfg.MinConcurrency {
		return fmt.Errorf(
			"the baseline arm's kernel concurrency is %.2f, below the %.2f floor: this "+
				"workload has almost no concurrency for serialization to destroy, so "+
				"every Tier A number it produces would be an underestimate. Raise "+
				"--gpu-streams, or lower --gpu-blocks so each kernel occupies less of "+
				"the device and more of them co-reside",
			base.MedianConcurrency, cfg.MinConcurrency)
	}
	if base.MedianKernelUs < cfg.MinKernelUs {
		return fmt.Errorf(
			"the baseline arm's mean kernel duration is %.1f us, below the %.1f us floor: "+
				"this is a launch-rate microbenchmark, which the plan rules out as the "+
				"instrument for this question. Raise --gpu-rounds",
			base.MedianKernelUs, cfg.MinKernelUs)
	}
	return nil
}

// assertDutyKnobDidSomething is the cross-arm proof that the Tier A arms are
// as many arms as they claim to be and not one arm under several names.
//
// Burst count over a fixed run length is inversely proportional to burst+gap,
// so 10% duty must open strictly more bursts than 5%, which must open strictly
// more than 2.5%, which must open strictly more than 1%. The check walks the
// whole table in descending duty, so the 1% arm is held to it on exactly the
// same terms as the others. If two came out equal the duty environment did not
// take, those "duties" are one duty, and a cost-over-duty ratio computed from
// them would be pure fiction.
func assertDutyKnobDidSomething(arms []schema.GPUPCArm) error {
	var a []schema.GPUPCArm
	for _, arm := range arms {
		if arm.Tier == gpu.PCSamplingNameSerialized {
			a = append(a, arm)
		}
	}
	if len(a) < 2 {
		return nil
	}
	sort.Slice(a, func(i, j int) bool { return a[i].DutyConfigured > a[j].DutyConfigured })
	for i := 1; i < len(a); i++ {
		hi := medianUint(burstCounts(a[i-1]))
		lo := medianUint(burstCounts(a[i]))
		if hi <= lo {
			return fmt.Errorf(
				"arm %q opened a median of %d bursts and the lower-duty arm %q opened %d: "+
					"a lower duty must open strictly fewer bursts over the same fixed "+
					"work. The duty environment did not take, so these Tier A arms "+
					"are one arm under different names and their cost-over-duty ratios "+
					"are meaningless",
				a[i-1].Name, hi, a[i].Name, lo)
		}
	}
	return nil
}

func burstCounts(arm schema.GPUPCArm) []uint64 {
	out := make([]uint64, 0, len(arm.Runs))
	for _, r := range arm.Runs {
		out = append(out, r.Evidence.ProducerBursts)
	}
	return out
}

// ---------------------------------------------------------------------------
// Summarizing and deciding. Everything below is pure and unit-tested.
// ---------------------------------------------------------------------------

// summarizeGPUPCArms fills in each arm's medians and, from the baseline's,
// each arm's cost and cost-over-duty ratio.
//
// Medians, not means, and fixed work rather than fixed time: §9.1's method,
// unchanged. A mean over five runs is moved by one slow run; a median is not,
// and one slow run on a shared machine is the common case.
func summarizeGPUPCArms(arms []schema.GPUPCArm) {
	for i := range arms {
		a := &arms[i]
		a.MedianWallMs = medianFloat(field(a.Runs, func(r schema.GPUPCRun) float64 { return r.WallMs }))
		a.MedianKernelsPerS = medianFloat(field(a.Runs, func(r schema.GPUPCRun) float64 { return r.KernelsPerS }))
		a.MedianConcurrency = medianFloat(field(a.Runs, func(r schema.GPUPCRun) float64 { return r.Concurrency }))
		a.MedianKernelUs = medianFloat(field(a.Runs, func(r schema.GPUPCRun) float64 { return r.MeanKernelUs }))
		a.DutyAchieved = medianFloat(field(a.Runs, func(r schema.GPUPCRun) float64 { return r.Evidence.ProducerDuty }))
	}
	if len(arms) == 0 || arms[0].MedianWallMs <= 0 {
		return
	}
	base := arms[0].MedianWallMs
	for i := 1; i < len(arms); i++ {
		a := &arms[i]
		a.CostPercent = (a.MedianWallMs - base) / base * 100
		if a.DutyConfigured > 0 {
			// Against the CONFIGURED duty, deliberately. It is the
			// number the thresholds name ("Tier A at 10% duty"), and
			// it is the conservative choice: the achieved duty is at
			// or above the configured one (the burst timer's
			// granularity can only overshoot), so dividing by the
			// configured value can only make the ratio LARGER —
			// pessimistic for Tier A, never flattering. The achieved
			// duty is reported beside it and is separately asserted
			// to be within a derived bound.
			a.CostOverDuty = (a.CostPercent / 100) / a.DutyConfigured
		}
	}
}

func field(runs []schema.GPUPCRun, f func(schema.GPUPCRun) float64) []float64 {
	out := make([]float64, 0, len(runs))
	for _, r := range runs {
		out = append(out, f(r))
	}
	return out
}

// medianFloat returns the median, or 0 for an empty slice. Even lengths take
// the mean of the two middle values.
func medianFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func medianUint(v []uint64) uint64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]uint64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

// decideGPUPC evaluates the four pre-committed threshold clauses against the
// arms and returns the verdict.
//
// Every clause is evaluated INDEPENDENTLY and every one that fires is
// recorded, because more than one can fire and the combination is itself
// information. The verdict is then resolved by severity, which is stated here
// rather than left to the order the clauses happen to be written in:
//
//	unshippable > deep-dive-only > smaller-duty > opt-in
//
// The residual case — the headline duty is over budget but a smaller one is
// not, and the ratio holds — is a named verdict of its own rather than a
// fallthrough. The plan's four clauses do not cover it and a harness that
// quietly picked the friendliest neighbouring answer would be making the
// decision the thresholds exist to take out of anyone's hands.
func decideGPUPC(arms []schema.GPUPCArm, maxWallPercent, maxCostOverDuty float64) schema.GPUPCDecision {
	d := schema.GPUPCDecision{
		MaxWallPercent: maxWallPercent,
		MaxCostOverDut: maxCostOverDuty,
	}

	var tierB *schema.GPUPCArm
	var tierA []schema.GPUPCArm
	for i := range arms {
		switch arms[i].Tier {
		case gpu.PCSamplingNameContinuous:
			tierB = &arms[i]
		case gpu.PCSamplingNameSerialized:
			tierA = append(tierA, arms[i])
		}
	}
	sort.Slice(tierA, func(i, j int) bool { return tierA[i].DutyConfigured > tierA[j].DutyConfigured })

	// ---- Tier B.
	switch {
	case tierB == nil:
		d.TierB = verdictTierBExplicitOnly
		d.Lines = append(d.Lines, "DECISION tier B: no continuous arm ran; "+
			"absent a measurement, Tier B does not ship as always-on")
	case tierB.CostPercent > maxWallPercent:
		d.Fired = append(d.Fired, clauseTierBOverBudget)
		d.TierB = verdictTierBExplicitOnly
		d.Lines = append(d.Lines, fmt.Sprintf(
			"THRESHOLD FIRED %s: tier B costs %+.2f%% wall-clock, above the %.1f%% bar",
			clauseTierBOverBudget, tierB.CostPercent, maxWallPercent))
		d.Lines = append(d.Lines, "DECISION tier B: "+verdictTierBExplicitOnly+
			" — Tier B does not ship as always-on; it becomes an explicitly-enabled "+
			"mode like Tier A")
	default:
		d.TierB = verdictTierBAlwaysOnCandidate
		d.Lines = append(d.Lines, fmt.Sprintf(
			"threshold %s not fired: tier B costs %+.2f%% wall-clock, within the %.1f%% bar",
			clauseTierBOverBudget, tierB.CostPercent, maxWallPercent))
		d.Lines = append(d.Lines, "DECISION tier B: "+verdictTierBAlwaysOnCandidate+
			" — Tier B remains a candidate for always-on on this evidence")
	}

	// ---- Tier A.
	if len(tierA) == 0 {
		d.TierA = verdictTierAIndeterminate
		d.Lines = append(d.Lines, "DECISION tier A: "+verdictTierAIndeterminate+
			" — no serialized arm ran, so no threshold can be evaluated")
		return d
	}
	headline := tierA[0] // the largest duty tested: the plan's "10% duty"
	smallest := tierA[len(tierA)-1]

	headlineWithin := headline.CostPercent <= maxWallPercent && headline.CostOverDuty <= maxCostOverDuty
	ratioOverEverywhere := true
	for _, a := range tierA {
		if a.CostOverDuty <= maxCostOverDuty {
			ratioOverEverywhere = false
			break
		}
	}
	smallestOverBudget := smallest.CostPercent > maxWallPercent

	if headlineWithin {
		d.Fired = append(d.Fired, clauseTierAHeadlineWithin)
		d.Lines = append(d.Lines, fmt.Sprintf(
			"THRESHOLD FIRED %s: %s costs %+.2f%% (bar %.1f%%) at cost/duty %.2f (bar %.1f)",
			clauseTierAHeadlineWithin, headline.Name, headline.CostPercent,
			maxWallPercent, headline.CostOverDuty, maxCostOverDuty))
	}
	if ratioOverEverywhere {
		d.Fired = append(d.Fired, clauseTierARatioOverAtAll)
		d.Lines = append(d.Lines, fmt.Sprintf(
			"THRESHOLD FIRED %s: cost/duty is above %.1f at every duty tested (%s)",
			clauseTierARatioOverAtAll, maxCostOverDuty, ratioList(tierA)))
	}
	if smallestOverBudget {
		d.Fired = append(d.Fired, clauseTierASmallestOverBud)
		d.Lines = append(d.Lines, fmt.Sprintf(
			"THRESHOLD FIRED %s: the lowest duty tested (%s) still costs %+.2f%%, above the %.1f%% bar",
			clauseTierASmallestOverBud, smallest.Name, smallest.CostPercent, maxWallPercent))
	}
	// The two harshest clauses can coincide arithmetically, and whether
	// they did on THIS table is stated rather than left to be noticed.
	//
	// cost/duty > R is the same statement as cost% > 100*R*duty, so the
	// two clauses are the identical condition at exactly
	// duty = W/(100*R) -- with the plan's bars (R = 2, W = 5%), 2.5%.
	// At or above that duty the every-duty ratio clause strictly IMPLIES
	// the lowest-duty wall clause and TIER_A_DEEP_DIVE_ONLY is
	// unreachable, because the unshippable verdict outranks it. Below it
	// the two separate and the deep-dive verdict becomes a real outcome.
	//
	// The arm table now goes down to 1%, so the separating case is the
	// normal one; the coinciding case remains reachable if the table is
	// ever trimmed back, and is still reported rather than assumed away.
	coincideAtDuty := maxWallPercent / (100 * maxCostOverDuty)
	if ratioOverEverywhere && smallestOverBudget {
		if smallest.DutyConfigured >= coincideAtDuty-1e-12 {
			d.Lines = append(d.Lines, fmt.Sprintf(
				"NOTE both harsh clauses fired, and at the lowest duty tested (%.2f%%) they "+
					"are the SAME condition: cost/duty > %.1f means cost > %.2f%%, and "+
					"the wall bar is %.1f%%. They separate only below %.2f%% duty, "+
					"which this table does not test — so %s could not have been "+
					"reached here whatever the numbers. Add a lower-duty arm if that "+
					"distinction is wanted.",
				smallest.DutyConfigured*100, maxCostOverDuty,
				maxCostOverDuty*smallest.DutyConfigured*100, maxWallPercent,
				coincideAtDuty*100, verdictTierADeepDiveOnly))
		} else {
			d.Lines = append(d.Lines, fmt.Sprintf(
				"NOTE both harsh clauses fired and at the lowest duty tested (%.2f%%) they "+
					"are DIFFERENT conditions — cost/duty > %.1f means cost > %.2f%% "+
					"there, well inside the %.1f%% wall bar. This table separates them "+
					"below %.2f%% duty, so %s was reachable and was not reached: the "+
					"lowest duty is over the wall bar on its own.",
				smallest.DutyConfigured*100, maxCostOverDuty,
				maxCostOverDuty*smallest.DutyConfigured*100, maxWallPercent,
				coincideAtDuty*100, verdictTierADeepDiveOnly))
		}
	}

	// The plan's fourth clause is worded "Tier A at 2.5% duty > 5%
	// wall-clock". Evaluating it at the lowest duty tested is what its own
	// reason ("duty-cycling has no remaining lever") asks for, but with a
	// 1% arm on the table that evaluation point is BELOW the duty the plan
	// wrote down, and the clause is therefore harder to fire than its
	// literal wording. When that difference changes the answer, the reader
	// is told in as many words — a verdict that got friendlier because the
	// table grew a lower arm must not read as a verdict the numbers earned.
	if planArm, ok := armAtDuty(tierA, gpuPCPlanNamedLowestDuty); ok &&
		!smallestOverBudget && planArm.CostPercent > maxWallPercent {
		d.Lines = append(d.Lines, fmt.Sprintf(
			"NOTE the plan words its fourth clause as \"Tier A at %.1f%% duty > %.1f%%\", and "+
				"the %s arm DOES cost %+.2f%%. It is evaluated at the lowest duty "+
				"tested (%s, %+.2f%%) because the clause's own reason is that "+
				"duty-cycling has no remaining lever, and %.1f%% duty is a remaining "+
				"lever. Read this result as \"unshippable does not fire at %.1f%% "+
				"duty\", NOT as \"%.1f%% duty is within budget\".",
			gpuPCPlanNamedLowestDuty*100, maxWallPercent, planArm.Name, planArm.CostPercent,
			smallest.Name, smallest.CostPercent, smallest.DutyConfigured*100,
			smallest.DutyConfigured*100, gpuPCPlanNamedLowestDuty*100))
	}

	switch {
	case smallestOverBudget:
		d.TierA = verdictTierAUnshippable
		d.Lines = append(d.Lines, "DECISION tier A: "+verdictTierAUnshippable+
			" — serialization costs more than the sampling window can explain and "+
			"duty-cycling has no remaining lever. Tier A is unshippable in this phase")
	case ratioOverEverywhere:
		d.TierA = verdictTierADeepDiveOnly
		d.Lines = append(d.Lines, "DECISION tier A: "+verdictTierADeepDiveOnly+
			" — duty-cycling is not buying what it appears to. Tier A ships only as a "+
			"deliberate deep-dive mode, with Task 11's operator warning and no "+
			"suggestion that it suits continuous use")
	case headlineWithin:
		d.TierA = verdictTierAOptIn
		d.Lines = append(d.Lines, "DECISION tier A: "+verdictTierAOptIn+
			" — ships as an opt-in tier, as planned")
	default:
		if best, ok := largestQualifyingDuty(tierA, maxWallPercent, maxCostOverDuty); ok {
			d.TierA = verdictTierASmallerDuty
			d.Lines = append(d.Lines, fmt.Sprintf(
				"DECISION tier A: %s — the headline %.1f%% duty is over budget, but "+
					"%s is within both bars (%+.2f%%, cost/duty %.2f). Ship opt-in "+
					"with that as the default duty, not %.1f%%",
				verdictTierASmallerDuty, headline.DutyConfigured*100, best.Name,
				best.CostPercent, best.CostOverDuty, headline.DutyConfigured*100))
		} else {
			d.TierA = verdictTierAIndeterminate
			d.Lines = append(d.Lines, "DECISION tier A: "+verdictTierAIndeterminate+
				" — no duty tested is within both bars, yet neither the "+
				"every-duty ratio clause nor the lowest-duty clause fired. "+
				"That combination means cost does not rise with duty, which "+
				"means the measurement is unsound. Re-run before deciding "+
				"anything; do not read this as a pass")
		}
	}
	return d
}

// armAtDuty finds the arm configured at a particular duty, if the table has
// one. Used only for reporting, never for deciding.
func armAtDuty(tierA []schema.GPUPCArm, duty float64) (schema.GPUPCArm, bool) {
	for _, a := range tierA {
		if math.Abs(a.DutyConfigured-duty) < 1e-9 {
			return a, true
		}
	}
	return schema.GPUPCArm{}, false
}

// largestQualifyingDuty returns the highest-duty arm that is within BOTH bars.
func largestQualifyingDuty(tierA []schema.GPUPCArm, maxWallPercent, maxCostOverDuty float64) (schema.GPUPCArm, bool) {
	for _, a := range tierA { // already sorted by descending duty
		if a.CostPercent <= maxWallPercent && a.CostOverDuty <= maxCostOverDuty {
			return a, true
		}
	}
	return schema.GPUPCArm{}, false
}

func ratioList(tierA []schema.GPUPCArm) string {
	parts := make([]string, 0, len(tierA))
	for _, a := range tierA {
		parts = append(parts, fmt.Sprintf("%.1f%% duty: %.2f", a.DutyConfigured*100, a.CostOverDuty))
	}
	return strings.Join(parts, ", ")
}

// renderGPUPCTable prints the arm table, the ratios and the evidence each arm
// produced. The evidence column is not decoration: it is what lets a reader
// see that the arm claiming to be Tier A at 2.5% duty really did open bursts
// and really did serialize executions.
func renderGPUPCTable(w io.Writer, res *schema.GPUPCOverhead) {
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "GPU PC-sampling overhead — %d kernels of fixed work per run, "+
		"%d streams, %d-round kernels\n", res.Workload.KernelsRun, res.Workload.Streams,
		res.Workload.Rounds)
	_, _ = fmt.Fprintln(w, "baseline = the shipping Phase 4 configuration with PC sampling OFF, "+
		"not an uninjected run")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "| %-30s | %5s | %10s | %8s | %9s | %8s | %6s | %8s |\n",
		"arm", "duty", "wall ms", "cost", "cost/duty", "kern/s", "conc", "kernel us")
	_, _ = fmt.Fprintf(w, "|%s|%s|%s|%s|%s|%s|%s|%s|\n", strings.Repeat("-", 32),
		strings.Repeat("-", 7), strings.Repeat("-", 12), strings.Repeat("-", 10),
		strings.Repeat("-", 11), strings.Repeat("-", 10), strings.Repeat("-", 8),
		strings.Repeat("-", 10))
	for i, a := range res.Arms {
		duty, cost, ratio := "—", "—", "—"
		if a.DutyConfigured > 0 {
			duty = fmt.Sprintf("%.1f%%", a.DutyConfigured*100)
			ratio = fmt.Sprintf("%.2f", a.CostOverDuty)
		}
		if i > 0 {
			cost = fmt.Sprintf("%+.2f%%", a.CostPercent)
		}
		_, _ = fmt.Fprintf(w, "| %-30s | %5s | %10.1f | %8s | %9s | %8.0f | %6.2f | %8.0f |\n",
			a.Name, duty, a.MedianWallMs, cost, ratio, a.MedianKernelsPerS,
			a.MedianConcurrency, a.MedianKernelUs)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "what each arm proved it ran (medians across runs):")
	for _, a := range res.Arms {
		_, _ = fmt.Fprintf(w, "  %-30s producer tier=%s pc_records=%d bursts=%d windows=%d duty=%.4f | "+
			"consumer pc_samples=%d windows=%d execs=%d serialized=%d not=%d unknown=%d\n",
			a.Name,
			firstEvidence(a).ProducerTier,
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.ProducerPCRecords })),
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.ProducerBursts })),
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.ProducerWindows })),
			a.DutyAchieved,
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.PCSamplesDecoded })),
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.SamplingWindowsReceived })),
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return uint64(r.Evidence.ExecutionsSeen) })), //nolint:gosec // a count
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.ExecutionsSerialized })),
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.ExecutionsNotSerialized })),
			medianUint(field64(a.Runs, func(r schema.GPUPCRun) uint64 { return r.Evidence.ExecutionsUnknown })),
		)
	}
	_, _ = fmt.Fprintln(w)
}

func firstEvidence(a schema.GPUPCArm) schema.GPUPCEvidence {
	if len(a.Runs) == 0 {
		return schema.GPUPCEvidence{}
	}
	return a.Runs[0].Evidence
}

func field64(runs []schema.GPUPCRun, f func(schema.GPUPCRun) uint64) []uint64 {
	out := make([]uint64, 0, len(runs))
	for _, r := range runs {
		out = append(out, f(r))
	}
	return out
}

// defaultShimPath and defaultConcurrentWorkload are resolved relative to the
// repository root when the flags are left unset, the same way the other
// scenarios auto-detect test/workloads.
func defaultShimPath() string { return filepath.Join("shim", "libperfagent-gpu-nvidia.so") }
func defaultConcurrentWorkload() string {
	return filepath.Join("shim", "nvidia", "testdata", "cuda_concurrent")
}

// runGPUPCScenario is the whole entry point for --scenario gpu-pc-overhead:
// the skip path, the arms, the JSON, and the exit code.
//
// The exit codes are the same shape the "self" scenario already uses: 0 when
// the measurement completed (whatever the verdict — an honest
// TIER_A_UNSHIPPABLE is a successful run of this benchmark), 3 when an
// assertion failed and the numbers therefore mean nothing. A skip is 0 with a
// single BENCH_SKIPPED line and no output file, because a partial file with no
// numbers in it is the thing most likely to be mistaken for a result.
func runGPUPCScenario(cfg gpuPCConfig, outPath string) {
	if reason := gpuPCSkipReason(cfg); reason != "" {
		_, _ = fmt.Fprintln(os.Stdout, "BENCH_SKIPPED: "+reason)
		os.Exit(0)
	}

	doc := &schema.Document{
		Scenario:  "gpu-pc-overhead",
		StartedAt: time.Now().UTC(),
		Config:    schema.Config{Runs: cfg.Runs},
		System:    gatherSystemInfo(),
	}
	ok := runGPUPCOverhead(doc, cfg, os.Stdout)

	out := outPath
	if out == "" {
		out = fmt.Sprintf("bench-gpu-pc-overhead-%d.json", time.Now().Unix())
	}
	f, err := os.Create(out)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create %s: %v\n", out, err)
		os.Exit(2)
	}
	if err := schema.Write(f, doc); err != nil {
		_ = f.Close()
		_, _ = fmt.Fprintf(os.Stderr, "write %s: %v\n", out, err)
		os.Exit(2)
	}
	if err := f.Close(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "close %s: %v\n", out, err)
		os.Exit(2)
	}
	_, _ = fmt.Fprintf(os.Stdout, "wrote %s\n", out)

	if !ok {
		_, _ = fmt.Fprintln(os.Stderr,
			"gpu-pc-overhead: the run did not prove what it measured; the numbers in "+
				"the output file are NOT a tier decision and must not be recorded as one")
		os.Exit(3)
	}
}
