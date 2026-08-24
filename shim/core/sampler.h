// Jittered-stride one-in-N launch sampling for CPU-stack capture.
//
// Why sampling at all: a batched launch probe cannot carry per-launch
// stacks, and firing it unbatched costs an uprobe trap (~1-2us) per launch,
// which is 0.4-0.8 s/s at the measured 393k launches/s ceiling. Spec §16
// names sampling as the remedy.
//
// # Why not `launch_seq % period == 0` (issue #50)
//
// That is what this sampler used to do, chosen for reproducibility, with the
// note "if bias against periodic launch patterns is ever measured, randomize
// here". It was measured. A fixed stride aliases against any workload whose
// launch pattern is periodic with a period sharing a factor with `period`,
// and the loss is total rather than partial: those launches NEVER get a
// stack, however long the profile runs.
//
// gpu-cuda-45.pb.gz, 4000 launches at period 8 on an RTX 3090, workload
// alternating two kernels:
//
//     perfagent_axpy    3,092,953 ns GPU     all 257 stacks
//     perfagent_scale   2,894,901 ns GPU           0 stacks
//
// 48% of GPU time with no call path at all, while every honesty invariant in
// spec §4 passed: 100% exact joins, full duration conservation. Alternating
// or round-robin submission is ordinary (double buffering, ping-pong
// buffers, forward/backward pairs, multi-stream pipelines), so this is not
// an exotic workload.
//
// # What it does instead
//
// A renewal process: after sampling launch s, the next sample is at
// s + gap, where gap is drawn uniformly from the 2k+1 consecutive integers
// centred on `period` (k = period/2):
//
//     period 8  ->  gap in [4, 12]     mean exactly 8
//     period 3  ->  gap in [2,  4]     mean exactly 3
//     period 1  ->  gap == 1           every launch, as before
//
// The set is symmetric about `period`, so the mean gap is exactly `period`
// and the long-run sampling rate is exactly 1/period -- the same rate the
// fixed stride had, and the same rate `sample_period` names in the record.
// Because the set always spans both parities (it holds at least two
// consecutive integers for any period >= 2), the phase cannot lock: over a
// c-cycle every residue mod c is reached.
//
// # Determinism is kept, and it is not incidental
//
// The gap is not read from a running PRNG state; it is a pure function of
// (seed, period, the sample point). So the whole schedule 0, s1, s2, ... is
// a deterministic chain replayable offline from the seed alone, the phase
// gate still gets an EXACT expected count, and a consumer can still verify
// the sampler -- see internal/gpuabi.SampleSchedule, which is the Go replica
// of gap_at() below and is pinned to it by a shared vector in both test
// suites. The seed is a fixed constant by default so two runs of the same
// workload sample identically; PERFAGENT_GPU_SAMPLE_SEED (read by the CUPTI
// adapter) buys per-process variation for anyone who wants to average
// several runs.
//
// A consumer that does not have the seed still has a checkable invariant,
// and a stronger one than `launch_seq % period == 0` was: consecutive
// sampled records from one process must have launch_seq gaps inside
// [period - period/2, period + period/2].
//
// # What determinism does and does not survive concurrency
//
// The chain of sample POINTS is thread-independent: next_ only ever takes the
// values 0, s1, s2, ..., because each successor is derived from the value
// being replaced, not from how many threads raced for it. So the sampled
// COUNT over L launches is exactly the number of chain points below L no
// matter how the launches were threaded -- which is what keeps the phase
// gate an equality.
//
// What can shift is WHICH launch carries each stack. A thread whose ordinal
// is well past a chain point claims that point, so under concurrent launches
// the sampled launch_seq can be later than the chain point (never earlier).
// A single-threaded launch stream -- the stub, the CUDA validation workload,
// and most real submission loops -- sees no shift at all, and only there is
// the gap-band invariant above exact rather than approximate. Sampling still
// decides nothing but which launches carry a stack, so no accounting depends
// on this.
//
// # Cost, measured rather than argued
//
// The non-sampling path -- 7 launches in 8 at the default period -- is one
// relaxed fetch_add, one acquire load and a compare, against the fixed
// stride's fetch_add and a 64-bit division. gap_at() runs only on the
// launches actually sampled: a splitmix64 finalizer and a multiply-shift, no
// division, no syscall, no lock, no allocation.
//
// That reads like it should be faster and it is not. Both are dominated by
// the atomic increment, and the extra acquire load costs more than the
// division saves. Single-threaded, -O2, period 8, 2e8 calls on this machine:
//
//     fixed stride    4.33 ns/launch
//     jittered        4.87 ns/launch      +0.5 ns, +12%
//
// At the measured 393k launches/s ceiling that is 0.0002 s/s -- 0.02% of one
// core, against an uprobe trap of 1-2us for each launch that IS sampled. The
// figure is here so the next person does not have to re-derive it, and so the
// claim is a number rather than an argument.
//
// # The ABI does not change
//
// sample_period still rides in every sampled record and still means "one
// launch in N carries a stack". It is now the mean stride rather than an
// exact stride; the rate it names is unchanged and it is still not a scale
// factor (durations are never scaled -- sampling decides which launches
// carry a stack, and every execution is measured and joined regardless).
#ifndef PERFAGENT_SAMPLER_H
#define PERFAGENT_SAMPLER_H

