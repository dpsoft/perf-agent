// The one lock over CUPTI, and the reason a future call site cannot forget it.
//
// Issue #99: the adapter deadlocked the profiled application, permanently, on
// the first Tier A burst. It held a mutex over the PC-sampling family and
// nothing at all over cuptiActivityFlushAll, so its 100 ms drain thread and its
// 10 ms burst thread were inside CUPTI at the same time, in different CUPTI
// subsystems, while the application sat in a launch callback. The bug was not a
// wrong lock -- it was a CALL SITE THAT TOOK NO LOCK, added later, next to call
// sites that did.
//
// What the headers actually say
// -----------------------------
// CUPTI documents thread safety per function, and it does so in three of its
// headers: cupti_result.h ("this function is thread safe"), cupti_events.h (32
// such notes) and cupti_callbacks.h (8, including "a subscriber must serialize
// access to cuptiGetCallbackState, cuptiEnableCallback, cuptiEnableDomain and
// cuptiEnableAllDomains ... if called concurrently, the results are
// undefined").
//
// cupti_activity.h and cupti_pcsampling.h contain ZERO thread-safety notes
// between them, in CUDA 13.3. Not "not thread safe" -- unstated. So neither
// cuptiActivityFlushAll nor any PC-sampling entry point is documented as safe
// to call concurrently with anything, and the concurrency the adapter had was
// never sanctioned by the vendor, only untested by us. That makes serialising
// them MANDATORY rather than defensive, and it is why this file exists instead
// of a rule about lock ordering written in a comment.
//
// The discipline, in one sentence
// -------------------------------
// EVERY CUPTI call the adapter initiates goes through a wrapper below, and
// every wrapper takes the one guard. There is no other way to reach CUPTI from
// this adapter: the raw entry points are #defined to identifiers that do not
// exist, at the bottom of this header, so a call site that skips the guard
// FAILS TO COMPILE with a message naming the wrapper it should have used.
// `make -C shim check-cupti-guard` proves that by trying it, the way
// check-cubin-defer proves the CubinView guard.
//
// Lock ordering
// -------------
// perfagent::cupti::guard() is the OUTERMOST lock in this adapter. Every other
// mutex it owns -- the clock fit, the device list, Batch, KernelNameTable,
// ReplayLog, CubinQueue, BurstController, PCDrainSchedule -- is a leaf: each is
// taken and released without calling CUPTI and without acquiring the guard, so
// they may all be taken while holding it and none may be held while acquiring
// it. With one lock over the whole vendor surface there is no ordering between
// OUR locks left to get wrong, which is the point of it being one lock rather
// than a lock per CUPTI subsystem.
//
// The guard is RE-ENTRANT on the owning thread; see core/callguard.h for why
// that is a requirement and not a convenience, and note that every re-entrant
// acquisition is counted.
//
// The one exemption, and its boundary
// -----------------------------------
// cuptiActivityGetNextRecord and cuptiActivityGetNumDroppedRecords, called
// from inside CUPTI's own buffer-completed callback, are NOT guarded, and that
// is deliberate rather than an oversight:
//
//   cuptiActivityFlushAll is documented as "a blocking call" that returns
//   buffers "using the callback registered in cuptiActivityRegisterCallbacks".
//   If CUPTI delivers those buffers on a worker thread while our thread holds
//   the guard across the flush, a guarded callback would block the worker on a
//   lock the flusher holds -- which is the exact deadlock shape this file
//   exists to remove, rebuilt inside the fix.
//
// The boundary is principled: the guard covers calls the adapter initiates on a
// thread of its own choosing. It does not cover the documented way to consume a
// buffer CUPTI has just handed us and, in its own words, "relinquished
// ownership of". The two wrappers carry that in their names so they cannot be
// reached for anywhere else by habit, and they COUNT a call made outside a
// buffer-completed callback rather than trusting the name.
#ifndef PERFAGENT_CUPTI_GUARD_H
#define PERFAGENT_CUPTI_GUARD_H

#include <cupti.h>
#include <cupti_pcsampling.h>

#include "callguard.h"

#include <atomic>
#include <cstdint>

