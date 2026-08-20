// Batching with per-probe sequence numbers and drop accounting.
// A batch never grows: when no consumer is attached, adds are counted and
// discarded, so an unprofiled application pays a branch and nothing else.
#ifndef PERFAGENT_BATCH_H
#define PERFAGENT_BATCH_H

#include <cstdint>
#include <cstddef>
#include <mutex>
#include <type_traits>

namespace perfagent {

using EnabledFn = bool (*)();

// Threading contract: a Batch instance is safe to share between one producer
// thread (calling add()) and one drain thread (calling flush() on a timer).
// All mutating operations and the accessors take an internal mutex; the lock
// is held across the emit_() call itself, because emit_ hands the USDT probe
// a pointer into buf_ and the eBPF consumer reads that memory while the probe
// fires — releasing the lock before emitting would let a concurrent add()
// overwrite buf_ mid-read.
template <typename T, size_t N>
class Batch {
public:
    // The emit callback is typed on T rather than on const void*, and that is
    // load-bearing. The eBPF consumer does not learn a record size from the
    // wire: it derives one from the attach cookie, which is keyed on the
    // *probe* it attached (record_size() in bpf/gpu_usdt.bpf.c, cookieFor()
    // in gpuprobe/consumer.go). Wiring a Batch to the emit thunk of a probe
    // that carries a different record — say Batch<gpu_module_load_v1> (40
    // bytes) through the launch probe (48) — would have the kernel copy
    // 48 * n bytes out of a 40 * n byte buffer, a wild read of 8 * n bytes
    // past the end. With the callback typed, that pairing does not compile.
    using Emit = void (*)(const T *ptr, unsigned long count, unsigned long seq);

    static_assert(N > 0, "a batch must be able to hold a record");
    static_assert(std::is_trivially_copyable<T>::value,
                  "records cross into the kernel by raw copy; T must be trivially copyable");
    static_assert(sizeof(T) % 8 == 0,
                  "USDT ABI records are 8-byte aligned and explicitly padded (spec §6.3)");

    Batch(Emit emit, EnabledFn enabled) : emit_(emit), enabled_(enabled) {}

    // Returns true if the record was accepted into the batch.
    bool add(const T &rec) {
        std::lock_guard<std::mutex> g(mu_);
        if (!enabled_()) { dropped_++; return false; }
        buf_[n_++] = rec;
        if (n_ == N) flush_locked();
        return true;
    }

    void flush() {
        std::lock_guard<std::mutex> g(mu_);
        flush_locked();
    }

    uint64_t seq() const {
        std::lock_guard<std::mutex> g(mu_);
        return seq_;
    }
    uint64_t dropped() const {
        std::lock_guard<std::mutex> g(mu_);
        return dropped_;
    }
    size_t pending() const {
        std::lock_guard<std::mutex> g(mu_);
        return n_;
    }

private:
    // Assumes mu_ is already held.
    void flush_locked() {
        if (n_ == 0) return;
        if (!enabled_()) { dropped_ += n_; n_ = 0; return; }
        emit_(buf_, (unsigned long)n_, (unsigned long)seq_);
        seq_++;
        n_ = 0;
    }

    Emit emit_;
    EnabledFn enabled_;
    mutable std::mutex mu_;
    T buf_[N];
    size_t n_ = 0;
    uint64_t seq_ = 0;
    uint64_t dropped_ = 0;
};

}  // namespace perfagent

#endif
