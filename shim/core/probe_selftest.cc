// Exists so the probe macros can be compiled and inspected without the rest
// of core/. internal/usdt's producer test builds this file.
#include "usdt_probe.h"

PERFAGENT_USDT_SEMAPHORE(gpu_launch_v1);

extern "C" void perfagent_selftest_emit(const void *ptr, unsigned long count,
                                        unsigned long seq) {
    if (!PERFAGENT_USDT_ENABLED(gpu_launch_v1)) return;
    PERFAGENT_USDT_PROBE3(gpu_launch_v1, ptr, count, seq);
}
