// Deterministic one-in-N launch sampling for CPU-stack capture.
//
// Why sampling at all: a batched launch probe cannot carry per-launch
// stacks, and firing it unbatched costs an uprobe trap (~1-2us) per launch,
// which is 0.4-0.8 s/s at the measured 393k launches/s ceiling. Spec §16
// names sampling as the remedy.
//
// Why deterministic rather than randomized: reproducibility. The consumer
// can verify the sampler from launch_seq alone, and the phase gate gets an
// exact expected count. If bias against periodic launch patterns is ever
// measured, randomize here — the ABI carries sample_period per record, so
// the consumer needs no change.
#ifndef PERFAGENT_SAMPLER_H
#define PERFAGENT_SAMPLER_H

#include <atomic>
#include <cstdint>

namespace perfagent {

class Sampler {
public:
    explicit Sampler(uint32_t period) : period_(period ? period : 1) {}

    // Call once per launch. True means capture a stack for this one.
    bool should_sample() {
        const uint64_t n = observed_.fetch_add(1, std::memory_order_relaxed);
        if (n % period_.load(std::memory_order_relaxed) != 0) return false;
        sampled_.fetch_add(1, std::memory_order_relaxed);
        return true;
    }

    uint32_t period() const { return period_.load(std::memory_order_relaxed); }
    void set_period(uint32_t p) { period_.store(p ? p : 1, std::memory_order_relaxed); }
    uint64_t observed() const { return observed_.load(std::memory_order_relaxed); }
    uint64_t sampled() const { return sampled_.load(std::memory_order_relaxed); }

private:
    std::atomic<uint32_t> period_;
    std::atomic<uint64_t> observed_{0};
    std::atomic<uint64_t> sampled_{0};
};

}  // namespace perfagent

#endif
