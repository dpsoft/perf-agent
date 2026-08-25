// Tier A's duty cycle: when a KERNEL_SERIALIZED PC-sampling burst starts,
// when it stops, and how long to wait before the next one.
//
// Why this is a closed loop and not a constant
// --------------------------------------------
// In CUPTI_PC_SAMPLING_COLLECTION_MODE_KERNEL_SERIALIZED every kernel that
// runs while sampling is on runs serialized. That is a measurable perturbation
// of the workload being measured, so the profiler owes the operator two
// things: a bound on how much of the run was perturbed, and a record of which
// part. This class is the first. The second is gpu_sampling_window_v1, which
// the adapter emits around every burst this class opens.
//
// A fixed gap cannot bound the yield: the number of (PC, stall) pairs a 50 ms
// burst produces depends on the workload's kernel occupancy and on the
// sampling period, and varies by orders of magnitude between an idle process
// and a saturating one. Parca's PC sampler ships ~50 ms bursts aimed at ~100
// pairs per second; holding a target rate is what makes the wire volume, the
// label cardinality and the perturbation all predictable instead of
// workload-dependent. So the gap is tuned by a controller and the burst length
// is held fixed.
//
// The duty ceiling is a HARD bound, not a target
// ----------------------------------------------
// The controller can only ever lengthen the gap past the ceiling's minimum; it
// can never shorten it below. burst/(burst+gap) <= max_duty holds for every
// gap this class will ever produce, for every observed rate, including zero,
// including a target rate of zero and including arithmetic that overflows into
// infinity or NaN. That is asserted directly in core/burst_test.cc by sweeping
// the observed rate across its whole range rather than by reasoning about the
// loop, because "the controller would never ask for that" is exactly the
// argument that has been wrong before on this project.
//
// Pure, so it is provable without a GPU
// -------------------------------------
// Nothing here calls CUPTI, allocates, locks or reads a clock: the caller
// supplies `now_ns` and the running (PC, stall) pair count, and the loop
// itself is a static function of (target rate, observed rate, elapsed). That
// is what lets core/burst_test.cc prove convergence, the duty ceiling and the
// zero-rate behaviour against a fake clock on a machine with no NVIDIA
// hardware at all.
#ifndef PERFAGENT_BURST_H
#define PERFAGENT_BURST_H

#include <cmath>
#include <cstdint>
#include <mutex>

namespace perfagent {

// What the burst timer should do at this instant.
enum class BurstAction {
    kNone,   // stay as you are
    kStart,  // cuptiPCSamplingStart(): open a window
    kStop,   // cuptiPCSamplingStop(): close the window, flush, then wait
};

// Why a burst stopped. It travels with the stop so the adapter's log and its
// counters can tell an ordinary duty-cycle stop from a refusal.
enum class BurstStopReason {
    kDutyCycle,  // the burst reached burst_ns
    kGraph,      // a CUDA-graph execution was observed; Tier A refuses to run
    kShutdown,   // teardown, finalize or exit
};

struct BurstConfig {
    // How long one burst samples for. Held fixed; the gap is what moves.
    uint64_t burst_ns = 50ull * 1000000ull;  // 50 ms

    // The (PC, stall) pairs per second the loop holds. Parca's default.
    double target_rate = 100.0;

    // The hard ceiling on burst/(burst+gap). 0.1 means at most a tenth of
    // wall-clock time may run serialized. Clamped into (0, 1] on use.
    double max_duty = 0.1;

    // The longest the loop may space bursts out. Without it a workload that
    // produces a huge pair count in one burst would push the next burst out
    // to hours and Tier A would silently stop reporting.
    uint64_t max_gap_ns = 10ull * 1000000000ull;  // 10 s

