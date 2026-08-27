// One lock, held across every call the adapter makes into a vendor library
// that does not document concurrent entry.
//
// Why this class exists (issue #99)
// ---------------------------------
// The CUPTI adapter deadlocked the profiled application on the first Tier A
// burst. Three threads, two of them ours:
//
//   application  __cudaLaunchKernel_helper -> CUPTI callback -> mutex
//   burst timer  cuptiPCSamplingStop                         -> rwlock
//   drain timer  cuptiActivityFlushAll                        (inside CUPTI)
//   CUPTI worker                                             -> rwlock
//
// The adapter held a mutex over the PC-sampling family and nothing at all over
// the activity flush, so two of its own threads were inside CUPTI, in
// different subsystems, at the same time. Neither cupti_activity.h nor
// cupti_pcsampling.h documents a single entry point in either family as thread
// safe -- while cupti_result.h, cupti_events.h and cupti_callbacks.h document
// theirs per function -- so the concurrency was never sanctioned, only
// untested.
//
// The three properties this class has, and why each one is load bearing
// --------------------------------------------------------------------
// 1. MUTUAL EXCLUSION between threads. That is the fix.
//
// 2. RE-ENTRANCY on one thread, counted. A vendor library may deliver a
//    callback on the very thread that is already inside a call we made -- the
//    CUPTI adapter's own reports list "can cuptiPCSamplingGetData re-enter our
//    resource callback" and "can cuptiPCSamplingStop" as open questions that
//    need hardware to answer. With a plain std::mutex that is a self-deadlock,
//    and widening the mutex to cover more of the vendor surface widens the
//    hazard rather than narrowing it. Same-thread re-entry is not concurrency,
//    so permitting it is sound for the purpose this lock serves; every
//    re-entrant acquisition is COUNTED so the answer to that open question
//    arrives as a number on the next hardware run instead of as a hang.
//
// 3. try_enter_uncontended(), which fails when the lock is held BY ANYONE,
//    the calling thread included. A fatal-error callback that must tear the
//    session down can neither block (it would hang the host) nor proceed while
//    a call is in flight (it would tear down underneath one). std::mutex has
//    nothing that answers that question: try_lock() on a mutex the calling
//    thread already owns is undefined behaviour, so the obvious spelling is
//    not merely awkward, it is wrong.
//
// No vendor headers, no CUDA, no clock: it is a lock and its accounting, so
// core/callguard_test.cc proves all three properties on a machine with no GPU.
#ifndef PERFAGENT_CALLGUARD_H
#define PERFAGENT_CALLGUARD_H

#include <atomic>
#include <cstdint>
#include <mutex>

namespace perfagent {

// A per-thread identity that costs no syscall: the address of a thread_local
// object is unique per thread and stable for its lifetime. Not a tid -- a tid
// is fine to log and wrong to compare after a thread exits and the number is
// reused.
inline uintptr_t call_guard_self() {
    static thread_local char tag = 0;
    return (uintptr_t)&tag;
}

class CallGuard {
public:
    // Blocking acquisition. Re-entrant on the owning thread.
    void enter() {
        const uintptr_t self = call_guard_self();
        if (owner_.load(std::memory_order_acquire) == self) {
            // Only this thread can have written `self` there, and it has not
            // released, so reading it is proof we hold the lock.
            depth_++;
            reentries_.fetch_add(1, std::memory_order_relaxed);
            if (depth_ > max_depth_) max_depth_ = depth_;
            return;
        }
        if (!mu_.try_lock()) {
            waits_.fetch_add(1, std::memory_order_relaxed);
            mu_.lock();
        }
        owner_.store(self, std::memory_order_release);
        depth_ = 1;
        if (max_depth_ < 1) max_depth_ = 1;
        entries_.fetch_add(1, std::memory_order_relaxed);
    }

    void leave() {
        if (depth_ > 1) {
            depth_--;
            return;
        }
        depth_ = 0;
        owner_.store(0, std::memory_order_release);
        mu_.unlock();
    }

    // For a path that must not block and must not proceed under an in-flight
    // call: fails if the lock is held by ANY thread, this one included.
    //
    // The self-held case is refused rather than granted, and that is the whole
    // reason this is not std::unique_lock(mu, try_to_lock). A teardown that
    // ran re-entrantly from inside a vendor call would disable the subsystem
    // the outer call is standing in.
    bool try_enter_uncontended() {
        if (owner_.load(std::memory_order_acquire) == call_guard_self()) {
            try_failed_self_.fetch_add(1, std::memory_order_relaxed);
            return false;
        }
        if (!mu_.try_lock()) {
            try_failed_other_.fetch_add(1, std::memory_order_relaxed);
            return false;
        }
        owner_.store(call_guard_self(), std::memory_order_release);
        depth_ = 1;
        entries_.fetch_add(1, std::memory_order_relaxed);
        return true;
    }

    // Whether this thread is inside the guard. The wrappers assert on it, so
    // a call site that reaches the vendor without the lock is counted rather
    // than merely commented against.
    bool held_by_this_thread() const {
        return owner_.load(std::memory_order_acquire) == call_guard_self();
    }

    // Every counter is assertable at a known value from a test, which is the
    // standing rule here: a lock that stopped being taken must not look like a
    // workload that stopped calling.
    uint64_t entries() const { return entries_.load(std::memory_order_relaxed); }
    uint64_t reentries() const { return reentries_.load(std::memory_order_relaxed); }
    uint64_t waits() const { return waits_.load(std::memory_order_relaxed); }
    uint64_t try_failed_self() const { return try_failed_self_.load(std::memory_order_relaxed); }
    uint64_t try_failed_other() const { return try_failed_other_.load(std::memory_order_relaxed); }
    // Read only while holding the guard, or after every thread has left.
    unsigned max_depth() const { return max_depth_; }

    // Blocking RAII. This is what call sites use.
    class Scope {
    public:
        explicit Scope(CallGuard &g) : g_(g) { g_.enter(); }
        ~Scope() { g_.leave(); }
        Scope(const Scope &) = delete;
        Scope &operator=(const Scope &) = delete;

    private:
        CallGuard &g_;
    };

    // Non-blocking RAII for the teardown path. owns() is false when the lock
    // was held by anybody, including this thread.
    class TryScope {
    public:
        explicit TryScope(CallGuard &g) : g_(g), owns_(g.try_enter_uncontended()) {}
        ~TryScope() {
            if (owns_) g_.leave();
        }
        bool owns() const { return owns_; }
        TryScope(const TryScope &) = delete;
        TryScope &operator=(const TryScope &) = delete;

    private:
        CallGuard &g_;
        bool owns_;
    };

private:
    std::mutex mu_;
    // Written only by the owning thread (under mu_), read by any thread.
    std::atomic<uintptr_t> owner_{0};
    // Touched only by the owner while it holds mu_, so plain ints are correct.
    unsigned depth_ = 0;
    unsigned max_depth_ = 0;
    std::atomic<uint64_t> entries_{0};
    std::atomic<uint64_t> reentries_{0};
    std::atomic<uint64_t> waits_{0};
    std::atomic<uint64_t> try_failed_self_{0};
    std::atomic<uint64_t> try_failed_other_{0};
};

}  // namespace perfagent

#endif
