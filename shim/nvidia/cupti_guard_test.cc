// A compile-time test with no runtime: it passes by FAILING to compile.
//
// Issue #99's bug was a CALL SITE THAT TOOK NO LOCK -- cuptiActivityFlushAll on
// the drain thread, added next to PC-sampling call sites that all took g_pc_mu.
// The fix's structural claim is that such a call site can no longer be written
// in the adapter: nvidia/cupti_guard.h poisons every raw CUPTI entry point
// after defining a guarded wrapper for each, so reaching CUPTI without the
// guard is a compile error rather than a review finding.
//
// A guarantee nobody has tried to violate is a claim, so this file tries, five
// ways, and shim/Makefile's `check-cupti-guard` target fails the build if any
// of them succeeds.
//
// Build it with -DPERFAGENT_CUPTI_UNGUARDED=<n>:
//
//   0  the COMPLIANT shape. Must compile. Without this control the check would
//      pass just as happily on a file that fails to compile for some unrelated
//      reason -- a typo, a missing include, a CUDA version bump -- and would
//      then be proving nothing at all.
//   1  the exact shape of the bug: cuptiActivityFlushAll from a timer
//   2  the other half of the deadlock: cuptiPCSamplingStop
//   3  a drain that skips the guard: cuptiPCSamplingGetData
//   4  "I'll take the address and call it later" -- the wrapper dodged by
//      indirection
//   5  "the macro is for unqualified names" -- a ::-qualified raw call
#include "cupti_guard.h"

#ifndef PERFAGENT_CUPTI_UNGUARDED
#define PERFAGENT_CUPTI_UNGUARDED 0
#endif

#if PERFAGENT_CUPTI_UNGUARDED == 0

// The compliant shape. Two CUPTI calls that must not interleave with another
// thread's, made atomic by one outer scope; the wrappers take the re-entrant
// guard again inside, which costs a depth increment and no lock.
CUptiResult compliant(CUcontext ctx) {
    perfagent::cupti::CallScope hold;
    CUpti_PCSamplingStopParams sp{};
    sp.size = CUpti_PCSamplingStopParamsSize;
    sp.ctx = ctx;
    const CUptiResult st = perfagent::cupti::PCSamplingStop(&sp);
    if (st != CUPTI_SUCCESS) return st;
    return perfagent::cupti::ActivityFlushAll(0);
}

#elif PERFAGENT_CUPTI_UNGUARDED == 1

// Issue #99 itself: the 100 ms drain tick flushing activity with no lock,
// while the burst thread is inside the PC-sampling API.
void on_tick_unguarded() { cuptiActivityFlushAll(0); }

#elif PERFAGENT_CUPTI_UNGUARDED == 2

// The other thread in the same deadlock.
void burst_close_unguarded(CUcontext ctx) {
    CUpti_PCSamplingStopParams sp{};
    sp.size = CUpti_PCSamplingStopParamsSize;
    sp.ctx = ctx;
    cuptiPCSamplingStop(&sp);
}

#elif PERFAGENT_CUPTI_UNGUARDED == 3

// A new drain path that forgets the guard the existing ones take.
void drain_unguarded(CUcontext ctx, CUpti_PCSamplingData *data) {
    CUpti_PCSamplingGetDataParams gp{};
    gp.size = CUpti_PCSamplingGetDataParamsSize;
    gp.ctx = ctx;
    gp.pcSamplingData = data;
    cuptiPCSamplingGetData(&gp);
}

#elif PERFAGENT_CUPTI_UNGUARDED == 4

// "I'll keep the function pointer and call it from wherever." Indirection does
// not dodge the poison: the name has to be written down somewhere.
using FlushFn = CUptiResult (*)(uint32_t);
FlushFn defer_by_pointer() { return &cuptiActivityFlushAll; }

#elif PERFAGENT_CUPTI_UNGUARDED == 5

// "The macro only catches unqualified names." It does not: the preprocessor
// runs before the qualification means anything.
void qualified_unguarded() { ::cuptiActivityFlushAll(0); }

#endif

int main() { return 0; }
