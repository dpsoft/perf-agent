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
#include "burst.h"
#include "clock.h"
#include "cubin.h"
#include "cubinqueue.h"
#include "drain.h"
#include "enroll.h"
#include "kernelnames.h"
#include "pcdrain.h"
#include "pctier.h"
#include "sampler.h"
#include "tickplan.h"
#include "usdt_abi.h"
#include "usdt_probe.h"

#include <cupti.h>
// cuptiGetCubinCrc lives here, not in cupti.h -- so this header is needed by
// the module-capture path as well as by the whole PC-sampling block below.
#include <cupti_pcsampling.h>

// MUST come after the CUPTI headers and before everything else in this file.
// It defines a guarded wrapper for every CUPTI entry point this adapter uses
// and then POISONS the raw names, so from this line down a call site that
// reaches CUPTI without the one lock is a compile error rather than a hang in
// somebody else's process. That is issue #99's fix; read that header first.
#include "cupti_guard.h"

#include <atomic>
#include <cstdarg>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <dlfcn.h>
#include <mutex>
#include <sys/syscall.h>
#include <unistd.h>
#include <vector>

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
// gpu_module_load_v1 fires UNBATCHED, one record per captured module. Module
// loads are tens per process rather than hundreds of thousands per second, so
// a batch would only delay the record behind a drain tick for no saving -- and
// the record must be emitted at a very particular instant (see
// on_cubin_captured), which a batch's flush would move.
PERFAGENT_USDT_EMITTER(gpu_module_load_v1, 40);
// PC sampling. All of these fire only when PERFAGENT_GPU_PC_SAMPLING is set
// AND a consumer is attached; the semaphore gate is inside the emitter, the
// tier gate is g_pc_enabled.
PERFAGENT_USDT_EMITTER(gpu_pc_sample_batch_v1, 40);
PERFAGENT_USDT_EMITTER(gpu_stall_reason_map_v1, 136);
PERFAGENT_USDT_EMITTER(gpu_config_v1, 24);
// Tier A ONLY, and the whole of that tier's honesty obligation: one record
// per PC-sampling burst, so the consumer can say which executions ran while
// kernels were serialized. Never fired in Tier B --- nothing is serialized
// there, so there is no window to disclose.
PERFAGENT_USDT_EMITTER(gpu_sampling_window_v1, 24);
// Producer-side loss of every class, including the two PC-sampling omissions
// CUPTI documents and cannot recover.
PERFAGENT_USDT_EMITTER(gpu_dropped_v1, 16);

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
    if (perfagent::cupti::GetTimestamp(&vendor) != CUPTI_SUCCESS) return;
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
// THE timer. Singular, and that is the structural half of issue #99's fix: the
// adapter has exactly one thread of its own from which it ever calls CUPTI, so
// "two of our threads inside CUPTI at once" is not a state this process can
// reach. g_tick_plan rate-limits the 100 ms activity drain on top of the
// shorter burst tick; see core/tickplan.h.
perfagent::Drainer *g_timer = nullptr;
perfagent::TickPlan *g_tick_plan = nullptr;
perfagent::CubinQueue *g_cubins = nullptr;
CUpti_SubscriberHandle g_subscriber = nullptr;

std::atomic<unsigned long> g_sampled_seq{0};
std::atomic<unsigned long> g_name_seq{0};
std::atomic<unsigned long> g_module_seq{0};
std::atomic<uint64_t> g_launch_ordinal{0};

// The offer budget, read once at init so the drain thread does not re-parse
// an environment variable every 100ms. Zero disables offers outright.
unsigned g_cubin_timeout_ms = 0;
// True when the startup rendezvous confirmed a consumer. See capture_enabled.
bool g_consumer_enrolled = false;

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
// A MODULE_LOADED callback we declined to copy because no consumer was
// believed present, and one whose descriptor carried no bytes at all. Both
// are modules that will read gpu_src_status "no-module" later, so both are
// counted here rather than being invisible.
std::atomic<uint64_t> g_module_unattached{0};
std::atomic<uint64_t> g_module_no_bytes{0};

bool g_names_was_attached = false;

// ------------------------------------------------------------ module path

// Whether a module's bytes are worth copying at all.
//
// The copy is a bounded memcpy on the APPLICATION's cuModuleLoad path, so an
// unprofiled process must not pay it. But the probe semaphore alone is the
// wrong gate here and issue #49 is why: at InitializeInjection the semaphore
// read ZERO on the RTX 3090 across three runs even though four thousand
// probes fired later in the same process, because it answers "has the kernel
// told this process yet" and not "is a consumer attached". CUDA's lazy
// loading can put the first MODULE_LOADED very close to that moment, and a
// module missed there is missed permanently -- there is no "copy it later"
// (spec 6.3 finding 2).
//
// So the gate is the same one #49's second fix settled on, plus the
// semaphore: the rendezvous CONNECT succeeded, which the consumer performs
// before it creates the uprobe link, or the semaphore has since armed. In an
// unprofiled process both read false and no module is ever copied.
bool capture_enabled() {
    return g_consumer_enrolled || gpu_module_load_v1_enabled();
}

// The join key, over the adapter-owned COPY -- CubinQueue hands this function
// the copy and cannot hand it anything else, which is the point of the shape
// in cubinqueue.h.
//
// Returning 0 means "could not". A zero CRC on the wire would be a module
// record joining to nothing, so CubinQueue counts it and sends nothing.
uint64_t cupti_cubin_crc(const void *bytes, size_t len) {
    CUpti_GetCubinCrcParams p;
    memset(&p, 0, sizeof(p));
    p.size = CUpti_GetCubinCrcParamsSize;
    p.cubinSize = len;
    p.cubin = bytes;
    // Takes the CUPTI guard, like every other call this adapter initiates.
    // It runs on the APPLICATION's cuModuleLoad path, so it can now wait
    // behind one of the timer thread's CUPTI calls -- see issue-99's report
    // for the size of that and why it is the right trade.
    if (perfagent::cupti::GetCubinCrc(&p) != CUPTI_SUCCESS) return 0;
    return p.cubinCrc;
}

// Fired by CubinQueue::capture, on the application's thread, at the one
// instant the copy is owned by nobody else: the CRC is computed and the entry
// has not been pushed, so the drain thread cannot yet have offered and freed
// it. That is what keeps bytes_ptr accurate.
//
// bytes_ptr stays in the ABI and stays true, and it is still NOT a transport:
// it points into this process's address space and the consumer would need
// CAP_SYS_PTRACE to read it. The bytes travel over the cubin channel
// (core/cubin.h); this record announces THAT a module loaded, with its CRC
// and size.
void on_cubin_captured(void *ctx, uint64_t crc, const void *bytes, size_t len) {
    if (!gpu_module_load_v1_enabled()) return;
    gpu_module_load_v1 r{};
    r.cubin_crc = crc;
    r.module_id = (uint64_t)(uintptr_t)ctx;
    r.size_bytes = (uint64_t)len;
    r.load_ns = mono_ns();
    r.bytes_ptr = (uint64_t)(uintptr_t)bytes;
    gpu_module_load_v1_emit(&r, 1, g_module_seq.fetch_add(1, std::memory_order_relaxed));
}

