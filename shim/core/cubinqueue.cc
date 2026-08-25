#include "cubinqueue.h"

#include <cstdlib>
#include <cstring>

namespace perfagent {

bool CubinView::copy_to(void *dst, size_t cap) const noexcept {
    if (bytes_ == nullptr || len_ == 0 || dst == nullptr || cap < len_) return false;
    memcpy(dst, bytes_, len_);
    return true;
}

CubinQueue::CubinQueue(const CubinQueueLimits &limits) : limits_(limits) {}

CubinQueue::~CubinQueue() {
    std::lock_guard<std::mutex> g(mu_);
    for (Entry &e : q_) free(e.bytes);
    q_.clear();
    queued_bytes_ = 0;
}

bool CubinQueue::capture(const CubinView &view, CubinCrcFn crc, CubinCapturedFn on_captured,
                         void *ctx) {
    // Everything before the memcpy is a decision NOT to copy - a bound, or a
    // missing input. Nothing here defers the copy, and nothing here may: see
    // cubinqueue.h and core/cubin_defer_test.cc.
    if (view.empty() || crc == nullptr) return false;
    const size_t len = view.size();
    if (len > limits_.max_cubin_bytes) {
        // Refused whole, never truncated. A truncated cubin parses into a
        // WRONG line table, which is the one failure worse than no line
        // table, and the consumer would have refused it for its size anyway.
        too_large_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }

    void *copy = malloc(len);
    if (copy == nullptr) {
        alloc_failed_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }
    // THE COPY. The vendor's buffer is valid here and provably nowhere else.
    if (!view.copy_to(copy, len)) {
        free(copy);
        alloc_failed_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }

    // Over the copy, never over the view: a CRC and the bytes it names must
    // come from one read of one buffer.
    const uint64_t key = crc(copy, len);
    if (key == 0) {
        // Zero is the ABI's "no module". Sending it would produce a module
        // record that joins to nothing, with every other counter green.
        free(copy);
        crc_failed_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }

    // One lock from the intern check to the push, so two threads loading the
    // same module cannot both decide they are the first and fire two records
    // for one CRC. on_captured runs inside it and is documented as forbidden
    // from re-entering the queue; it is a probe fire, which is a nop when no
    // consumer is attached and a ~1-2us uprobe trap when one is.
    std::unique_lock<std::mutex> g(mu_);

    if (interned_.count(key) != 0) {
        g.unlock();
        free(copy);
        reload_skipped_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }

    // Fired here, before the push, because this is the last moment the copy
    // is owned by nobody else: after the push the drain thread may free it,
    // and gpu_module_load_v1.bytes_ptr would then name freed memory. The
    // record stays accurate exactly because it is emitted here.
    if (on_captured != nullptr) on_captured(ctx, key, copy, len);

    if (q_.size() >= limits_.max_entries || queued_bytes_ + len > limits_.max_queued_bytes) {
        // The OFFER is dropped; the copy has already served its purpose of
        // being taken in time. The module goes unresolvable rather than the
        // application going slow, and this counter is the size of that.
        //
        // Deliberately NOT interned: a later re-load of the same module gets
        // another chance at a queue that may since have drained.
        g.unlock();
        free(copy);
        queue_full_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }

    // Interning is bounded like everything else. Past the bound we stop
    // remembering rather than stop capturing: the consequence of forgetting
    // is a duplicate offer, which the consumer answers with a counted no-op
    // (CubinsDuplicate), and the consequence of not capturing is a module
    // nothing can resolve. The cheaper mistake is the one we make.
    if (interned_.size() < limits_.max_interned) interned_.insert(key);

    Entry e;
    e.crc = key;
    e.bytes = copy;
    e.len = len;
    q_.push_back(e);
    queued_bytes_ += len;
    g.unlock();

    captured_.fetch_add(1, std::memory_order_relaxed);
    return true;
}

size_t CubinQueue::drain(CubinOfferFn offer, unsigned timeout_ms) {
    if (offer == nullptr) return 0;
    size_t budget;
    {
        std::lock_guard<std::mutex> g(mu_);
        budget = q_.size();
    }
    size_t done = 0;
    for (; done < budget; done++) {
        Entry e;
        {
            std::lock_guard<std::mutex> g(mu_);
            if (q_.empty()) break;
            e = q_.front();
            q_.pop_front();
            queued_bytes_ -= e.len;
        }
        // The mutex is NOT held here. An offer can block for its whole
        // timeout, and holding the lock across it would hand that timeout to
        // the application's next module load - which is the exact cost this
        // whole split exists to avoid.
        const CubinOfferResult r = offer(e.bytes, e.len, e.crc, timeout_ms);
        free(e.bytes);
        if (r == kCubinOfferAccepted) {
            sent_.fetch_add(1, std::memory_order_relaxed);
        } else {
            // Broader than cubin.cc's cubins_send_failed(), on purpose: that
            // counter excludes "nobody was listening", because an unprofiled
            // process must not accumulate failures. Here the queue only ever
            // holds bytes when a consumer was believed present, so a
            // no-listener result IS a module the consumer will not have.
            send_failed_.fetch_add(1, std::memory_order_relaxed);
        }
    }
    return done;
}

size_t CubinQueue::depth() const {
    std::lock_guard<std::mutex> g(mu_);
    return q_.size();
}

}  // namespace perfagent