    // The loop's gain: how much of the newly computed gap to adopt each
    // cycle. 1.0 is deadbeat and chases noise; 0 never moves. 0.5 halves the
    // error per cycle, which converges in a handful of bursts and still
    // damps a single anomalous burst.
    double gain = 0.5;
};

// The smallest gap the duty ceiling permits after a burst of cfg.burst_ns.
//
// duty = burst / (burst + gap) <= max_duty  <=>  gap >= burst * (1/max_duty - 1)
//
// max_duty is clamped into (0, 1] first: a zero or negative ceiling would ask
// for an infinite gap and a ceiling above 1 is not a ceiling at all.
inline uint64_t burst_min_gap_ns(const BurstConfig &cfg) {
    double duty = cfg.max_duty;
    if (!(duty > 0.0)) duty = 0.001;  // also catches NaN
    if (duty > 1.0) duty = 1.0;
    const double g = (double)cfg.burst_ns * (1.0 / duty - 1.0);
    if (!(g > 0.0)) return 0;
    if (g >= (double)cfg.max_gap_ns) return cfg.max_gap_ns;
    return (uint64_t)g;
}

// THE LOOP, as a pure function of (target rate, observed rate, elapsed).
//
//   observed_rate = pairs / elapsed                 pairs per second
//   ratio         = observed_rate / target_rate     >1 means too many
//   cycle         = elapsed * ratio                 the cycle length that
//                                                   would have hit target
//   raw_gap       = cycle - burst_ns
//   gap           = prev_gap + gain * (raw_gap - prev_gap)   damped
//   gap           = clamp(gap, min_gap, max_gap)
//
// The model this assumes, stated rather than left implicit: pairs are produced
// only while a burst is open, so the pairs-per-burst figure does not depend on
// the gap. Under that model raw_gap is constant for a steady workload and the
// damped update is a geometric sequence with ratio (1 - gain) — it converges
// monotonically, which core/burst_test.cc asserts by running it.
//
// Every degenerate input lands on a clamp rather than on undefined behaviour:
//   elapsed_ns == 0    no information; hold prev_gap (still clamped)
//   pairs == 0         ratio 0, raw_gap 0 -> the gap walks DOWN to min_gap.
//                      Never to zero: min_gap is the duty ceiling's floor and
//                      is what a workload producing nothing converges on, so
//                      a quiet GPU samples at the ceiling and no faster.
//   target_rate <= 0   ratio is +inf or NaN -> clamped to max_gap
//   huge pairs         clamped to max_gap
// The comparisons are written so NaN falls through to the safe side; `!(x >
// y)` rather than `x <= y` is deliberate everywhere below.
inline uint64_t burst_next_gap_ns(const BurstConfig &cfg, uint64_t prev_gap_ns,
                                  uint64_t pairs, uint64_t elapsed_ns) {
    const uint64_t lo = burst_min_gap_ns(cfg);
    const uint64_t hi = cfg.max_gap_ns > lo ? cfg.max_gap_ns : lo;

    double gap;
    if (elapsed_ns == 0) {
        gap = (double)prev_gap_ns;
    } else {
        const double observed_rate = (double)pairs * 1e9 / (double)elapsed_ns;
        const double ratio = observed_rate / cfg.target_rate;
        const double cycle = (double)elapsed_ns * ratio;
        double raw_gap = cycle - (double)cfg.burst_ns;
        if (!(raw_gap > 0.0)) raw_gap = 0.0;  // also catches NaN
        double gain = cfg.gain;
        if (!(gain > 0.0)) gain = 0.0;
        if (gain > 1.0) gain = 1.0;
        gap = (double)prev_gap_ns + gain * (raw_gap - (double)prev_gap_ns);
    }

    // NaN and -inf land here; so does any value below the duty floor.
    if (!(gap > (double)lo)) return lo;
    if (gap >= (double)hi) return hi;
    return (uint64_t)gap;
}

// The state machine the adapter's burst timer polls. One mutex, because the
// poll runs on the burst thread while the shutdown path runs on whoever is
// exiting.
//
// The cycle has three calls and they are three because of WHEN the yield is
// known, not for symmetry:
//
//   poll()    -> kStart   open a window, cuptiPCSamplingStart()
//   poll()    -> kStop    cuptiPCSamplingStop(), close the window
//   closed()              after the range-end flush, once the pairs that
//                         burst produced are actually on the wire
//
// CUPTI hands the burst's PC records over on the flush that FOLLOWS the stop,
// so a controller that read the pair count at stop time would measure every
// burst as having produced nothing and would sit at the duty floor forever.
// That is not a hypothetical: it is what the first draft of this class did,
// and the convergence test below is what caught it. closed() is therefore a
// separate call made after the drain.
//
// A caller that forgets closed() degrades rather than breaks: stop_locked
// schedules the next burst at the CURRENT gap, so the duty cycle keeps
// running at whatever rate it had last converged on and only the loop stops
// adapting.
class BurstController {
public:
    explicit BurstController(BurstConfig cfg)
        : cfg_(cfg), gap_ns_(burst_min_gap_ns(cfg)) {}

