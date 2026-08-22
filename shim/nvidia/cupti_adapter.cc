// The NVIDIA CUPTI adapter: a vendor plug for shim/core's runtime.
//
// It is loaded into an ordinary CUDA process by the driver, through
// CUDA_INJECTION64_PATH, and turns CUPTI's callbacks and activity records
// into the frozen USDT records in core/usdt_abi.h. Every design point below
// was measured on an RTX 3090 in the Phase 3 spike; the comments say which.
//
// The shape mirrors shim/stub/stub.cc, which wires the same core pieces --
// Batch, Sampler, KernelNameTable, ClockFit, Drainer -- to synthetic events.
// If this file emits what the stub emits, the consumer, the BPF program, the
// stack capture and the projection all work unchanged.
#include "batch.h"
#include "clock.h"
#include "drain.h"
#include "enroll.h"
#include "kernelnames.h"
#include "sampler.h"
#include "usdt_abi.h"
#include "usdt_probe.h"

#include <cupti.h>

#include <atomic>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <mutex>
#include <sys/syscall.h>
#include <unistd.h>

// One line per probe: semaphore, enabled/emit thunks, and the frozen wire
// size the consumer's BPF attach cookie assumes. See PERFAGENT_USDT_EMITTER.
PERFAGENT_USDT_EMITTER(gpu_launch_v1, 48);
PERFAGENT_USDT_EMITTER(gpu_exec_v1, 48);
// gpu_launch_sampled_v1 fires UNBATCHED -- one probe per sampled launch,
// never through Batch<T,N>. The eBPF consumer captures the calling thread's
// stack the instant the probe fires, so a batch would staple one stack onto
// N unrelated launches.
PERFAGENT_USDT_EMITTER(gpu_launch_sampled_v1, 56);
PERFAGENT_USDT_EMITTER(gpu_kernel_name_v1, 272);