namespace perfagent {
namespace cupti {

// The one guard. Never destroyed, deliberately and for the same reason every
// other object in this adapter is leaked: CUPTI worker threads and the atexit
// handler both reach it during process teardown, and a destroyed mutex under
// them is undefined behaviour in somebody else's process.
inline CallGuard &guard() {
    static CallGuard *g = new CallGuard();
    return *g;
}

// Held across a sequence of CUPTI calls that must not be interleaved with
// another thread's -- stop every context, then flush, for instance. The
// individual wrappers take the guard too; it is re-entrant, so nesting is
// free and the outer scope is what makes the sequence atomic.
class CallScope {
public:
    CallScope() : s_(guard()) {}

private:
    CallGuard::Scope s_;
};

// The teardown path's acquisition: never blocks, and refuses when the guard is
// held by ANY thread including this one. See on_finalize in cupti_adapter.cc.
class TryCallScope {
public:
    TryCallScope() : s_(guard()) {}
    bool owns() const { return s_.owns(); }

private:
    CallGuard::TryScope s_;
};

// ------------------------------------------------- the buffer-completed hole

// True while this thread is inside CUPTI's buffer-completed callback. The two
// unguarded wrappers below check it, so using them anywhere else is counted
// instead of being merely discouraged by their names.
inline bool &in_buffer_completed() {
    static thread_local bool flag = false;
    return flag;
}

inline std::atomic<uint64_t> &misplaced_callback_calls() {
    static std::atomic<uint64_t> *n = new std::atomic<uint64_t>(0);
    return *n;
}

class BufferCompletedScope {
public:
    BufferCompletedScope() { in_buffer_completed() = true; }
    ~BufferCompletedScope() { in_buffer_completed() = false; }
};

// --------------------------------------------------------- guarded wrappers
//
// One per CUPTI entry point the adapter uses. Each takes the guard; none does
// anything else, so the wrapper layer adds a lock and no behaviour.

inline CUptiResult GetTimestamp(uint64_t *ts) {
    CallScope s;
    return cuptiGetTimestamp(ts);
}

inline CUptiResult GetResultString(CUptiResult r, const char **msg) {
    // cupti_result.h: "Thread-safety: this function is thread safe." It is
    // guarded anyway. The exemption list is a liability and one documented
    // exception does not earn a second; this call happens on error paths only.
    CallScope s;
    return cuptiGetResultString(r, msg);
}

inline CUptiResult GetCubinCrc(CUpti_GetCubinCrcParams *p) {
    CallScope s;
    return cuptiGetCubinCrc(p);
}

inline CUptiResult Subscribe(CUpti_SubscriberHandle *sub, CUpti_CallbackFunc cb, void *ud) {
    CallScope s;
    return cuptiSubscribe(sub, cb, ud);
}

inline CUptiResult EnableDomain(uint32_t enable, CUpti_SubscriberHandle sub,
                                CUpti_CallbackDomain domain) {
    CallScope s;
    return cuptiEnableDomain(enable, sub, domain);
}

inline CUptiResult ActivityRegisterCallbacks(CUpti_BuffersCallbackRequestFunc req,
                                             CUpti_BuffersCallbackCompleteFunc done) {
    CallScope s;
    return cuptiActivityRegisterCallbacks(req, done);
}

inline CUptiResult ActivityEnable(CUpti_ActivityKind kind) {
    CallScope s;
    return cuptiActivityEnable(kind);
}

inline CUptiResult ActivityFlushAll(uint32_t flag) {
    CallScope s;
    return cuptiActivityFlushAll(flag);
}

inline CUptiResult PCSamplingEnable(CUpti_PCSamplingEnableParams *p) {
    CallScope s;
    return cuptiPCSamplingEnable(p);
}

inline CUptiResult PCSamplingDisable(CUpti_PCSamplingDisableParams *p) {
    CallScope s;
    return cuptiPCSamplingDisable(p);
}

inline CUptiResult PCSamplingStart(CUpti_PCSamplingStartParams *p) {
    CallScope s;
    return cuptiPCSamplingStart(p);
}

inline CUptiResult PCSamplingStop(CUpti_PCSamplingStopParams *p) {
    CallScope s;
    return cuptiPCSamplingStop(p);
}

inline CUptiResult PCSamplingGetData(CUpti_PCSamplingGetDataParams *p) {
    CallScope s;
    return cuptiPCSamplingGetData(p);
}

inline CUptiResult PCSamplingSetConfigurationAttribute(
    CUpti_PCSamplingConfigurationInfoParams *p) {
    CallScope s;
    return cuptiPCSamplingSetConfigurationAttribute(p);
}

inline CUptiResult PCSamplingGetConfigurationAttribute(
    CUpti_PCSamplingConfigurationInfoParams *p) {
    CallScope s;
    return cuptiPCSamplingGetConfigurationAttribute(p);
}

inline CUptiResult PCSamplingGetNumStallReasons(
    CUpti_PCSamplingGetNumStallReasonsParams *p) {
    CallScope s;
    return cuptiPCSamplingGetNumStallReasons(p);
}

inline CUptiResult PCSamplingGetStallReasons(CUpti_PCSamplingGetStallReasonsParams *p) {
    CallScope s;
    return cuptiPCSamplingGetStallReasons(p);
}

// ------------------------------------------- the two unguarded exemptions
//
// Long names on purpose: these are the only two ways into CUPTI in this
// adapter that do not take the guard, and the name is the warning. See the
// header comment for why guarding them would rebuild the deadlock.

inline CUptiResult ActivityGetNextRecord_InBufferCompletedOnly(uint8_t *buffer,
                                                               size_t valid_size,
                                                               CUpti_Activity **record) {
    if (!in_buffer_completed()) misplaced_callback_calls().fetch_add(1, std::memory_order_relaxed);
    return cuptiActivityGetNextRecord(buffer, valid_size, record);
}

inline CUptiResult ActivityGetNumDroppedRecords_InBufferCompletedOnly(CUcontext ctx,
                                                                      uint32_t stream_id,
                                                                      size_t *dropped) {
    if (!in_buffer_completed()) misplaced_callback_calls().fetch_add(1, std::memory_order_relaxed);
    return cuptiActivityGetNumDroppedRecords(ctx, stream_id, dropped);
}

}  // namespace cupti
}  // namespace perfagent

