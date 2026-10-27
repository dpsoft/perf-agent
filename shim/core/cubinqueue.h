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
//
// # Three steps, not two: the CRC is not computed in the callback
//
// capture() copies and returns. settle() -- on the adapter's own thread --
// computes the CRC, de-duplicates, fires gpu_module_load_v1 and hands the
// entry to drain(). The CRC used to be taken inside capture(), and that
// deadlocked the profiled process (issue #211): on the vendor side the CRC is
// cuptiGetCubinCrc, which takes the adapter's one CUPTI guard, and the
// callback can fire from inside cuLaunchKernel (lazy module loading) on an
// application thread that holds a driver lock. The timer thread, holding the
// guard across cuptiActivityFlushAll, waited for a CUPTI worker that waited
// for that driver lock. An application thread inside a vendor callback must
// never wait for the guard, and the way to make that structural rather than
// a rule is for the callback half to take no CRC function at all: capture()
// has no parameter through which one could be passed.
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

// Called once per settled module, on settle()'s thread, while the copy is
// owned by nobody else: it has left the pending list and has not yet been
// handed to drain(), so nothing can have offered and freed it. This is where
// gpu_module_load_v1 is fired, and it is the only moment at which the
// record's bytes_ptr provably points at live adapter-owned bytes. It must not
// call back into the queue.
//
// module_id and load_ns are what capture() was given, carried through
// unchanged: the record's load time is the callback's, not the settle tick's,
// so moving the CRC off the application's thread does not move the timestamp.
typedef void (*CubinCapturedFn)(void *ctx, uint64_t module_id, uint64_t load_ns, uint64_t crc,
                                const void *bytes, size_t len);

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
    // Sized for RETENTION, not for a 100 ms hop.
    //
    // These bounds used to describe a queue that was drained every drain
    // tick, so 32 entries was generous: nothing could accumulate. Since the
    // adapter retains captures while no consumer is attached (a late attach
    // would otherwise find every module already gone -- measured 100% loss),
    // the queue holds a process's modules for as long as nobody has arrived,
    // and 32 is not a process's worth of modules.
    //
    // 512 and 64 MiB are the CONSUMER's ModuleStore defaults, taken
    // deliberately rather than invented: there is no value in retaining more
    // modules than the store on the other end can hold, and no reason to
    // retain fewer. max_queued_bytes is the real bound -- it is the memory an
    // opted-in process can be made to hold -- and max_entries is the count
    // guard beside it.
    size_t max_entries = 512;                    // queued offers
    size_t max_queued_bytes = 64u * 1024 * 1024; // their total
    size_t max_cubin_bytes = 8u * 1024 * 1024;   // one module, the memcpy bound
    size_t max_interned = 4096;                  // distinct CRCs remembered

    // The pending list: copies taken in the callback and not yet settled.
    // It is emptied every settle tick, so it only fills when more than
    // max_entries modules (or max_queued_bytes of them) load inside ONE
    // tick. It is bounded separately from the offer queue on purpose: a
    // retained queue that is full must still let a new module reach settle(),
    // because settle() is where its gpu_module_load_v1 fires, and the
    // consumer learning THAT a module loaded is worth more than its bytes.
    // The price is that the worst case briefly holds both bounds' worth.
    size_t max_pending_entries = 512;
    size_t max_pending_bytes = 64u * 1024 * 1024;
};

// Bounded, mutex-guarded, and drained by somebody else's thread.
class CubinQueue {
public:
    explicit CubinQueue(const CubinQueueLimits &limits = CubinQueueLimits());
    ~CubinQueue();

    CubinQueue(const CubinQueue &) = delete;
    CubinQueue &operator=(const CubinQueue &) = delete;

    // The callback half. Copies view NOW and parks the copy as pending.
    // Calls nothing it was not built with -- no CRC, no record, no vendor
    // API -- so an application thread inside a vendor callback waits on at
    // most this queue's own leaf mutex. Returns true when the copy was made
    // and parked.
    //
    // Every early return has a counter; none is silent.
    bool capture(const CubinView &view, uint64_t module_id, uint64_t load_ns);

    // The settle half, on the adapter's own thread and never on an
    // application's: for each pending copy, computes the CRC over it, skips a
    // CRC already seen, fires on_captured, and enqueues the copy for drain().
    // Settles at most the entries present on entry, and holds the mutex
    // across neither crc nor on_captured -- crc is a vendor call that takes
    // the CUPTI guard, and a capture() waiting on our mutex behind it would
    // rebuild issue #211 one lock further down. Returns how many it settled.
    //
    // Must be called from one thread at a time: the intern set is its alone.
    size_t settle(CubinCrcFn crc, CubinCapturedFn on_captured, void *ctx);

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
    uint64_t cubin_pending_full() const { return pending_full_.load(std::memory_order_relaxed); }
    uint64_t cubins_sent() const { return sent_.load(std::memory_order_relaxed); }

    // Settled-but-not-yet-offered. A producer waiting for its offers to land
    // polls this.
    size_t depth() const;

    // Captured-but-not-yet-settled. depth() + pending() is the size of what
    // a hard exit would take with it.
    size_t pending() const;

private:
    struct Entry {
        uint64_t crc;   // 0 while pending: the CRC is settle()'s to compute
        uint64_t module_id;
        uint64_t load_ns;
        void *bytes;
        size_t len;
    };

    const CubinQueueLimits limits_;

    mutable std::mutex mu_;
    std::deque<Entry> pending_;
    size_t pending_bytes_ = 0;
    std::deque<Entry> q_;
    size_t queued_bytes_ = 0;
    // settle()'s alone, so it is not under mu_.
    std::unordered_set<uint64_t> interned_;

    std::atomic<uint64_t> captured_{0};
    std::atomic<uint64_t> reload_skipped_{0};
    std::atomic<uint64_t> queue_full_{0};
    std::atomic<uint64_t> send_failed_{0};
    std::atomic<uint64_t> too_large_{0};
    std::atomic<uint64_t> crc_failed_{0};
    std::atomic<uint64_t> alloc_failed_{0};
    std::atomic<uint64_t> pending_full_{0};
    std::atomic<uint64_t> sent_{0};
};

}  // namespace perfagent

#endif
