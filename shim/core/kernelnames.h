// kernel_id -> name interning, with replay for late-attaching consumers.
// Names are variable-length and repeat constantly, so they must not ride on
// every execution record (spec §6.3).
#ifndef PERFAGENT_KERNELNAMES_H
#define PERFAGENT_KERNELNAMES_H

#include "usdt_abi.h"

#include <cstring>
#include <functional>
#include <mutex>
#include <unordered_set>
#include <vector>

namespace perfagent {

class KernelNameTable {
public:
    // Fills *out and returns true the first time this id is seen.
    bool intern(uint64_t id, const char *name, gpu_kernel_name_v1 *out) {
        std::lock_guard<std::mutex> g(mu_);
        if (seen_.count(id)) return false;

        gpu_kernel_name_v1 rec {};
        rec.kernel_id = id;
        size_t n = name ? strlen(name) : 0;
        if (n > GPU_KERNEL_NAME_MAX) { n = GPU_KERNEL_NAME_MAX; rec.truncated = 1; }
        rec.name_len = (uint16_t)n;
        if (n) memcpy(rec.name, name, n);

        seen_.insert(id);
        records_.push_back(rec);
        *out = rec;
        return true;
    }

    void replay(const std::function<void(const gpu_kernel_name_v1 &)> &fn) {
        std::vector<gpu_kernel_name_v1> copy;
        { std::lock_guard<std::mutex> g(mu_); copy = records_; }
        for (const auto &r : copy) fn(r);   // callback outside the lock
    }

    size_t size() const { std::lock_guard<std::mutex> g(mu_); return records_.size(); }

private:
    mutable std::mutex mu_;
    std::unordered_set<uint64_t> seen_;
    std::vector<gpu_kernel_name_v1> records_;
};

}  // namespace perfagent

#endif
