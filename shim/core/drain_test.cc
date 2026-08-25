#include "drain.h"
#include <cassert>
#include <cstdio>
#include <atomic>
#include <thread>
#include <chrono>
#include <cstring>
#include <vector>

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
    // The stall map replays on the same edge as the config and the modules,
    // and interns by index. A consumer that attaches after PC sampling was
    // enabled -- which is every consumer, since the table is queried at
    // context creation -- gets the table or gets no resolvable stall reason
    // at all for the whole run.
    {
        ReplayLog log;
        struct gpu_config_v1 c {}; c.sm_count = 82;
        struct gpu_stall_reason_map_v1 s0 {}; s0.index = 3; s0.name_len = 4; memcpy(s0.name, "smem", 4);
        struct gpu_stall_reason_map_v1 s1 {}; s1.index = 7; s1.name_len = 3; memcpy(s1.name, "imc", 3);

        log.record_config(c);
        assert(log.record_stall_reason(s0));
        assert(log.record_stall_reason(s1));
        // A second context on the same device queries the same table; the
        // index is the identity, so the duplicate is refused rather than
        // re-emitted.
        assert(!log.record_stall_reason(s0));
        assert(log.stall_reasons() == 2);

        int configs = 0;
        std::vector<uint32_t> order;
        log.on_replay_config([&](const gpu_config_v1 &r) { assert(r.sm_count == 82); configs++; });
        log.on_replay_stall([&](const gpu_stall_reason_map_v1 &r) { order.push_back(r.index); });

        log.replay_if_newly_attached(true);
        assert(configs == 1);
        assert(order.size() == 2 && order[0] == 3 && order[1] == 7);

        log.replay_if_newly_attached(true);      // still attached: no re-replay
        assert(order.size() == 2);
    }

    printf("drain_test OK\n");
    return 0;
}
