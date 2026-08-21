// A GPU-free producer. Emits the same records a vendor adapter would, so the
// consumer and the whole pipeline can be exercised on a machine with no GPU
// (spec §14, Phase 3 gate).
#include "batch.h"
#include "clock.h"
#include "drain.h"
#include "kernelnames.h"
#include "sampler.h"
#include "usdt_abi.h"
#include "usdt_probe.h"

#include <chrono>
#include <cstdio>
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

static uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

// Default visibility: this is the one symbol libperfagent-gpu-stub.so must
// export. Everything else in core/ and this file stays hidden (the Makefile
// builds with -fvisibility=hidden) so the shim does not leak symbols into
// whatever process it's injected into.
extern "C" __attribute__((visibility("default"))) void
perfagent_stub_run(unsigned launches, unsigned period_us, unsigned sample_period) {
    perfagent::Batch<gpu_launch_v1, 32> lb(gpu_launch_v1_emit, gpu_launch_v1_enabled);
    perfagent::Batch<gpu_exec_v1, 32> eb(gpu_exec_v1_emit, gpu_exec_v1_enabled);
    perfagent::Sampler sampler(sample_period);
    perfagent::KernelNameTable names;
    unsigned long sampled_seq = 0;
    unsigned long name_seq = 0;
    bool names_was_attached = false;

    perfagent::Drainer drainer;
    drainer.on_tick([&] {
        lb.flush();
        eb.flush();
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
        lb.add(l);

        gpu_exec_v1 e{};
        e.correlation = i;
        e.kernel_id = l.kernel_id;
        e.queue_id = 1;
        e.device_id = 0;
        e.start_ns = now + 10000;          // 10us after the launch
        e.end_ns = now + 10000 + 50000;    // 50us on device
        eb.add(e);

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

        if (period_us) std::this_thread::sleep_for(std::chrono::microseconds(period_us));
    }

    lb.flush();
    eb.flush();
    drainer.stop();
    fprintf(stderr, "stub: launches=%u observed=%llu sampled=%llu period=%u "
                    "launch_dropped=%llu exec_dropped=%llu\n",
            launches, (unsigned long long)sampler.observed(),
            (unsigned long long)sampler.sampled(), sampler.period(),
            (unsigned long long)lb.dropped(), (unsigned long long)eb.dropped());
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
