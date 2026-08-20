// A GPU-free producer. Emits the same records a vendor adapter would, so the
// consumer and the whole pipeline can be exercised on a machine with no GPU
// (spec §14, Phase 3 gate).
#include "batch.h"
#include "clock.h"
#include "drain.h"
#include "usdt_abi.h"
#include "usdt_probe.h"

#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <sys/syscall.h>
#include <thread>
#include <unistd.h>

static inline uint32_t current_tid() { return (uint32_t)syscall(SYS_gettid); }

// One line per probe: the semaphore, the enabled/emit thunks, and the frozen
// wire size the consumer's attach cookie assumes. Because the thunk's
// parameter type and the probe it fires come from the same token, and
// Batch's emit callback is typed on its record, a Batch can only be wired to
// the probe whose records it actually holds -- see PERFAGENT_USDT_EMITTER.
PERFAGENT_USDT_EMITTER(gpu_launch_v1, 48);
PERFAGENT_USDT_EMITTER(gpu_exec_v1, 48);

static uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

// Default visibility: this is the one symbol libperfagent-gpu-stub.so must
// export. Everything else in core/ and this file stays hidden (the Makefile
// builds with -fvisibility=hidden) so the shim does not leak symbols into
// whatever process it's injected into.
extern "C" __attribute__((visibility("default"))) void
perfagent_stub_run(unsigned launches, unsigned period_us) {
    perfagent::Batch<gpu_launch_v1, 32> lb(gpu_launch_v1_emit, gpu_launch_v1_enabled);
    perfagent::Batch<gpu_exec_v1, 32> eb(gpu_exec_v1_emit, gpu_exec_v1_enabled);

    perfagent::Drainer drainer;
    drainer.on_tick([&] { lb.flush(); eb.flush(); });
    drainer.start(100);

    for (unsigned i = 1; i <= launches; i++) {
        const uint64_t now = mono_ns();
        gpu_launch_v1 l{};
        l.correlation = i;                 // never zero: spec §6.1
        l.kernel_id = (i % 2) ? 0x1111 : 0x2222;
        l.queue_id = 1;
        l.context_id = 1;
        l.time_ns = now;
        l.tid = current_tid();
        lb.add(l);

        gpu_exec_v1 e{};
        e.correlation = i;
        e.kernel_id = l.kernel_id;
        e.queue_id = 1;
        e.device_id = 0;
        e.start_ns = now + 10000;          // 10us after the launch
        e.end_ns = now + 10000 + 50000;    // 50us on device
        eb.add(e);

        if (period_us) std::this_thread::sleep_for(std::chrono::microseconds(period_us));
    }

    lb.flush();
    eb.flush();
    drainer.stop();
    fprintf(stderr, "stub: launches=%u launch_dropped=%llu exec_dropped=%llu\n",
            launches, (unsigned long long)lb.dropped(), (unsigned long long)eb.dropped());
}

int main(int argc, char **argv) {
    const unsigned n = argc > 1 ? (unsigned)atoi(argv[1]) : 1000;
    const unsigned us = argc > 2 ? (unsigned)atoi(argv[2]) : 100;
    perfagent_stub_run(n, us);
    return 0;
}
