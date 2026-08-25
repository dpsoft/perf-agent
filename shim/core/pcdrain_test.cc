// The PC-sampling drain schedule, against a fake clock.
//
// No GPU, no CUDA toolkit and no real time: the schedule takes the clock as a
// parameter precisely so the module-unload rule --- the one whose failure
// corrupts PC identity silently --- can be proven here rather than inferred
// from a hardware run.
#include "pcdrain.h"

#include <cassert>
#include <cstdio>
#include <thread>
#include <vector>

using perfagent::PCDrainReason;
using perfagent::PCDrainSchedule;

static const uint64_t kMs = 1000000ull;

int main() {
    // The first tick always drains: a process that starts and never loads a
    // module must not wait a period before its first pull.
    {
        PCDrainSchedule s(100 * kMs);
        assert(s.due(0));
        assert(s.periodic() == 1);
    }

    // Inside the period, a tick does not drain and says so.
    {
        PCDrainSchedule s(100 * kMs);
        assert(s.due(1000 * kMs));
        assert(!s.due(1050 * kMs));
        assert(!s.due(1099 * kMs));
        assert(s.due(1100 * kMs));           // exactly the period: due
        assert(s.periodic() == 2);
        assert(s.coalesced() == 2);
        assert(s.unload() == 0 && s.teardown() == 0);
        assert(s.total() == 2);
    }

    // A module unload always drains, whatever the phase. This is the rule
    // cupti_pcsampling.h states for CONTINUOUS mode, and it is not a rate
    // limit: skipping it makes two instructions share a PC.
    {
        PCDrainSchedule s(100 * kMs);
        assert(s.due(0));
        for (int i = 1; i <= 5; i++) {
            s.force((uint64_t)i * kMs, PCDrainReason::kModuleUnload);   // all within one period
        }
        assert(s.unload() == 5);
        assert(s.periodic() == 1);
    }

    // A forced drain resets the phase, so the timer does not immediately pull
    // again on data that was just taken. Without this a module-churning
    // process drains twice in quick succession forever.
    {
        PCDrainSchedule s(100 * kMs);
        assert(s.due(1000 * kMs));
        s.force(1090 * kMs, PCDrainReason::kModuleUnload);
        assert(!s.due(1100 * kMs));          // would have been due off the tick
        assert(s.coalesced() == 1);
        assert(s.due(1190 * kMs));           // a period after the FORCED drain
        assert(s.periodic() == 2);
    }

    // Teardown drains are counted apart from unload drains: one is a
    // correctness requirement of continuous mode, the other is the last pull
    // before the data becomes unreachable, and an operator debugging missing
    // PC data needs to know which happened.
    {
        PCDrainSchedule s(100 * kMs);
        s.force(5 * kMs, PCDrainReason::kTeardown);
        assert(s.teardown() == 1 && s.unload() == 0 && s.periodic() == 0);
        assert(s.total() == 1);
    }

    // period_ns == 0 degrades to "every tick", never to "never".
    {
        PCDrainSchedule s(0);
        assert(s.due(10));
        assert(s.due(10));
        assert(s.due(10));
        assert(s.periodic() == 3 && s.coalesced() == 0);
    }

    // The unload callback runs on the application's thread and the tick on
    // the drain thread, so the two really do race. The counters must add up
    // under that race: every call is either a drain or a coalesce, never
    // neither and never both.
    {
        PCDrainSchedule s(1 * kMs);
        const int kIter = 2000;
        std::vector<std::thread> ts;
        ts.emplace_back([&] {
            for (int i = 0; i < kIter; i++) s.force((uint64_t)i, PCDrainReason::kModuleUnload);
        });
        ts.emplace_back([&] {
            for (int i = 0; i < kIter; i++) (void)s.due((uint64_t)i);
        });
        for (auto &t : ts) t.join();
        assert(s.unload() == (uint64_t)kIter);
        assert(s.periodic() + s.coalesced() == (uint64_t)kIter);
    }

    // Tier A's range-end drain: mandatory like an unload, counted apart from
    // it, and it resets the phase so the next tick does not immediately
    // repeat the pull. cupti_pcsampling.h requires a flush after every
    // cuptiPCSamplingStop(), so a range-end drain that could be skipped is
    // the same class of silent PC-identity corruption the unload drain
    // exists to prevent.
    {
        PCDrainSchedule s(100 * kMs);
        assert(s.due(0));
        for (int i = 1; i <= 5; i++) s.force((uint64_t)i * kMs, PCDrainReason::kRangeEnd);
        assert(s.range_end() == 5);
        assert(s.unload() == 0);
        assert(s.teardown() == 0);
        assert(s.periodic() == 1);
        assert(s.total() == 6);
        // The phase moved with the last forced drain, so a tick 50ms later
        // coalesces rather than pulling again.
        assert(!s.due(55 * kMs));
        assert(s.coalesced() == 1);
        assert(s.due(106 * kMs));
    }

    printf("pcdrain_test OK\n");
    return 0;
}
