// A GPU-free producer. Emits the same records a vendor adapter would, so the
// consumer and the whole pipeline can be exercised on a machine with no GPU
// (spec §14, Phase 3 gate).
#include "batch.h"
#include "clock.h"
#include "cubin.h"
#include "cubinqueue.h"
#include "drain.h"
#include "enroll.h"
#include "kernelnames.h"
#include "sampler.h"
#include "usdt_abi.h"
#include "usdt_probe.h"

#include <chrono>
#include <cstdio>
#include <string>
#include <vector>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <cerrno>
#include <poll.h>
#include <sys/syscall.h>
#include <thread>
#include <unistd.h>

static inline uint32_t current_tid() { return (uint32_t)syscall(SYS_gettid); }

// One line per probe: the semaphore, the enabled/emit thunks, and the frozen
// wire size the consumer's attach cookie assumes. Because the thunk's
// parameter type and the probe it fires come from the same token, and
// Batch's emit callback is typed on its record, a Batch can only be wired to
// the probe whose records it actually holds -- see PERFAGENT_USDT_EMITTER.
PERFAGENT_USDT_EMITTER(gpu_launch_v1, 48);
PERFAGENT_USDT_EMITTER(gpu_exec_v1, 48);
// gpu_launch_sampled_v1 fires UNBATCHED -- one probe per launch selected by
// the Sampler, never through Batch<T,N>. Batching would attach one captured
// stack to N unrelated launches, defeating the entire feature.
PERFAGENT_USDT_EMITTER(gpu_launch_sampled_v1, 56);
PERFAGENT_USDT_EMITTER(gpu_kernel_name_v1, 272);
// gpu_module_load_v1 fires UNBATCHED, one record per module, exactly as the
// CUPTI adapter fires it -- and from inside CubinQueue::capture, at the one
// instant the copy is owned by nobody else, so bytes_ptr is true when the
// probe reads it.
PERFAGENT_USDT_EMITTER(gpu_module_load_v1, 40);

static uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

// ------------------------------------------------------- the fake module load
//
// A GPU-free MODULE_LOADED. It reads a checked-in cubin from disk, runs it
// through the SAME CubinQueue the CUPTI adapter uses -- same capture, same
// CubinView, same crc-over-the-copy, same drain-thread offer -- and fires
// gpu_module_load_v1 from inside capture(). That drives the cubin transport
// (Task 3), the module store (Task 4), the consumer's decode (Task 7) and the
// projection's source labels (Task 9) end to end on a machine with no GPU,
// which is what the phase gate needs.
//
// PERFAGENT_STUB_CUBINS is a ':'-separated list of paths, in the shape PATH
// itself uses, so a gate can drive several modules -- the -lineinfo fixture
// and the no-lineinfo one -- through one run and reach more than one
// gpu_src_status value.
//
// Reuse the fixtures in internal/cubin/testdata/ rather than inventing bytes:
// the same cubins are then exercised by the reader's tests, by the module
// store's, and by the transport's, so "the reader parses what the transport
// delivers" is one fixture rather than two that agree by assumption.

// The stand-in for cuptiGetCubinCrc(), and it is NOT that function.
//
// CUPTI's polynomial is unpublished and there is no CUDA toolkit on this
// path. What the join actually requires is that ONE number identifies one set
// of bytes and that the same number reaches both ends -- so a stub that
// declares a content hash produces a pipeline that is exercised exactly as a
// real one is, keyed on a number that means the same thing.
//
// FNV-1a, the same spelling hash_name() uses in the adapter, so a test can
// recompute it independently and assert the key rather than read it back.
static uint64_t stub_cubin_crc(const void *bytes, size_t len) {
    const unsigned char *p = (const unsigned char *)bytes;
    uint64_t h = 1469598103934665603ULL;
    for (size_t i = 0; i < len; i++) {
        h ^= p[i];
        h *= 1099511628211ULL;
    }
    return h ? h : 1;   // zero is the ABI's "no module"
}

static unsigned long g_stub_module_seq = 0;

