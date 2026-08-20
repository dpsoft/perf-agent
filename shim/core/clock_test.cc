#include "clock.h"
#include <cassert>
#include <cstdio>

using perfagent::ClockFit;

int main() {
    // An unsampled fit converts nothing.
    { ClockFit f; assert(!f.valid()); }

    // The conversion is a plain offset: vendor - (vendor0 - mono0).
    {
        ClockFit f;
        f.resample(1'000'000'000ULL, 500'000'000ULL);   // offset = 500ms
        assert(f.valid());
        assert(f.to_monotonic(1'000'000'000ULL) == 500'000'000ULL);
        assert(f.to_monotonic(1'200'000'000ULL) == 700'000'000ULL);
        assert(f.steps() == 0);
    }

    // Slew is absorbed silently: small offset movement is normal on an
    // NTP-disciplined box (measured under 15us over 20s).
    {
        ClockFit f;
        f.resample(1'000'000'000ULL, 500'000'000ULL);
        f.resample(2'000'000'010ULL, 1'500'000'000ULL); // offset moved 10ns
        assert(f.steps() == 0);
        assert(f.to_monotonic(2'000'000'010ULL) == 1'500'000'000ULL);
    }

    // A step is detected and re-anchored, not smoothed away.
    {
        ClockFit f;
        f.resample(1'000'000'000ULL, 500'000'000ULL);
        f.resample(5'000'000'000ULL, 1'500'000'000ULL); // offset jumped 3s
        assert(f.steps() == 1);
        // Re-anchored on the new pair rather than averaging the two.
        assert(f.to_monotonic(5'000'000'000ULL) == 1'500'000'000ULL);
    }

    // A backward step (vendor clock jumps backward relative to monotonic,
    // e.g., an administrator corrects a fast clock) is also counted as a step
    // and re-anchored. The step detector must treat jumps as steps regardless
    // of direction.
    {
        ClockFit f;
        f.resample(5'000'000'000ULL, 2'000'000'000ULL);   // offset = 3s
        f.resample(6'000'000'000ULL, 4'900'000'000ULL);   // offset jumped -1.1s (backward)
        assert(f.steps() == 1);
        // Re-anchored on the new pair with new offset.
        assert(f.to_monotonic(6'000'000'000ULL) == 4'900'000'000ULL);
    }

    printf("clock_test OK\n");
    return 0;
}
