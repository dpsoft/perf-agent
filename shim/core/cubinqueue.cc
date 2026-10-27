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
    for (Entry &e : pending_) free(e.bytes);
    pending_.clear();
    pending_bytes_ = 0;
    for (Entry &e : q_) free(e.bytes);
    q_.clear();
    queued_bytes_ = 0;
}

bool CubinQueue::capture(const CubinView &view, uint64_t module_id, uint64_t load_ns) {
    // Everything before the memcpy is a decision NOT to copy - a bound, or a
    // missing input. Nothing here defers the copy, and nothing here may: see
    // cubinqueue.h and core/cubin_defer_test.cc.
    if (view.empty()) return false;
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

    // And that is all the callback does. The CRC is NOT taken here: on the
    // vendor side it takes the CUPTI guard, and this thread may be inside
    // cuLaunchKernel holding a driver lock the guard's holder is waiting
    // for (issue #211). settle() computes it, over this same copy, on a
    // thread that holds nothing the driver needs.
    std::unique_lock<std::mutex> g(mu_);
    if (pending_.size() >= limits_.max_pending_entries ||
        pending_bytes_ + len > limits_.max_pending_bytes) {
        // More modules inside one settle tick than the pending bounds hold.
        // Unlike a full offer queue this loses the record as well as the
        // bytes, because the CRC the record carries is never computed --
        // which is why it has a counter of its own.
        g.unlock();
        free(copy);
        pending_full_.fetch_add(1, std::memory_order_relaxed);
        return false;
    }
    Entry e;
    e.crc = 0;
    e.module_id = module_id;
    e.load_ns = load_ns;
    e.bytes = copy;
    e.len = len;
    pending_.push_back(e);
    pending_bytes_ += len;
    return true;
}

size_t CubinQueue::settle(CubinCrcFn crc, CubinCapturedFn on_captured, void *ctx) {
    if (crc == nullptr) return 0;
    size_t budget;
    {
        std::lock_guard<std::mutex> g(mu_);
        budget = pending_.size();
    }
    size_t done = 0;
    for (; done < budget; done++) {
        Entry e;
        {
            std::lock_guard<std::mutex> g(mu_);
            if (pending_.empty()) break;
            e = pending_.front();
            pending_.pop_front();
            pending_bytes_ -= e.len;
        }

        // Over the copy, never over the view: a CRC and the bytes it names
        // must come from one read of one buffer. The mutex is NOT held: this
        // is a vendor call behind the CUPTI guard, and a capture() blocked on
        // mu_ behind it is the #211 cycle again with one more lock in it.
        e.crc = crc(e.bytes, e.len);
        if (e.crc == 0) {
            // Zero is the ABI's "no module". Sending it would produce a
            // module record that joins to nothing, with every other counter
            // green.
            free(e.bytes);
            crc_failed_.fetch_add(1, std::memory_order_relaxed);
            continue;
        }

        // Only settle() touches interned_, and only one thread settles, so
        // two loads of one module can no longer both decide they are first:
        // they are settled one after the other on the same thread.
        if (interned_.count(e.crc) != 0) {
            free(e.bytes);
            reload_skipped_.fetch_add(1, std::memory_order_relaxed);
            continue;
        }

        // Fired here, before the push, because this is the last moment the
        // copy is owned by nobody else: after the push drain() may free it,
        // and gpu_module_load_v1.bytes_ptr would then name freed memory.
        if (on_captured != nullptr)
            on_captured(ctx, e.module_id, e.load_ns, e.crc, e.bytes, e.len);

        std::unique_lock<std::mutex> g(mu_);
        if (q_.size() >= limits_.max_entries || queued_bytes_ + e.len > limits_.max_queued_bytes) {
            // The OFFER is dropped; the copy has already served its purpose
            // of being taken in time, and the record above has told the
            // consumer the module exists. The module goes unresolvable
            // rather than the application going slow, and this counter is
            // the size of that.
            //
            // Deliberately NOT interned: a later re-load of the same module
            // gets another chance at a queue that may since have drained.
            g.unlock();
            free(e.bytes);
            queue_full_.fetch_add(1, std::memory_order_relaxed);
            continue;
        }
        q_.push_back(e);
        queued_bytes_ += e.len;
        g.unlock();

        // Interning is bounded like everything else. Past the bound we stop
        // remembering rather than stop capturing: the consequence of
        // forgetting is a duplicate offer, which the consumer answers with a
        // counted no-op (CubinsDuplicate), and the consequence of not
        // capturing is a module nothing can resolve. The cheaper mistake is
        // the one we make.
        if (interned_.size() < limits_.max_interned) interned_.insert(e.crc);
        captured_.fetch_add(1, std::memory_order_relaxed);
    }
    return done;
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
            // process must not accumulate failures. Here the caller only
            // drains when it believes a consumer is present -- the adapter
            // retains rather than drains while unattached, precisely so an
            // offer is never made into the void -- so a no-listener result IS
            // a module the consumer will not have.
            send_failed_.fetch_add(1, std::memory_order_relaxed);
        }
    }
    return done;
}

size_t CubinQueue::depth() const {
    std::lock_guard<std::mutex> g(mu_);
    return q_.size();
}

size_t CubinQueue::pending() const {
    std::lock_guard<std::mutex> g(mu_);
    return pending_.size();
}

}  // namespace perfagent
