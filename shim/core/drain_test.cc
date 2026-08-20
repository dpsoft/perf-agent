#include "drain.h"
#include <cassert>
#include <cstdio>
#include <atomic>
#include <thread>
#include <chrono>

using perfagent::Drainer;
using perfagent::ReplayLog;

int main() {
    // The timer ticks on its own thread until stopped.
    {
        std::atomic<int> ticks{0};
        Drainer d;
        d.on_tick([&] { ticks++; });
        d.start(20);
        std::this_thread::sleep_for(std::chrono::milliseconds(130));
        d.stop();
        const int seen = ticks.load();
        assert(seen >= 3);            // ~6 expected; loose for slow CI
        std::this_thread::sleep_for(std::chrono::milliseconds(60));
        assert(ticks.load() == seen); // stopped means stopped
    }

    // Replay fires on the transition from unattached to attached, once.
    {
        ReplayLog log;
        struct gpu_module_load_v1 m {}; m.cubin_crc = 0xabc;
        log.record_module(m);
        int emitted = 0;
        log.on_replay_module([&](const gpu_module_load_v1 &r) {
            assert(r.cubin_crc == 0xabc);
            emitted++;
        });

        log.replay_if_newly_attached(false);
        assert(log.replays() == 0 && emitted == 0);

        log.replay_if_newly_attached(true);
        assert(log.replays() == 1 && emitted == 1);

        log.replay_if_newly_attached(true);   // still attached: no re-replay
        assert(log.replays() == 1 && emitted == 1);

        log.replay_if_newly_attached(false);  // detach
        log.replay_if_newly_attached(true);   // re-attach replays again
        assert(log.replays() == 2 && emitted == 2);
    }
    printf("drain_test OK\n");
    return 0;
}