    // The whole start/stop decision, taken once per burst-timer tick.
    //
    // graphs_observed is the CUDA-graph refusal, and it is IN this function
    // rather than beside it on purpose. A graph launch fires one runtime
    // callback for N kernels, so N executions share one correlation and Tier
    // A's entire claim --- that a PC sample's correlation names the kernel
    // that stalled --- becomes false while still looking exact. The refusal is
    // therefore structural: once this reads true the controller never returns
    // kStart again, and it returns kStop first if a burst is open, so the
    // window closes honestly instead of being abandoned. It is not a downgrade
    // to Tier B: this class does not know how to become Tier B, and silently
    // becoming it is what the plan forbids.
    //
    // Side-effecting by design, exactly like PCDrainSchedule::due: it records
    // the transition it just returned, so "ask, then forget to act" is not a
    // shape this API offers.
    BurstAction poll(uint64_t now_ns, bool graphs_observed) {
        std::lock_guard<std::mutex> g(mu_);
        if (graphs_observed && !refused_) {
            refused_ = true;
            graph_refusals_++;
        }
        if (sampling_) {
            if (refused_) return stop_locked(now_ns, BurstStopReason::kGraph);
            if (now_ns - burst_start_ns_ < cfg_.burst_ns) return BurstAction::kNone;
            return stop_locked(now_ns, BurstStopReason::kDutyCycle);
        }
        if (refused_) return BurstAction::kNone;
        if (started_ && now_ns < next_start_ns_) return BurstAction::kNone;
        sampling_ = true;
        if (!started_) {
            started_ = true;
            first_start_ns_ = now_ns;
        }
        burst_start_ns_ = now_ns;
        bursts_++;
        return BurstAction::kStart;
    }

    // Called after the range-end flush, with the process-wide RUNNING TOTAL of
    // (PC, stall) pairs put on the wire. The controller differences it itself,
    // so the caller cannot get the delta wrong by forgetting to reset
    // something.
    //
    // The cycle the loop measures is burst + gap: the pairs a burst produced,
    // divided by the wall time the next burst will be spaced out to. That is
    // why the fixed point is independent of the current gap and the loop
    // converges geometrically rather than oscillating.
    void closed(uint64_t pairs_total) {
        std::lock_guard<std::mutex> g(mu_);
        if (!have_open_yield_) return;
        have_open_yield_ = false;
        // The delta is taken between consecutive closes rather than from the
        // burst's own start, because pairs are only produced while a burst is
        // open: anything that arrives late still belongs to the burst that
        // produced it, and this attributes it there instead of losing it.
        const uint64_t pairs =
            pairs_total > pairs_at_last_close_ ? pairs_total - pairs_at_last_close_ : 0;
        pairs_at_last_close_ = pairs_total;
        pairs_ += pairs;
        const uint64_t elapsed = last_burst_dur_ + gap_ns_;
        gap_ns_ = burst_next_gap_ns(cfg_, gap_ns_, pairs, elapsed);
        next_start_ns_ = last_burst_end_ns_ + gap_ns_;
    }