// Fired by CubinQueue::capture, before the copy is visible to any drain, so
// bytes_ptr names live adapter-owned bytes at the moment the probe reads it.
// It is still not a transport: the consumer cannot read another process's
// address space without CAP_SYS_PTRACE. The bytes go over the cubin channel.
static void stub_on_cubin_captured(void *ctx, uint64_t crc, const void *bytes, size_t len) {
    if (!gpu_module_load_v1_enabled()) return;
    gpu_module_load_v1 r{};
    r.cubin_crc = crc;
    r.module_id = (uint64_t)(uintptr_t)ctx;
    r.size_bytes = (uint64_t)len;
    r.load_ns = mono_ns();
    r.bytes_ptr = (uint64_t)(uintptr_t)bytes;
    gpu_module_load_v1_emit(&r, 1, g_stub_module_seq++);
}

static bool read_whole_file(const char *path, std::vector<char> *out) {
    FILE *f = fopen(path, "rbe");
    if (!f) return false;
    char buf[65536];
    for (;;) {
        const size_t n = fread(buf, 1, sizeof(buf), f);
        if (n) out->insert(out->end(), buf, buf + n);
        if (n < sizeof(buf)) break;
    }
    const bool ok = ferror(f) == 0;
    fclose(f);
    return ok;
}

// Captures every module named by PERFAGENT_STUB_CUBINS. Returns how many
// reached the queue. Reading the file is the stub's stand-in for the vendor
// handing us a buffer, so the CubinView is built over the file buffer and
// dies with this function, exactly as the adapter's dies with its callback.
static unsigned stub_capture_modules(perfagent::CubinQueue &q) {
    const char *list = getenv("PERFAGENT_STUB_CUBINS");
    if (!list || !*list) return 0;
    unsigned n = 0;
    std::string spec(list);
    size_t pos = 0;
    while (pos <= spec.size()) {
        const size_t sep = spec.find(':', pos);
        const std::string path =
            spec.substr(pos, sep == std::string::npos ? std::string::npos : sep - pos);
        pos = (sep == std::string::npos) ? spec.size() + 1 : sep + 1;
        if (path.empty()) continue;

        std::vector<char> bytes;
        if (!read_whole_file(path.c_str(), &bytes) || bytes.empty()) {
            fprintf(stderr, "stub: module read failed path=%s\n", path.c_str());
            continue;
        }
        const uint64_t crc = stub_cubin_crc(bytes.data(), bytes.size());
        const perfagent::CubinView view(bytes.data(), bytes.size());
        const bool ok = q.capture(view, stub_cubin_crc, stub_on_cubin_captured,
                                  (void *)(uintptr_t)(n + 1));
        fprintf(stderr, "stub: module id=%u path=%s size=%zu crc=0x%016llx captured=%s\n",
                n + 1, path.c_str(), bytes.size(), (unsigned long long)crc,
                ok ? "yes" : "no");
        if (ok) n++;
    }
    return n;
}

