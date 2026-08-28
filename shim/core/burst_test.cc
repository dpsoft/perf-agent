// Tier A's duty cycle, against a fake clock.
//
// No GPU, no CUDA toolkit, no real time. The controller takes `now_ns` and a
// running (PC, stall) pair count as parameters precisely so the three
// properties that bound Tier A's perturbation --- it converges to the target
// rate, it never exceeds the duty ceiling whatever the workload does, and a
// zero observed rate does not collapse the gap --- can be PROVEN here instead
// of being inferred from a hardware run that this machine cannot do.
#include "burst.h"

#include <cassert>
#include <cstdio>
#include <limits>
#include <vector>

using perfagent::BurstAction;
using perfagent::BurstConfig;
using perfagent::BurstController;
using perfagent::BurstStopReason;
using perfagent::burst_min_gap_ns;
using perfagent::burst_next_gap_ns;

static const uint64_t kMs = 1000000ull;
static const uint64_t kSec = 1000000000ull;

// Runs the controller against a fake clock, with a workload that produces
// `pairs_per_burst` (PC, stall) pairs for every full burst and nothing during
// a gap. Returns the achieved pair rate over the whole simulated interval.
struct SimResult {
    double rate;          // pairs per second, over the whole run
    double duty;          // burst_ns / wall_ns
    uint64_t final_gap;   // the gap the loop settled on
    uint64_t bursts;
    uint64_t wall_ns;
};

static SimResult simulate(BurstConfig cfg, uint64_t pairs_per_burst, unsigned cycles,
                          uint64_t tick_ns = 5 * kMs) {
    BurstController c(cfg);
    uint64_t now = 0, pairs = 0;
    uint64_t open_at = 0;
    unsigned done = 0;
    // Bounded: every cycle is at most burst + max_gap, so this cannot spin.
    const uint64_t deadline = (uint64_t)cycles * (cfg.burst_ns + cfg.max_gap_ns) + kSec;
    while (done < cycles && now < deadline) {
        const BurstAction a = c.poll(now, false);
        if (a == BurstAction::kStart) {
            open_at = now;
        } else if (a == BurstAction::kStop) {
            // The pairs land on the range-end FLUSH, which is after the stop:
            // that ordering is CUPTI's and it is the reason closed() is a
            // separate call. Proportional to how long the burst was actually
            // open --- the tick granularity makes a "50 ms" burst 50..55 ms.
            const uint64_t dur = now - open_at;
            pairs += pairs_per_burst * dur / cfg.burst_ns;
            c.closed(pairs);
            done++;
        }
        now += tick_ns;
    }
    SimResult r{};
    r.wall_ns = now;
    r.rate = now ? (double)pairs * 1e9 / (double)now : 0.0;
    r.duty = now ? (double)c.burst_ns() / (double)now : 0.0;
    r.final_gap = c.gap_ns();
    r.bursts = c.bursts();
    return r;
}

