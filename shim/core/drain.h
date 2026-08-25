// The drain timer and the late-attach replay log.
//
// Drain: both vendors hand over event buffers only when full, so an idle GPU
// delivers nothing for as long as it takes to fill one - measured at 7.6s p50
// and 15s max at 25 launches/s, with nothing delivered until process exit
// (spec §10). CUPTI's own cuptiActivityFlushPeriod cannot help: it returns
// only full buffers. core/ therefore owns the timer.
//
// Replay: module loads, stall maps and device config happen before a consumer
// attaches. Their records are retained and replayed on the unattached ->
// attached transition (spec §6.1).
#ifndef PERFAGENT_DRAIN_H
#define PERFAGENT_DRAIN_H

#include "usdt_abi.h"

#include <atomic>
#include <chrono>
#include <cstdint>
#include <functional>
#include <mutex>
#include <thread>
#include <vector>

namespace perfagent {

class Drainer {
public:
    using TickFn = std::function<void()>;

    ~Drainer() { stop(); }

    void on_tick(TickFn fn) { tick_ = std::move(fn); }

    void start(unsigned period_ms) {
        stop_.store(false);
        thread_ = std::thread([this, period_ms] {
            while (!stop_.load()) {
                std::this_thread::sleep_for(std::chrono::milliseconds(period_ms));
                if (stop_.load()) break;
                if (tick_) tick_();
            }
        });
    }

    void stop() {
        if (!thread_.joinable()) return;
        stop_.store(true);
        thread_.join();
    }

private:
    TickFn tick_;
    std::thread thread_;
    std::atomic<bool> stop_{false};
};

class ReplayLog {
public:
    using ModuleFn = std::function<void(const gpu_module_load_v1 &)>;
    using ConfigFn = std::function<void(const gpu_config_v1 &)>;
    using StallFn = std::function<void(const gpu_stall_reason_map_v1 &)>;

    void on_replay_module(ModuleFn fn) { module_fn_ = std::move(fn); }
    void on_replay_config(ConfigFn fn) { config_fn_ = std::move(fn); }
    void on_replay_stall(StallFn fn) { stall_fn_ = std::move(fn); }

    void record_module(const gpu_module_load_v1 &m) {
        std::lock_guard<std::mutex> g(mu_);
        modules_.push_back(m);
    }

    void record_config(const gpu_config_v1 &c) {
        std::lock_guard<std::mutex> g(mu_);
        config_ = c;
        have_config_ = true;
    }

    // The device's index -> name stall table. Queried once, when PC sampling
    // is first enabled, which on a CUDA process is long before a consumer can
    // realistically have attached -- so without replay the indices on every
    // PC sample would be unresolvable for the whole run. Interned by index so
    // a second context on the same device does not duplicate the table.
    //
    // Returns true when the entry was new, so the caller can emit it live on
    // first sight and leave the replay to this log.
    bool record_stall_reason(const gpu_stall_reason_map_v1 &s) {
        std::lock_guard<std::mutex> g(mu_);
        for (const auto &have : stalls_)
            if (have.index == s.index) return false;
        stalls_.push_back(s);
        return true;
    }

    // Call on every drain tick with the probe's current enabled state.
    void replay_if_newly_attached(bool enabled_now) {
        std::lock_guard<std::mutex> g(mu_);
        if (enabled_now && !was_attached_) {
            // Config, then the stall map, then modules. The order is the
            // order a consumer needs them in: a PC sample's stall index means
            // nothing without the map, and gpuprobe's pending-name handling
            // exists precisely because that guarantee cannot be made for
            // records that were already in flight.
            if (config_fn_ && have_config_) config_fn_(config_);
            if (stall_fn_) for (const auto &s : stalls_) stall_fn_(s);
            if (module_fn_) for (const auto &m : modules_) module_fn_(m);
            replays_++;
        }
        was_attached_ = enabled_now;
    }

    uint64_t replays() const { return replays_; }
    size_t stall_reasons() const { std::lock_guard<std::mutex> g(mu_); return stalls_.size(); }

private:
    mutable std::mutex mu_;
    std::vector<gpu_module_load_v1> modules_;
    std::vector<gpu_stall_reason_map_v1> stalls_;
    gpu_config_v1 config_{};
    bool have_config_ = false;
    bool was_attached_ = false;
    uint64_t replays_ = 0;
    ModuleFn module_fn_;
    ConfigFn config_fn_;
    StallFn stall_fn_;
};

}  // namespace perfagent

#endif
