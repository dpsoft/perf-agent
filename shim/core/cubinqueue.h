// The two halves of a cubin capture, split by their deadlines rather than by
// taste, and held apart by the type system rather than by a comment.
//
// # Two deadlines, and why collapsing them costs the application
//
// The vendor hands the bytes to a callback that runs on the APPLICATION's
// thread, inside cuModuleLoad. Spec §6.3 finding 2 measured what happens to
// that buffer afterwards: following cuModuleUnload it is still mapped and
// still readable, and its CONTENTS HAVE CHANGED. A late reader therefore gets
// silently wrong bytes rather than a fault - the worst available failure,
// because a wrong cubin parses into a wrong line table and every source line
// derived from it is confidently incorrect. CUDA's lazy loading puts loads
// and unloads at arbitrary points in a long-running process, so there is no
// safe definition of "copy later".
//
//   The COPY is deadline-bound by the vendor's buffer lifetime.
//   The SEND is deadline-bound by nothing at all.
//
// So the copy happens in the callback and the offer does not. A connect()
// plus an up-to-8 MiB handover on cuModuleLoad would stall the application
// for the profiler's benefit. Capture is a bounded malloc, a memcpy and a
// mutexed push; the offer runs on the drain thread, which nothing is waiting
// on. A full queue drops the OFFER and never the copy-in-time decision, so
// the failure mode is a module that reads gpu_src_status "no-module" rather
// than an application that got slower - and cubin_queue_full() is exactly the
// size of that trade.
//
// # Why CubinView is shaped the way it is
//
// "No deferral may sit between callback entry and the memcpy" is a structural
// assertion here, not a comment that a later edit can quietly falsify.
// CubinView is a non-owning view of the vendor's buffer with:
//
//   - no copy constructor, no move constructor, no assignment: it cannot be
//     stored in a queue node, a std::vector, a struct field or a lambda
//     capture-by-value;
//   - deleted operator new: it cannot be heap-allocated and the address kept;
//   - NO ACCESSOR FOR THE POINTER AT ALL. copy_to() is the only member that
//     touches it, and copy_to writes into a buffer the caller already owns.
//
// The consequence is that the borrowed pointer has exactly one consumer -
// the copy - and cannot reach any data structure that outlives the callback.
// Deferring the copy does not compile. core/cubin_defer_test.cc spells five
// ways to try it and shim/Makefile's `test` target fails the build if any of
// them compiles, plus a compliant control that must.
//
// The CRC runs over the COPY, never over the view, for the same reason: the
// crc function is handed the owned bytes, so a CRC and the bytes it names can
// never come from two different reads of a buffer that changed in between.
#ifndef PERFAGENT_CUBINQUEUE_H
#define PERFAGENT_CUBINQUEUE_H

#include "cubin.h"

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <mutex>
#include <unordered_set>

namespace perfagent {

// A non-owning view of the vendor's buffer, valid only for the duration of
// the callback that was handed it. See the header comment: it exists to make
// the pointer unable to escape, so read what it forbids before adding to it.
class CubinView {
public:
    CubinView(const void *bytes, size_t len) noexcept : bytes_(bytes), len_(len) {}

    // Every way of keeping one is deleted. These are the assertion.
    CubinView() = delete;
    CubinView(const CubinView &) = delete;
    CubinView &operator=(const CubinView &) = delete;
    CubinView(CubinView &&) = delete;
    CubinView &operator=(CubinView &&) = delete;
    static void *operator new(size_t) = delete;
    static void *operator new[](size_t) = delete;

    size_t size() const noexcept { return len_; }
    bool empty() const noexcept { return bytes_ == nullptr || len_ == 0; }