// Default visibility: this is the one symbol libperfagent-gpu-stub.so must
// export. Everything else in core/ and this file stays hidden (the Makefile
// builds with -fvisibility=hidden) so the shim does not leak symbols into
// whatever process it's injected into.
extern "C" __attribute__((visibility("default"))) void
perfagent_stub_run(unsigned launches, unsigned period_us, unsigned sample_period) {
    // The #49 startup rendezvous, before the first launch and therefore
    // before the first probe: wait for the consumer to install this process's
    // CFI tables, so the kernel-side walk of every sampled launch has them.
    //
    // NOT gated on the probe semaphore. See core/enroll.h: the semaphore
    // answers "has the kernel told this process a consumer is attached yet",
    // which is a different question from "is one attached", and on the CUDA
    // path the two differ at exactly this moment. The connect is the gate -
    // an unbound abstract address refuses immediately.
    const unsigned sem_at_enroll = gpu_launch_sampled_v1_semaphore_count();
    // See the adapter: the name is logged so a disagreement between the two
    // ends is one string comparison rather than a round trip of inference.
    char enroll_name[128];
    if (!perfagent::enroll_self_name(enroll_name, sizeof(enroll_name)))
        snprintf(enroll_name, sizeof(enroll_name), "<no-address>");
    // The cubin channel's name, printed for the reason the rendezvous name is:
    // the two ends derive it independently and never exchange it, so a
    // disagreement makes every counter on both sides read zero.
    char cubin_name[128];
    if (!perfagent::cubin_self_name(cubin_name, sizeof(cubin_name)))
        snprintf(cubin_name, sizeof(cubin_name), "<no-address>");
    perfagent::EnrollResult enrolled =
        perfagent::enroll_with_consumer(perfagent::enroll_timeout_ms(2000));

    perfagent::Batch<gpu_launch_v1, 32> lb(gpu_launch_v1_emit, gpu_launch_v1_enabled);
    perfagent::Batch<gpu_exec_v1, 32> eb(gpu_exec_v1_emit, gpu_exec_v1_enabled);
    perfagent::Sampler sampler(sample_period);
    perfagent::KernelNameTable names;
    unsigned long sampled_seq = 0;
    unsigned long name_seq = 0;
    bool names_was_attached = false;

    // The same queue the CUPTI adapter runs, wired the same way: capture on
    // the caller's thread, offer on the drain thread.
    perfagent::CubinQueue cubins;
    const unsigned cubin_timeout_ms = perfagent::cubin_timeout_ms(2000);

    perfagent::Drainer drainer;
    drainer.on_tick([&] {
        lb.flush();
        eb.flush();
        // The offer, on the drain thread and nowhere else. In an application
        // this is what keeps a connect() and a multi-megabyte handover off
        // the cuModuleLoad path; here it is what proves that wiring works.
        cubins.drain(perfagent::cubin_offer_to_consumer, cubin_timeout_ms);
        // Late-attach replay: only on the unattached -> attached transition,
        // so an already-attached consumer does not get names re-sent every
        // tick (spec §6.1's replay contract).
        const bool now_attached = gpu_kernel_name_v1_enabled();
        if (now_attached && !names_was_attached) {
            names.replay([&](const gpu_kernel_name_v1 &r) {
                if (gpu_kernel_name_v1_enabled())
                    gpu_kernel_name_v1_emit(&r, 1, name_seq++);
            });
        }
        names_was_attached = now_attached;
    });
    drainer.start(100);

    // Modules load before kernels run, as they do in a CUDA process. The
    // capture is synchronous; the offers are not, so wait -- bounded -- for
    // the drain thread to have emptied the queue before the launches start,
    // so a gate can assert on a module the consumer provably already holds.
    const unsigned modules = stub_capture_modules(cubins);
    if (modules) {
        const uint64_t wait_until = mono_ns() + 5000000000ULL;
        while (cubins.depth() && mono_ns() < wait_until) {
            std::this_thread::sleep_for(std::chrono::milliseconds(5));
        }
    }

    for (unsigned i = 1; i <= launches; i++) {
        const uint64_t now = mono_ns();
        const uint64_t kernel_id = (i % 2) ? 0x1111 : 0x2222;

        gpu_kernel_name_v1 nrec{};
        char namebuf[32];
        snprintf(namebuf, sizeof(namebuf), "kernel_%llx", (unsigned long long)kernel_id);
        if (names.intern(kernel_id, namebuf, &nrec) && gpu_kernel_name_v1_enabled()) {
            gpu_kernel_name_v1_emit(&nrec, 1, name_seq++);
        }

        gpu_launch_v1 l{};
        l.correlation = i;                 // never zero: spec §6.1
        l.kernel_id = kernel_id;
        l.queue_id = 1;
        l.context_id = 1;
        l.time_ns = now;
        l.tid = current_tid();

        // The sampled probe fires BEFORE the launch reaches its batch, and
        // that order is the fix for issue #67 rather than a stylistic
        // preference. The two records are twins - same correlation, one
        // carrying the launch, the other only the CPU stack the consumer
        // staples onto it (gpuprobe/sampledstacks.go) - and the consumer's
        // two join paths are not equally safe. Sampled first parks the stack
        // in pendingStacks, where only the twin can claim it and any number
        // of unrelated batches may pass in between. Batched first holds the
        // launch in deferredLaunches, which the very next batch of any other
        // kind releases stackless - deliberately, since the timeline wants
        // launches promptly - leaving the stack to park with nothing to join.
        //
        // With the add() first, a launch that both FILLED the batch and was
        // sampled put the batched record on the wire inside that add(), and
        // the exec batch below landed between the twins: 58 sampled, 57
        // attached, 1 parked forever on the privileged gate. Firing here
        // instead makes sampled-first unconditional, because a record cannot
        // be in a batch before add() puts it there - so no flush, on this
        // thread or on the Drainer's, can carry the twin past this probe.
        //
        // should_sample() is called on every launch regardless (&& is
        // short-circuit and the sampler call is the left operand), so the
        // schedule does not depend on whether a consumer is attached.
        //
        // Unbatched: the eBPF consumer captures the calling thread's stack
        // the instant this probe fires, so the record must ride alone.
        if (sampler.should_sample() && gpu_launch_sampled_v1_enabled()) {
            gpu_launch_sampled_v1 sl{};
            sl.correlation = i;
            sl.kernel_id = l.kernel_id;
            sl.queue_id = 1;
            sl.context_id = 1;
            sl.time_ns = now;
            sl.tid = current_tid();
            sl.sample_period = sampler.period();
            sl.launch_seq = i - 1;         // ordinal among ALL launches
            gpu_launch_sampled_v1_emit(&sl, 1, sampled_seq++);
        }

        lb.add(l);

        gpu_exec_v1 e{};
        e.correlation = i;
        e.kernel_id = l.kernel_id;
        e.queue_id = 1;
        e.device_id = 0;
        e.start_ns = now + 10000;          // 10us after the launch
        e.end_ns = now + 10000 + 50000;    // 50us on device
        eb.add(e);

        if (period_us) std::this_thread::sleep_for(std::chrono::microseconds(period_us));
    }

    lb.flush();
    eb.flush();
    // A final pass for anything the tick did not reach. drain() is bounded by
    // the depth it saw on entry, so this cannot become an unbounded wait.
    cubins.drain(perfagent::cubin_offer_to_consumer, cubin_timeout_ms);
    drainer.stop();
    // Every cubin counter, always -- not only when modules were requested.
    // A run that asked for none must read zero everywhere, and a line that
    // appears only on the interesting runs is a line no one checks on the
    // boring ones.
    fprintf(stderr, "stub: cubins requested=%u captured=%llu reload_skipped=%llu "
                    "queue_full=%llu too_large=%llu crc_failed=%llu alloc_failed=%llu "
                    "sent=%llu send_failed=%llu pending=%zu "
                    "offered=%llu transport_send_failed=%llu timeout_ms=%u cubin_addr=@%s\n",
            modules, (unsigned long long)cubins.modules_captured(),
            (unsigned long long)cubins.module_reload_skipped(),
            (unsigned long long)cubins.cubin_queue_full(),
            (unsigned long long)cubins.cubin_too_large(),
            (unsigned long long)cubins.cubin_crc_failed(),
            (unsigned long long)cubins.cubin_alloc_failed(),
            (unsigned long long)cubins.cubins_sent(),
            (unsigned long long)cubins.cubin_send_failed(),
            cubins.depth(),
            (unsigned long long)perfagent::cubins_offered(),
            (unsigned long long)perfagent::cubins_send_failed(),
            cubin_timeout_ms, cubin_name);
    // seed= is part of the accounting, not decoration: the sampler's schedule
    // is a deterministic chain from (seed, period), so this line is what makes
    // the exact sampled= count above reproducible and auditable offline
    // (internal/gpuabi.SampleSchedule replays it).
    //
    // sem_at_enroll vs sem_at_exit is the measurement issue #49's first fix
    // needed and did not have: 0 then non-zero means the semaphore had not
    // armed when the rendezvous ran, which is why gating on it lost the CUDA
    // path entirely.
    fprintf(stderr, "stub: launches=%u observed=%llu sampled=%llu period=%u seed=0x%016llx "
                    "launch_dropped=%llu exec_dropped=%llu enroll=%s "
                    "sem_at_enroll=%u sem_at_exit=%u enroll_addr=@%s\n",
            launches, (unsigned long long)sampler.observed(),
            (unsigned long long)sampler.sampled(), sampler.period(),
            (unsigned long long)sampler.seed(),
            (unsigned long long)lb.dropped(), (unsigned long long)eb.dropped(),
            perfagent::enroll_result_name(enrolled),
            sem_at_enroll, gpu_launch_sampled_v1_semaphore_count(), enroll_name);
}