// CUPTI_CBID_RESOURCE_MODULE_LOADED.
//
// Everything this function does with the vendor's buffer happens before it
// returns, and the type system is what says so: CubinView has no copy, no
// move, no assignment, no operator new and no accessor for its pointer, so
// the pointer has exactly one consumer -- the memcpy inside capture(). See
// core/cubinqueue.h and `make -C shim check-cubin-defer`, which fails the
// build if any of five ways to defer the copy starts compiling.
//
// The reason the deadline is real: CUPTI's header says the module data is
// valid only within this callback, and the spike measured what "invalid"
// means here -- after cuModuleUnload the buffer is still mapped and still
// readable, with DIFFERENT CONTENTS. A deferred read gets silently wrong
// bytes, which parse into a wrong line table and produce confidently
// incorrect source lines. A fault would have been the kinder failure.
void on_module_loaded(const CUpti_ResourceData *rd) {
    const CUpti_ModuleResourceData *m =
        (const CUpti_ModuleResourceData *)rd->resourceDescriptor;
    if (!m || !m->pCubin || m->cubinSize == 0) {
        g_module_no_bytes.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    // g_cubins is constructed before cuptiSubscribe, so a callback cannot
    // arrive ahead of it -- but a null here would be a segfault in somebody
    // else's process, which is not a way to find that out.
    if (!capture_enabled() || !g_cubins) {
        g_module_unattached.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    const perfagent::CubinView view(m->pCubin, m->cubinSize);
    // moduleId travels as the context, not as a captured pointer: nothing
    // about this call may outlive the callback except the owned copy.
    g_cubins->capture(view, cupti_cubin_crc, on_cubin_captured,
                      (void *)(uintptr_t)m->moduleId);
}

// Defined further down with the rest of the process setup; declared here
// because the PC-sampling block below is the first thing that needs them.
bool check(CUptiResult r, const char *what);
unsigned env_uint(const char *name, unsigned dflt);

// --------------------------------------------------------- PC sampling
//
// Two collection tiers, mutually exclusive, both OFF BY DEFAULT. Nothing
// below runs, allocates or calls CUPTI unless PERFAGENT_GPU_PC_SAMPLING names
// a tier, so merging either of them cannot degrade a profiler that is shipping
// today. OFF MEANS OFF: no PC buffer, no extra CUPTI domain, no cupti PC entry
// point and no PC-sampling probe fire. See core/pctier.h for the parse and for
// why naming both tiers is refused rather than resolved.
//
// Tier B --- PERFAGENT_GPU_PC_SAMPLING=continuous (1),
// CUPTI_PC_SAMPLING_COLLECTION_MODE_CONTINUOUS. Kernels are NOT serialized in this mode, which is the only
// reason it is a candidate for always-on profiling; the cost is that every PC
// record's correlationId is zero, so a PC sample joins to a kernel through its
// module and never to the launch that issued it.
//
// Tier A --- PERFAGENT_GPU_PC_SAMPLING=serialized (2),
// CUPTI_PC_SAMPLING_COLLECTION_MODE_KERNEL_SERIALIZED with ENABLE_START_STOP_CONTROL, duty-cycled by
// core/burst.h. CUPTI populates correlationId on every PC record here, so a
// sample joins to a launch --- and therefore to a CPU stack --- exactly. The
// price is that every kernel that runs while a burst is open runs SERIALIZED,
// which perturbs the very durations the profile reports. Three things follow
// and all three are implemented below rather than documented and skipped:
//
//   1. The perturbation is BOUNDED by the duty cycle (core/burst.h), not left
//      to run continuously.
//   2. The perturbation is DISCLOSED: every burst emits gpu_sampling_window_v1
//      the moment it opens and again when it closes, and the consumer marks
//      every execution overlapping a window gpu_serialized="true". The window,
//      not the set of sampled kernels, is the honest unit --- every kernel
//      that ran inside a burst ran serialized whether it was sampled or not.
//   3. Tier A REFUSES to run where CUDA graphs have been observed. A graph
//      launch fires one runtime callback for N kernels, so N executions share
//      one correlation and Tier A's exactness claim becomes false while still
//      looking exact. The refusal is loud and counted (g_tier_a_graph_refused),
//      never a silent downgrade to Tier B.
//
// Start/Stop, not Enable/Disable. cuptiPCSamplingEnable/Disable tears the
// configuration down and rebuilds it, which is not what the start/stop control
// exists for; Start/Stop is the documented way to duty-cycle a configured
// context. CUPTI additionally requires a PC-data flush "after every range end
// i.e. cuptiPCSamplingStop()" in this configuration, so the stop path drains
// immediately and marks the shared PCDrainSchedule so the periodic tick does
// not redundantly repeat it.
//
// What CUPTI will not tell us, which the profile must state rather than imply
// -------------------------------------------------------------------------
// cupti_pcsampling.h documents two omissions on CUpti_PCSamplingData:
//
//   "CUPTI does not provide PC records for non-user kernels."
//   "CUPTI does not provide PC records for instructions for which all
//    selected stall reason metrics counts are zero."
//
// Neither is a drop and no counter can recover the records. The first has a
// size --- nonUsrKernelsTotalSamples --- and that number rides out as a
// gpu_dropped_v1 under GPU_DROP_CLASS_PC_NON_USER_KERNEL, so a reader can see
// how much of the device's sampled time this mechanism structurally cannot
// attribute. The second has no size at all: an instruction that never stalled
// for any selected reason simply is not in the data, and the profile can only
// say so in prose. Neither is presented as a measurement of anything else.
//
// totalSamples is NOT loss --- it is every sample the hardware took, including
// the ones that became records. The identity
//
//   totalSamples == sum(record counts) + droppedSamples + nonUsrKernelsTotalSamples
//                   + the all-zero-stall instructions
//
// is therefore checkable only here, in this process's log, and the last term
// is unobservable, so even here it is an inequality. It is stated as a
// limitation and never claimed as a check; closing it would need a
// gpu_config_v2 field for totalSamples, which is not worth a version bump for
// a diagnostic.

// The sampling period CUPTI takes is an EXPONENT: one sample per 2^period SM
// cycles, valid 5..31. Zero means "leave CUPTI's own default alone", and in
// that case the value is read back rather than guessed, so gpu_config_v1
// reports what the device is actually doing.
constexpr uint32_t kPCPeriodMin = 5;
constexpr uint32_t kPCPeriodMax = 31;

// How many distinct PCs one GetData call may return.
//
// cupti_pcsampling.h documents collectNumPcs purely as a CAPACITY --- "Number
// of PCs to be collected", sizing the buffer the caller hands over. It is not
// only that. CUPTI also WITHHOLDS data until it can nearly fill the number
// asked for, which makes this value decide how often any data arrives at all.
// That behaviour is undocumented and it was measured (issue #102), on a 3090 /
// CUDA 13.3, at the 50 ms Tier A burst length:
//
//   collectNumPcs    bursts yielding nothing        PCs
//         32            1 / 124   (  0%)           2553
//         64            1 / 125   (  0%)           2484
//        128            1 / 125   (  0%)           2308
//        256           93 / 124   ( 75%)            150
//        512          124 / 124   (100%)              0
//       2048          124 / 124   (100%)              0
//
// The old default was 2048, chosen as "six times the measured need" from the
// Phase 3 spike's 352 records in CONTINUOUS mode. In CONTINUOUS that reasoning
// held, because nothing there depends on when a buffer fills. For Tier A it
// meant every burst serialized its kernels, paid the throughput cost in full,
// and collected NOTHING --- while bursts, duty, start_failed, stop_failed and
// getdata_failed all read healthy.
//
// 64 rather than 32: the same yield, with headroom. And well below 256 rather
// than just below it, because where the cliff sits depends on how fast the
// workload produces PCs --- a quieter GPU fills any given buffer more slowly,
// so the safe direction is smaller. g_bursts_empty is what says the cliff has
// moved on some other device instead of leaving it silent.
//
// The drain loop below keeps calling while remainingNumPcs is non-zero, so
// this bounds one call's allocation, not the run's data.
constexpr size_t kPCDefaultCollectNumPcs = 64;

// The bound on that loop. CUPTI hands back remainingNumPcs and we keep
// pulling, but an unbounded loop inside a drain tick is a hang in somebody
// else's process; hitting the bound is counted rather than retried forever.
constexpr unsigned kPCMaxDrainRounds = 64;

// The selected tier, and the two booleans derived from it. All three come
// from ONE parse of ONE variable (core/pctier.h), so the tiers are mutually
// exclusive by construction rather than by a check that can be forgotten.
perfagent::PCSamplingTier g_pc_tier = perfagent::PCSamplingTier::kOff;
bool g_pc_enabled = false;                // g_pc_tier != kOff
bool g_pc_tier_a = false;                 // g_pc_tier == kSerialized
// A setting that named no tier we know, or named two. Counted rather than only
// logged: a startup log line in somebody else's process is routinely swallowed
// by whatever captures its stderr, and "PC sampling produced nothing" and "PC
// sampling was refused at startup" must not look the same in the report.
std::atomic<uint64_t> g_pc_tier_refused{0};
uint32_t g_pc_period = 0;                 // the exponent actually in force
size_t g_pc_collect_num_pcs = kPCDefaultCollectNumPcs;
size_t g_pc_scratch_bytes = 0;            // 0 = CUPTI's default
size_t g_pc_hw_buffer_bytes = 0;          // 0 = CUPTI's default

// The device's stall-reason table. Queried once, from the first context that
// enables sampling: the indices are the device's own and are not stable across
// devices or driver versions, which is why gpu_stall_reason_map_v1 exists at
// all. Read-only after the query, so the drain path needs no lock for it.
std::vector<uint32_t> g_stall_indices;
size_t g_num_stall_reasons = 0;
bool g_stall_queried = false;

// Per-context PC sampling state. Enable, configure, drain and disable are all
// per CUcontext in CUPTI --- not per process --- so a process with two
// contexts that only tracked one would silently sample half its work.
struct PCContext {
    CUcontext ctx = nullptr;
    CUpti_PCSamplingData data{};
    std::vector<CUpti_PCSamplingPCData> pcs;
    std::vector<CUpti_PCSamplingStallReason> stalls;
    // Cumulative counters CUPTI reports; the wire carries deltas so a
    // consumer can add drop records up without double counting.
    uint64_t last_dropped = 0;
    uint64_t last_non_user = 0;
    bool enabled = false;
};

// The per-context map and buffers are covered by the SAME lock every CUPTI
// call in this adapter takes: perfagent::cupti::guard(), declared in
// nvidia/cupti_guard.h.
//
// It used to be a mutex of its own, g_pc_mu, and that is what issue #99 was.
// It serialised the PC-sampling family against itself and nothing else, so the
// 100 ms drain timer's cuptiActivityFlushAll -- which took no lock at all --
// ran concurrently with the burst timer's cuptiPCSamplingStop. Two of our
// threads inside CUPTI, in different CUPTI subsystems, with the application in
// a launch callback: the profiled process deadlocked, permanently, on the
// first Tier A burst.
//
// One lock over the whole vendor surface, rather than one per subsystem, is
// what removes the ordering question between our own locks entirely. Holding
// it across cuptiPCSamplingGetData is still what makes the module-unload drain
// safe: that callback runs on the application's thread while the timer runs on
// ours, and CUPTI's buffer is a single-writer structure. The unload path still
// BLOCKS rather than trying the lock, because a skipped unload flush is
// exactly the silent PC-identity corruption this whole path exists to avoid.
std::vector<PCContext *> g_pc_ctxs;       // leaked with everything else; see below

perfagent::PCDrainSchedule *g_pc_schedule = nullptr;
perfagent::Batch<gpu_pc_sample_batch_v1, 32> *g_pcb = nullptr;
perfagent::ReplayLog *g_replay = nullptr;

std::atomic<unsigned long> g_pc_seq{0};
std::atomic<unsigned long> g_stall_seq{0};
std::atomic<unsigned long> g_config_seq{0};
std::atomic<unsigned long> g_dropped_seq{0};
std::atomic<unsigned long> g_window_seq{0};

// ------------------------------------------------------ Tier A duty cycle
//
// The burst controller. It used to have a timer thread of its own, and issue
// #99 is why it no longer does: two timer threads meant two of our threads
// could be inside CUPTI at once, which deadlocked the profiled application.
//
// The burst cycle still cannot be expressed on a 100 ms drain tick -- a 50 ms
// burst quantized to 100 ms silently doubles the burst length and therefore
// the duty fraction, the one number this tier exists to bound. So the SINGLE
// timer runs at the burst tick (a fifth of the burst length) and the activity
// drain is rate-limited on top of it by core/tickplan.h. The burst poll keeps
// its granularity exactly; what it loses is isolation from a slow flush, which
// is measured in `duty=` rather than assumed away.
//
// The flush CUPTI requires after every range end still runs on the stop,
// immediately, inside the same critical section as the stop, and marks the
// SHARED PCDrainSchedule so the periodic drain coalesces instead of repeating
// it.
perfagent::BurstController *g_burst = nullptr;
// Bursts and their total open time. The pair is what bounds the perturbation:
// burst_ns / wall_ns is the fraction of the run that ran serialized, and it is
// REPORTED rather than assumed to equal the configured ceiling.
std::atomic<uint64_t> g_sampling_bursts{0};
std::atomic<uint64_t> g_sampling_burst_ns{0};
// Windows actually put on the wire. Two per burst on the ordinary path --- one
// open at the start, one closed at the end --- so the consumer sees an open
// window if the process dies mid-burst instead of seeing nothing at all.
std::atomic<uint64_t> g_windows_emitted{0};
// cuptiPCSamplingStart / Stop failures. MUST be 0 on a healthy run: a failed
// start means a window was announced for a burst that never sampled, and a
// failed stop means kernels stayed serialized past the window's end.
std::atomic<uint64_t> g_burst_start_failed{0};
std::atomic<uint64_t> g_burst_stop_failed{0};
// The CUDA-graph refusal. Non-zero means Tier A stopped, permanently, because
// exact launch attribution had become false. It is never zero-and-silent: the
// same condition also rides gpu_dropped_v1 under GPU_DROP_CLASS_GRAPH_EXEC.
std::atomic<uint64_t> g_tier_a_graph_refused{0};

// Issue #101. Tier A's cuptiPCSamplingStart/Stop run on the APPLICATION's
// thread, from inside the launch callback; a timer of ours must never make
// them. This is the counter that says so, and it is the reason the timer's tid
// is recorded at all: taking a PC start or stop on the timer thread is exactly
// the deadlock this design exists to remove, so it is worth a number rather
// than a comment. MUST be 0 on every run, healthy or not.
std::atomic<uint64_t> g_timer_tid{0};
std::atomic<uint64_t> g_burst_calls_on_timer{0};
// The pre-start device sync. cuCtxSynchronize is resolved at runtime rather
// than linked: this adapter deliberately does not link libcuda, and libcuda is
// unconditionally present by the time a launch callback runs. missing MUST be
// 0 on any run that opened a burst --- a non-zero value means bursts started
// against a busy GPU, which is the condition parcagpu's comment says CUPTI
// does not want.
std::atomic<uint64_t> g_burst_sync_missing{0};
std::atomic<uint64_t> g_burst_sync_failed{0};
// Launch boundaries the burst path actually saw. Both must move on a Tier A
// run: zero opens with a non-zero launch count would mean the transitions are
// no longer reachable at all, which is what a return-early bug looks like from
// the outside and is otherwise indistinguishable from an idle GPU.
std::atomic<uint64_t> g_burst_enter_polls{0};
std::atomic<uint64_t> g_burst_exit_polls{0};
// Bursts that closed having pulled NOT ONE PC from CUPTI.
//
// This found a real one (issue #102). CUPTI withholds PC data until it can
// nearly fill collectNumPcs, which the header documents only as a buffer
// capacity. At the old default of 2048 EVERY 50 ms burst came back empty ---
// 124 of 124 --- while every other counter on this path read healthy:
// bursts=124, duty within its ceiling, start_failed=0, stop_failed=0,
// getdata_failed=0, dropped_hw=0. Nothing reported a fault, because there was
// none: the calls all succeeded and CUPTI was simply still buffering.
//
// That is the exact shape this pipeline treats as a defect rather than a
// tuning matter: a profiler paying the full price of its perturbation and
// silently collecting none of the data it perturbed the workload to get. So it
// is counted, and an all-empty run says so in as many words at exit.
std::atomic<uint64_t> g_bursts_empty{0};
// g_pc_pcs as it stood when the open burst started. Sampled at OPEN and not
// merely around the range-end drain: the periodic drain runs on the timer and
// will already have pulled most of a long burst's PCs by the time it closes,
// so a before/after around the range end alone reports a productive burst as
// empty --- which it did, on the first hardware run of this counter. One
// global is enough because BurstController never has two bursts open.
std::atomic<uint64_t> g_pc_pcs_at_open{0};

// Every one of these is assertable at a known value on a healthy run, which is
// the whole point: a context that failed to enable is otherwise a silent hole
// in coverage, and a drop class with no counter is a loss nobody can see.
std::atomic<uint64_t> g_ctx_seen{0};            // CONTEXT_CREATED callbacks
std::atomic<uint64_t> g_ctx_enabled{0};         // sampling enabled and configured
std::atomic<uint64_t> g_ctx_enable_failed{0};   // MUST be 0 on a healthy run
std::atomic<uint64_t> g_ctx_destroyed{0};       // CONTEXT_DESTROY_STARTING
std::atomic<uint64_t> g_ctx_disable_failed{0};  // MUST be 0 on a healthy run
std::atomic<uint64_t> g_pc_records{0};          // (PC, stall) pairs put on the wire
std::atomic<uint64_t> g_pc_pcs{0};              // distinct PCs seen
std::atomic<uint64_t> g_pc_getdata_calls{0};
std::atomic<uint64_t> g_pc_getdata_failed{0};
std::atomic<uint64_t> g_pc_drain_rounds_capped{0};
std::atomic<uint64_t> g_pc_dropped_hw{0};       // CUpti_PCSamplingData.droppedSamples
std::atomic<uint64_t> g_pc_buffer_full{0};      // hardwareBufferFull observations
std::atomic<uint64_t> g_pc_non_user{0};         // nonUsrKernelsTotalSamples
// The two halves of the identity the agent cannot check. Process-wide rather
// than per-context, so a context destroyed mid-run does not take its share of
// the accounting with it.
std::atomic<uint64_t> g_pc_total_samples{0};    // log only; see the note above
std::atomic<uint64_t> g_pc_emitted_counts{0};   // log only
std::atomic<uint64_t> g_pc_unattached{0};       // records dropped: no consumer
std::atomic<uint64_t> g_pc_zero_stall{0};       // (PC, stall) pairs with count 0
std::atomic<uint64_t> g_module_unload_drains{0};
std::atomic<uint64_t> g_finalize_seen{0};
// Finalize arrived while a drain held the PC lock and the teardown was
// skipped rather than deadlocking the host. MUST be 0 on a healthy run.
std::atomic<uint64_t> g_finalize_contended{0};
std::atomic<uint64_t> g_exec_from_graph{0};     // executions launched from a graph
std::atomic<uint64_t> g_graph_exec_reported{0}; // the delta already on the wire
std::atomic<uint64_t> g_config_emitted{0};
std::atomic<uint64_t> g_config_no_device{0};    // config emitted with sm_count 0

// Device facts for gpu_config_v1, filled from a CUPTI_ACTIVITY_KIND_DEVICE
// record. There is no cuptiDeviceGetAttribute for either, and the adapter does
// not link libcuda, so the activity record is the only source.
std::atomic<uint32_t> g_sm_count{0};
std::atomic<uint64_t> g_core_clock_hz{0};

// The multi-GPU guard. gpu_pc_sample_batch_v1 carries no device_id and two
// devices running the same binary produce the SAME cubin_crc, so their PC
// samples are indistinguishable on the wire. Detection is on gpu_exec_v1,
// which does carry device_id. For this phase a two-GPU process is a
// configuration mistake, not a condition to tolerate: it is logged once here
// and the agent marks the affected samples.
std::mutex g_dev_mu;
std::vector<uint64_t> g_devices_seen;
std::atomic<uint64_t> g_multi_device{0};
// The single-device fast path. This runs on every kernel activity record, on
// the CUPTI worker thread, whether or not Tier B is on, so the common case
// must not take a lock: one relaxed load and a compare.
std::atomic<uint64_t> g_first_device{~0ull};

void note_device(uint64_t device_id) {
    if (g_first_device.load(std::memory_order_relaxed) == device_id) return;
    std::lock_guard<std::mutex> g(g_dev_mu);
    if (g_devices_seen.empty()) g_first_device.store(device_id, std::memory_order_relaxed);
    for (uint64_t d : g_devices_seen)
        if (d == device_id) return;
    g_devices_seen.push_back(device_id);
    if (g_devices_seen.size() > 1) {
        g_multi_device.fetch_add(1, std::memory_order_relaxed);
        logf("perfagent-cupti: WARNING multiple devices in one process "
             "(device_id=%llu, %zu distinct so far). gpu_pc_sample_batch_v1 "
             "carries no device_id and identical binaries produce identical "
             "cubin_crc, so PC samples from these devices are not separable on "
             "the wire. PC sampling is single-GPU in this phase.\n",
             (unsigned long long)device_id, g_devices_seen.size());
    }
}

// Producer-side loss, one record per class per drain. Fires unbatched with
// count 1: it happens a handful of times per process and batching it would buy
// nothing while making a partial batch at exit lose the last drop counts.
void emit_dropped(uint64_t count, uint8_t klass) {
    if (!count) return;
    if (!gpu_dropped_v1_enabled()) return;
    gpu_dropped_v1 d{};
    d.count = count;
    d.klass = klass;
    gpu_dropped_v1_emit(&d, 1, g_dropped_seq.fetch_add(1, std::memory_order_relaxed));
}

// Queries the device's stall-reason table and puts it on the wire. Called once
// per process, under the CUPTI guard, from the first context that enables
// sampling.
//
// The table is also handed to the ReplayLog: the query happens at context
// creation, which on a CUDA process is before a consumer can realistically
// have attached, so without replay every stall index for the whole run would
// arrive unresolvable.
bool pc_query_stall_reasons(CUcontext ctx) {
    if (g_stall_queried) return g_num_stall_reasons > 0;
    g_stall_queried = true;

    size_t num = 0;
    CUpti_PCSamplingGetNumStallReasonsParams np{};
    np.size = CUpti_PCSamplingGetNumStallReasonsParamsSize;
    np.ctx = ctx;
    np.numStallReasons = &num;
    if (!check(perfagent::cupti::PCSamplingGetNumStallReasons(&np),
               "cuptiPCSamplingGetNumStallReasons"))
        return false;
    if (!num) {
        logf("perfagent-cupti: PC sampling reports zero stall reasons; not enabling\n");
        return false;
    }

    std::vector<uint32_t> indices(num);
    std::vector<char *> names(num);
    // One flat block, so the char* array CUPTI fills points into storage that
    // outlives the call and is freed exactly once.
    std::vector<char> storage(num * CUPTI_STALL_REASON_STRING_SIZE, 0);
    for (size_t i = 0; i < num; i++) names[i] = &storage[i * CUPTI_STALL_REASON_STRING_SIZE];

    CUpti_PCSamplingGetStallReasonsParams sp{};
    sp.size = CUpti_PCSamplingGetStallReasonsParamsSize;
    sp.ctx = ctx;
    sp.numStallReasons = num;
    sp.stallReasonIndex = indices.data();
    sp.stallReasons = names.data();
    if (!check(perfagent::cupti::PCSamplingGetStallReasons(&sp), "cuptiPCSamplingGetStallReasons"))
        return false;

    g_stall_indices = indices;
    g_num_stall_reasons = num;

    for (size_t i = 0; i < num; i++) {
        gpu_stall_reason_map_v1 r{};
        r.index = indices[i];
        const char *nm = names[i] ? names[i] : "";
        size_t n = strnlen(nm, CUPTI_STALL_REASON_STRING_SIZE);
        if (n > GPU_STALL_NAME_MAX) { n = GPU_STALL_NAME_MAX; r.truncated = 1; }
        r.name_len = (uint16_t)n;
        if (n) memcpy(r.name, nm, n);
        // Recorded first, emitted second: the replay copy must exist even if
        // no consumer is attached right now, which is the common case.
        g_replay->record_stall_reason(r);
        if (gpu_stall_reason_map_v1_enabled())
            gpu_stall_reason_map_v1_emit(&r, 1, g_stall_seq.fetch_add(1, std::memory_order_relaxed));
    }
    logf("perfagent-cupti: pc sampling stall reasons=%zu\n", num);
    return true;
}

// Sizes and wires one context's parsed-data buffer. CUPTI writes into memory
// the client owns: a CUpti_PCSamplingData whose pPcData points at an array of
// collectNumPcs CUpti_PCSamplingPCData, each of whose stallReason points at
// its own numStallReasons-entry array.
void pc_setup_buffer(PCContext *c) {
    c->pcs.assign(g_pc_collect_num_pcs, CUpti_PCSamplingPCData{});
    c->stalls.assign(g_pc_collect_num_pcs * g_num_stall_reasons, CUpti_PCSamplingStallReason{});
    for (size_t i = 0; i < g_pc_collect_num_pcs; i++) {
        c->pcs[i].size = sizeof(CUpti_PCSamplingPCData);
        c->pcs[i].stallReason = &c->stalls[i * g_num_stall_reasons];
    }
    c->data = CUpti_PCSamplingData{};
    c->data.size = sizeof(CUpti_PCSamplingData);
    c->data.collectNumPcs = g_pc_collect_num_pcs;
    c->data.pPcData = c->pcs.data();
}

void pc_disable_ctx(PCContext *c, const char *why) {
    if (!c->enabled) return;
    CUpti_PCSamplingDisableParams dp{};
    dp.size = CUpti_PCSamplingDisableParamsSize;
    dp.ctx = c->ctx;
    if (!check(perfagent::cupti::PCSamplingDisable(&dp), why))
        g_ctx_disable_failed.fetch_add(1, std::memory_order_relaxed);
    c->enabled = false;
}

// Enables and configures PC sampling on one context. Caller holds the CUPTI
// guard (perfagent::cupti::guard()); the wrappers below take it again, which
// is free because it is re-entrant on the owning thread.
//
// Enable first, then configure: cuptiPCSamplingGetNumStallReasons and the
// configuration attributes are all keyed on a context CUPTI already knows is
// being sampled, and this is the order NVIDIA's own continuous-sampling sample
// uses. If any step after the enable fails, the enable is undone rather than
// left half-applied.
void pc_enable_ctx(CUcontext ctx) {
    // The CUDA-graph refusal, at its earliest reachable point. If a graph
    // execution has already been seen, Tier A's exact-correlation claim is
    // already false for this process and the tier must not start at all. It
    // does NOT fall back to CONTINUOUS: a silent downgrade would leave the
    // operator reading a Tier B profile while believing they asked for Tier A.
    if (g_pc_tier_a && g_exec_from_graph.load(std::memory_order_relaxed)) {
        if (g_tier_a_graph_refused.fetch_add(1, std::memory_order_relaxed) == 0) {
            logf("perfagent-cupti: REFUSING Tier A: %llu CUDA-graph execution(s) already "
                 "observed. A graph launch fires one callback for N kernels, so N "
                 "executions share one correlation and Tier A's exact launch "
                 "attribution would be confidently wrong. PC sampling is NOT enabled "
                 "for this context; this is not a downgrade to Tier B.\n",
                 (unsigned long long)g_exec_from_graph.load(std::memory_order_relaxed));
        }
        g_ctx_enable_failed.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    CUpti_PCSamplingEnableParams en{};
    en.size = CUpti_PCSamplingEnableParamsSize;
    en.ctx = ctx;
    if (!check(perfagent::cupti::PCSamplingEnable(&en), "cuptiPCSamplingEnable")) {
        g_ctx_enable_failed.fetch_add(1, std::memory_order_relaxed);
        return;
    }

    PCContext *c = new PCContext();
    c->ctx = ctx;
    c->enabled = true;

    if (!pc_query_stall_reasons(ctx)) {
        pc_disable_ctx(c, "cuptiPCSamplingDisable (stall query failed)");
        g_ctx_enable_failed.fetch_add(1, std::memory_order_relaxed);
        delete c;
        return;
    }
    pc_setup_buffer(c);

    CUpti_PCSamplingConfigurationInfo info[8]{};
    size_t n = 0;
    info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_COLLECTION_MODE;
    info[n++].attributeData.collectionModeData.collectionMode =
        g_pc_tier_a ? CUPTI_PC_SAMPLING_COLLECTION_MODE_KERNEL_SERIALIZED
                    : CUPTI_PC_SAMPLING_COLLECTION_MODE_CONTINUOUS;
    if (g_pc_tier_a) {
        // The duty cycle's mechanism. Without it cuptiPCSamplingStart/Stop
        // return an error and the only way to bound the perturbation would be
        // Enable/Disable per burst --- which tears the configuration down and
        // rebuilds it every 500 ms, re-queries nothing, and is not what the
        // start/stop control exists for.
        info[n].attributeType =
            CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_ENABLE_START_STOP_CONTROL;
        info[n++].attributeData.enableStartStopControlData.enableStartStopControl = 1;
    }
    // All stall reasons. The label cardinality is device-fixed (38 on GA102),
    // so collecting all of them costs bytes per PC and nothing that grows with
    // the length of the run.
    info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_STALL_REASON;
    info[n].attributeData.stallReasonData.stallReasonCount = g_num_stall_reasons;
    info[n++].attributeData.stallReasonData.pStallReasonIndex = g_stall_indices.data();
    info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_OUTPUT_DATA_FORMAT;
    info[n++].attributeData.outputDataFormatData.outputDataFormat =
        CUPTI_PC_SAMPLING_OUTPUT_DATA_FORMAT_PARSED;
    info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SAMPLING_DATA_BUFFER;
    info[n++].attributeData.samplingDataBufferData.samplingDataBuffer = &c->data;
    if (g_pc_period) {
        info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SAMPLING_PERIOD;
        info[n++].attributeData.samplingPeriodData.samplingPeriod = g_pc_period;
    }
    if (g_pc_scratch_bytes) {
        info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SCRATCH_BUFFER_SIZE;
        info[n++].attributeData.scratchBufferSizeData.scratchBufferSize = g_pc_scratch_bytes;
    }
    if (g_pc_hw_buffer_bytes) {
        info[n].attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_HARDWARE_BUFFER_SIZE;
        info[n++].attributeData.hardwareBufferSizeData.hardwareBufferSize = g_pc_hw_buffer_bytes;
    }
    // In Tier B, ENABLE_START_STOP_CONTROL is deliberately left off: turning
    // it on in CONTINUOUS mode would change when a flush is required
    // (cupti_pcsampling.h: "after every range end") without buying that tier
    // anything. It is set above for Tier A, where it is the whole mechanism.

    CUpti_PCSamplingConfigurationInfoParams cp{};
    cp.size = CUpti_PCSamplingConfigurationInfoParamsSize;
    cp.ctx = ctx;
    cp.numAttributes = n;
    cp.pPCSamplingConfigurationInfo = info;
    if (!check(perfagent::cupti::PCSamplingSetConfigurationAttribute(&cp),
               "cuptiPCSamplingSetConfigurationAttribute")) {
        pc_disable_ctx(c, "cuptiPCSamplingDisable (configure failed)");
        g_ctx_enable_failed.fetch_add(1, std::memory_order_relaxed);
        delete c;
        return;
    }
    // Per-attribute status, not just the call's. A configuration call can
    // succeed with an individual attribute refused, and a silently unapplied
    // COLLECTION_MODE would serialize every kernel in the process while the
    // profile claimed it had not.
    for (size_t i = 0; i < n; i++) {
        if (info[i].attributeStatus != CUPTI_SUCCESS) {
            const char *msg = "?";
            perfagent::cupti::GetResultString(info[i].attributeStatus, &msg);
            logf("perfagent-cupti: pc sampling attribute %d refused: %s\n",
                 (int)info[i].attributeType, msg);
            pc_disable_ctx(c, "cuptiPCSamplingDisable (attribute refused)");
            g_ctx_enable_failed.fetch_add(1, std::memory_order_relaxed);
            delete c;
            return;
        }
    }

    // Read the period back when we did not set one, so gpu_config_v1 reports
    // what the device is doing rather than what we asked for.
    if (!g_pc_period) {
        CUpti_PCSamplingConfigurationInfo q{};
        q.attributeType = CUPTI_PC_SAMPLING_CONFIGURATION_ATTR_TYPE_SAMPLING_PERIOD;
        CUpti_PCSamplingConfigurationInfoParams qp{};
        qp.size = CUpti_PCSamplingConfigurationInfoParamsSize;
        qp.ctx = ctx;
        qp.numAttributes = 1;
        qp.pPCSamplingConfigurationInfo = &q;
        if (perfagent::cupti::PCSamplingGetConfigurationAttribute(&qp) == CUPTI_SUCCESS &&
            q.attributeStatus == CUPTI_SUCCESS)
            g_pc_period = q.attributeData.samplingPeriodData.samplingPeriod;
    }

    g_pc_ctxs.push_back(c);
    g_ctx_enabled.fetch_add(1, std::memory_order_relaxed);
    // The CUcontext itself, not a uid: CUpti_ResourceData carries no
    // contextUid, and inventing a zero would put a plausible-looking id in a
    // log that no other record shares.
    logf("perfagent-cupti: pc sampling enabled ctx=%p period=%u collect_num_pcs=%zu\n",
         (void *)ctx, g_pc_period, g_pc_collect_num_pcs);
}

// Pulls whatever CUPTI has for one context and turns it into wire records.
// Caller holds the CUPTI guard.
//
// ONE RECORD PER (PC, stall reason) PAIR. That is the price of the ABI's
// fixed-size record rule (spec §6.3): CUpti_PCSamplingPCData carries a
// variable-length stall array and gpu_pc_sample_batch_v1 cannot, so the array
// is flattened. A (PC, stall) pair whose count is zero is not emitted --- it
// carries no information and would multiply the wire volume by the number of
// stall reasons the device has.
void pc_drain_ctx_locked(PCContext *c) {
    if (!c->enabled) return;
    for (unsigned round = 0; round < kPCMaxDrainRounds; round++) {
        c->data.totalNumPcs = 0;
        c->data.remainingNumPcs = 0;
        c->data.droppedSamples = 0;
        c->data.totalSamples = 0;
        c->data.nonUsrKernelsTotalSamples = 0;
        c->data.hardwareBufferFull = 0;

        CUpti_PCSamplingGetDataParams gp{};
        gp.size = CUpti_PCSamplingGetDataParamsSize;
        gp.ctx = c->ctx;
        gp.pcSamplingData = &c->data;
        g_pc_getdata_calls.fetch_add(1, std::memory_order_relaxed);
        const CUptiResult st = perfagent::cupti::PCSamplingGetData(&gp);
        if (st == CUPTI_ERROR_OUT_OF_MEMORY) {
            // The documented "hardware buffer is full" return. The PC data for
            // this window is gone; the fact that it is gone is not.
            g_pc_buffer_full.fetch_add(1, std::memory_order_relaxed);
            emit_dropped(1, GPU_DROP_CLASS_PC_BUFFER_FULL);
            return;
        }
        if (st != CUPTI_SUCCESS) {
            g_pc_getdata_failed.fetch_add(1, std::memory_order_relaxed);
            check(st, "cuptiPCSamplingGetData");
            return;
        }

        // hardwareBufferFull is also reported in-band, on an otherwise
        // successful call. Both spellings feed the same class.
        if (c->data.hardwareBufferFull) {
            g_pc_buffer_full.fetch_add(1, std::memory_order_relaxed);
            emit_dropped(1, GPU_DROP_CLASS_PC_BUFFER_FULL);
        }
        // Deltas, not totals: CUPTI's counters are cumulative per context and
        // the consumer sums drop records.
        if (c->data.droppedSamples > c->last_dropped) {
            const uint64_t d = c->data.droppedSamples - c->last_dropped;
            c->last_dropped = c->data.droppedSamples;
            g_pc_dropped_hw.fetch_add(d, std::memory_order_relaxed);
            emit_dropped(d, GPU_DROP_CLASS_PC_DROPPED_HW);
        }
        if (c->data.nonUsrKernelsTotalSamples > c->last_non_user) {
            const uint64_t d = c->data.nonUsrKernelsTotalSamples - c->last_non_user;
            c->last_non_user = c->data.nonUsrKernelsTotalSamples;
            g_pc_non_user.fetch_add(d, std::memory_order_relaxed);
            emit_dropped(d, GPU_DROP_CLASS_PC_NON_USER_KERNEL);
        }
        g_pc_total_samples.fetch_add(c->data.totalSamples, std::memory_order_relaxed);

        size_t npcs = c->data.totalNumPcs;
        if (npcs > g_pc_collect_num_pcs) npcs = g_pc_collect_num_pcs;
        g_pc_pcs.fetch_add(npcs, std::memory_order_relaxed);
        for (size_t i = 0; i < npcs; i++) {
            const CUpti_PCSamplingPCData &pc = c->pcs[i];
            size_t nst = pc.stallReasonCount;
            if (nst > g_num_stall_reasons) nst = g_num_stall_reasons;
            for (size_t j = 0; j < nst; j++) {
                const CUpti_PCSamplingStallReason &sr = pc.stallReason[j];
                if (!sr.samples) {
                    g_pc_zero_stall.fetch_add(1, std::memory_order_relaxed);
                    continue;
                }
                gpu_pc_sample_batch_v1 r{};
                r.cubin_crc = pc.cubinCrc;
                // Zero in CONTINUOUS mode, always. The ABI says so and the
                // consumer relies on it to route these through the module
                // join rather than the exact one.
                r.correlation = pc.correlationId;
                r.pc_offset = pc.pcOffset;
                r.function_index = pc.functionIndex;
                r.stall_index = sr.pcSamplingStallReasonIndex;
                r.count = sr.samples;
                g_pc_emitted_counts.fetch_add(sr.samples, std::memory_order_relaxed);
                if (g_pcb->add(r))
                    g_pc_records.fetch_add(1, std::memory_order_relaxed);
                else
                    g_pc_unattached.fetch_add(1, std::memory_order_relaxed);
            }
        }
        if (!c->data.remainingNumPcs) return;
        if (round + 1 == kPCMaxDrainRounds)
            g_pc_drain_rounds_capped.fetch_add(1, std::memory_order_relaxed);
    }
}

void pc_drain_all(perfagent::PCDrainReason reason) {
    perfagent::cupti::CallScope hold;
    for (PCContext *c : g_pc_ctxs) pc_drain_ctx_locked(c);
    if (reason != perfagent::PCDrainReason::kPeriodic && g_pcb) g_pcb->flush();
}

// ---------------------------------------------- Tier A: bursts and windows
//
// gpu_sampling_window_v1, twice per burst.
//
// The OPEN record goes out the instant cuptiPCSamplingStart succeeds, with
// end_ns = 0. That is what makes end_ns == 0 mean something on the wire: if
// this process is killed mid-burst, the consumer still holds a record saying a
// burst was open from start_ns and never closed, and every execution from
// start_ns onward is gpu_serialized="unknown". Emitting only on the stop would
// lose the whole burst on a hard exit, and the executions inside it would then
// read "false" --- "not perturbed" when the truth is "cannot tell", which is
// the one answer that must never be reachable by accident.
//
// The CLOSED record goes out on the stop with the real end_ns and the SAME
// start_ns, and supersedes the open one in the consumer's store. A closed
// record never loses to an open one there, so the two orderings a lossy
// transport can produce both end up correct.
void emit_window(uint64_t start_ns, uint64_t end_ns) {
    if (!gpu_sampling_window_v1_enabled()) return;
    gpu_sampling_window_v1 w{};
    w.start_ns = start_ns;
    w.end_ns = end_ns;
    w.mode = GPU_SAMPLING_MODE_KERNEL_SERIALIZED;
    // UNBATCHED, like gpu_dropped_v1 and for the same reason: two records per
    // burst at a few bursts per second is not volume, and a window still
    // sitting in a partly filled batch when the process dies is a window that
    // never existed --- which would take the hard-exit disclosure with it.
    gpu_sampling_window_v1_emit(&w, 1, g_window_seq.fetch_add(1, std::memory_order_relaxed));
    g_windows_emitted.fetch_add(1, std::memory_order_relaxed);
}

// Issue #101's standing check, on the two calls that caused it.
//
// The deadlock was not a race we lost occasionally: cuptiPCSamplingStop taken
// on a thread of ours blocks in pthread_rwlock_wrlock against the CUDA
// runtime's own launch path, and the application blocks behind it forever. The
// fix is that these two calls only ever run on the application's thread, from
// inside the launch callback. That is a property of the CALL SITES, which is
// exactly the kind of property that quietly stops holding when somebody adds a
// third one. So it is checked here, at the calls themselves, where a new call
// site inherits the check whether or not its author knew about it.
void note_burst_thread() {
    const uint64_t timer = g_timer_tid.load(std::memory_order_relaxed);
    if (timer != 0 && (uint64_t)current_tid() == timer)
        g_burst_calls_on_timer.fetch_add(1, std::memory_order_relaxed);
}

// cuCtxSynchronize, resolved at runtime.
//
// parcagpu quiesces the GPU before every cuptiPCSamplingStart --- "CUPTI
// requires the GPU to be idle before starting PC sampling". That requirement
// is NOT in cupti_pcsampling.h; it is their empirical finding, and it is
// carried here because theirs is the configuration known to work rather than
// because the header asks for it. Being at a launch ENTER does not make the
// GPU idle: kernels issued earlier on other streams are very likely still
// running.
//
// dlsym rather than a link: this adapter does not link libcuda (see the note
// at ActivityEnable(DEVICE)), and it does not need to --- nothing can reach a
// launch callback in a process where libcuda is absent.
//
// RTLD_DEFAULT is NOT enough and that was measured, not assumed: the first
// version of this looked there and reported sync_missing=7 out of 7 bursts on
// the 3090. The CUDA runtime dlopens libcuda into a LOCAL scope, so its
// symbols never enter the global one and RTLD_DEFAULT cannot see them. The
// handle has to be asked for by name.
//
// RTLD_NOLOAD: find the copy this process already has, or nothing. Loading a
// second libcuda from a profiler would be a far worse bug than not quiescing.
using CuCtxSynchronizeFn = int (*)(void);
CuCtxSynchronizeFn g_cu_ctx_sync = nullptr;

// Resolved on FIRST USE, not at injection time. InitializeInjection can run
// before the CUDA runtime has finished bringing libcuda up; the first launch
// callback cannot. Doing it here removes a startup ordering question that has
// no other reason to exist.
//
// Called only from the burst-open path, which is single-threaded per burst by
// BurstController's own mutex... except that it is NOT: two application
// threads can be inside burst_open_at_launch at once, and only one of them
// gets kStart. The resolve therefore has to tolerate a race. It does: both
// racers compute the same pointer and store it, so the write is idempotent and
// a torn read is impossible for a pointer-sized relaxed atomic.
std::atomic<bool> g_cu_ctx_sync_tried{false};

void pc_resolve_ctx_sync() {
    // The global scope first: cheap, and correct for an application that links
    // libcuda directly rather than going through the runtime.
    CuCtxSynchronizeFn f = (CuCtxSynchronizeFn)dlsym(RTLD_DEFAULT, "cuCtxSynchronize");
    if (!f) {
        if (void *h = dlopen("libcuda.so.1", RTLD_NOLOAD | RTLD_LAZY))
            f = (CuCtxSynchronizeFn)dlsym(h, "cuCtxSynchronize");
    }
    g_cu_ctx_sync = f;
    g_cu_ctx_sync_tried.store(true, std::memory_order_relaxed);
}

// Deliberately OUTSIDE the CUPTI guard: this is not a CUPTI call, and holding
// the guard across a full device synchronise would park the drain timer for
// however long the GPU takes to go idle --- which on a saturated device is the
// longest lock hold in the adapter, taken for no reason.
void pc_sync_before_start() {
    if (!g_cu_ctx_sync_tried.load(std::memory_order_relaxed)) pc_resolve_ctx_sync();
    if (!g_cu_ctx_sync) {
        g_burst_sync_missing.fetch_add(1, std::memory_order_relaxed);
        return;
    }
    if (g_cu_ctx_sync() != 0)  // CUDA_SUCCESS
        g_burst_sync_failed.fetch_add(1, std::memory_order_relaxed);
}

void pc_start_ctxs_locked() {
    note_burst_thread();
    for (PCContext *c : g_pc_ctxs) {
        if (!c->enabled) continue;
        CUpti_PCSamplingStartParams sp{};
        sp.size = CUpti_PCSamplingStartParamsSize;
        sp.ctx = c->ctx;
        if (!check(perfagent::cupti::PCSamplingStart(&sp), "cuptiPCSamplingStart"))
            g_burst_start_failed.fetch_add(1, std::memory_order_relaxed);
    }
}

void pc_stop_ctxs_locked() {
    note_burst_thread();
    for (PCContext *c : g_pc_ctxs) {
        if (!c->enabled) continue;
        CUpti_PCSamplingStopParams sp{};
        sp.size = CUpti_PCSamplingStopParamsSize;
        sp.ctx = c->ctx;
        if (!check(perfagent::cupti::PCSamplingStop(&sp), "cuptiPCSamplingStop"))
            g_burst_stop_failed.fetch_add(1, std::memory_order_relaxed);
    }
}

// Closes an open burst: stop every context, drain (CUPTI requires the flush
// after every range end), close the window, and hand the yield to the loop.
//
// The stop and its mandatory flush are ONE critical section, which they were
// not before issue #99: the old code released the PC mutex between them, so
// another thread's CUPTI call could land in the gap between a range ending and
// the flush the header requires after it. pc_drain_all takes the guard again
// inside this scope, which costs a depth increment and no lock -- that is what
// the re-entrancy in core/callguard.h buys, spent on making a two-call
// sequence atomic rather than on convenience.
//
// The guard is released before the window is emitted and before the controller
// is told, so a USDT fire and the controller's own mutex are never taken with
// the whole vendor surface locked.
void burst_close(uint64_t now_ns, uint64_t start_ns) {
    const uint64_t pcs_at_open = g_pc_pcs_at_open.load(std::memory_order_relaxed);
    {
        perfagent::cupti::CallScope hold;
        pc_stop_ctxs_locked();
        // The range-end flush. cupti_pcsampling.h: "If configuration option
        // ENABLE_START_STOP_CONTROL is enabled, then after every range end
        // i.e. cuptiPCSamplingStop()". Missing it does not lose data --- it
        // makes two instructions share a PC identity, silently. force() also
        // moves the shared schedule's phase so the periodic drain coalesces
        // rather than repeating the pull microseconds later.
        if (g_pc_schedule) g_pc_schedule->force(now_ns, perfagent::PCDrainReason::kRangeEnd);
        pc_drain_all(perfagent::PCDrainReason::kRangeEnd);
    }
    // AFTER the range-end drain above, so the PCs this burst produced but that
    // only arrive on the post-stop flush are counted where they belong.
    if (g_pc_pcs.load(std::memory_order_relaxed) == pcs_at_open)
        g_bursts_empty.fetch_add(1, std::memory_order_relaxed);
    emit_window(start_ns, now_ns);
    g_sampling_burst_ns.fetch_add(now_ns > start_ns ? now_ns - start_ns : 0,
                                  std::memory_order_relaxed);
    // AFTER the drain, which is the whole reason BurstController::closed is a
    // separate call: the pairs this burst produced only reach g_pc_records on
    // the flush that follows the stop, so a loop that read the count at stop
    // time would measure every burst as having yielded nothing and would sit
    // at the duty floor for the entire run.
    if (g_burst) g_burst->closed(g_pc_records.load(std::memory_order_relaxed));
}

// Tier A's two transitions, taken on the APPLICATION's thread at a kernel-launch
// boundary: START on a launch ENTER, STOP on a launch EXIT.
//
// Issue #101, and this is the shape of the fix rather than a detail of it.
//
// These used to be one function called from the drain timer. That deadlocks,
// and not rarely: cuptiPCSamplingStop taken on any thread of ours blocks in
// pthread_rwlock_wrlock inside CUPTI while the application sits in
// __cudaLaunchKernel_helper holding the other side, and neither ever moves. It
// survived issue #99 --- which was real, and which did collapse our two timer
// threads into one --- because the surviving conflict is not between two
// threads of OURS. It is between our thread and the application's, and no
// amount of self-serialization reaches it. Only not having a thread does.
//
// parcagpu (parca-dev/parcagpu, src/cupti.cpp:591) does not have one: it
// creates no threads at all, and every PC-sampling start and stop runs on the
// application's own thread from inside a CUPTI callback. Their comment states
// the rule --- "PC sampling windows are aligned to kernel-launch boundaries:
// start on a launch ENTER, stop on a launch EXIT" --- and their configuration
// is otherwise identical to ours: ENABLE_START_STOP_CONTROL, KERNEL_SERIALIZED,
// Enable/Disable per context rather than per burst. The calling thread is the
// only difference between a design that works and one that hangs.
//
// One deliberate divergence: parcagpu takes the DRIVER_API launch callbacks and
// this takes the RUNTIME_API ones it already subscribes to. That is not a
// compromise, it is strictly the safer position --- a runtime cudaLaunchKernel
// ENTER fires BEFORE the runtime enters the driver, and its EXIT after the
// driver has returned, so these calls are made from further outside CUPTI's
// launch path than parcagpu's are. It also avoids the correlation-id cost that
// adding DRIVER_API was measured to have (2.242 vs 1.004 ids per launch; see
// the note at EnableDomain).
//
// What this gives up, stated rather than discovered later: a burst can only
// open or close when the application launches a kernel. An application that
// opens a burst and then stops launching for a second leaves the window open
// for that second, and the duty figure counts it as perturbed. That
// OVER-states the perturbation, which is the safe direction and the same
// direction every other approximation on this path leans --- no kernel is
// launching, so nothing is in fact being serialized. The exit path still
// closes the window with a real timestamp (burst_shutdown), so end_ns == 0 on
// the wire continues to mean a hard exit and nothing else.

// The refusal log, on whichever of the two boundaries first observes a graph.
void note_graph_refusal(bool was_refused, bool graphs) {
    if (was_refused || !g_burst->refused() || !graphs) return;
    g_tier_a_graph_refused.fetch_add(1, std::memory_order_relaxed);
    logf("perfagent-cupti: REFUSING Tier A: %llu CUDA-graph execution(s) observed "
         "mid-run. N executions share one correlation, so exact launch "
         "attribution is false while still looking exact. Bursts have STOPPED "
         "permanently; this is not a downgrade to Tier B. Executions already "
         "inside a window stay marked serialized.\n",
         (unsigned long long)g_exec_from_graph.load(std::memory_order_relaxed));
}

// Launch ENTER. Opens a burst if the duty cycle is due for one; can never
// close one --- see BurstController::poll_open for why that is enforced there
// rather than by testing sampling() here.
void burst_open_at_launch(uint64_t now) {
    if (!g_pc_tier_a || !g_burst) return;
    // No enabled context means nothing can be serialized, so there is no burst
    // to open and no window to announce. A launch can reach this before the
    // first CONTEXT_CREATED callback has been processed, and without this the
    // first window would cover executions that were never perturbed.
    if (!g_ctx_enabled.load(std::memory_order_relaxed)) return;
    g_burst_enter_polls.fetch_add(1, std::memory_order_relaxed);
    // The graph refusal, re-checked at every boundary and not only at enable
    // time: a process can run for minutes before its first graph launch, and
    // the moment one arrives Tier A's exactness claim stops being true.
    const bool graphs = g_exec_from_graph.load(std::memory_order_relaxed) != 0;
    const bool was_refused = g_burst->refused();
    if (g_burst->poll_open(now, graphs) == perfagent::BurstAction::kStart) {
        // Before the guard is taken, and before the start: quiescing the
        // device is not a CUPTI call and must not hold the CUPTI guard.
        pc_sync_before_start();
        {
            perfagent::cupti::CallScope hold;
            pc_start_ctxs_locked();
        }
        g_pc_pcs_at_open.store(g_pc_pcs.load(std::memory_order_relaxed),
                               std::memory_order_relaxed);
        g_sampling_bursts.fetch_add(1, std::memory_order_relaxed);
        // The window is announced even if some or all contexts refused to
        // start (g_burst_start_failed counts that, and must be 0 on a healthy
        // run). Over-stating the perturbation for one burst marks executions
        // "true" that were not perturbed, which is the SAFE direction: the
        // answer that must never be reachable by accident is "false", and no
        // path here can produce it.
        emit_window(now, 0);   // open; closed by the EXIT below
    }
    note_graph_refusal(was_refused, graphs);
}

// Launch EXIT. Closes an open burst once it has run its length, or immediately
// if a graph launch has been observed. Can never open one.
void burst_close_at_launch(uint64_t now) {
    if (!g_pc_tier_a || !g_burst) return;
    g_burst_exit_polls.fetch_add(1, std::memory_order_relaxed);
    const bool graphs = g_exec_from_graph.load(std::memory_order_relaxed) != 0;
    const bool was_refused = g_burst->refused();
    // Read BEFORE the poll: the poll is what clears it.
    const uint64_t start_ns = g_burst->burst_start_ns();
    if (g_burst->poll_close(now, graphs) == perfagent::BurstAction::kStop)
        burst_close(now, start_ns);
    note_graph_refusal(was_refused, graphs);
}

// Teardown for the duty cycle. Closes an open burst with a real end timestamp,
// which is exactly what makes end_ns == 0 on the wire mean a HARD exit and
// nothing else.
void burst_shutdown() {
    if (!g_pc_tier_a || !g_burst) return;
    const uint64_t now = mono_ns();
    const uint64_t start_ns = g_burst->burst_start_ns();
    if (g_burst->shutdown(now) == perfagent::BurstAction::kStop) burst_close(now, start_ns);
}

// gpu_config_v1: the sampling configuration in force, emitted once, replayed
// on late attach. sampling_factor is "one PC sample per N SM cycles" --- it is
// NOT a scale factor and no count is ever multiplied by it.
void pc_emit_config_once() {
    if (g_config_emitted.load(std::memory_order_relaxed)) return;
    gpu_config_v1 cfg{};
    cfg.vendor = GPU_VENDOR_NVIDIA;
    cfg.sm_count = g_sm_count.load(std::memory_order_relaxed);
    cfg.clock_hz = g_core_clock_hz.load(std::memory_order_relaxed);
    // 2^period cycles between samples. The exponent is what CUPTI takes; the
    // cycle count is what a reader can reason about, and 2^31 still fits.
    cfg.sampling_factor = (g_pc_period >= kPCPeriodMin && g_pc_period <= kPCPeriodMax)
                              ? (1u << g_pc_period)
                              : 0;
    if (!cfg.sm_count) g_config_no_device.fetch_add(1, std::memory_order_relaxed);
    g_replay->record_config(cfg);
    if (gpu_config_v1_enabled())
        gpu_config_v1_emit(&cfg, 1, g_config_seq.fetch_add(1, std::memory_order_relaxed));
    g_config_emitted.fetch_add(1, std::memory_order_relaxed);
}

// The finalize handler. There has never been one in shim/: the adapter's only
// teardown path was atexit, and calling cuptiFinalize with PC sampling still
// enabled is undefined for us.
//
// It disables sampling on every tracked context BEFORE anything else, because
// cuptiPCSamplingDisable is also the API that tears down CUPTI's worker
// threads and copies the last records into our buffer --- so a drain has to
// happen first or the tail of the profile is silently lost.
//
// Reached from two places: the CUPTI_CB_DOMAIN_STATE fatal-error callback,
// which is exactly when CUPTI invokes cuptiFinalize itself, and the atexit
// handler.
void on_finalize(const char *why) {
    static std::atomic<bool> done{false};
    bool expected = false;
    if (!done.compare_exchange_strong(expected, true)) return;
    g_finalize_seen.fetch_add(1, std::memory_order_relaxed);
    if (!g_pc_enabled) return;

    // Tier A: stop the duty cycle before anything else, so no burst can open
    // against contexts this function is about to disable. Only the
    // controller's own mutex is taken here --- deliberately NOT the CUPTI
    // guard, which this function may fail to acquire below and which the
    // fatal-error callback can already be holding.
    //
    // On the fatal-error path an open window is left OPEN on the wire. That is
    // correct and it is the point: a CUPTI fatal error IS the hard case, and
    // end_ns == 0 is how the consumer learns that the tail of the run cannot
    // be said to have run unperturbed. The ordinary exit path closes it first
    // (at_exit_handler -> burst_shutdown), which is what makes a zero here
    // mean the hard case specifically.
    if (g_burst) g_burst->shutdown(mono_ns());

    // A try, not a blocking acquire, and this is the one place that is right.
    //
    // The fatal-error callback can arrive on the very thread that is inside a
    // CUPTI call this adapter made while holding the guard --- a drain, say ---
    // and a blocking acquire there would deadlock somebody else's process at
    // the worst possible moment. Skipping the teardown loses the tail of the
    // profile; deadlocking loses the application. The skip is counted so it is
    // a known outcome rather than an invisible one.
    //
    // TryCallScope, not std::unique_lock(mu, std::try_to_lock), and the
    // difference is not cosmetic. The guard is re-entrant, so a plain try from
    // a thread that already holds it would SUCCEED, and this function would
    // then disable PC sampling out from under the CUPTI call the outer frame
    // is standing in. TryCallScope refuses when the guard is held by anybody,
    // this thread included. (std::mutex could not express that at all:
    // try_lock() on a mutex the calling thread owns is undefined behaviour,
    // which is what the line this replaces did on exactly that path.)
    perfagent::cupti::TryCallScope g;
    if (!g.owns()) {
        g_finalize_contended.fetch_add(1, std::memory_order_relaxed);
        logf("perfagent-cupti: finalize (%s): the CUPTI guard is held (a call is in "
             "flight); teardown skipped rather than deadlocking the host\n", why);
        return;
    }
    logf("perfagent-cupti: finalize (%s): disabling pc sampling on %zu context(s)\n",
         why, g_pc_ctxs.size());
    for (PCContext *c : g_pc_ctxs) {
        // Drain before disable: cuptiPCSamplingDisable joins CUPTI's worker
        // threads and copies what it has into our buffer, discarding whatever
        // did not fit. Whatever we did not pull first is simply gone.
        pc_drain_ctx_locked(c);
        pc_disable_ctx(c, "cuptiPCSamplingDisable (finalize)");
    }
    if (g_pcb) g_pcb->flush();
}

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

    // The sampled probe fires BEFORE the launch reaches its batch (issue
    // #67), and the record is fully built above so this reorder changes only
    // when the two probes fire, never what they carry.
    //
    // The two records are twins - same correlation, one carrying the launch,
    // the other only the CPU stack the consumer staples onto it - and the
    // consumer's two join paths are not equally safe. Sampled first parks the
    // stack in pendingStacks, where only the twin can claim it. Batched first
    // holds the launch in deferredLaunches, which the next batch of any other
    // kind releases stackless, leaving the stack with nothing to join
    // (gpuprobe/sampledstacks.go).
    //
    // With the add() first, a launch that FILLED the batch put its own
    // batched record on the wire inside that add(), before this probe. In the
    // stub that collision is same-thread and was measured on the gate; here
    // it is a thread race - the exec batch is flushed by the CUPTI worker and
    // the drain timer, not by this callback - so the window between add() and
    // this probe was small but real, and nothing made it impossible. Firing
    // here closes it: a record cannot be in a batch before add() puts it
    // there, so no flush on any thread can carry the twin past this probe.
    //
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

    g_lb->add(l);
}

// The RESOURCE callbacks PC sampling needs. Everything here is a no-op unless
// Tier B is on.
void on_resource(CUpti_CallbackId cbid, const CUpti_ResourceData *rd) {
    if (!g_pc_enabled || !rd) return;
    switch (cbid) {
        case CUPTI_CBID_RESOURCE_CONTEXT_CREATED: {
            g_ctx_seen.fetch_add(1, std::memory_order_relaxed);
            perfagent::cupti::CallScope hold;
            pc_enable_ctx(rd->context);
            break;
        }
        case CUPTI_CBID_RESOURCE_CONTEXT_DESTROY_STARTING: {
            g_ctx_destroyed.fetch_add(1, std::memory_order_relaxed);
            perfagent::cupti::CallScope hold;
            for (size_t i = 0; i < g_pc_ctxs.size(); i++) {
                if (g_pc_ctxs[i]->ctx != rd->context) continue;
                PCContext *c = g_pc_ctxs[i];
                // Drain before disabling: cuptiPCSamplingDisable tears down
                // CUPTI's worker threads, and whatever has not been pulled by
                // then is discarded.
                pc_drain_ctx_locked(c);
                pc_disable_ctx(c, "cuptiPCSamplingDisable (context destroy)");
                g_pc_ctxs.erase(g_pc_ctxs.begin() + (long)i);
                delete c;
                break;
            }
            if (g_pcb) g_pcb->flush();
            if (g_pc_schedule) g_pc_schedule->force(mono_ns(), perfagent::PCDrainReason::kTeardown);
            break;
        }
        case CUPTI_CBID_RESOURCE_MODULE_UNLOAD_STARTING: {
            // cupti_pcsampling.h: in CONTINUOUS mode a PC-data flush is
            // REQUIRED after every module load-unload-load to keep PCs
            // unique. Missing it does not lose data -- it makes two different
            // instructions share a PC identity, silently, which is the worst
            // failure available here. So this drains before returning, and it
            // BLOCKS on the CUPTI guard rather than trying it: a skipped flush
            // is exactly what must not happen. Since #99 the guard covers every
            // CUPTI call the adapter makes rather than only the PC-sampling
            // family, so this now waits for an activity flush as well -- one
            // CUPTI call's duration, on the application's cuModuleUnload path.
            g_module_unload_drains.fetch_add(1, std::memory_order_relaxed);
            if (g_pc_schedule)
                g_pc_schedule->force(mono_ns(), perfagent::PCDrainReason::kModuleUnload);
            pc_drain_all(perfagent::PCDrainReason::kModuleUnload);
            break;
        }
        default:
            break;
    }
}

void CUPTIAPI on_callback(void *, CUpti_CallbackDomain domain, CUpti_CallbackId cbid,
                          const void *cbdata) {
    if (domain == CUPTI_CB_DOMAIN_RESOURCE) {
        if (cbid < kResourceCbidMax)
            g_resource_events[cbid].fetch_add(1, std::memory_order_relaxed);
        // The histogram stays: the RESOURCE subscription is enabled for the
        // whole process and this is still the only way to see what it costs
        // and which cbids actually arrive. What changes is that one of them
        // is now read rather than only counted.
        if (cbid == CUPTI_CBID_RESOURCE_MODULE_LOADED && cbdata)
            on_module_loaded((const CUpti_ResourceData *)cbdata);
        // The other half of this domain, and it must run for EVERY cbid, not
        // as an else: on_resource owns CONTEXT_CREATED, CONTEXT_DESTROY and
        // -- the one that must never be skipped -- MODULE_UNLOAD_STARTING,
        // whose PC-data flush is what keeps PCs unique in CONTINUOUS mode.
        // It is disjoint by cbid from the capture above (MODULE_LOADED hits
        // its default: arm) and a no-op entirely unless Tier B is on, so
        // neither path can shadow the other.
        on_resource(cbid, (const CUpti_ResourceData *)cbdata);
        return;
    }
    if (domain == CUPTI_CB_DOMAIN_STATE) {
        // CUPTI invokes cuptiFinalize() itself on a fatal error, which would
        // pull the rug from under an enabled PC sampling session. This is the
        // only notification we get, so it is where the finalize handler runs.
        if (cbid == CUPTI_CBID_STATE_FATAL_ERROR) on_finalize("cupti fatal error");
        return;
    }
    if (domain != CUPTI_CB_DOMAIN_RUNTIME_API) return;
    const CUpti_CallbackData *cb = (const CUpti_CallbackData *)cbdata;
    // Launch cbids only, on BOTH sites now. The EXIT half is new with issue
    // #101 and it is the only reason this function looks at EXIT at all: it
    // carries no record and does no work of its own, it exists so that Tier A
    // has a place to call cuptiPCSamplingStop from the application's thread.
    if (!is_launch_cbid(cbid)) return;
    if (cb->callbackSite == CUPTI_API_ENTER) {
        // The burst opens BEFORE the launch it is about to serialize, which is
        // the whole point of taking the ENTER: a window opened after the
        // launch had been issued would not cover it, and the execution would
        // be reported unperturbed while running inside a serialized burst.
        burst_open_at_launch(mono_ns());
        on_launch(cb);
        return;
    }
    if (cb->callbackSite == CUPTI_API_EXIT) burst_close_at_launch(mono_ns());
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
    note_device(k->deviceId);
    // A CUDA graph launch fires ONE runtime callback for the whole graph, so
    // gpu_launch_v1 gets one record where N kernels ran. The activity records
    // still arrive per node and carry graphId -- but gpu_exec_v1 is frozen at
    // 48 bytes with nowhere to put it, so the one-launch-to-many-executions
    // shape reaches the join undeclared and produces confident, exact-looking
    // attribution of many kernels to one call site.
    //
    // This adapter cannot fix that (it needs gpu_exec_v2 and a new join
    // shape). What it can do is refuse to be silent: the condition is counted
    // and put on the wire under its own drop class, so Tier A can refuse to
    // start in such a process. Tier B is unaffected -- its attribution runs
    // through the module, not the launch.
    if (k->graphId != 0) g_exec_from_graph.fetch_add(1, std::memory_order_relaxed);

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

// CUPTI's buffer-completed callback, and the ONE place in this adapter that
// reaches CUPTI without taking the guard.
//
// It is an exemption with a reason rather than an oversight.
// cuptiActivityFlushAll is documented as "a blocking call" that hands buffers
// back "using the callback registered in cuptiActivityRegisterCallbacks". If
// CUPTI delivers them on a worker thread while our timer thread holds the
// guard across that flush, a guarded callback would block the worker on a lock
// the flusher is waiting for it to release -- issue #99's deadlock, rebuilt
// inside its own fix.
//
// The boundary: the guard covers calls this adapter initiates on a thread of
// its own choosing. It does not cover the documented way to consume a buffer
// CUPTI has just handed us and, in its own words, "relinquished ownership of".
// The two entry points used here carry that in their names, and they COUNT a
// call made outside this scope rather than trusting the name -- see
// perfagent::cupti::misplaced_callback_calls(), reported as
// cupti_calls_misplaced and required to be 0.
void CUPTIAPI buffer_completed(CUcontext, uint32_t, uint8_t *buffer, size_t,
                               size_t valid_size) {
    const perfagent::cupti::BufferCompletedScope in_callback;
    g_buffers.fetch_add(1, std::memory_order_relaxed);
    if (buffer && valid_size) {
        CUpti_Activity *record = nullptr;
        for (;;) {
            const CUptiResult st =
                perfagent::cupti::ActivityGetNextRecord_InBufferCompletedOnly(
                    buffer, valid_size, &record);
            if (st == CUPTI_ERROR_MAX_LIMIT_REACHED) break;
            if (st != CUPTI_SUCCESS) break;
            if (record->kind == CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL ||
                record->kind == CUPTI_ACTIVITY_KIND_KERNEL) {
                handle_kernel((const CUpti_ActivityKernel12 *)record);
            } else if (record->kind == CUPTI_ACTIVITY_KIND_DEVICE) {
                // The only source for gpu_config_v1's sm_count and clock_hz:
                // there is no cuptiDeviceGetAttribute for either and this
                // adapter does not link libcuda. Enabled only under Tier B.
                const CUpti_ActivityDevice6 *d = (const CUpti_ActivityDevice6 *)record;
                g_sm_count.store(d->numMultiprocessors, std::memory_order_relaxed);
                // coreClockRate is kHz.
                g_core_clock_hz.store((uint64_t)d->coreClockRate * 1000ull,
                                      std::memory_order_relaxed);
            } else {
                g_activity_other.fetch_add(1, std::memory_order_relaxed);
            }
        }
    }
    // CUPTI's own overflow. Counted, never silent.
    size_t dropped = 0;
    if (perfagent::cupti::ActivityGetNumDroppedRecords_InBufferCompletedOnly(
            nullptr, 0, &dropped) == CUPTI_SUCCESS &&
        dropped)
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
         "modules_captured=%llu module_reload_skipped=%llu module_unattached=%llu "
         "module_no_bytes=%llu cubin_too_large=%llu cubin_crc_failed=%llu "
         "cubin_alloc_failed=%llu cubin_queue_full=%llu cubin_queue_depth=%zu "
         "cubins_sent=%llu cubin_send_failed=%llu "
         "cubins_offered=%llu cubin_transport_send_failed=%llu "
         "clock_steps=%llu clock_offset_ns=%lld sem_at_exit=%u\n",
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
         // The four Task 5 counters, plus the three drop paths they do not
         // cover and the two the transport itself keeps. A module counted
         // anywhere but modules_captured/cubins_sent is a module whose PC
         // samples will read gpu_src_status "no-module", so the gap between
         // the first number here and cubins_sent is exactly the size of what
         // this process could not explain.
         (unsigned long long)(g_cubins ? g_cubins->modules_captured() : 0),
         (unsigned long long)(g_cubins ? g_cubins->module_reload_skipped() : 0),
         (unsigned long long)g_module_unattached.load(),
         (unsigned long long)g_module_no_bytes.load(),
         (unsigned long long)(g_cubins ? g_cubins->cubin_too_large() : 0),
         (unsigned long long)(g_cubins ? g_cubins->cubin_crc_failed() : 0),
         (unsigned long long)(g_cubins ? g_cubins->cubin_alloc_failed() : 0),
         (unsigned long long)(g_cubins ? g_cubins->cubin_queue_full() : 0),
         g_cubins ? g_cubins->depth() : (size_t)0,
         (unsigned long long)(g_cubins ? g_cubins->cubins_sent() : 0),
         (unsigned long long)(g_cubins ? g_cubins->cubin_send_failed() : 0),
         (unsigned long long)perfagent::cubins_offered(),
         (unsigned long long)perfagent::cubins_send_failed(),
         (unsigned long long)g_clock.steps(),
         (long long)g_clock.offset_ns(),
         gpu_launch_sampled_v1_semaphore_count());
    for (unsigned i = 0; i < kResourceCbidMax; i++) {
        const uint64_t n = g_resource_events[i].load();
        if (n) logf("perfagent-cupti: resource cbid=%u count=%llu\n", i,
                    (unsigned long long)n);
    }
    // Reported whatever the tier: both are properties of the process, not of
    // PC sampling, and both are conditions this pipeline cannot represent on
    // the wire. Silence about them with the tier off would be the same silence
    // the drop classes exist to end.
    logf("perfagent-cupti: graph_execs=%llu multi_device=%llu devices=%zu\n",
         (unsigned long long)g_exec_from_graph.load(),
         (unsigned long long)g_multi_device.load(), g_devices_seen.size());
    // The CUPTI call guard (issue #99). Every one of these is assertable:
    //
    //   cupti_calls        acquisitions; must move on any run that touched CUPTI
    //   cupti_reentrant    acquisitions by a thread already inside one. NOT
    //                      expected to be zero: nearly every wrapper call is
    //                      made inside an outer CallScope, and burst_close
    //                      nests two deep on purpose. cupti_max_depth is the
    //                      counter that discriminates -- see below.
    //   cupti_max_depth    the deepest nesting reached. 3 on a healthy Tier A
    //                      run (burst_close -> pc_drain_all -> the GetData
    //                      wrapper), 2 in Tier B and on the context callbacks,
    //                      1 with PC sampling off. ANYTHING ABOVE 3 means CUPTI
    //                      re-entered us through a callback while we were
    //                      inside one of its calls -- the hazard the Task 6
    //                      report (item 13) and the Task 10 report (item 11)
    //                      both listed as unverified. This is the answer to
    //                      them, and before this guard existed that same
    //                      condition was a self-deadlock rather than a number.
    //   cupti_waits        acquisitions that had to block. Non-zero is healthy:
    //                      it is the application's callbacks meeting the timer.
    //                      Zero on a Tier A run would mean the guard is never
    //                      contended and therefore proving nothing.
    //   cupti_try_failed_* the fatal-error teardown finding the guard held, by
    //                      another thread or by its own. MUST be 0 on a healthy
    //                      run; they are the two halves of finalize_contended.
    //   cupti_calls_misplaced  a buffer-completed-only entry point called from
    //                      outside that callback. MUST be 0: it is the one
    //                      unguarded hole in the adapter and this is what says
    //                      nobody widened it.
    //   timer_ticks/drains/skipped  the merged timer. drains must be about
    //                      ticks * tick_ms / drain_ms; a drains of 0 with ticks
    //                      moving is a drain that silently stopped.
    logf("perfagent-cupti: cupti_calls=%llu cupti_reentrant=%llu cupti_waits=%llu "
         "cupti_try_failed_self=%llu cupti_try_failed_other=%llu cupti_max_depth=%u "
         "cupti_calls_misplaced=%llu "
         "timer_ticks=%llu timer_drains=%llu timer_skipped=%llu\n",
         (unsigned long long)perfagent::cupti::guard().entries(),
         (unsigned long long)perfagent::cupti::guard().reentries(),
         (unsigned long long)perfagent::cupti::guard().waits(),
         (unsigned long long)perfagent::cupti::guard().try_failed_self(),
         (unsigned long long)perfagent::cupti::guard().try_failed_other(),
         perfagent::cupti::guard().max_depth(),
         (unsigned long long)perfagent::cupti::misplaced_callback_calls().load(),
         (unsigned long long)(g_tick_plan ? g_tick_plan->ticks() : 0),
         (unsigned long long)(g_tick_plan ? g_tick_plan->drains() : 0),
         (unsigned long long)(g_tick_plan ? g_tick_plan->skipped() : 0));
    if (!g_pc_enabled) {
        logf("perfagent-cupti: pc_sampling=off tier_refused=%llu "
             "(set PERFAGENT_GPU_PC_SAMPLING=continuous or =serialized)\n",
             (unsigned long long)g_pc_tier_refused.load());
        return;
    }
    // Every drop class and every context-enable failure has a counter here,
    // and on a healthy run ctx_enable_failed, ctx_disable_failed,
    // getdata_failed, drain_rounds_capped, pc_dropped_hw, pc_buffer_full and
    // multi_device are all zero. pc_non_user is NOT expected to be zero --- it
    // is the size of a structural omission, not a fault.
    if (g_pc_tier_a) {
        // The perturbation, reported rather than assumed. bursts x burst_ns is
        // how much of this run ran with kernels serialized; duty is that as a
        // fraction of the interval since the first burst opened. windows must
        // be 2 x bursts on a clean run (one open, one closed per burst) and
        // 2 x bursts - 1 when the process died mid-burst.
        const uint64_t now = mono_ns();
        logf("perfagent-cupti: tier A bursts=%llu burst_ns=%llu duty=%.4f gap_ns=%llu "
             "windows=%llu range_end_drains=%llu start_failed=%llu stop_failed=%llu "
             "graph_refused=%llu sampling_now=%d\n",
             (unsigned long long)g_sampling_bursts.load(),
             (unsigned long long)g_sampling_burst_ns.load(),
             g_burst ? g_burst->duty(now) : 0.0,
             (unsigned long long)(g_burst ? g_burst->gap_ns() : 0),
             (unsigned long long)g_windows_emitted.load(),
             (unsigned long long)(g_pc_schedule ? g_pc_schedule->range_end() : 0),
             (unsigned long long)g_burst_start_failed.load(),
             (unsigned long long)g_burst_stop_failed.load(),
             (unsigned long long)g_tier_a_graph_refused.load(),
             g_burst && g_burst->sampling() ? 1 : 0);
        // Issue #101's evidence, and every one of these is assertable:
        //
        //   on_timer   PC starts/stops taken on the timer thread. MUST be 0.
        //              This is the deadlock itself, counted. It is not a
        //              proxy for the bug -- a non-zero value IS the condition
        //              that hung the 3090, observed rather than inferred.
        //   enter/exit launch boundaries the burst path saw. BOTH must move on
        //              any Tier A run that launched a kernel. Zero with a
        //              non-zero launch count means the transitions became
        //              unreachable, which from the outside is indistinguishable
        //              from an idle GPU and is what a stray early return looks
        //              like. exit is expected to be within one of enter.
        //   sync_*     the pre-start quiesce. missing MUST be 0 once a burst
        //              has opened: it means cuCtxSynchronize was not found and
        //              bursts started against a possibly busy GPU. failed
        //              counts a non-zero CUDA return from it.
        logf("perfagent-cupti: tier A boundaries enter=%llu exit=%llu on_timer=%llu "
             "sync_missing=%llu sync_failed=%llu empty_bursts=%llu\n",
             (unsigned long long)g_burst_enter_polls.load(),
             (unsigned long long)g_burst_exit_polls.load(),
             (unsigned long long)g_burst_calls_on_timer.load(),
             (unsigned long long)g_burst_sync_missing.load(),
             (unsigned long long)g_burst_sync_failed.load(),
             (unsigned long long)g_bursts_empty.load());
        // The one condition on this path that is otherwise invisible: the run
        // paid for its perturbation and got nothing back. Loud, because every
        // other counter reads healthy while it holds.
        const uint64_t closed = g_bursts_empty.load();
        const uint64_t total = g_sampling_bursts.load();
        if (total && closed == total) {
            logf("perfagent-cupti: WARNING every one of the %llu Tier A burst(s) in this run "
                 "pulled ZERO PCs. Kernels were serialized and the throughput cost was paid "
                 "in full, for no data. The usual cause is PERFAGENT_GPU_PC_MAX_PCS being too "
                 "large: CUPTI withholds PC data until it can nearly fill collectNumPcs, so a "
                 "high value means nothing is delivered inside a burst. It is %llu here and "
                 "the burst is %llums; on GA102/CUDA 13.3 at a 50ms burst, 64 yielded on every "
                 "burst and 512 on none. Lower it, lengthen the burst, or turn Tier A off. "
                 "See issue #102.\n",
                 (unsigned long long)total,
                 (unsigned long long)g_pc_collect_num_pcs,
                 (unsigned long long)(g_burst ? g_burst->config().burst_ns / 1000000ull : 0));
        }
    }
    logf("perfagent-cupti: pc %s tier=%s period=%u(=%u cycles) stall_reasons=%zu "
         "ctx_seen=%llu ctx_enabled=%llu ctx_enable_failed=%llu "
         "ctx_destroyed=%llu ctx_disable_failed=%llu "
         "pc_records=%llu pcs=%llu pc_batch_dropped=%llu pc_unattached=%llu "
         "zero_stall_pairs=%llu getdata=%llu getdata_failed=%llu "
         "drain_rounds_capped=%llu drains_periodic=%llu drains_unload=%llu "
         "drains_range_end=%llu drains_teardown=%llu drains_coalesced=%llu "
         "module_unload_drains=%llu "
         "dropped_hw=%llu buffer_full=%llu non_user_samples=%llu "
         "total_samples=%llu emitted_counts=%llu "
         "graph_execs=%llu multi_device=%llu finalize_seen=%llu "
         "finalize_contended=%llu "
         "config_emitted=%llu config_no_device=%llu sm_count=%u clock_hz=%llu\n",
         why, perfagent::pc_tier_name(g_pc_tier), g_pc_period,
         (g_pc_period >= kPCPeriodMin && g_pc_period <= kPCPeriodMax) ? (1u << g_pc_period) : 0u,
         g_num_stall_reasons,
         (unsigned long long)g_ctx_seen.load(),
         (unsigned long long)g_ctx_enabled.load(),
         (unsigned long long)g_ctx_enable_failed.load(),
         (unsigned long long)g_ctx_destroyed.load(),
         (unsigned long long)g_ctx_disable_failed.load(),
         (unsigned long long)g_pc_records.load(),
         (unsigned long long)g_pc_pcs.load(),
         (unsigned long long)(g_pcb ? g_pcb->dropped() : 0),
         (unsigned long long)g_pc_unattached.load(),
         (unsigned long long)g_pc_zero_stall.load(),
         (unsigned long long)g_pc_getdata_calls.load(),
         (unsigned long long)g_pc_getdata_failed.load(),
         (unsigned long long)g_pc_drain_rounds_capped.load(),
         (unsigned long long)(g_pc_schedule ? g_pc_schedule->periodic() : 0),
         (unsigned long long)(g_pc_schedule ? g_pc_schedule->unload() : 0),
         (unsigned long long)(g_pc_schedule ? g_pc_schedule->range_end() : 0),
         (unsigned long long)(g_pc_schedule ? g_pc_schedule->teardown() : 0),
         (unsigned long long)(g_pc_schedule ? g_pc_schedule->coalesced() : 0),
         (unsigned long long)g_module_unload_drains.load(),
         (unsigned long long)g_pc_dropped_hw.load(),
         (unsigned long long)g_pc_buffer_full.load(),
         (unsigned long long)g_pc_non_user.load(),
         (unsigned long long)g_pc_total_samples.load(),
         (unsigned long long)g_pc_emitted_counts.load(),
         (unsigned long long)g_exec_from_graph.load(),
         (unsigned long long)g_multi_device.load(),
         (unsigned long long)g_finalize_seen.load(),
         (unsigned long long)g_finalize_contended.load(),
         (unsigned long long)g_config_emitted.load(),
         (unsigned long long)g_config_no_device.load(),
         g_sm_count.load(), (unsigned long long)g_core_clock_hz.load());
    // The identity CUPTI does not let the agent check. totalSamples counts
    // every sample the hardware took, including samples for instructions whose
    // selected stall counts were all zero -- which produce no record at all --
    // so this is an inequality, not an equation, and it is stated here rather
    // than claimed anywhere as a check. Closing it would need a
    // gpu_config_v2 field, which is not worth a version bump for a diagnostic.
    logf("perfagent-cupti: pc identity (log only, not a check): "
         "emitted_counts + dropped_hw + non_user = %llu <= total_samples = %llu; "
         "the gap is instructions with all selected stall counts zero, which "
         "CUPTI never reports\n",
         (unsigned long long)(g_pc_emitted_counts.load() + g_pc_dropped_hw.load() +
                              g_pc_non_user.load()),
         (unsigned long long)g_pc_total_samples.load());
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
    // THE call site issue #99 was. It took no lock; it now goes through the
    // guard like every other, and it cannot be written any other way -- the
    // raw name does not compile below nvidia/cupti_guard.h.
    perfagent::cupti::ActivityFlushAll(0);
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

    // The SEND half of a cubin capture, and the whole reason this is here and
    // not in the MODULE_LOADED callback: a connect() plus an up-to-8 MiB
    // handover on the application's cuModuleLoad path would stall the
    // application for the profiler's benefit. Nothing is waiting on this
    // thread. The copy already happened, on time, in the callback.
    //
    // It MUST stay above the Tier B gate below. Cubin capture is not part of
    // PC sampling and is on whenever a consumer is attached, so putting this
    // after `if (!g_pc_enabled) return;` would silence the offer half of every
    // module capture in the DEFAULT configuration -- with modules_captured
    // still counting up and cubins_sent stuck at zero.
    if (g_cubins) g_cubins->drain(perfagent::cubin_offer_to_consumer, g_cubin_timeout_ms);

    // Graph-launched executions: counted continuously, put on the wire as a
    // delta whenever it moves. Deliberately BEFORE the Tier B gate --- the
    // condition it discloses is a property of the join, not of PC sampling,
    // and it is exactly as invisible with PC sampling off as with it on.
    // Emitting only the first would leave an operator unable to tell one graph
    // launch from a million; emitting the running total would double-count.
    const uint64_t graphs = g_exec_from_graph.load(std::memory_order_relaxed);
    const uint64_t reported = g_graph_exec_reported.load(std::memory_order_relaxed);
    if (graphs > reported) {
        g_graph_exec_reported.store(graphs, std::memory_order_relaxed);
        emit_dropped(graphs - reported, GPU_DROP_CLASS_GRAPH_EXEC);
    }

    if (!g_pc_enabled) return;

    // The config record is emitted from the tick rather than from the enable
    // path, because sm_count and clock_hz come from a DEVICE activity record
    // that the flush above is what delivers. By the first tick after a context
    // was enabled it is normally in hand; if it is not, the record still goes
    // out with sm_count 0 and g_config_no_device counts that, rather than the
    // record being withheld for a field it does not need.
    if (g_ctx_enabled.load(std::memory_order_relaxed)) pc_emit_config_once();

    // The schedule, not the tick, decides. A module unload may already have
    // pulled this data microseconds ago, and draining again would cost a
    // CUPTI call per context for nothing; skipped ticks are counted so a
    // schedule that stopped firing cannot look like a workload that stopped
    // stalling. See core/pcdrain.h.
    if (g_pc_schedule->due(mono_ns()))
        pc_drain_all(perfagent::PCDrainReason::kPeriodic);
    g_pcb->flush();

    // The stall map and the config record are one-shot and are queried at
    // context creation, long before a consumer can attach. ReplayLog replays
    // both on the unattached -> attached edge.
    g_replay->replay_if_newly_attached(gpu_stall_reason_map_v1_enabled());
}

// THE tick. One timer thread, and therefore one thread of ours that can ever
// be inside CUPTI --- which is the structural half of issue #99's fix: it does
// not exclude the bad state, it makes it unrepresentable. There is no second
// timer to race with, whatever a future call site does.
//
// It no longer polls the burst. Issue #101: PC-sampling start and stop cannot
// be taken from a thread of ours at all, whether or not that thread is alone,
// because the conflict is with the APPLICATION's thread inside CUPTI's launch
// path. Those two transitions now happen at launch boundaries on the
// application's own thread (burst_open_at_launch / burst_close_at_launch), and
// what is left here is the activity drain, which has run from this timer since
// before Tier A existed and is measured at -0.006% / +0.017% over two hardware
// runs.
//
// The drain is still rate-limited on top of the tick by core/tickplan.h. With
// the burst poll gone the tick no longer needs the shorter of the two periods
// on a Tier A run --- but tick_period_ns keeps deciding that, because
// PERFAGENT_GPU_DRAIN_MS is the only knob an operator has and a Tier A run
// still wants its PC data pulled promptly after each burst.
void on_timer_tick() {
    // Recorded here rather than at timer start because THIS is the thread that
    // matters: the one note_burst_thread() must never find inside a PC start
    // or stop. Idempotent and relaxed --- it is written every tick with the
    // same value, and read only by a counter that must stay 0.
    g_timer_tid.store(current_tid(), std::memory_order_relaxed);
    if (!g_tick_plan || g_tick_plan->drain_due(mono_ns())) on_tick();
}

// The documented CUPTI shutdown hook. Without it the records still sitting in
// a partly filled CUPTI buffer at exit are lost, and so is whatever is in the
// batches.
void at_exit_handler() {
    // The timer thread first of all, and this is a fix in its own right: the
    // old handler stopped the burst timer and left the DRAIN timer running, so
    // its cuptiActivityFlushAll ran concurrently with on_finalize's
    // cuptiPCSamplingDisable below -- the same two-threads-in-CUPTI shape as
    // issue #99, on the exit path. There is one timer now and it is stopped
    // and JOINED here, so from this line on the only thread of ours that can
    // reach CUPTI is this one.
    //
    // Stopping before burst_shutdown also means no tick can open a burst
    // against contexts the finalize below is about to disable, and the open
    // window is closed with the exit timestamp -- which is precisely what
    // makes end_ns == 0 on the wire mean a HARD exit and nothing else.
    if (g_timer) g_timer->stop();
    burst_shutdown();
    // PC sampling next, and specifically before cuptiActivityFlushAll: the
    // finalize handler drains and then disables each context, and
    // cuptiPCSamplingDisable is what joins CUPTI's PC worker threads. Doing it
    // after the activity flush would leave those threads running across the
    // flush for no reason, and doing it not at all is the undefined state this
    // handler exists to remove.
    on_finalize("exit");
    perfagent::cupti::ActivityFlushAll(1);
    if (g_lb) g_lb->flush();
    if (g_eb) g_eb->flush();
    if (g_pcb) g_pcb->flush();
    // One last offer pass, for modules captured inside the final drain
    // interval. Bounded: drain() offers at most the entries present when it
    // was entered, the queue holds at most CubinQueueLimits::max_entries, and
    // an offer to an absent listener refuses immediately -- so this cannot
    // turn process exit into a multi-second wait.
    if (g_cubins) g_cubins->drain(perfagent::cubin_offer_to_consumer, g_cubin_timeout_ms);
    report("exit");
}

unsigned env_uint(const char *name, unsigned dflt) {
    const char *v = getenv(name);
    if (!v || !*v) return dflt;
    const long n = strtol(v, nullptr, 10);
    if (n <= 0) return dflt;
    return (unsigned)n;
}

// The sampler's seed. Defaulted to Sampler::kDefaultSeed rather than randomized
// per process: a fixed seed is what keeps two runs of the same workload
// sampling the same launches, which is why this sampler was deterministic in
// the first place (shim/core/sampler.h). Set PERFAGENT_GPU_SAMPLE_SEED to any
// non-zero value -- decimal or 0x-prefixed -- to vary the schedule per process,
// e.g. to average several runs of the same workload.
uint64_t env_u64(const char *name, uint64_t dflt) {
    const char *v = getenv(name);
    if (!v || !*v) return dflt;
    char *end = nullptr;
    const unsigned long long n = strtoull(v, &end, 0);
    if (end == v || *end) return dflt;
    return (uint64_t)n;
}

bool check(CUptiResult r, const char *what) {
    if (r == CUPTI_SUCCESS) return true;
    const char *msg = "?";
    perfagent::cupti::GetResultString(r, &msg);
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
    // NOT gated on the probe semaphore, and that is the whole of issue #49's
    // second fix. The first version ran the rendezvous only under
    // gpu_launch_sampled_v1_enabled(), and on the RTX 3090 that read ZERO
    // here - 500 launches were sampled and 4000 probes fired later in the
    // same process, so the semaphore armed, just not by the time the driver
    // called InitializeInjection. Measured: UnwindEnrollRequests was 0 across
    // three runs and NoTables was unchanged at ~175/500.
    //
    // The semaphore answers "has the kernel told this process yet", not "is a
    // consumer attached". Under CUDA_INJECTION64_PATH this function runs
    // essentially the instant libcuda dlopens us, which is the earliest point
    // that question can be asked and the least likely to be answered yet.
    //
    // The connect is the gate instead, and it is a better one: the consumer
    // binds the rendezvous BEFORE it creates the uprobe link, so it is
    // listening whenever profiling is happening, and an unbound abstract
    // address refuses immediately in an unprofiled process. sem_at_init is
    // recorded so the arming time stays visible rather than inferred.
    const unsigned sem_at_init = gpu_launch_sampled_v1_semaphore_count();
    // Logged BEFORE the attempt: the two ends derive this name independently
    // and never exchange it, so when they disagree every counter on both sides
    // reads zero and nothing says why. Compare it against
    // Stats.UnwindEnrollAddress.
    char enroll_name[128];
    if (!perfagent::enroll_self_name(enroll_name, sizeof(enroll_name)))
        snprintf(enroll_name, sizeof(enroll_name), "<no-address>");
    // Logged for the same reason and compared against Stats.CubinsAddress:
    // the two ends derive this name independently and never exchange it, so
    // when they disagree every cubin counter on both sides reads zero and
    // every module is unresolvable with nothing saying why.
    char cubin_name[128];
    if (!perfagent::cubin_self_name(cubin_name, sizeof(cubin_name)))
        snprintf(cubin_name, sizeof(cubin_name), "<no-address>");
    perfagent::EnrollResult enrolled =
        perfagent::enroll_with_consumer(perfagent::enroll_timeout_ms(2000));
    // The gate for cubin capture -- see capture_enabled(). A confirmed
    // rendezvous is a positive statement that a consumer is attached, made
    // at a moment when the probe semaphore may still read zero.
    g_consumer_enrolled = (enrolled == perfagent::kEnrollConfirmed);

    // Leaked on purpose, never deleted: CUPTI worker threads keep calling
    // buffer_completed during process teardown, and a destroyed Batch or
    // Sampler under them is a use-after-free in somebody else's process.
    g_lb = new perfagent::Batch<gpu_launch_v1, 32>(gpu_launch_v1_emit, gpu_launch_v1_enabled);
    g_eb = new perfagent::Batch<gpu_exec_v1, 32>(gpu_exec_v1_emit, gpu_exec_v1_enabled);
    g_sampler = new perfagent::Sampler(env_uint("PERFAGENT_GPU_SAMPLE_PERIOD", 8),
                                      env_u64("PERFAGENT_GPU_SAMPLE_SEED",
                                              perfagent::Sampler::kDefaultSeed));
    g_names = new perfagent::KernelNameTable();
    g_timer = new perfagent::Drainer();
    // Leaked with the rest, and for the same reason: a CUPTI worker thread
    // can still be inside a RESOURCE callback during teardown, and a
    // destroyed queue under it is a use-after-free in somebody else's
    // process.
    g_cubins = new perfagent::CubinQueue();
    g_cubin_timeout_ms = perfagent::cubin_timeout_ms(2000);

    g_replay = new perfagent::ReplayLog();

    // The tier. OFF unless asked for, and read before cuptiSubscribe so the
    // RESOURCE callback cannot reach a half-initialized PC path.
    //
    // off | continuous | serialized (0 | 1 | 2), one variable, so the two
    // tiers cannot both be selected: they configure the same per-CUcontext
    // COLLECTION_MODE attribute, and "both" would produce a profile whose
    // attribution quality varied along an axis the operator can neither see
    // nor control. See core/pctier.h; the agent's half is gpu/tier.go.
    //
    // Every refusal below falls CLOSED to off and says so at length. It never
    // picks a tier: an unreadable setting resolved to "the cheaper one" is a
    // decision the operator did not make and cannot see in the output.
    {
        const char *raw = getenv("PERFAGENT_GPU_PC_SAMPLING");
        char bad[96];
        switch (perfagent::pc_tier_parse(raw, &g_pc_tier, bad, sizeof(bad))) {
        case perfagent::PCTierParse::kOK:
            break;
        case perfagent::PCTierParse::kUnknown:
            g_pc_tier_refused.fetch_add(1, std::memory_order_relaxed);
            logf("perfagent-cupti: PERFAGENT_GPU_PC_SAMPLING=\"%s\" names \"%s\", which is "
                 "not a tier. The three values are off, continuous and serialized (0, 1, 2). "
                 "PC SAMPLING IS OFF for this process -- a setting that cannot be read is not "
                 "resolved to a guess.\n", raw ? raw : "", bad);
            break;
        case perfagent::PCTierParse::kNotExclusive:
            g_pc_tier_refused.fetch_add(1, std::memory_order_relaxed);
            logf("perfagent-cupti: PERFAGENT_GPU_PC_SAMPLING=\"%s\" names MORE THAN ONE TIER. "
                 "They are mutually exclusive and the selection is process-wide: "
                 "COLLECTION_MODE is a single per-CUcontext CUPTI attribute, and which context "
                 "a kernel lands on is the application's choice rather than the profiler's, so "
                 "\"both\" would produce one profile whose attribution quality varied along an "
                 "axis the operator can neither see nor control. PC SAMPLING IS OFF for this "
                 "process; name exactly one of off, continuous, serialized.\n", bad);
            break;
        }
    }
    g_pc_enabled = g_pc_tier != perfagent::PCSamplingTier::kOff;
    g_pc_tier_a = g_pc_tier == perfagent::PCSamplingTier::kSerialized;
    if (g_pc_enabled) {
        // 0 = leave CUPTI's own SM-count-derived default, which is then read
        // back so gpu_config_v1 reports the real period. Anything outside
        // CUPTI's documented 5..31 is refused here rather than passed through
        // to be rejected per-attribute later.
        const unsigned want = env_uint("PERFAGENT_GPU_PC_PERIOD", 0);
        if (want && (want < kPCPeriodMin || want > kPCPeriodMax)) {
            logf("perfagent-cupti: PERFAGENT_GPU_PC_PERIOD=%u outside %u..%u; "
                 "using CUPTI's default\n", want, kPCPeriodMin, kPCPeriodMax);
        } else {
            g_pc_period = want;
        }
        g_pc_collect_num_pcs = env_uint("PERFAGENT_GPU_PC_MAX_PCS",
                                        (unsigned)kPCDefaultCollectNumPcs);
        g_pc_scratch_bytes = (size_t)env_uint("PERFAGENT_GPU_PC_SCRATCH_MB", 0) << 20;
        g_pc_hw_buffer_bytes = (size_t)env_uint("PERFAGENT_GPU_PC_HW_BUFFER_MB", 0) << 20;
        g_pcb = new perfagent::Batch<gpu_pc_sample_batch_v1, 32>(
            gpu_pc_sample_batch_v1_emit, gpu_pc_sample_batch_v1_enabled);
        // The PC drain rides the drain timer, so its period is the drain
        // period unless overridden. See core/pcdrain.h for why the schedule is
        // a thing of its own rather than an `if` in on_tick.
        g_pc_schedule = new perfagent::PCDrainSchedule(
            (uint64_t)env_uint("PERFAGENT_GPU_PC_DRAIN_MS",
                               env_uint("PERFAGENT_GPU_DRAIN_MS", 100)) * 1000000ull);
        g_replay->on_replay_stall([](const gpu_stall_reason_map_v1 &r) {
            if (gpu_stall_reason_map_v1_enabled())
                gpu_stall_reason_map_v1_emit(&r, 1,
                    g_stall_seq.fetch_add(1, std::memory_order_relaxed));
        });
        g_replay->on_replay_config([](const gpu_config_v1 &r) {
            if (gpu_config_v1_enabled())
                gpu_config_v1_emit(&r, 1, g_config_seq.fetch_add(1, std::memory_order_relaxed));
        });
    }
    if (g_pc_tier_a) {
        perfagent::BurstConfig bc;
        bc.burst_ns = (uint64_t)env_uint("PERFAGENT_GPU_PC_BURST_MS", 50) * 1000000ull;
        bc.target_rate = (double)env_uint("PERFAGENT_GPU_PC_TARGET_RATE", 100);
        // Per-mille, so a ceiling can be expressed without a float in the
        // environment. 100 = 10%.
        bc.max_duty = (double)env_uint("PERFAGENT_GPU_PC_MAX_DUTY_PERMILLE", 100) / 1000.0;
        bc.max_gap_ns = (uint64_t)env_uint("PERFAGENT_GPU_PC_MAX_GAP_MS", 10000) * 1000000ull;
        g_burst = new perfagent::BurstController(bc);
        // No burst tick, and no PERFAGENT_GPU_PC_BURST_TICK_MS: issue #101
        // moved both transitions onto the application's launch callbacks, so
        // the duty cycle's granularity is now the launch rate, not a period of
        // ours. A knob that no longer reaches anything is worse than no knob,
        // so it is gone rather than accepted-and-ignored.
        //
        // What this changes in practice: a burst is 50 ms PLUS however long
        // the application takes to make its next launch after the 50 ms
        // elapses. On any workload Tier A is worth running on that is
        // microseconds. On one that stops launching entirely it is unbounded,
        // and the window stays open --- which over-states the perturbation and
        // is the safe direction. See burst_open_at_launch.
        logf("perfagent-cupti: tier A duty cycle burst_ms=%llu target_rate=%.0f/s "
             "max_duty=%.3f min_gap_ms=%llu max_gap_ms=%llu boundary=launch\n",
             (unsigned long long)(bc.burst_ns / 1000000ull), bc.target_rate, bc.max_duty,
             (unsigned long long)(perfagent::burst_min_gap_ns(bc) / 1000000ull),
             (unsigned long long)(bc.max_gap_ns / 1000000ull));
    }

    if (!check(perfagent::cupti::Subscribe(&g_subscriber, (CUpti_CallbackFunc)on_callback,
                                          nullptr),
               "cuptiSubscribe"))
        return 0;
    // RUNTIME_API and RESOURCE only. Adding DRIVER_API more than doubled
    // correlation-id consumption in the spike (2.242 vs 1.004 ids per launch),
    // which halves the time to a uint32 wrap, and buys this ABI nothing.
    check(perfagent::cupti::EnableDomain(1, g_subscriber, CUPTI_CB_DOMAIN_RUNTIME_API),
          "enable RUNTIME_API");
    check(perfagent::cupti::EnableDomain(1, g_subscriber, CUPTI_CB_DOMAIN_RESOURCE),
          "enable RESOURCE");
    if (g_pc_enabled) {
        // The only notification that CUPTI is about to finalize itself. Not
        // subscribed when Tier B is off: with no PC sampling enabled there is
        // nothing for the handler to tear down, and the subscription is not
        // free.
        check(perfagent::cupti::EnableDomain(1, g_subscriber, CUPTI_CB_DOMAIN_STATE),
              "enable STATE");
    }

    check(perfagent::cupti::ActivityRegisterCallbacks(buffer_requested, buffer_completed),
          "cuptiActivityRegisterCallbacks");
    check(perfagent::cupti::ActivityEnable(CUPTI_ACTIVITY_KIND_CONCURRENT_KERNEL),
          "enable CONCURRENT_KERNEL");
    if (g_pc_enabled) {
        // sm_count and clock_hz for gpu_config_v1 have no other source: CUPTI
        // has no device attribute for either and this adapter does not link
        // libcuda. One record per device, delivered once.
        check(perfagent::cupti::ActivityEnable(CUPTI_ACTIVITY_KIND_DEVICE), "enable DEVICE");
    }

    // Seed the fit before any activity record can be converted.
    resample_clock();

    const unsigned drain_ms = env_uint("PERFAGENT_GPU_DRAIN_MS", 100);
    // ONE timer, serving ONE job: the activity drain. Two timer threads was
    // issue #99; a timer that touched PC sampling at all was issue #101, and
    // that half is now on the application's launch callbacks. The burst-tick
    // arguments are passed as "no burst poll" because there is none --- the
    // shorter-tick case in tick_period_ns is retained for a future second job
    // on this timer, not exercised by this one.
    const uint64_t tick_ns = perfagent::tick_period_ns(
        (uint64_t)drain_ms * 1000000ull, /*burst_tick_ns=*/0, /*tier_a=*/false);
    // drain_limit_ns, not the drain period: with Tier A off the timer already
    // runs at the drain period and a rate limit on top of it would skip any
    // tick that arrived a hair early. See core/tickplan.h.
    g_tick_plan = new perfagent::TickPlan(
        perfagent::drain_limit_ns((uint64_t)drain_ms * 1000000ull, tick_ns));

    // ARMED BEFORE THE TIMER STARTS, which the two-timer version got by
    // accident from its ordering and this one has to get on purpose: a burst
    // that opened before the exit handler existed could not be closed by it,
    // and an unclosed window would report a hard exit that did not happen.
    atexit(at_exit_handler);

    g_tick_plan->start(mono_ns());
    g_timer->on_tick(on_timer_tick);
    g_timer->start((unsigned)(tick_ns / 1000000ull));

    // The seed is logged because it IS the schedule: with it and the period,
    // the exact set of sampled launch ordinals is replayable offline
    // (internal/gpuabi.SampleSchedule). Without it a run using a non-default
    // seed could not be audited after the fact.
    //
    // sem_at_init vs sem_after_init is the measurement #49 needed: the first
    // fix gated the rendezvous on the semaphore and lost the CUDA path.
    logf("perfagent-cupti: pc_sampling=%s tier=%s\n",
         g_pc_enabled ? "on" : "off",
         !g_pc_enabled ? "none" : (g_pc_tier_a ? "A/kernel-serialized" : "B/continuous"));
    if (g_pc_tier_a) {
        logf("perfagent-cupti: WARNING Tier A SERIALIZES GPU kernels while a burst is "
             "open. Kernel durations inside a window are inflated by the measurement "
             "and are marked gpu_serialized=\"true\"; CPU and off-CPU samples taken "
             "during a burst are distorted and carry NO marking at all; and Tier A "
             "refuses to run where CUDA graphs are in use.\n");
    }
    logf("perfagent-cupti: initialized pid=%d sample_period=%u sample_seed=0x%016llx "
         "drain_ms=%u clock_offset_ns=%lld enroll=%s sem_at_init=%u sem_after_init=%u "
         "enroll_addr=@%s cubin_addr=@%s cubin_timeout_ms=%u\n",
         (int)getpid(), g_sampler->period(),
         (unsigned long long)g_sampler->seed(), drain_ms, (long long)g_clock.offset_ns(),
         perfagent::enroll_result_name(enrolled), sem_at_init,
         gpu_launch_sampled_v1_semaphore_count(), enroll_name, cubin_name,
         g_cubin_timeout_ms);
    return 1;
}
