// One timer thread for both the activity drain and Tier A's burst cycle.
//
// Why one thread and not two (issue #99)
// --------------------------------------
// The adapter used to run two: a 100 ms drain timer calling
// cuptiActivityFlushAll, and a 10 ms burst timer calling cuptiPCSamplingStop.
// Nothing serialised them, so two of the profiler's own threads could be
// inside CUPTI at the same time in different subsystems, and on an RTX 3090
// that deadlocked the profiled application permanently on the first burst.
//
// A lock over both call sites excludes that state. One thread makes it
// UNREPRESENTABLE: with a single timer there is no second thread of ours to be
// in CUPTI concurrently, whatever a future call site does. The lock is still
// there (the application's own threads enter CUPTI through our callbacks) but
// it is no longer the only thing standing between the profiler and a hang.
//
// What the merge costs, stated rather than discovered
// ---------------------------------------------------
// The two jobs have different periods -- 100 ms for the drain, burst_ns/5 for
// the burst poll -- so the merged timer runs at the SHORTER of the two and the
// drain is rate-limited on top of it. The burst poll therefore keeps its
// granularity exactly. What it loses is isolation: a tick that runs the drain
// does the flush BEFORE the next burst poll, so a slow flush delays a burst
// transition by its own duration. Tier A's burst length is measured from the
// timestamps this file's caller takes around the start and stop, so a delayed
// stop lengthens the burst that is actually observed and reported --- it is
// visible in `duty=` in the adapter's report, which is measured and not
// assumed, and it is bounded by one flush.
//
// With Tier A off there is no burst poll, the timer runs at the drain period
// and this class changes nothing about the drain's cadence.
//
// Pure, so the schedule is provable without a GPU: the caller supplies now_ns.
#ifndef PERFAGENT_TICKPLAN_H
#define PERFAGENT_TICKPLAN_H

#include <cstdint>
#include <mutex>

namespace perfagent {

// The period the single timer thread runs at.
//
// Tier A off: the drain period, unchanged -- there is no burst poll to serve.
// Tier A on:  the shorter of the two, so NEITHER job's cadence is lengthened
//             by the merge. Never zero: a zero-period timer is a spin.
inline uint64_t tick_period_ns(uint64_t drain_ns, uint64_t burst_tick_ns, bool tier_a) {
    uint64_t t = drain_ns;
    if (tier_a && burst_tick_ns && burst_tick_ns < t) t = burst_tick_ns;
    return t ? t : 1000000ull;   // 1 ms floor
}

// The rate limit to put on the drain, given the timer it now rides.
//
// ZERO -- "drain on every tick" -- when the timer already runs at or slower
// than the drain period, and that case is not a curiosity: it is the DEFAULT
// configuration. With Tier A off the merged timer runs at exactly the drain
// period, and a rate limit of one drain per period would turn any tick that
// arrived a hair early into a skipped drain and push the next one out a whole
// period. Sleeping timers arrive early often enough that this would have
// halved the delivery rate of a profiler that is shipping today, in the arm
// that has nothing to do with the bug being fixed.
inline uint64_t drain_limit_ns(uint64_t drain_ns, uint64_t tick_ns) {
    return tick_ns >= drain_ns ? 0 : drain_ns;
}

// The drain's rate limit on top of the merged tick.
//
// Side-effecting on the true path, exactly like PCDrainSchedule::due: the
// caller is expected to drain, so "ask and then forget to act" is not a shape
// this API offers.
class TickPlan {
public:
    // period_ns of 0 means "drain on every tick", which is what the caller
    // gets if the drain period is set at or below the tick: the plan degrades
    // to the timer rather than to never draining.
    explicit TickPlan(uint64_t period_ns) : period_ns_(period_ns) {}

    // Called once, immediately before the timer thread starts, so the first
    // drain lands one full period in rather than on the first tick. Without
    // it a 10 ms tick would drain immediately and then again at 100 ms.
    void start(uint64_t now_ns) {
        std::lock_guard<std::mutex> g(mu_);
        last_ns_ = now_ns;
        started_ = true;
    }

    bool drain_due(uint64_t now_ns) {
        std::lock_guard<std::mutex> g(mu_);
        ticks_++;
        if (started_ && now_ns - last_ns_ < period_ns_) {
            skipped_++;
            return false;
        }
        // The phase is set from NOW, not from last + period. A tick that
        // arrived late -- because the flush before it was slow, which is
        // exactly the cost the merge introduces -- must not leave a debt that
        // fires several drains back to back afterwards.
        last_ns_ = now_ns;
        started_ = true;
        drains_++;
        return true;
    }

    uint64_t period_ns() const { return period_ns_; }
    // Assertable at known values from a test: a merged timer that quietly
    // stopped draining must not look like a workload that stopped running.
    uint64_t ticks() const { std::lock_guard<std::mutex> g(mu_); return ticks_; }
    uint64_t drains() const { std::lock_guard<std::mutex> g(mu_); return drains_; }
    uint64_t skipped() const { std::lock_guard<std::mutex> g(mu_); return skipped_; }

private:
    mutable std::mutex mu_;
    const uint64_t period_ns_;
    uint64_t last_ns_ = 0;
    bool started_ = false;
    uint64_t ticks_ = 0;
    uint64_t drains_ = 0;
    uint64_t skipped_ = 0;
};

}  // namespace perfagent

#endif