    // The ONLY consumer of the borrowed pointer, and deliberately the only
    // member that can reach it. Copies exactly size() bytes into dst, or
    // returns false without writing anything when dst is too small.
    bool copy_to(void *dst, size_t cap) const noexcept;

private:
    // No accessor. Adding one re-opens the escape route the whole type
    // exists to close, and core/cubin_defer_test.cc mode 4 is the test that
    // would then start passing when it must not.
    const void *bytes_;
    size_t len_;
};

// Computes the join key over the OWNED COPY. On the vendor side this wraps
// cuptiGetCubinCrc(); the stub substitutes a documented stand-in. Returning
// zero means "could not", and is counted rather than sent - a zero CRC joins
// to nothing and would be an unresolvable module with every counter green.
typedef uint64_t (*CubinCrcFn)(const void *bytes, size_t len);

// Called once per captured module, on the callback's thread, while the copy
// is owned by nobody else. This is where gpu_module_load_v1 is fired, and it
// is the only moment at which the record's bytes_ptr provably points at live
// adapter-owned bytes. It must not call back into the queue.
typedef void (*CubinCapturedFn)(void *ctx, uint64_t crc, const void *bytes, size_t len);

// The send. Signature-compatible with cubin_offer_to_consumer by design, so
// the production wiring is a name and the tests are a substitute.
typedef CubinOfferResult (*CubinOfferFn)(const void *bytes, size_t len, uint64_t crc,
                                         unsigned timeout_ms);

// All four bounds are on the APPLICATION's side of the trade. Defaults are
// sized for the observed shape of a CUDA process - tens of modules, a few
// hundred KB each - with a per-cubin ceiling matching the consumer's own
// Config.CubinMaxBytes default so a cubin this end copies is not one the
// other end was always going to refuse.
struct CubinQueueLimits {
    size_t max_entries = 32;                     // queued offers
    size_t max_queued_bytes = 64u * 1024 * 1024; // their total
    size_t max_cubin_bytes = 8u * 1024 * 1024;   // one module, the memcpy bound
    size_t max_interned = 4096;                  // distinct CRCs remembered
};

// Bounded, mutex-guarded, and drained by somebody else's thread.
class CubinQueue {
public:
    explicit CubinQueue(const CubinQueueLimits &limits = CubinQueueLimits());
    ~CubinQueue();

    CubinQueue(const CubinQueue &) = delete;
    CubinQueue &operator=(const CubinQueue &) = delete;

    // The callback half. Copies view NOW, computes the CRC over the copy,
    // calls on_captured while the copy is still exclusively ours, and pushes.
    // Returns true only when the copy was made AND enqueued for offer.
    //
    // Every early return has a counter; none is silent.
    bool capture(const CubinView &view, CubinCrcFn crc, CubinCapturedFn on_captured,
                 void *ctx);

    // The drain-thread half. Offers at most the entries present on entry, so
    // a producer that keeps capturing cannot pin this thread here, and never
    // holds the mutex across an offer - doing so would put the offer's whole
    // timeout back onto the application's next capture.
    size_t drain(CubinOfferFn offer, unsigned timeout_ms);

    // The plan's four, plus the three drop paths the plan's four do not
    // cover. A drop with no counter is the failure mode this project has
    // shipped eleven times.
    uint64_t modules_captured() const { return captured_.load(std::memory_order_relaxed); }
    uint64_t module_reload_skipped() const { return reload_skipped_.load(std::memory_order_relaxed); }
    uint64_t cubin_queue_full() const { return queue_full_.load(std::memory_order_relaxed); }
    uint64_t cubin_send_failed() const { return send_failed_.load(std::memory_order_relaxed); }
    uint64_t cubin_too_large() const { return too_large_.load(std::memory_order_relaxed); }
    uint64_t cubin_crc_failed() const { return crc_failed_.load(std::memory_order_relaxed); }
    uint64_t cubin_alloc_failed() const { return alloc_failed_.load(std::memory_order_relaxed); }
    uint64_t cubins_sent() const { return sent_.load(std::memory_order_relaxed); }

    // Queued-but-not-yet-offered. A producer waiting for its offers to land
    // polls this; an operator reads it as the size of what a hard exit would
    // take with it.
    size_t depth() const;

private:
    struct Entry {
        uint64_t crc;
        void *bytes;
        size_t len;
    };

    const CubinQueueLimits limits_;

    mutable std::mutex mu_;
    std::deque<Entry> q_;
    size_t queued_bytes_ = 0;
    std::unordered_set<uint64_t> interned_;

    std::atomic<uint64_t> captured_{0};
    std::atomic<uint64_t> reload_skipped_{0};
    std::atomic<uint64_t> queue_full_{0};
    std::atomic<uint64_t> send_failed_{0};
    std::atomic<uint64_t> too_large_{0};
    std::atomic<uint64_t> crc_failed_{0};
    std::atomic<uint64_t> alloc_failed_{0};
    std::atomic<uint64_t> sent_{0};
};

}  // namespace perfagent

#endif
