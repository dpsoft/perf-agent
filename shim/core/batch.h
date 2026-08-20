// Batching with per-probe sequence numbers and drop accounting.
// A batch never grows: when no consumer is attached, adds are counted and
// discarded, so an unprofiled application pays a branch and nothing else.
#ifndef PERFAGENT_BATCH_H
#define PERFAGENT_BATCH_H

#include <cstdint>
#include <cstddef>

namespace perfagent {

using EmitFn = void (*)(const void *ptr, unsigned long count, unsigned long seq);
using EnabledFn = bool (*)();

template <typename T, size_t N>
class Batch {
public:
    Batch(EmitFn emit, EnabledFn enabled) : emit_(emit), enabled_(enabled) {}

    // Returns true if the record was accepted into the batch.
    bool add(const T &rec) {
        if (!enabled_()) { dropped_++; return false; }
        buf_[n_++] = rec;
        if (n_ == N) flush();
        return true;
    }

    void flush() {
        if (n_ == 0) return;
        if (!enabled_()) { dropped_ += n_; n_ = 0; return; }
        emit_(buf_, (unsigned long)n_, (unsigned long)seq_);
        seq_++;
        n_ = 0;
    }

    uint64_t seq() const { return seq_; }
    uint64_t dropped() const { return dropped_; }
    size_t pending() const { return n_; }

private:
    EmitFn emit_;
    EnabledFn enabled_;
    T buf_[N];
    size_t n_ = 0;
    uint64_t seq_ = 0;
    uint64_t dropped_ = 0;
};

}  // namespace perfagent

#endif