namespace {

// ---------------------------------------------------------------- logging

// Silent unless PERFAGENT_GPU_LOG names a destination: this library is
// injected into somebody else's process and must not write to their stderr
// uninvited. "stderr" or "-" means stderr; anything else is a file path.
FILE *g_log = nullptr;

void logf(const char *fmt, ...) {
    if (!g_log) return;
    va_list ap;
    va_start(ap, fmt);
    vfprintf(g_log, fmt, ap);
    va_end(ap);
    fflush(g_log);
}

void open_log() {
    const char *dest = getenv("PERFAGENT_GPU_LOG");
    if (!dest || !*dest) return;
    if (!strcmp(dest, "stderr") || !strcmp(dest, "-")) { g_log = stderr; return; }
    g_log = fopen(dest, "ae");
}

// ------------------------------------------------------------------ clock

uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

uint32_t current_tid() { return (uint32_t)syscall(SYS_gettid); }

// cuptiGetTimestamp is CLOCK_REALTIME, not a device clock: 2000/2000 spike
// trials were bracketed by realtime reads, 0/2000 by monotonic, and the
// offset sat exactly 37s off TAI. Activity record start/end share that
// domain. So the conversion to the CPU-monotonic clock every *_ns field on
// the wire uses is a pure offset -- and the hazard is a realtime STEP, which
// ClockFit already detects and counts.
//
// ClockFit itself is not thread safe (a plain int64 plus a flag) and the
// activity buffers are decoded on a CUPTI worker thread while the drain
// thread resamples, so the mutex is required rather than defensive.
perfagent::ClockFit g_clock;
std::mutex g_clock_mu;

void resample_clock() {
    uint64_t vendor = 0;
    if (cuptiGetTimestamp(&vendor) != CUPTI_SUCCESS) return;
    const uint64_t mono = mono_ns();
    std::lock_guard<std::mutex> g(g_clock_mu);
    g_clock.resample(vendor, mono);
}

// Returns false when the fit has never been seeded, rather than silently
// converting to a nonsense zero-based timestamp.
bool vendor_to_mono(uint64_t vendor_ns, uint64_t *out) {
    std::lock_guard<std::mutex> g(g_clock_mu);
    if (!g_clock.valid()) return false;
    *out = g_clock.to_monotonic(vendor_ns);
    return true;
}

// ------------------------------------------------------------ correlation

// CUPTI's correlationId is a process-wide uint32 counter that WRAPS: at the
// 542k launches/s measured in the spike, 2^32 is exhausted in about 2.2
// hours, and §6.1 requires a unique, non-zero correlation. So the wire
// correlation is (epoch+1) << 32 | id -- epochs are 1-based, which is what
// makes the result non-zero even for id 0.
//
// State is one atomic word, (epoch << 32 | last_id), so an epoch and the id
// that justified it can never be read torn apart.
std::atomic<uint64_t> g_corr{0};

// A backwards jump only counts as a wrap if it is enormous. Launches from
// several threads land here slightly out of order, and a naive `id < last`
// would promote every one of those reorderings into a new epoch.
constexpr uint32_t kWrapGuard = 1u << 31;

uint64_t make_corr(uint32_t epoch, uint32_t id) {
    return ((uint64_t)(epoch + 1) << 32) | (uint64_t)id;
}

// Called on the launch path, which sees ids in (near) issue order and is
// therefore the only place the epoch may advance.
uint64_t correlate_launch(uint32_t id) {
    uint64_t cur = g_corr.load(std::memory_order_relaxed);
    for (;;) {
        const uint32_t last = (uint32_t)cur;
        const uint32_t epoch = (uint32_t)(cur >> 32);
        uint32_t next_epoch = epoch;
        uint64_t next = cur;
        if (last > id && (uint32_t)(last - id) > kWrapGuard) {
            next_epoch = epoch + 1;                       // the counter wrapped
            next = ((uint64_t)next_epoch << 32) | id;
        } else if (id > last) {
            next = ((uint64_t)epoch << 32) | id;          // ordinary advance
        } else {
            return make_corr(epoch, id);                  // a small reordering
        }
        if (g_corr.compare_exchange_weak(cur, next, std::memory_order_relaxed))
            return make_corr(next_epoch, id);
    }
}

// Called on the activity path, which sees ids that were issued in the past
// and must never move the epoch itself. An id far AHEAD of the last launched
// id can only be a record left over from before the most recent wrap.
uint64_t correlate_activity(uint32_t id) {
    const uint64_t cur = g_corr.load(std::memory_order_relaxed);
    const uint32_t last = (uint32_t)cur;
    uint32_t epoch = (uint32_t)(cur >> 32);
    if (id > last && (uint32_t)(id - last) > kWrapGuard && epoch > 0) epoch--;
    return make_corr(epoch, id);
}

// ----------------------------------------------------------- kernel names

// FNV-1a. kernel_id has to be derived from the name alone, because the launch
// callback and the activity record reach it by different routes and must
// agree; a pointer-keyed cache would not survive CUPTI handing back a
// different copy of the same string.
uint64_t hash_name(const char *s) {
    uint64_t h = 1469598103934665603ULL;
    if (!s) s = "<unknown>";
    for (; *s; s++) {
        h ^= (unsigned char)*s;
        h *= 1099511628211ULL;
    }
    return h ? h : 1;   // zero is reserved for "no kernel"
}

// ------------------------------------------------------------------ state

perfagent::Batch<gpu_launch_v1, 32> *g_lb = nullptr;
perfagent::Batch<gpu_exec_v1, 32> *g_eb = nullptr;
perfagent::Sampler *g_sampler = nullptr;
perfagent::KernelNameTable *g_names = nullptr;
perfagent::Drainer *g_drainer = nullptr;
CUpti_SubscriberHandle g_subscriber = nullptr;

std::atomic<unsigned long> g_sampled_seq{0};
std::atomic<unsigned long> g_name_seq{0};
std::atomic<uint64_t> g_launch_ordinal{0};

// Every discard has a counter. Nothing here is allowed to be silent (§6.1).
std::atomic<uint64_t> g_launch_unattached{0};   // no consumer at launch time
std::atomic<uint64_t> g_exec_unattached{0};     // no consumer at activity time
std::atomic<uint64_t> g_exec_no_clock{0};       // fit not seeded yet
std::atomic<uint64_t> g_exec_no_time{0};        // CUPTI reported start==end==0
std::atomic<uint64_t> g_activity_records{0};    // kernel records decoded
std::atomic<uint64_t> g_activity_other{0};      // records of kinds we ignore
std::atomic<uint64_t> g_cupti_dropped{0};       // CUPTI's own overflow count
// Resource callbacks are counted per cbid rather than in one lump: the
// subscription set is RUNTIME_API + RESOURCE (§spike finding 2) and this
// adapter reads none of the resource events yet, so the histogram is the only
// way to see what that subscription actually costs.
constexpr unsigned kResourceCbidMax = 32;
std::atomic<uint64_t> g_resource_events[kResourceCbidMax];
std::atomic<uint64_t> g_buffers{0};
// A buffer CUPTI asked for and we could not allocate. CUPTI's behaviour on a
// declined request is not documented, so whatever it does with the records
// for that window, the refusal itself is on the record here.
std::atomic<uint64_t> g_buffer_alloc_failed{0};

bool g_names_was_attached = false;

// ------------------------------------------------------------ launch path

// The runtime entry points that submit a kernel. CUDA 13 routes
// cudaLaunchKernel through __cudaLaunchKernel, so the older ids alone would
// see nothing on this toolkit; both spellings, and both the default and the
// per-thread-default-stream variants, are listed.
bool is_launch_cbid(CUpti_CallbackId cbid) {
    switch (cbid) {
        case CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernel_v7000:
        case CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernel_ptsz_v7000:
        case CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernelExC_v11060:
        case CUPTI_RUNTIME_TRACE_CBID_cudaLaunchKernelExC_ptsz_v11060:
        case CUPTI_RUNTIME_TRACE_CBID___cudaLaunchKernel_v13000:
        case CUPTI_RUNTIME_TRACE_CBID___cudaLaunchKernel_ptsz_v13000:
            return true;
        default:
            return false;
    }
}

void emit_name_if_new(uint64_t kernel_id, const char *name) {
    if (!gpu_kernel_name_v1_enabled()) return;
    gpu_kernel_name_v1 rec{};
    if (g_names->intern(kernel_id, name ? name : "<unknown>", &rec) &&
        gpu_kernel_name_v1_enabled()) {
        gpu_kernel_name_v1_emit(&rec, 1, g_name_seq.fetch_add(1, std::memory_order_relaxed));
    }
}

void on_launch(const CUpti_CallbackData *cb) {
    // The ordinal and the sampling decision are taken on EVERY launch,
    // attached or not: two relaxed atomics, and keeping them unconditional is
    // what makes launch_seq and the one-in-N period reproducible across an
    // attach that happens mid-run.
    const uint64_t ordinal = g_launch_ordinal.fetch_add(1, std::memory_order_relaxed);
    const bool sample = g_sampler->should_sample();

    // The semaphore gate. Everything past this point -- hashing the kernel
    // name, the intern table's mutex, Batch's mutex -- is work an unprofiled
    // application must not pay, so the whole path is skipped and the launches
    // it skipped are counted instead.
    if (!gpu_launch_v1_enabled() && !gpu_launch_sampled_v1_enabled()) {
        g_launch_unattached.fetch_add(1, std::memory_order_relaxed);
        return;
    }

    const uint64_t kernel_id = hash_name(cb->symbolName);
    emit_name_if_new(kernel_id, cb->symbolName);

    const uint64_t now = mono_ns();

    gpu_launch_v1 l{};
    l.correlation = correlate_launch(cb->correlationId);
    l.kernel_id = kernel_id;
    // queue_id stays zero -- the ABI's "unknown". The launch side cannot
    // derive a stream id that matches the activity record's, and guessing one
    // would produce a queue that never appears again (§6.3 finding 1).
    l.queue_id = 0;
    l.context_id = cb->contextUid;
    l.time_ns = now;
    l.tid = current_tid();
    g_lb->add(l);

    // Unbatched, one record per fire: this probe is the whole reason the
    // consumer can attribute GPU time to a CPU stack.
    if (sample && gpu_launch_sampled_v1_enabled()) {
        gpu_launch_sampled_v1 s{};
        s.correlation = l.correlation;
        s.kernel_id = kernel_id;
        s.queue_id = 0;
        s.context_id = cb->contextUid;
        s.time_ns = now;
        s.tid = l.tid;
        s.sample_period = g_sampler->period();
        s.launch_seq = ordinal;
        gpu_launch_sampled_v1_emit(&s, 1, g_sampled_seq.fetch_add(1, std::memory_order_relaxed));
    }
}

void CUPTIAPI on_callback(void *, CUpti_CallbackDomain domain, CUpti_CallbackId cbid,
                          const void *cbdata) {
    if (domain == CUPTI_CB_DOMAIN_RESOURCE) {
        if (cbid < kResourceCbidMax)
            g_resource_events[cbid].fetch_add(1, std::memory_order_relaxed);
        return;
    }
    if (domain != CUPTI_CB_DOMAIN_RUNTIME_API) return;
    const CUpti_CallbackData *cb = (const CUpti_CallbackData *)cbdata;
    if (cb->callbackSite != CUPTI_API_ENTER) return;
    if (!is_launch_cbid(cbid)) return;
    on_launch(cb);
}

// -------------------------------------------------------------- exec path

// 4 MiB: large enough that a busy process fills one every few tens of ms
// rather than every millisecond, small enough that the drain timer's forced
// handover of a partly filled buffer costs nothing.
constexpr size_t kBufferSize = 4u * 1024 * 1024;
constexpr size_t kBufferAlign = 8;

void CUPTIAPI buffer_requested(uint8_t **buffer, size_t *size, size_t *max_records) {
    void *p = aligned_alloc(kBufferAlign, kBufferSize);
    if (!p) g_buffer_alloc_failed.fetch_add(1, std::memory_order_relaxed);
    *buffer = (uint8_t *)p;
    *size = p ? kBufferSize : 0;
    *max_records = 0;   // pack as many records as fit
}

void handle_kernel(const CUpti_ActivityKernel12 *k) {
    g_activity_records.fetch_add(1, std::memory_order_relaxed);

    if (!gpu_exec_v1_enabled()) {
        g_exec_unattached.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    // CUPTI documents start == end == 0 as "no timestamp could be collected".
    if (k->start == 0 && k->end == 0) {
        g_exec_no_time.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    uint64_t start = 0, end = 0;
    if (!vendor_to_mono(k->start, &start) || !vendor_to_mono(k->end, &end)) {
        g_exec_no_clock.fetch_add(1, std::memory_order_relaxed);
        return;
    }

    const uint64_t kernel_id = hash_name(k->name);
    // Intern from this side too: a kernel whose launch happened before the
    // consumer attached has no name on the wire yet, and the activity record
    // carries the same string the launch callback would have.
    emit_name_if_new(kernel_id, k->name);

    gpu_exec_v1 e{};
    e.correlation = correlate_activity(k->correlationId);
    e.kernel_id = kernel_id;
    // The activity record is authoritative for queue and device: this is the
    // only place a stream id that means anything is available (§6.3 finding 1).
    e.queue_id = k->streamId;
    e.device_id = k->deviceId;
    e.start_ns = start;
    e.end_ns = end;
    g_eb->add(e);
}

void CUPTIAPI buffer_completed(CUcontext, uint32_t, uint8_t *buffer, size_t,
                               size_t valid_size) {
    g_buffers.fetch_add(1, std::memory_order_relaxed);
    if (buffer && valid_size) {
        CUpti_Activity *record = nullptr;
        for (;;) {
            const CUptiResult st = cuptiActivityGetNextRecord(buffer, valid_size, &record);
            if (st == CUPTI_ERROR_MAX_LIMIT_REACHED) break;
            if (st != CUPTI_SUCCESS) break;
            if (record->kind == CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL ||
                record->kind == CUPTI_ACTIVITY_KIND_KERNEL) {
                handle_kernel((const CUpti_ActivityKernel12 *)record);
            } else {
                g_activity_other.fetch_add(1, std::memory_order_relaxed);
            }
        }
    }
    // CUPTI's own overflow. Counted, never silent.
    size_t dropped = 0;
    if (cuptiActivityGetNumDroppedRecords(nullptr, 0, &dropped) == CUPTI_SUCCESS && dropped)
        g_cupti_dropped.fetch_add(dropped, std::memory_order_relaxed);
    free(buffer);   // free(nullptr) is a no-op, and a declined request lands here too
}

// ----------------------------------------------------------------- report

void report(const char *why) {
    logf("perfagent-cupti: %s pid=%d launches=%llu sampled=%llu period=%u "
         "launch_unattached=%llu launch_batch_dropped=%llu "
         "activity_kernels=%llu activity_other=%llu buffers=%llu buffer_alloc_failed=%llu "
         "exec_unattached=%llu exec_batch_dropped=%llu exec_no_clock=%llu "
         "exec_no_time=%llu cupti_dropped=%llu names=%zu "
         "clock_steps=%llu clock_offset_ns=%lld\n",
         why, (int)getpid(),
         (unsigned long long)g_sampler->observed(),
         (unsigned long long)g_sampler->sampled(), g_sampler->period(),
         (unsigned long long)g_launch_unattached.load(),
         (unsigned long long)g_lb->dropped(),
         (unsigned long long)g_activity_records.load(),
         (unsigned long long)g_activity_other.load(),
         (unsigned long long)g_buffers.load(),
         (unsigned long long)g_buffer_alloc_failed.load(),
         (unsigned long long)g_exec_unattached.load(),
         (unsigned long long)g_eb->dropped(),
         (unsigned long long)g_exec_no_clock.load(),
         (unsigned long long)g_exec_no_time.load(),
         (unsigned long long)g_cupti_dropped.load(),
         g_names->size(),
         (unsigned long long)g_clock.steps(),
         (long long)g_clock.offset_ns());
    for (unsigned i = 0; i < kResourceCbidMax; i++) {
        const uint64_t n = g_resource_events[i].load();
        if (n) logf("perfagent-cupti: resource cbid=%u count=%llu\n", i,
                    (unsigned long long)n);
    }
}

// ------------------------------------------------------------------ drain

// CUPTI hands a buffer back only when it is FULL. On an idle GPU that is up
// to 15 seconds of nothing, and cuptiActivityFlushPeriod does not help --- it
// flushes full buffers only. The timer is ours (§10); it bounded delivery to
// ~100ms in the spike at no measurable cost.
void on_tick() {
    // Resample first, so the conversion the flush is about to feed through
    // ClockFit is the freshest offset available.
    resample_clock();
    cuptiActivityFlushAll(0);
    g_lb->flush();
    g_eb->flush();

    // Late attach: replay the interned names exactly once, on the
    // unattached -> attached edge, so an already-attached consumer is not
    // re-sent the whole table every 100ms (§6.1's replay contract).
    const bool now_attached = gpu_kernel_name_v1_enabled();
    if (now_attached && !g_names_was_attached) {
        g_names->replay([](const gpu_kernel_name_v1 &r) {
            if (gpu_kernel_name_v1_enabled())
                gpu_kernel_name_v1_emit(&r, 1, g_name_seq.fetch_add(1, std::memory_order_relaxed));
        });
    }
    g_names_was_attached = now_attached;
}

// The documented CUPTI shutdown hook. Without it the records still sitting in
// a partly filled CUPTI buffer at exit are lost, and so is whatever is in the
// batches.
void at_exit_handler() {
    cuptiActivityFlushAll(1);
    if (g_lb) g_lb->flush();
    if (g_eb) g_eb->flush();
    report("exit");
}

unsigned env_uint(const char *name, unsigned dflt) {
    const char *v = getenv(name);
    if (!v || !*v) return dflt;
    const long n = strtol(v, nullptr, 10);
    if (n <= 0) return dflt;
    return (unsigned)n;
}

bool check(CUptiResult r, const char *what) {
    if (r == CUPTI_SUCCESS) return true;
    const char *msg = "?";
    cuptiGetResultString(r, &msg);
    logf("perfagent-cupti: %s failed: %s\n", what, msg);
    return false;
}

}  // namespace

// The one exported symbol. `extern "C"` alone is NOT enough: this library is
// built with -fvisibility=hidden, which hides an extern "C" function just as
// thoroughly as a C++ one, and the CUDA driver then fails to find the entry
// point and moves on WITHOUT SAYING ANYTHING. That silence cost real time in
// the spike; the attribute is the fix.
extern "C" __attribute__((visibility("default"))) int InitializeInjection(void) {
    static std::atomic<bool> done{false};
    bool expected = false;
    if (!done.compare_exchange_strong(expected, true)) return 1;

    open_log();

    // The #49 startup rendezvous, and this is the one place in a CUDA process
    // where it can be done: the driver dlopened us during cuInit, so libcuda,
    // libcupti and the application are all mapped, and NO kernel has been
    // launched yet. Blocking here until the consumer has installed this
    // process's CFI tables is what makes the kernel-side walk of the first
    // sampled launch -- and of every launch during what would otherwise be a
    // ~73ms libcuda compile -- find tables instead of falling through to the
    // frame-pointer path.
    //
    // BEFORE cuptiSubscribe, not after: subscribing is what makes on_callback
    // reachable, and on_callback is the only thing that fires a probe. Doing
    // the rendezvous first means no probe of any kind can have fired from
    // this process before it completes -- not even from another thread
    // already inside a CUDA call.
    //
    // Only when a consumer is attached (the semaphore is what says so, and it
    // was armed by the kernel when this library was mapped), bounded by
    // PERFAGENT_GPU_ENROLL_TIMEOUT_MS, and never fatal: see core/enroll.h.
    perfagent::EnrollResult enrolled = perfagent::kEnrollDisabled;
    if (gpu_launch_sampled_v1_enabled()) {
        enrolled = perfagent::enroll_with_consumer(perfagent::enroll_timeout_ms(2000));
    }

    // Leaked on purpose, never deleted: CUPTI worker threads keep calling
    // buffer_completed during process teardown, and a destroyed Batch or
    // Sampler under them is a use-after-free in somebody else's process.
    g_lb = new perfagent::Batch<gpu_launch_v1, 32>(gpu_launch_v1_emit, gpu_launch_v1_enabled);
    g_eb = new perfagent::Batch<gpu_exec_v1, 32>(gpu_exec_v1_emit, gpu_exec_v1_enabled);
    g_sampler = new perfagent::Sampler(env_uint("PERFAGENT_GPU_SAMPLE_PERIOD", 8));
    g_names = new perfagent::KernelNameTable();
    g_drainer = new perfagent::Drainer();

    if (!check(cuptiSubscribe(&g_subscriber, (CUpti_CallbackFunc)on_callback, nullptr),
               "cuptiSubscribe"))
        return 0;
    // RUNTIME_API and RESOURCE only. Adding DRIVER_API more than doubled
    // correlation-id consumption in the spike (2.242 vs 1.004 ids per launch),
    // which halves the time to a uint32 wrap, and buys this ABI nothing.
    check(cuptiEnableDomain(1, g_subscriber, CUPTI_CB_DOMAIN_RUNTIME_API),
          "enable RUNTIME_API");
    check(cuptiEnableDomain(1, g_subscriber, CUPTI_CB_DOMAIN_RESOURCE),
          "enable RESOURCE");

    check(cuptiActivityRegisterCallbacks(buffer_requested, buffer_completed),
          "cuptiActivityRegisterCallbacks");
    check(cuptiActivityEnable(CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL),
          "enable CONCURRENT_KERNEL");

    // Seed the fit before any activity record can be converted.
    resample_clock();

    const unsigned drain_ms = env_uint("PERFAGENT_GPU_DRAIN_MS", 100);
    g_drainer->on_tick(on_tick);
    g_drainer->start(drain_ms);

    atexit(at_exit_handler);

    logf("perfagent-cupti: initialized pid=%d sample_period=%u drain_ms=%u "
         "clock_offset_ns=%lld enroll=%s\n",
         (int)getpid(), g_sampler->period(), drain_ms, (long long)g_clock.offset_ns(),
         perfagent::enroll_result_name(enrolled));
    return 1;
}