// linger keeps this process alive after its records are flushed, until the
// parent closes our stdin or linger_ms elapses -- whichever comes first.
//
// This exists for the consumer, not for us. A sampled launch's CPU stack is
// symbolized against /proc/<pid>/maps of the LAUNCHING process, and the
// kernel tears that down the moment the process exits. A producer that
// flushes and exits immediately therefore races its own consumer: any record
// still in the ringbuf when we die symbolizes to bare addresses. Lingering
// hands the consumer a window in which the maps it needs still exist.
//
// EOF on stdin, not a fixed sleep: it lets the consumer release us the
// instant it has what it needs, so the gate does not trade a race for a
// guessed delay. linger_ms is the backstop for a stub run by hand from a
// terminal, where stdin never reaches EOF -- it must never hang forever.
// Not static, and not named `linger`: the FP-less producer
// (stub/fpless_producer.cc) is a second `main` over this same
// translation unit, and it needs the identical linger contract. A
// second copy of this loop would be a second thing to keep correct.
extern "C" void perfagent_stub_linger(unsigned linger_ms) {
    if (!linger_ms) return;
    struct pollfd p{};
    p.fd = STDIN_FILENO;
    p.events = POLLIN;
    // Loop, because poll() returns on readable stdin as well as on EOF and a
    // parent that writes something is not a parent that released us. Drain
    // and keep waiting; only a zero-length read (EOF) or the deadline ends it.
    const uint64_t deadline = mono_ns() + (uint64_t)linger_ms * 1000000ULL;
    for (;;) {
        const uint64_t now = mono_ns();
        if (now >= deadline) return;
        const int left = (int)((deadline - now) / 1000000ULL) + 1;
        const int rc = poll(&p, 1, left);
        if (rc < 0) {
            if (errno == EINTR) continue;
            return;
        }
        if (rc == 0) return;                      // deadline
        char buf[256];
        const ssize_t got = read(STDIN_FILENO, buf, sizeof(buf));
        if (got <= 0) return;                     // EOF, or a broken stdin
    }
}

// PERFAGENT_STUB_NO_MAIN lets this file be linked under a different
// entry point without duplicating perfagent_stub_run or the probe
// emitters. stub/fpless_producer.cc defines that other main; it
// reaches perfagent_stub_run through a frame-pointer-less bridge so
// the stack the consumer walks has an FP-less frame in it, which the
// straight-line binary built from this file alone does not guarantee.
#ifndef PERFAGENT_STUB_NO_MAIN
int main(int argc, char **argv) {
    const unsigned n = argc > 1 ? (unsigned)atoi(argv[1]) : 1000;
    const unsigned us = argc > 2 ? (unsigned)atoi(argv[2]) : 100;
    const unsigned sp = argc > 3 ? (unsigned)atoi(argv[3]) : 8;
    const unsigned lm = argc > 4 ? (unsigned)atoi(argv[4]) : 0;
    perfagent_stub_run(n, us, sp);
    perfagent_stub_linger(lm);
    return 0;
}
#endif // PERFAGENT_STUB_NO_MAIN