int main() {
    // ---- The duty ceiling, derived rather than asserted by eye.
    {
        BurstConfig cfg;  // 50 ms burst, 5% ceiling
        const uint64_t lo = burst_min_gap_ns(cfg);
        // gap >= burst * (1/max_duty - 1) = 50ms * 19 = 950ms. The literal is
        // kept alongside the derivation on purpose: it is what catches the
        // default moving without anyone noticing, and it moved once already
        // when the measured overhead at a 10% ceiling came in over budget.
        assert(lo == 950 * kMs);
        assert(lo == (uint64_t)((double)cfg.burst_ns * (1.0 / cfg.max_duty - 1.0)));
        const double duty = (double)cfg.burst_ns / (double)(cfg.burst_ns + lo);
        assert(duty <= cfg.max_duty + 1e-12);
    }

    // ---- The loop NEVER produces a gap below the duty floor, whatever the
    // observed rate is. This is the property the plan calls out, and it is
    // swept rather than argued: the whole point of a hard bound is that it
    // does not depend on the controller being reasonable.
    {
        BurstConfig cfg;
        const uint64_t lo = burst_min_gap_ns(cfg);
        const uint64_t elapsed = 500 * kMs;
        const uint64_t pairs_cases[] = {
            0, 1, 7, 50, 100, 1000, 100000, 10000000, 1000000000ull,
            std::numeric_limits<uint64_t>::max() / 2,
            std::numeric_limits<uint64_t>::max(),
        };
        const uint64_t prev_cases[] = {0, 1, lo, 2 * lo, cfg.max_gap_ns, cfg.max_gap_ns * 2};
        for (uint64_t p : pairs_cases) {
            for (uint64_t prev : prev_cases) {
                const uint64_t g = burst_next_gap_ns(cfg, prev, p, elapsed);
                assert(g >= lo);
                assert(g <= cfg.max_gap_ns);
                const double duty = (double)cfg.burst_ns / (double)(cfg.burst_ns + g);
                assert(duty <= cfg.max_duty + 1e-12);
            }
        }
    }

    // ---- A zero observed rate does not drive the gap to zero. It walks the
    // gap DOWN to the duty floor and stops there: an idle GPU samples at the
    // ceiling and no faster, which is the opposite of the failure this test
    // exists to exclude (a quiet workload sampled continuously).
    {
        BurstConfig cfg;
        const uint64_t lo = burst_min_gap_ns(cfg);
        uint64_t gap = cfg.max_gap_ns;
        for (int i = 0; i < 100; i++) gap = burst_next_gap_ns(cfg, gap, 0, 500 * kMs);
        assert(gap == lo);
        assert(gap > 0);
    }

    // ---- Degenerate configuration lands on a clamp, never on zero and never
    // on undefined behaviour.
    {
        BurstConfig cfg;
        cfg.target_rate = 0.0;                       // ratio -> +inf
        assert(burst_next_gap_ns(cfg, 0, 10, 500 * kMs) == cfg.max_gap_ns);
        cfg.target_rate = -1.0;                      // ratio -> negative
        assert(burst_next_gap_ns(cfg, 0, 10, 500 * kMs) == burst_min_gap_ns(cfg));
        BurstConfig nan_cfg;
        nan_cfg.target_rate = std::numeric_limits<double>::quiet_NaN();
        const uint64_t g = burst_next_gap_ns(nan_cfg, 0, 10, 500 * kMs);
        assert(g >= burst_min_gap_ns(nan_cfg));
        BurstConfig zero_duty;
        zero_duty.max_duty = 0.0;
        assert(burst_min_gap_ns(zero_duty) > 0);
        BurstConfig over_duty;
        over_duty.max_duty = 5.0;                    // "500%" is not a ceiling
        assert(burst_min_gap_ns(over_duty) == 0);
        // elapsed 0 carries no information: hold, clamped.
        assert(burst_next_gap_ns(cfg, 3 * kSec, 10, 0) >= burst_min_gap_ns(cfg));
    }

    // ---- Convergence. A workload producing 5,000 pairs per 50 ms burst is
    // 100,000 pairs/s while sampling; at a 100/s target the loop must space
    // bursts out to ~50 s of cycle --- which the 10 s max_gap clamps --- so
    // use a gentler workload where the fixed point is inside the clamps.
    //
    // 100 pairs per burst at a 100/s target wants a 1 s cycle: 50 ms burst
    // plus a 950 ms gap. The floor is 450 ms and the ceiling 10 s, so the
    // fixed point is interior and the loop must actually find it.
    {
        BurstConfig cfg;
        const SimResult r = simulate(cfg, 100, 60);
        // Within 15%: the tick granularity (5 ms on a 50 ms burst) and the
        // first few un-converged cycles are both in the average.
        assert(r.rate > 85.0 && r.rate < 115.0);
        // And the gap landed near the analytic fixed point of 950 ms.
        assert(r.final_gap > 850 * kMs && r.final_gap < 1100 * kMs);
        assert(r.bursts == 60);
        printf("burst_test: converged rate=%.1f/s gap=%llums bursts=%llu\n",
               r.rate, (unsigned long long)(r.final_gap / kMs),
               (unsigned long long)r.bursts);
    }

    // ---- Monotone convergence from both sides: starting far above and far
    // below the fixed point both land in the same place.
    {
        BurstConfig cfg;
        uint64_t from_below = burst_min_gap_ns(cfg);
        uint64_t from_above = cfg.max_gap_ns;
        for (int i = 0; i < 40; i++) {
            from_below = burst_next_gap_ns(cfg, from_below, 100, 1 * kSec);
            from_above = burst_next_gap_ns(cfg, from_above, 100, 1 * kSec);
        }
        const uint64_t d = from_below > from_above ? from_below - from_above
                                                   : from_above - from_below;
        assert(d < kMs);
        assert(from_below > 900 * kMs && from_below < 1000 * kMs);
    }

    // ---- The duty ceiling holds over a whole simulated run, for a workload
    // that produces nothing at all (the case that pushes the gap to its floor
    // and therefore the duty to its ceiling).
    {
        BurstConfig cfg;
        const SimResult r = simulate(cfg, 0, 40);
        // The tick granularity makes a burst 50..55 ms rather than exactly
        // 50, so the achieved duty can exceed the nominal ceiling by that
        // ratio and no more. Asserted with the granularity in it rather than
        // fudged: the ceiling bounds what the LOOP asks for, and the timer's
        // resolution is a separate, stated cost.
        assert(r.duty <= cfg.max_duty * 1.15);
        assert(r.final_gap == burst_min_gap_ns(cfg));
        printf("burst_test: idle duty=%.3f (ceiling %.2f) gap=%llums\n",
               r.duty, cfg.max_duty, (unsigned long long)(r.final_gap / kMs));
    }

    // ---- The state machine: start and stop strictly alternate, the burst is
    // at least burst_ns long, and burst_ns() accumulates what was actually
    // open rather than what was asked for.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        uint64_t now = 0, pairs = 0, opened = 0, closed = 0, open_at = 0;
        bool open = false;
        for (int i = 0; i < 2000; i++) {
            const BurstAction a = c.poll(now, false);
            if (a == BurstAction::kStart) {
                assert(!open);
                open = true;
                open_at = now;
                opened++;
            } else if (a == BurstAction::kStop) {
                assert(open);
                assert(now - open_at >= cfg.burst_ns);
                open = false;
                closed++;
                pairs += 100;
                c.closed(pairs);
            }
            now += 5 * kMs;
        }
        assert(opened == closed + (open ? 1 : 0));
        assert(c.bursts() == opened);
        assert(c.burst_ns() >= closed * cfg.burst_ns);
        assert(c.pairs_observed() == closed * 100);
    }

    // ---- The CUDA-graph refusal. Once a graph execution has been observed
    // the controller closes an open burst and NEVER starts another. It does
    // not become Tier B --- it stops, loudly and counted.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        uint64_t now = 0;
        assert(c.poll(now, false) == BurstAction::kStart);
        now += 10 * kMs;
        // The graph arrives mid-burst: the burst is cut short, not abandoned.
        assert(c.poll(now, true) == BurstAction::kStop);
        c.closed(5);
        assert(c.last_stop_reason() == BurstStopReason::kGraph);
        assert(c.refused());
        assert(c.graph_refusals() == 1);
        // The short burst is still accounted: 10 ms of the workload really
        // did run serialized and the window really was open for it.
        assert(c.burst_ns() == 10 * kMs);
        // And no burst ever starts again, however long the clock runs.
        for (int i = 0; i < 1000; i++) {
            now += 100 * kMs;
            assert(c.poll(now, true) == BurstAction::kNone);
        }
        assert(c.bursts() == 1);
        assert(c.graph_refusals() == 1);  // counted once, not per poll
    }

    // ---- A graph observed BEFORE the first burst means Tier A never starts
    // at all --- the "refuses to start" case, distinct from "stops".
    {
        BurstConfig cfg;
        BurstController c(cfg);
        uint64_t now = 0;
        for (int i = 0; i < 100; i++) {
            assert(c.poll(now, true) == BurstAction::kNone);
            now += 100 * kMs;
        }
        assert(c.bursts() == 0);
        assert(c.burst_ns() == 0);
        assert(c.refused());
        assert(c.graph_refusals() == 1);
    }

    // ---- Shutdown closes an open burst with a real timestamp. This is what
    // makes end_ns == 0 on the wire mean a HARD exit and nothing else: on the
    // ordinary path the atexit handler comes through here and the window is
    // closed.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        assert(c.poll(0, false) == BurstAction::kStart);
        assert(c.sampling());
        assert(c.burst_start_ns() == 0);
        assert(c.shutdown(20 * kMs) == BurstAction::kStop);
        assert(!c.sampling());
        assert(c.last_stop_reason() == BurstStopReason::kShutdown);
        assert(c.burst_ns() == 20 * kMs);
        c.closed(42);
        assert(c.pairs_observed() == 42);
        // Idempotent, and never reopens.
        assert(c.shutdown(30 * kMs) == BurstAction::kNone);
        assert(c.poll(10 * kSec, false) == BurstAction::kNone);
        assert(c.bursts() == 1);
    }

    // ---- Shutdown with no burst open is a no-op, so a caller that always
    // calls it does not emit a phantom window.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        assert(c.shutdown(kSec) == BurstAction::kNone);
        assert(c.bursts() == 0);
        assert(c.burst_ns() == 0);
    }

    // ---- Issue #101: the split halves, and the state that makes the split
    // load-bearing rather than tidy.
    //
    // A launch EXIT may only ever CLOSE. The dangerous instant is the one
    // where the controller would happily start: no burst open, and the gap
    // already elapsed. A plain poll() there returns kStart and COMMITS it ---
    // which at an EXIT call site means the controller believes it is sampling
    // while CUPTI is stopped, and every execution until the next transition is
    // labelled serialized when it was not. poll_close must decline, and must
    // leave no trace that it was asked.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        // Reach the dangerous state: open, close, let the gap run out.
        assert(c.poll_open(0, false) == BurstAction::kStart);
        assert(c.poll_close(cfg.burst_ns, false) == BurstAction::kStop);
        c.closed(0);
        assert(!c.sampling());
        const uint64_t after_gap = cfg.burst_ns + c.gap_ns() + kSec;
        // The control: poll() WOULD start here. This is what makes the
        // assertion below a real one rather than a restatement of kNone.
        {
            BurstController probe(cfg);
            assert(probe.poll_open(0, false) == BurstAction::kStart);
            assert(probe.poll_close(cfg.burst_ns, false) == BurstAction::kStop);
            probe.closed(0);
            assert(probe.poll(after_gap, false) == BurstAction::kStart);
        }
        const uint64_t bursts_before = c.bursts();
        assert(c.poll_close(after_gap, false) == BurstAction::kNone);
        assert(!c.sampling());                    // did not open
        assert(c.bursts() == bursts_before);      // and did not count one
    }

    // ---- The mirror: a launch ENTER may only ever OPEN. With a burst open
    // and past its length, poll() would return kStop; poll_open must decline
    // and must leave the burst OPEN, because the ENTER site cannot stop CUPTI.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        assert(c.poll_open(0, false) == BurstAction::kStart);
        const uint64_t past = cfg.burst_ns + kSec;
        {
            BurstController probe(cfg);
            assert(probe.poll_open(0, false) == BurstAction::kStart);
            assert(probe.poll(past, false) == BurstAction::kStop);   // the control
        }
        assert(c.poll_open(past, false) == BurstAction::kNone);
        assert(c.sampling());                     // still open, as CUPTI is
        assert(c.burst_start_ns() == 0);          // and it is the SAME burst
        assert(c.poll_close(past, false) == BurstAction::kStop);
    }

    // ---- Swept: over a full duty cycle driven at launch boundaries, neither
    // half ever returns the other's action. This is the invariant the call
    // sites rely on, asserted across the whole state space the cycle visits
    // rather than at the two points above.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        uint64_t now = 0, pairs = 0, opens = 0, closes = 0;
        for (unsigned i = 0; i < 20000; i++) {
            const BurstAction o = c.poll_open(now, false);
            assert(o != BurstAction::kStop);      // ENTER never stops
            if (o == BurstAction::kStart) opens++;
            now += kMs;
            const BurstAction cl = c.poll_close(now, false);
            assert(cl != BurstAction::kStart);    // EXIT never starts
            if (cl == BurstAction::kStop) {
                closes++;
                pairs += 100;
                c.closed(pairs);
            }
            now += kMs;
        }
        // The cycle actually ran -- otherwise the two assertions above hold
        // vacuously, which is the failure mode this project keeps finding.
        assert(opens > 5);
        // Open and close stay paired: at most one burst outstanding, ever.
        assert(opens - closes <= 1);
        assert(c.duty(now) <= cfg.max_duty * 1.05);
    }

    // ---- Both halves observe the graph refusal, because either call site may
    // be the first to see a graph launch. Refused at an ENTER, the open burst
    // still closes with kGraph at the next EXIT rather than being abandoned.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        assert(c.poll_open(0, false) == BurstAction::kStart);
        assert(c.poll_open(kMs, true) == BurstAction::kNone);   // notices, cannot act
        assert(c.refused());
        assert(c.sampling());                                   // still open
        assert(c.poll_close(2 * kMs, false) == BurstAction::kStop);
        assert(c.last_stop_reason() == BurstStopReason::kGraph);
        assert(c.graph_refusals() == 1);
        // Never reopens.
        assert(c.poll_open(10 * kSec, false) == BurstAction::kNone);
    }

    // ---- A graph seen first at an EXIT refuses there too.
    {
        BurstConfig cfg;
        BurstController c(cfg);
        assert(c.poll_open(0, false) == BurstAction::kStart);
        assert(c.poll_close(kMs, true) == BurstAction::kStop);
        assert(c.last_stop_reason() == BurstStopReason::kGraph);
        assert(c.graph_refusals() == 1);
    }

    // ---- Issue #101: a burst that OVERRUNS its configured length must not
    // break the duty ceiling. Since the transitions moved onto launch
    // boundaries a burst ends at the first launch after its length elapses,
    // which on a synchronising workload was measured at 50 ms configured ->
    // 104 ms actual. Charging the next gap for the nominal 50 ms would let the
    // achieved duty run to 0.17 against a ceiling of 0.10.
    {
        BurstConfig cfg;
        cfg.burst_ns = 50 * kMs;
        cfg.max_duty = 0.1;
        BurstController c(cfg);
        uint64_t now = 0;
        const uint64_t overrun = 2;   // every burst runs twice its length
        unsigned bursts = 0;
        uint64_t first_open = 0;
        // A workload that yields NOTHING, which is what pins the ceiling
        // rather than the loop. With pairs coming in at the target rate the
        // controller holds the gap far above either floor and the floor is
        // never the binding constraint -- a version of this test that fed it
        // 100 pairs per burst passed against the unfixed code, which is why it
        // does not do that. At zero yield the loop walks the gap DOWN to the
        // floor, so the floor is the only thing holding the bound.
        for (unsigned i = 0; i < 200000 && bursts < 12; i++) {
            if (c.poll_open(now, false) == BurstAction::kStart) {
                if (!bursts) first_open = now;
                // Closed only after `overrun` x the configured length.
                now += overrun * cfg.burst_ns;
                const BurstAction a = c.poll_close(now, false);
                assert(a == BurstAction::kStop);
                bursts++;
                c.closed(0);
            }
            now += kMs;
        }
        assert(bursts == 12);
        // The gap must have been charged for the burst that HAPPENED: a
        // 100 ms burst at a 0.1 ceiling needs 900 ms, not the 450 ms a 50 ms
        // burst needs.
        assert(c.gap_ns() >= burst_min_gap_ns(cfg) * overrun);
        // And the achieved duty must be under the ceiling. Measured, not
        // reasoned about -- this is the number the ceiling is a claim about.
        //
        // Measured at the end of the last COMPLETE cycle, not at the last
        // close: duty() over an interval that ends mid-gap counts the burst
        // but not the gap it is owed, which over-states it by one gap and
        // would fail here for a reason that has nothing to do with the bound.
        now += c.gap_ns();
        const double d = c.duty(now);
        printf("burst_test: overrun x%llu duty=%.4f (ceiling %.2f) gap=%llums\n",
               (unsigned long long)overrun, d, cfg.max_duty,
               (unsigned long long)(c.gap_ns() / kMs));
        assert(d <= cfg.max_duty);
        (void)first_open;
    }

    printf("burst_test: OK\n");
    return 0;
}
