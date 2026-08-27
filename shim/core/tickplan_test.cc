// The merged timer's schedule, against a fake clock.
//
// Issue #99's fix puts the activity drain and Tier A's burst poll on ONE
// thread, so that two of the profiler's threads can never be inside CUPTI at
// once. The deadlock that motivated it cannot be reproduced here -- it needs
// the GPU -- but the scheduling consequences can be, and they are what would
// otherwise be discovered on hardware: a burst poll that lost its granularity
// or a drain that quietly stopped happening.
#include "tickplan.h"

#include <cassert>
#include <cstdio>

using perfagent::drain_limit_ns;
using perfagent::TickPlan;
using perfagent::tick_period_ns;

static const uint64_t kMs = 1000000ull;

int main() {
    // With Tier A off the timer runs at the drain period: the merge changes
    // nothing when there is no burst poll to serve.
    assert(tick_period_ns(100 * kMs, 10 * kMs, false) == 100 * kMs);

    // With Tier A on it runs at the SHORTER of the two, so neither job's
    // cadence is lengthened. A 10 ms burst tick quantized onto a 100 ms drain
    // would silently multiply the burst length -- and therefore the duty
    // fraction, the one number Tier A exists to bound -- by ten.
    assert(tick_period_ns(100 * kMs, 10 * kMs, true) == 10 * kMs);
    // A burst tick longer than the drain period does not lengthen the drain.
    assert(tick_period_ns(100 * kMs, 250 * kMs, true) == 100 * kMs);
    // Degenerate configuration lands on the floor, never on a spin.
    assert(tick_period_ns(0, 0, true) == 1 * kMs);
    assert(tick_period_ns(0, 0, false) == 1 * kMs);
    assert(tick_period_ns(100 * kMs, 0, true) == 100 * kMs);

    // The DEFAULT configuration -- Tier A off, so the timer runs at the drain
    // period -- gets NO rate limit, and this is the case that matters most.
    // A limit there would turn a tick that arrived a hair early into a skipped
    // drain and push the next one out a whole period, halving the delivery
    // rate of a profiler that ships today in the arm that has nothing to do
    // with issue #99.
    {
        const uint64_t tick = tick_period_ns(100 * kMs, 10 * kMs, false);
        assert(tick == 100 * kMs);
        assert(drain_limit_ns(100 * kMs, tick) == 0);
        TickPlan p(drain_limit_ns(100 * kMs, tick));
        p.start(0);
        // Every tick drains, including the ones that arrived early.
        assert(p.drain_due(99 * kMs));
        assert(p.drain_due(197 * kMs));
        assert(p.drain_due(298 * kMs));
        assert(p.drains() == 3);
        assert(p.skipped() == 0);
    }

    // Tier A on: the timer is faster than the drain, so the limit is the drain
    // period and the drain is quantized onto the tick.
    {
        const uint64_t tick = tick_period_ns(100 * kMs, 10 * kMs, true);
        assert(tick == 10 * kMs);
        assert(drain_limit_ns(100 * kMs, tick) == 100 * kMs);
    }

    // One drain per ten 10 ms ticks, and the first lands a full period in
    // rather than on the first tick.
    {
        TickPlan p(100 * kMs);
        p.start(0);
        uint64_t drains = 0;
        for (int i = 1; i <= 100; i++)
            if (p.drain_due((uint64_t)i * 10 * kMs)) drains++;
        assert(drains == 10);
        assert(p.ticks() == 100);
        assert(p.drains() == 10);
        assert(p.skipped() == 90);
    }

    // The drain period is not lengthened by the merge: over a simulated
    // minute the gap between consecutive drains never exceeds the period plus
    // one tick, which is the whole of the quantization the merge introduces.
    {
        TickPlan p(100 * kMs);
        p.start(0);
        uint64_t last = 0, worst = 0;
        for (int i = 1; i <= 6000; i++) {
            const uint64_t now = (uint64_t)i * 10 * kMs;
            if (!p.drain_due(now)) continue;
            const uint64_t gap = now - last;
            if (gap > worst) worst = gap;
            last = now;
        }
        assert(worst <= 100 * kMs + 10 * kMs);
        assert(p.drains() == 600);
    }

    // A late tick -- the cost the merge introduces, a slow flush delaying the
    // next tick -- does not leave a debt that fires several drains back to
    // back afterwards. One drain, then the phase restarts from the late tick.
    {
        TickPlan p(100 * kMs);
        p.start(0);
        assert(p.drain_due(1000 * kMs));      // 900 ms late
        assert(!p.drain_due(1010 * kMs));
        assert(!p.drain_due(1090 * kMs));
        assert(p.drain_due(1100 * kMs));
        assert(p.drains() == 2);
    }

    // period 0 degrades to "every tick", never to "never".
    {
        TickPlan p(0);
        p.start(0);
        for (int i = 1; i <= 5; i++) assert(p.drain_due((uint64_t)i * kMs));
        assert(p.drains() == 5);
        assert(p.skipped() == 0);
    }

    // Without start(), the first tick drains -- a plan nobody seeded must not
    // wait for a period it has no phase for.
    {
        TickPlan p(100 * kMs);
        assert(p.drain_due(50 * kMs));
        assert(!p.drain_due(60 * kMs));
    }

    printf("tickplan_test OK\n");
    return 0;
}
