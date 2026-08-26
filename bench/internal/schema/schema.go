package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// SchemaVersion is bumped when the JSON layout changes incompatibly.
const SchemaVersion = 1

// Document is the top-level JSON object written per scenario run.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Scenario      string    `json:"scenario"`
	Config        Config    `json:"config"`
	System        System    `json:"system"`
	StartedAt     time.Time `json:"started_at"`
	Runs          []Run     `json:"runs"`

	// GPUPC holds the gpu-pc-overhead scenario's whole result. It hangs
	// off Document rather than off Run because that scenario's unit of
	// result is the ARM (five of them, five interleaved runs each), not
	// the run. Nil, and absent from the JSON, for every other scenario.
	GPUPC *GPUPCOverhead `json:"gpu_pc_overhead,omitempty"`
}

type Config struct {
	Processes   int            `json:"processes"`
	Runs        int            `json:"runs"`
	DropCache   bool           `json:"drop_cache"`
	UnwindMode  string         `json:"unwind_mode,omitzero"`
	WorkloadMix map[string]int `json:"workload_mix,omitempty"`
}

type System struct {
	Kernel          string `json:"kernel"`
	CPUModel        string `json:"cpu_model"`
	NCPU            int    `json:"ncpu"`
	GoVersion       string `json:"go_version"`
	PerfAgentCommit string `json:"perf_agent_commit"`
}

type Run struct {
	RunN                int      `json:"run_n"`
	TotalMs             float64  `json:"total_ms"`
	PIDCount            int      `json:"pid_count"`
	DistinctBinaryCount int      `json:"distinct_binary_count"`
	PerBinary           []Binary `json:"per_binary,omitempty"`

	// Self holds the metrics emitted by the "self" scenario (a
	// second perf-agent profiling the first). Omitted in JSON for
	// other scenarios via omitzero.
	Self SelfMetrics `json:"self,omitzero"`
}

// SelfMetrics captures the measurements produced by the "self"
// scenario: perf-agent #1 profiles a workload; perf-agent #2
// profiles perf-agent #1. The "did this PR regress anything?" gate
// in CI looks at:
//
//   - CPUOverheadRatio: how much CPU perf-agent #1 burns relative
//     to the workload it's profiling. Above the budget = regression.
//   - KernelResolutionRate: fraction of kernel-side samples in
//     perf-agent #1's own pprof that resolved to a named symbol
//     instead of "0x<hex>". A drop = blazesym kernel symbolization
//     broke (the original v1.2.0 lockdown class of bug).
type SelfMetrics struct {
	WorkloadPID             int     `json:"workload_pid"`
	AgentPID                int     `json:"agent_pid"`
	WorkloadCPUSamples      int     `json:"workload_cpu_samples"`
	AgentCPUSamples         int     `json:"agent_cpu_samples"`
	CPUOverheadRatio        float64 `json:"cpu_overhead_ratio"`
	KernelLocationsTotal    int     `json:"kernel_locations_total"`
	KernelLocationsNamed    int     `json:"kernel_locations_named"`
	KernelResolutionRate    float64 `json:"kernel_resolution_rate"`
	CPUOverheadBudgetMet    bool    `json:"cpu_overhead_budget_met"`
	ResolutionRateBudgetMet bool    `json:"resolution_rate_budget_met"`
}

type Binary struct {
	Path         string  `json:"path"`
	BuildID      string  `json:"build_id"`
	EhFrameBytes int     `json:"eh_frame_bytes"`
	CompileMs    float64 `json:"compile_ms"`
}

// SortPerBinary sorts each Run's PerBinary by CompileMs descending so a
// human reader sees hot binaries at the top.
func (d *Document) SortPerBinary() {
	for i := range d.Runs {
		sort.Slice(d.Runs[i].PerBinary, func(a, b int) bool {
			return d.Runs[i].PerBinary[a].CompileMs > d.Runs[i].PerBinary[b].CompileMs
		})
	}
}

// Write encodes d to w as indented JSON, stamping SchemaVersion and
// sorting per_binary descending by compile_ms.
func Write(w io.Writer, d *Document) error {
	d.SchemaVersion = SchemaVersion
	d.SortPerBinary()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// ErrSchemaMismatch is returned by Read when the input's schema_version
// does not match the build's SchemaVersion.
var ErrSchemaMismatch = errors.New("schema version mismatch")

// Read decodes a Document from r. Returns ErrSchemaMismatch if the
// schema_version field doesn't match this package's SchemaVersion.
func Read(r io.Reader) (*Document, error) {
	var d Document
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, err
	}
	if d.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrSchemaMismatch, d.SchemaVersion, SchemaVersion)
	}
	return &d, nil
}