// ------------------------------------------------------------------- poison
//
// Everything above this line is the only code in the adapter permitted to name
// a raw CUPTI entry point. Below it, the raw names expand to identifiers that
// are declared nowhere, so a new call site that reaches CUPTI without the
// guard is a COMPILE ERROR whose message names the wrapper to use instead.
//
// That is the structural half of the fix for issue #99. The bug was a call
// site that took no lock; this makes such a call site impossible to write in
// this translation unit rather than merely wrong. `make -C shim
// check-cupti-guard` proves it, with a compliant control that must compile so
// the check cannot pass for the wrong reason.
//
// Only function names are poisoned. CUpti_* types, enumerators and the
// CUPTIAPI macro are untouched.
#define cuptiGetTimestamp                        PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_GetTimestamp
#define cuptiGetResultString                     PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_GetResultString
#define cuptiGetCubinCrc                         PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_GetCubinCrc
#define cuptiSubscribe                           PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_Subscribe
#define cuptiEnableDomain                        PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_EnableDomain
#define cuptiActivityRegisterCallbacks           PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityRegisterCallbacks
#define cuptiActivityEnable                      PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityEnable
#define cuptiActivityFlushAll                    PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityFlushAll
#define cuptiActivityGetNextRecord               PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityGetNextRecord_InBufferCompletedOnly
#define cuptiActivityGetNumDroppedRecords        PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_ActivityGetNumDroppedRecords_InBufferCompletedOnly
#define cuptiPCSamplingEnable                    PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingEnable
#define cuptiPCSamplingDisable                   PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingDisable
#define cuptiPCSamplingStart                     PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingStart
#define cuptiPCSamplingStop                      PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingStop
#define cuptiPCSamplingGetData                   PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingGetData
#define cuptiPCSamplingSetConfigurationAttribute PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingSetConfigurationAttribute
#define cuptiPCSamplingGetConfigurationAttribute PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingGetConfigurationAttribute
#define cuptiPCSamplingGetNumStallReasons        PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingGetNumStallReasons
#define cuptiPCSamplingGetStallReasons           PERFAGENT_UNGUARDED_CUPTI_CALL_use_perfagent_cupti_PCSamplingGetStallReasons

#endif