#include <atomic>
#include <cstdint>

namespace perfagent {

class Sampler {
public:
    // Fixed by default: reproducibility is why this sampler was deterministic
    // in the first place, and the phase gate's exact sampled count depends on
    // it. Any 64-bit value works; this one is the golden ratio constant.
    static constexpr uint64_t kDefaultSeed = 0x9E3779B97F4A7C15ULL;

    explicit Sampler(uint32_t period, uint64_t seed = kDefaultSeed)
        : period_(period ? period : 1), seed_(seed) {}

    // Call once per launch. True means capture a stack for this one.
    bool should_sample() {
        const uint64_t n = observed_.fetch_add(1, std::memory_order_relaxed);
        uint64_t next = next_.load(std::memory_order_acquire);
        // next_ starts at 0, so the very first launch is always sampled and a
        // short-lived process is not silently stack-less.
        //
        // The loop is the multi-threaded case: launches arrive on whatever
        // threads the application uses, and several may be at or past the
        // sample point at once. Exactly one of them wins the CAS and takes
        // the stack; the losers re-read next_ and check again, because a
        // thread holding a much later ordinal must still be able to claim a
        // sample point the winner did not advance past.
        while (n >= next) {
            const uint64_t gap = gap_at(next, period_.load(std::memory_order_relaxed), seed_);
            if (next_.compare_exchange_weak(next, next + gap,
                                            std::memory_order_acq_rel,
                                            std::memory_order_acquire)) {
                sampled_.fetch_add(1, std::memory_order_relaxed);
                return true;
            }
            // compare_exchange_weak reloaded `next` on failure.
        }
        return false;
    }

    // The gap from the sample point at `seq` to the next one: uniform over
    // the 2k+1 integers centred on `period`, k = period/2. Pure -- no state,
    // no side effects -- which is what makes the schedule replayable and
    // what lets two threads racing on the same sample point compute the same
    // successor.
    //
    // Kept public and static because it IS the schedule: the Go replica in
    // internal/gpuabi mirrors this function, and sampler_test.cc pins it to
    // the same vector the Go test uses.
    static uint64_t gap_at(uint64_t seq, uint32_t period, uint64_t seed) {
        if (period <= 1) return 1;
        const uint32_t k = period / 2;
        const uint32_t span = 2u * k + 1u;   // odd, so the set is symmetric
        const uint64_t r = mix(seed ^ (seq * 0x9E3779B97F4A7C15ULL));
        // Lemire's multiply-shift in place of `% span`: a 32x32 multiply and
        // a shift instead of a division. The residual non-uniformity is
        // bounded by span/2^32 (~2e-9 at any sane period), which moves the
        // mean gap by less than a part in a hundred million -- far below the
        // sampling noise it sits inside.
        const uint32_t off = (uint32_t)(((r >> 32) * (uint64_t)span) >> 32);
        return (uint64_t)period - (uint64_t)k + (uint64_t)off;
    }

    uint32_t period() const { return period_.load(std::memory_order_relaxed); }

    // Re-arms: the next launch is sampled and the chain restarts from there
    // under the new period, rather than honouring a gap drawn under the old
    // one (which at a large old period could hold off capture for thousands
    // of launches). Nothing in the shipped shim changes the period mid-run;
    // the schedule is replayable from (seed, period) precisely because of
    // that, and a caller that does change it forfeits offline replay.
    void set_period(uint32_t p) {
        period_.store(p ? p : 1, std::memory_order_relaxed);
        next_.store(observed_.load(std::memory_order_relaxed), std::memory_order_release);
    }

    uint64_t seed() const { return seed_; }
    uint64_t observed() const { return observed_.load(std::memory_order_relaxed); }
    uint64_t sampled() const { return sampled_.load(std::memory_order_relaxed); }

private:
    // splitmix64's finalizer: full avalanche in three xor-shifts and two
    // multiplies, which is all this needs -- it is a hash of the sample
    // point, not a generator with state to carry.
    static uint64_t mix(uint64_t x) {
        x ^= x >> 30;
        x *= 0xBF58476D1CE4E5B9ULL;
        x ^= x >> 27;
        x *= 0x94D049BB133111EBULL;
        x ^= x >> 31;
        return x;
    }

    std::atomic<uint32_t> period_;
    const uint64_t seed_;
    // The launch ordinal at which the next stack is taken. Starts at 0.
    std::atomic<uint64_t> next_{0};
    std::atomic<uint64_t> observed_{0};
    std::atomic<uint64_t> sampled_{0};
};

}  // namespace perfagent

#endif