// ---------------------------------------------------------------------------
// The GPU PC-sampling overhead scenario (plan Task 12).
//
// Everything below is additive and every field is omitzero/omitempty, so a
// document from any other scenario is byte-identical to what it was and
// SchemaVersion does not move.
//
// The shape is deliberately "one row per arm, plus the decision", because the
// point of this scenario is not a measurement, it is a DECISION taken against
// thresholds committed before the data existed. A reader who has to compute
// the ratio themselves is a reader who can talk themselves into a different
// answer.
// ---------------------------------------------------------------------------

// GPUPCOverhead is the whole result of the gpu-pc-overhead scenario: the
// workload it ran, the arms, and the verdict the pre-committed thresholds
// produce from them.
type GPUPCOverhead struct {
	// Workload is the fixed work every arm ran, verbatim as configured.
	Workload GPUPCWorkload `json:"workload"`

	// Calibration is one UNINJECTED run of the same fixed work, taken
	// before the arms. It is NOT the baseline and is never used as one:
	// the baseline is the shipping Phase 4 configuration with PC sampling
	// off, because §9.1 already measured injection and the activity path
	// and those costs are paid whether or not PC sampling is on. This row
	// exists to prove the workload is sized sanely and to warm the device
	// clocks before the first arm.
	Calibration GPUPCArm `json:"calibration,omitzero"`

	// Arms are the five arms in the order they were run, baseline first.
	Arms []GPUPCArm `json:"arms"`

	// Decision is what the thresholds say. See GPUPCDecision.
	Decision GPUPCDecision `json:"decision"`
}

// GPUPCWorkload records the fixed work, so a number can never be compared
// against one produced by a differently-sized run.
type GPUPCWorkload struct {
	Path       string `json:"path"`
	Iters      int    `json:"iters"`
	Warmup     int    `json:"warmup"`
	Streams    int    `json:"streams"`
	Rounds     int    `json:"rounds"`
	Blocks     int    `json:"blocks"`
	Threads    int    `json:"threads"`
	SyncEvery  int    `json:"sync_every"`
	KernelsRun int    `json:"kernels_run"`
}

// GPUPCArm is one arm: its configuration, its per-run measurements, the
// medians, and the evidence that it actually ran in the mode it claims.
type GPUPCArm struct {
	// Name is the arm as the report table names it.
	Name string `json:"name"`
	// Tier is the PC-sampling tier as the agent spells it: off,
	// continuous or serialized.
	Tier string `json:"tier"`
	// BurstMs and GapMs are zero except on the Tier A arms.
	BurstMs int `json:"burst_ms,omitzero"`
	GapMs   int `json:"gap_ms,omitzero"`
	// DutyConfigured is burst/(burst+gap) as configured: 0.10, 0.05,
	// 0.025. It is the denominator the pre-committed thresholds name
	// ("Tier A at 10% duty"), so it is what the ratio is computed
	// against. DutyAchieved is what the producer reported it actually
	// did, and the two disagreeing by more than the tolerance FAILS the
	// arm rather than being reported as a smaller ratio.
	DutyConfigured float64 `json:"duty_configured,omitzero"`
	DutyAchieved   float64 `json:"duty_achieved,omitzero"`

	// Runs holds every measurement, not just the median, so the spread
	// is visible. A median across five runs whose spread is larger than
	// the effect is not a measurement.
	Runs []GPUPCRun `json:"runs"`

	// The medians across Runs. WallMs is the workload's own fixed-work
	// wall clock (its printed elapsed_ms), which excludes CUDA
	// initialization, allocation and the warm-up.
	MedianWallMs      float64 `json:"median_wall_ms"`
	MedianKernelsPerS float64 `json:"median_kernels_per_s"`
	// MedianConcurrency is sum(exec duration) / (max end - min start)
	// over the executions the profile retained: how many kernels were
	// resident at once, on average. It is the property Tier A destroys,
	// and the baseline arm's value is asserted against a floor -- a
	// workload that turned out to be serial would produce a small, green
	// and meaningless Tier A cost.
	MedianConcurrency float64 `json:"median_concurrency,omitzero"`
	// MedianKernelUs is the mean GPU kernel duration in microseconds.
	// Asserted against a floor on the baseline arm: the plan requires
	// non-trivial kernel durations, and a microbenchmark's would not be.
	MedianKernelUs float64 `json:"median_kernel_us,omitzero"`

	// CostPercent is (this arm's median - baseline's median) / baseline's
	// median, as a percentage. Zero on the baseline arm itself.
	CostPercent float64 `json:"cost_percent,omitzero"`
	// CostOverDuty is CostPercent/100 divided by DutyConfigured. THE
	// number that decides Tier A: serialization's damage does not stop
	// when the burst does, because concurrency has to refill afterwards.
	// Near 1 means duty-cycling works and the tier is tunable to any
	// budget; far above 1 means a smaller duty will not rescue it.
	CostOverDuty float64 `json:"cost_over_duty,omitzero"`
}

