// CallGuard: the three properties issue #99's fix rests on, proven without a
// GPU, a CUDA toolkit or CUPTI.
//
// The deadlock this class exists to prevent cannot be reproduced on this
// machine -- it needs an RTX 3090 and a CUPTI that deadlocks. What CAN be
// proven here is the lock discipline itself: that two threads are never inside
// at once, that one thread may re-enter without hanging, that the teardown
// path's try never blocks and never grants itself entry from inside a call,
// and that every one of those outcomes is counted rather than silent.
#include "callguard.h"

#include <atomic>
#include <cassert>
#include <cstdio>
#include <thread>
#include <vector>

using perfagent::CallGuard;

int main() {
    // Ordinary acquisition and release, and the counters that say so.
    {
        CallGuard g;
        assert(!g.held_by_this_thread());
        {
            CallGuard::Scope s(g);
            assert(g.held_by_this_thread());
        }
        assert(!g.held_by_this_thread());
        assert(g.entries() == 1);
        assert(g.reentries() == 0);
        assert(g.max_depth() == 1);
    }

    // RE-ENTRANCY. A vendor library that delivers a callback on the thread
    // already inside a call we made must not hang the host. Three deep,
    // counted, and still held throughout.
    {
        CallGuard g;
        CallGuard::Scope a(g);
        {
            CallGuard::Scope b(g);
            {
                CallGuard::Scope c(g);
                assert(g.held_by_this_thread());
                assert(g.max_depth() == 3);
            }
            assert(g.held_by_this_thread());   // still held at depth 2
        }
        assert(g.held_by_this_thread());       // and at depth 1
        assert(g.entries() == 1);              // one acquisition...
        assert(g.reentries() == 2);            // ...and two re-entries
    }

    // try_enter_uncontended REFUSES when this thread already holds the lock.
    // This is the property std::mutex cannot express: try_lock() on a mutex
    // the calling thread owns is undefined behaviour, and "succeed" would let
    // a fatal-error teardown run underneath an in-flight vendor call.
    {
        CallGuard g;
        CallGuard::Scope s(g);
        CallGuard::TryScope t(g);
        assert(!t.owns());
        assert(g.try_failed_self() == 1);
        assert(g.try_failed_other() == 0);
    }

    // ...and when ANOTHER thread holds it, without blocking.
    {
        CallGuard g;
        std::atomic<bool> held{false}, release{false};
        std::thread other([&] {
            CallGuard::Scope s(g);
            held.store(true);
            while (!release.load()) std::this_thread::yield();
        });
        while (!held.load()) std::this_thread::yield();
        {
            CallGuard::TryScope t(g);
            assert(!t.owns());
        }
        assert(g.try_failed_other() == 1);
        assert(g.try_failed_self() == 0);
        release.store(true);
        other.join();
        // Free again once the holder left.
        CallGuard::TryScope t(g);
        assert(t.owns());
    }

    // MUTUAL EXCLUSION, which is the fix itself: no two threads inside at
    // once. The counter is incremented non-atomically on purpose -- under a
    // guard that does not exclude, the reads and writes race and the total
    // comes out short. `inside` additionally catches an overlap the arithmetic
    // would have absorbed.
    {
        CallGuard g;
        long counter = 0;
        std::atomic<int> inside{0};
        std::atomic<int> overlaps{0};
        std::vector<std::thread> ts;
        constexpr int kThreads = 8;
        constexpr int kIters = 2000;
        for (int t = 0; t < kThreads; t++) {
            ts.emplace_back([&] {
                for (int i = 0; i < kIters; i++) {
                    CallGuard::Scope s(g);
                    if (inside.fetch_add(1) != 0) overlaps.fetch_add(1);
                    counter++;
                    // Re-enter from inside, as a nested vendor callback would.
                    {
                        CallGuard::Scope inner(g);
                        counter++;
                    }
                    inside.fetch_sub(1);
                }
            });
        }
        for (auto &t : ts) t.join();
        assert(overlaps.load() == 0);
        assert(counter == (long)kThreads * kIters * 2);
        assert(g.entries() == (uint64_t)kThreads * kIters);
        assert(g.reentries() == (uint64_t)kThreads * kIters);
        assert(g.max_depth() == 2);
    }

    // A guard released by re-entrant scopes is genuinely free afterwards: an
    // unbalanced leave would leave the mutex locked and the next acquisition
    // from another thread would hang rather than fail an assert.
    {
        CallGuard g;
        {
            CallGuard::Scope a(g);
            CallGuard::Scope b(g);
        }
        std::thread other([&] {
            CallGuard::Scope s(g);
            assert(g.held_by_this_thread());
        });
        other.join();
        assert(!g.held_by_this_thread());
        assert(g.entries() == 2);
    }

    printf("callguard_test OK\n");
    return 0;
}
