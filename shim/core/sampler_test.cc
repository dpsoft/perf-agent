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
    printf("sampler_test OK\n");
    return 0;
}
