// Exists so the probe macros can be compiled and inspected without the rest
// of core/. internal/usdt's producer test builds this file.
//
// Declares TWO probes (matching what the stub emits) so this file exercises
// PERFAGENT_USDT_BASE being expanded at more than one call site in a single
// translation unit — a single-probe self-test cannot catch a regression in
// the base-section guard, and one shipped undetected until shim/stub/stub.cc
// composed two probes for the first time.
#include "usdt_probe.h"

PERFAGENT_USDT_SEMAPHORE(gpu_launch_v1);
PERFAGENT_USDT_SEMAPHORE(gpu_exec_v1);

extern "C" void perfagent_selftest_emit(const void *ptr, unsigned long count,
                                        unsigned long seq) {
    if (!PERFAGENT_USDT_ENABLED(gpu_launch_v1)) return;
    PERFAGENT_USDT_PROBE3(gpu_launch_v1, ptr, count, seq);
}

extern "C" void perfagent_selftest_emit_exec(const void *ptr, unsigned long count,
                                             unsigned long seq) {
    if (!PERFAGENT_USDT_ENABLED(gpu_exec_v1)) return;
    PERFAGENT_USDT_PROBE3(gpu_exec_v1, ptr, count, seq);
}
