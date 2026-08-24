#include "sampler.h"
#include "kernelnames.h"
#include <atomic>
#include <cassert>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>
#include <vector>

using perfagent::Sampler;
using perfagent::KernelNameTable;

int main() {
    // Unbuffered: these tests print the numbers a failure needs to be
    // diagnosed, and assert() aborts without flushing stdout -- a buffered
    // run loses exactly the line that explains the abort.
    setvbuf(stdout, nullptr, _IONBF, 0);
    // One in N, deterministically, including the very first launch.
    {
        Sampler s(4);
        int hits = 0;
        for (int i = 0; i < 40; i++) if (s.should_sample()) hits++;
        assert(hits == 10);
        assert(s.observed() == 40);
        assert(s.sampled() == 10);
    }
    // The first launch is always sampled, so a short-lived process is not
    // silently stack-less.
    {
        Sampler s(1000);
        assert(s.should_sample());
    }
    // Period 1 samples everything; period 0 is coerced to 1 rather than
    // dividing by zero or disabling capture silently.
    {
        Sampler s(1);
        for (int i = 0; i < 5; i++) assert(s.should_sample());
        Sampler z(0);
        assert(z.period() == 1);
    }
    // Changing the period mid-run takes effect and does not lose the counts.
    {
        Sampler s(2);
        for (int i = 0; i < 10; i++) s.should_sample();
        s.set_period(5);
        assert(s.period() == 5);
        assert(s.observed() == 10);
    }

    // ---- issue #50: the sampler must not lock phase --------------------
    //
    // The regression test this fix exists for. A workload that alternates
    // two kernels is an ordinary shape (double buffering, ping-pong buffers,
    // forward/backward pairs, multi-stream pipelines), and the stub and the
    // CUDA validation workload both have it. Under the old `seq % period`
    // sampler at an even period every sampled index had the same parity, so
    // one of the two kernels received every stack and the other received
    // none -- for the whole run, no matter how long it was.
    //
    // gpu-cuda-45.pb.gz measured it on an RTX 3090: perfagent_axpy took all
    // 257 stacks, perfagent_scale took 0 while holding 48% of GPU time.
    {
        Sampler s(8);
        int stacks[2] = {0, 0};
        for (int i = 0; i < 4000; i++) {
            if (s.should_sample()) stacks[i % 2]++;
        }
        printf("  alternating 2-kernel at period 8: kernel_a=%d kernel_b=%d\n",
               stacks[0], stacks[1]);
        if (stacks[0] == 0 || stacks[1] == 0) {
            printf("  FAIL: the sampler locked phase against a 2-cycle: "
                   "one kernel of the pair received every stack and the other received none\n");
        }
        assert(stacks[0] > 0);
        assert(stacks[1] > 0);
    }
    // The same statement generalized: over a 2-, 3- and 4-cycle, every phase
    // must be reachable. A scheme that fixed only the 2-cycle (say, by
    // alternating two strides) would pass the case above and still lose a
    // kernel in a 3-stream pipeline.
    {
        for (int cycle = 2; cycle <= 4; cycle++) {
            for (int period = 2; period <= 12; period++) {
                Sampler s((uint32_t)period);
                std::vector<int> phase((size_t)cycle, 0);
                for (int i = 0; i < 20000; i++) {
                    if (s.should_sample()) phase[(size_t)(i % cycle)]++;
                }
                for (int p = 0; p < cycle; p++) {
                    if (phase[(size_t)p] == 0) {
                        printf("  FAIL: period=%d locks phase against a %d-cycle: "
                               "residue %d never sampled\n", period, cycle, p);
                    }
                    assert(phase[(size_t)p] > 0);
                }
            }
        }
    }
    // The rate the ABI's sample_period claims. Gaps are drawn uniformly from
    // a set of consecutive integers centred on `period`, so the mean gap is
    // exactly `period` and the long-run rate is exactly 1/period. Over 200k
    // launches the deterministic schedule must land within 2% of it -- a
    // stated tolerance, not a hope: at period 8 the per-gap standard
    // deviation is ~2.6 launches, so 25k gaps put the 2% band far outside
    // any plausible deviation, and only a scheme whose mean gap is actually
    // wrong can miss it.
    {
        const int kLaunches = 200000;
        for (uint32_t period : {2u, 3u, 4u, 7u, 8u, 16u, 64u}) {
            Sampler s(period);
            for (int i = 0; i < kLaunches; i++) s.should_sample();
            const double want = (double)kLaunches / (double)period;
            const double got = (double)s.sampled();
            const double err = (got - want) / want;
            printf("  rate at period %2u: sampled=%llu expected=%.1f err=%+.3f%%\n",
                   period, (unsigned long long)s.sampled(), want, err * 100.0);
            assert(err < 0.02 && err > -0.02);
        }
    }
    // Determinism: same period, same seed, same schedule. This is what keeps
    // the phase gate's exact sampled count exact, and what lets a consumer
    // replay the sampler offline. Two independently constructed samplers
    // must agree launch for launch, not just in total.
    {
        Sampler a(8), b(8);
        for (int i = 0; i < 5000; i++) assert(a.should_sample() == b.should_sample());
        assert(a.sampled() == b.sampled());
    }
    // A different seed gives a different schedule -- otherwise the seed is
    // decoration and per-process variation is not actually available.
    {
        Sampler a(8), b(8, 0x1234567890ABCDEFULL);
        bool differed = false;
        for (int i = 0; i < 5000; i++) {
            if (a.should_sample() != b.should_sample()) differed = true;
        }
        assert(differed);
    }
    // Every gap lies in the band the schedule promises: [period - period/2,
    // period + period/2]. This is the invariant a consumer can check from
    // the sampled records' launch_seq values ALONE, with no seed -- it is
    // what replaces `launch_seq % period == 0` as the producer-side check.
    {
        const uint32_t period = 8;
        Sampler s(period);
        long prev = -1;
        for (long i = 0; i < 20000; i++) {
            if (!s.should_sample()) continue;
            if (prev >= 0) {
                const long gap = i - prev;
                assert(gap >= (long)(period - period / 2));
                assert(gap <= (long)(period + period / 2));
            }
            prev = i;
        }
    }
    // The shared pin between this sampler and its Go replica in
    // internal/gpuabi/sampler.go. TestTheGoReplicaMatchesTheShimSampler
    // asserts these exact numbers on the other side, so a change to the hash,
    // the seed or the gap band breaks BOTH suites instead of silently letting
    // the two implementations drift -- which is the failure mode that would
    // make the consumer's verification of the sampler worthless.
    {
        static const uint64_t kGapsPeriod8[10] = {11, 4, 9, 6, 4, 8, 4, 8, 9, 5};
        static const uint64_t kGapsPeriod3[10] = {4, 2, 3, 2, 2, 3, 2, 3, 3, 2};
        for (uint64_t seq = 0; seq < 10; seq++) {
            assert(Sampler::gap_at(seq, 8, Sampler::kDefaultSeed) == kGapsPeriod8[seq]);
            assert(Sampler::gap_at(seq, 3, Sampler::kDefaultSeed) == kGapsPeriod3[seq]);
        }
        static const uint64_t kFirstPoints[10] = {0, 11, 19, 31, 38, 43, 51, 62, 70, 77};
        uint64_t point = 0;
        for (int i = 0; i < 10; i++) {
            assert(point == kFirstPoints[i]);
            point += Sampler::gap_at(point, 8, Sampler::kDefaultSeed);
        }
        // The two counts the phase gate and cmd/gpu-cuda-profile depend on.
        for (int launches : {500, 4000}) {
            Sampler s(8);
            for (int i = 0; i < launches; i++) s.should_sample();
            printf("  schedule pin: %d launches at period 8 -> %llu sampled\n",
                   launches, (unsigned long long)s.sampled());
        }
        {
            Sampler s(8);
            for (int i = 0; i < 500; i++) s.should_sample();
            assert(s.sampled() == 58);
        }
        {
            Sampler s(8);
            for (int i = 0; i < 4000; i++) s.should_sample();
            assert(s.sampled() == 505);
        }
    }
    // Interning: first sight yields a record, repeats do not.
    {
        KernelNameTable t;
        gpu_kernel_name_v1 rec {};
        assert(t.intern(0xAAAA, "_Z4kAddPfi", &rec));
        assert(rec.kernel_id == 0xAAAA);
        assert(rec.name_len == 10);
        assert(!rec.truncated);
        assert(memcmp(rec.name, "_Z4kAddPfi", 10) == 0);
        assert(!t.intern(0xAAAA, "_Z4kAddPfi", &rec));
        assert(t.size() == 1);
    }
    // Over-long names are truncated AND flagged.
    {
        KernelNameTable t;
        std::string huge(GPU_KERNEL_NAME_MAX + 50, 'x');
        gpu_kernel_name_v1 rec {};
        assert(t.intern(1, huge.c_str(), &rec));
        assert(rec.name_len == GPU_KERNEL_NAME_MAX);
        assert(rec.truncated == 1);
    }
    // Replay hands back every interned name, for a late-attaching consumer.
    {
        KernelNameTable t;
        gpu_kernel_name_v1 rec {};
        t.intern(1, "a", &rec);
        t.intern(2, "b", &rec);
        std::vector<uint64_t> seen;
        t.replay([&](const gpu_kernel_name_v1 &r) { seen.push_back(r.kernel_id); });
        assert(seen.size() == 2);
    }
    // Launches arrive on whatever threads the application uses, so
    // should_sample() must be safe under concurrent callers. Four threads
    // each call it 1000 times on a shared Sampler; the counters must land
    // exactly, and sampled() must equal the number of true returns the
    // threads actually counted -- proving the atomics do what they claim,
    // not just that no crash occurred.
    {
        Sampler s(3);
        std::atomic<uint64_t> true_returns{0};
        std::vector<std::thread> threads;
        for (int t = 0; t < 4; t++) {
            threads.emplace_back([&] {
                for (int i = 0; i < 1000; i++) {
                    if (s.should_sample()) true_returns.fetch_add(1, std::memory_order_relaxed);
                }
            });
        }
        for (auto &th : threads) th.join();
        assert(s.observed() == 4000);
        assert(s.sampled() == true_returns.load());
    }
    // ...and the count it lands on is the SAME count a single-threaded run of
    // the same length lands on. That is not obvious and it is what the phase
    // gate's equality rests on: next_ walks the chain 0, s1, s2, ... whatever
    // the interleaving, because each successor is derived from the value being
    // replaced rather than from how many threads raced for it. Threading can
    // move which launch carries a stack; it cannot move how many do.
    {
        Sampler serial(3);
        for (int i = 0; i < 4000; i++) serial.should_sample();
        for (int trial = 0; trial < 8; trial++) {
            Sampler s(3);
            std::vector<std::thread> threads;
            for (int t = 0; t < 4; t++) {
                threads.emplace_back([&] {
                    for (int i = 0; i < 1000; i++) s.should_sample();
                });
            }
            for (auto &th : threads) th.join();
            if (s.sampled() != serial.sampled()) {
                printf("  FAIL: 4 threads sampled %llu of 4000, one thread sampled %llu:"
                       " the sampled count is not thread-independent\n",
                       (unsigned long long)s.sampled(), (unsigned long long)serial.sampled());
            }
            assert(s.sampled() == serial.sampled());
        }
    }
    printf("sampler_test OK\n");
    return 0;
}
