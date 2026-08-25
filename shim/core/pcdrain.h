// When PC-sampling data is pulled out of CUPTI, and why it is not simply
// "every drain tick".
//
// In CUPTI_PC_SAMPLING_COLLECTION_MODE_CONTINUOUS the client owns the pull.
// cupti_pcsampling.h states two things that together make this a scheduling
// problem rather than a timer callback:
//
//   1. "Flushing of GPU PC Sampling data is required at following point to
//      maintain uniqueness of PCs: for CONTINUOUS, after every module
//      load-unload-load." A missed flush there does not lose data --- it makes
//      two different instructions share a PC identity, so the profile is
//      confidently wrong with nothing counted anywhere. That is the worst
//      failure available here, so an unload-driven drain is MANDATORY and
//      cannot be skipped because the periodic one ran recently.
//   2. cuptiPCSamplingGetData "never does device synchronization" and returns
//      only what CUPTI has at that moment, so pulling more often than needed
//      is cheap but not free, and pulling on a timer alone is not sufficient.
//
// So there are two drain reasons with different rules, and the phase has to
// be shared between them: a forced drain satisfies the period, otherwise a
// module-heavy process drains twice in quick succession forever --- once for
// the unload and again a moment later because the timer never learned the
// data was already taken.
//
// This class is the whole of that decision and holds no CUPTI state, so it is
// testable against a fake clock with no GPU and no toolkit (core/pcdrain_test.cc).
// The adapter supplies the clock; nothing here calls clock_gettime.
#ifndef PERFAGENT_PCDRAIN_H
#define PERFAGENT_PCDRAIN_H

#include <cstdint>
#include <mutex>

namespace perfagent {

// Why a drain is happening. It travels with the drain so the adapter's log
// can say which rule produced each pull rather than leaving an operator to
// infer it from timing.
enum class PCDrainReason {
    kPeriodic,      // the drain timer, and the period had elapsed
    kModuleUnload,  // CUPTI_CBID_RESOURCE_MODULE_UNLOAD_STARTING
    kTeardown,      // context destroy, finalize or exit
};

class PCDrainSchedule {
public:
    // period_ns of 0 means "drain on every tick", which is what the adapter
    // gets if someone sets the drain period to the PC period: the schedule
    // degrades to the timer rather than to never draining.
    explicit PCDrainSchedule(uint64_t period_ns) : period_ns_(period_ns) {}

    // The drain timer asking whether it is time. Returns true at most once
    // per period, and records the drain if it returns true --- the caller is
    // expected to drain, so a caller that asks and then does nothing has
    // consumed a period. Made deliberately non-const and side-effecting so
    // "check, then forget to record" is not a shape this API offers.
    bool due(uint64_t now_ns) {
        std::lock_guard<std::mutex> g(mu_);
        if (drained_ && now_ns - last_ns_ < period_ns_) {
            coalesced_++;
            return false;
        }
        mark_locked(now_ns);
        periodic_++;
        return true;
    }

    // A module unload, a context destroy or finalize. Always drains: the PC
    // uniqueness requirement is not a rate limit, and a forced drain resets
    // the phase so the next tick does not immediately repeat it.
    void force(uint64_t now_ns, PCDrainReason reason) {
        std::lock_guard<std::mutex> g(mu_);
        mark_locked(now_ns);
        if (reason == PCDrainReason::kModuleUnload) {
            unload_++;
        } else {
            teardown_++;
        }
    }

    uint64_t period_ns() const { return period_ns_; }

    // Every counter below is assertable at a known value from a test, which
    // is the point: a drain schedule that silently stopped firing would
    // otherwise look exactly like a workload that stopped stalling.
    uint64_t periodic() const { std::lock_guard<std::mutex> g(mu_); return periodic_; }
    uint64_t unload() const { std::lock_guard<std::mutex> g(mu_); return unload_; }
    uint64_t teardown() const { std::lock_guard<std::mutex> g(mu_); return teardown_; }
    // Ticks that found the period had not elapsed, because a forced drain had
    // just taken the data. Non-zero is healthy on a module-churning process
    // and zero is healthy on a quiet one; what it must never be is the only
    // non-zero counter, which would mean every drain was a skipped one.
    uint64_t coalesced() const { std::lock_guard<std::mutex> g(mu_); return coalesced_; }
    uint64_t total() const {
        std::lock_guard<std::mutex> g(mu_);
        return periodic_ + unload_ + teardown_;
    }

private:
    void mark_locked(uint64_t now_ns) {
        last_ns_ = now_ns;
        drained_ = true;
    }

    mutable std::mutex mu_;
    const uint64_t period_ns_;
    uint64_t last_ns_ = 0;
    bool drained_ = false;
    uint64_t periodic_ = 0;
    uint64_t unload_ = 0;
    uint64_t teardown_ = 0;
    uint64_t coalesced_ = 0;
};

}  // namespace perfagent

#endif
