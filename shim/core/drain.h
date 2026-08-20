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

// usdt_abi.h is written for C (it has its own C-only test, usdt_abi_test.c)
// and uses C11's `_Static_assert`, which is not a C++ keyword -- only bare
// `static_assert` is. This is the first C++ translation unit to include it,
// so the C++ front end has never had to parse those lines before. Rather
// than edit usdt_abi.h (out of scope for this task and shared with the C
// build), alias the spelling for the duration of this one include.
#if defined(__cplusplus) && !defined(_Static_assert)
#define _Static_assert static_assert
#define PERFAGENT_DRAIN_UNDEF_STATIC_ASSERT
#endif
#include "usdt_abi.h"
#ifdef PERFAGENT_DRAIN_UNDEF_STATIC_ASSERT
#undef _Static_assert
#undef PERFAGENT_DRAIN_UNDEF_STATIC_ASSERT
#endif

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

    void on_replay_module(ModuleFn fn) { module_fn_ = std::move(fn); }
    void on_replay_config(ConfigFn fn) { config_fn_ = std::move(fn); }

    void record_module(const gpu_module_load_v1 &m) {
        std::lock_guard<std::mutex> g(mu_);
        modules_.push_back(m);
    }

    void record_config(const gpu_config_v1 &c) {
        std::lock_guard<std::mutex> g(mu_);
        config_ = c;
        have_config_ = true;
    }

    // Call on every drain tick with the probe's current enabled state.
    void replay_if_newly_attached(bool enabled_now) {
        std::lock_guard<std::mutex> g(mu_);
        if (enabled_now && !was_attached_) {
            if (config_fn_ && have_config_) config_fn_(config_);
            if (module_fn_) for (const auto &m : modules_) module_fn_(m);
            replays_++;
        }
        was_attached_ = enabled_now;
    }

    uint64_t replays() const { return replays_; }

private:
    std::mutex mu_;
    std::vector<gpu_module_load_v1> modules_;
    gpu_config_v1 config_{};
    bool have_config_ = false;
    bool was_attached_ = false;
    uint64_t replays_ = 0;
    ModuleFn module_fn_;
    ConfigFn config_fn_;
};

}  // namespace perfagent

#endif