    // Teardown. Returns kStop when a burst was open, so the caller closes the
    // window with a real end timestamp on the ordinary exit path --- which is
    // precisely what makes end_ns == 0 on the wire mean a HARD exit and
    // nothing else.
    BurstAction shutdown(uint64_t now_ns) {
        std::lock_guard<std::mutex> g(mu_);
        refused_ = true;  // no further bursts after teardown begins
        if (!sampling_) return BurstAction::kNone;
        return stop_locked(now_ns, BurstStopReason::kShutdown);
    }

    bool sampling() const { std::lock_guard<std::mutex> g(mu_); return sampling_; }
    bool refused() const { std::lock_guard<std::mutex> g(mu_); return refused_; }
    // The start timestamp of the burst that is currently open. Zero when none
    // is; the caller checks sampling() rather than testing this for zero.
    uint64_t burst_start_ns() const { std::lock_guard<std::mutex> g(mu_); return burst_start_ns_; }
    BurstStopReason last_stop_reason() const {
        std::lock_guard<std::mutex> g(mu_);
        return last_stop_;
    }

    // Every counter here is assertable at a known value from a test, which is
    // the standing rule on this pipeline: a duty cycle that stopped firing
    // must not look like a workload that stopped stalling.
    uint64_t bursts() const { std::lock_guard<std::mutex> g(mu_); return bursts_; }
    uint64_t burst_ns() const { std::lock_guard<std::mutex> g(mu_); return burst_ns_; }
    uint64_t pairs_observed() const { std::lock_guard<std::mutex> g(mu_); return pairs_; }
    uint64_t graph_refusals() const { std::lock_guard<std::mutex> g(mu_); return graph_refusals_; }
    uint64_t gap_ns() const { std::lock_guard<std::mutex> g(mu_); return gap_ns_; }
    const BurstConfig &config() const { return cfg_; }

    // The duty fraction actually achieved so far, over the interval since the
    // first burst opened. Reported rather than assumed: max_duty bounds what
    // the loop may ASK for, and this is what the process actually did.
    double duty(uint64_t now_ns) const {
        std::lock_guard<std::mutex> g(mu_);
        if (!started_ || now_ns <= first_start_ns_) return 0.0;
        return (double)burst_ns_ / (double)(now_ns - first_start_ns_);
    }

private:
    BurstAction stop_locked(uint64_t now_ns, BurstStopReason why) {
        sampling_ = false;
        last_stop_ = why;
        const uint64_t dur = now_ns > burst_start_ns_ ? now_ns - burst_start_ns_ : 0;
        burst_ns_ += dur;
        last_burst_dur_ = dur;
        last_burst_end_ns_ = now_ns;
        have_open_yield_ = true;
        // Provisional: the next burst is scheduled at the gap already in
        // force, so a caller that never calls closed() keeps duty-cycling
        // instead of either stalling or free-running.
        next_start_ns_ = now_ns + gap_ns_;
        return BurstAction::kStop;
    }

    mutable std::mutex mu_;
    const BurstConfig cfg_;

    bool sampling_ = false;
    bool started_ = false;
    bool refused_ = false;
    bool have_open_yield_ = false;
    uint64_t burst_start_ns_ = 0;
    uint64_t last_burst_end_ns_ = 0;
    uint64_t last_burst_dur_ = 0;
    uint64_t first_start_ns_ = 0;
    uint64_t next_start_ns_ = 0;
    uint64_t pairs_at_last_close_ = 0;
    uint64_t gap_ns_ = 0;
    uint64_t bursts_ = 0;
    uint64_t burst_ns_ = 0;
    uint64_t pairs_ = 0;
    uint64_t graph_refusals_ = 0;
    BurstStopReason last_stop_ = BurstStopReason::kDutyCycle;
};

}  // namespace perfagent

#endif