// GPUPCRun is one execution of one arm.
type GPUPCRun struct {
	RunN int `json:"run_n"`
	// WallMs is the workload's own fixed-work elapsed time; ProcessMs is
	// the whole child process including CUDA init and teardown. Both are
	// recorded because a divergence between them means the cost moved
	// into startup, which the fixed-work number would hide.
	WallMs       float64 `json:"wall_ms"`
	ProcessMs    float64 `json:"process_ms"`
	KernelsPerS  float64 `json:"kernels_per_s"`
	MaxAbsErr    float64 `json:"max_abs_err"`
	Concurrency  float64 `json:"concurrency,omitzero"`
	MeanKernelUs float64 `json:"mean_kernel_us,omitzero"`

	// Evidence is what this run PROVED about the mode it ran in, from
	// both ends independently: the producer's own report line and the
	// consumer's counters. Recorded rather than merely asserted, so a
	// reader of the JSON can re-check the assertions the harness made.
	Evidence GPUPCEvidence `json:"evidence"`
}

// GPUPCEvidence is the proof that an arm ran in the mode it says it ran in.
//
// This exists because the standing failure mode on this project is a check
// reading green exactly when things are worst, and its benchmark-shaped
// instance is an arm that did not actually enable the tier it claims. Such an
// arm reports a wonderfully small overhead. Every field here is asserted
// against the arm's expectations and a mismatch fails the whole run.
type GPUPCEvidence struct {
	// From the producer's report line on stderr (PERFAGENT_GPU_LOG=stderr).
	ProducerTier        string  `json:"producer_tier"`
	ProducerPCRecords   uint64  `json:"producer_pc_records"`
	ProducerBursts      uint64  `json:"producer_bursts,omitzero"`
	ProducerWindows     uint64  `json:"producer_windows,omitzero"`
	ProducerDuty        float64 `json:"producer_duty,omitzero"`
	ProducerStartFailed uint64  `json:"producer_start_failed,omitzero"`
	ProducerStopFailed  uint64  `json:"producer_stop_failed,omitzero"`
	ProducerGraphExecs  uint64  `json:"producer_graph_execs,omitzero"`
	ProducerGraphRefuse uint64  `json:"producer_graph_refused,omitzero"`

	// From the consumer: gpuprobe.Stats and gpu.Snapshot.
	PCSamplesDecoded        uint64 `json:"pc_samples_decoded"`
	SamplingWindowsDecoded  uint64 `json:"sampling_windows_decoded"`
	SamplingWindowsReceived uint64 `json:"sampling_windows_received"`
	ExecutionsSeen          int    `json:"executions_seen"`
	ExecutionsSerialized    uint64 `json:"executions_serialized"`
	ExecutionsNotSerialized uint64 `json:"executions_not_serialized"`
	ExecutionsUnknown       uint64 `json:"executions_serialization_unknown"`
	SnapshotTier            string `json:"snapshot_tier"`
}

// GPUPCDecision is the verdict the pre-committed thresholds produce. A
// threshold decided after seeing the data is not a threshold, so these are
// evaluated mechanically and reported as an outcome, never as a discussion.
type GPUPCDecision struct {
	// Thresholds echoes the numbers that were committed in the plan, so
	// a reader of the JSON can see they were not adjusted to fit.
	MaxWallPercent float64 `json:"max_wall_percent"`
	MaxCostOverDut float64 `json:"max_cost_over_duty"`

	// TierB and TierA are the two verdicts, as stable identifiers. See
	// the constants in the scenario.
	TierB string `json:"tier_b"`
	TierA string `json:"tier_a"`

	// Fired lists every threshold clause that fired, in the plan's order.
	Fired []string `json:"fired,omitempty"`
	// Lines is the human-readable rendering the harness printed.
	Lines []string `json:"lines,omitempty"`
}
