// Proves the producer emits a sampled launch's record BEFORE the batched
// gpu_launch_v1 record for that same launch.
//
// Why that order is the thing under test (issue #67)
// --------------------------------------------------
// The two records are twins: gpu_launch_sampled_v1 carries no launch of its
// own, only the CPU stack the consumer must staple onto the batched
// gpu_launch_v1 with the same correlation (gpuprobe/sampledstacks.go). The
// consumer can join them in either order, but the two orders are not equally
// safe:
//
//   - sampled first: the stack parks in pendingStacks and nothing but the
//     twin can claim it. Any number of unrelated batches may arrive in
//     between; the join still happens.
//   - batched first: the launch is held in deferredLaunches, and the FIRST
//     batch of any other kind releases it stackless - deliberately, because
//     the timeline wants launches promptly. The stack then arrives with
//     nothing to join and parks forever.
//
// The producer used to add the launch to its batch and only then fire the
// sampled probe, so a launch that both FILLED the batch and was sampled put
// the batched record on the wire first - with the exec batch of that same
// loop iteration landing between the twins. Measured on the privileged gate:
// 58 sampled, 57 attached, 1 parked forever. At the old fixed sampler stride
// this was arithmetically unreachable at 500 launches (a multiple of 8 is
// never 31 mod 32); the jittered stride of issue #50 made it reachable.
//
// Firing the sampled probe first makes sampled-first unconditional: a record
// cannot be in a batch before add() puts it there, so no flush - on this
// thread or the drain thread - can carry the twin past the sampled record.
//
// How this test sees the wire order without any privilege
// -------------------------------------------------------
// The same trick core/probe_args_test.cc uses: read our own .note.stapsdt to
// find the probe sites, patch their one-byte nops with int3 (which is all a
// uprobe does), and read the arguments out of the trapped context in the
// SIGTRAP handler - exactly what bpf_probe_read_user does in
// bpf/gpu_usdt.bpf.c. Every probe fire is stamped with a global tick, so the
// order the consumer would see is reconstructed exactly. No CAP_BPF, no
// consumer, no GPU.
#include "usdt_abi.h"

#include <cassert>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <elf.h>
#include <fcntl.h>
#include <signal.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#if !defined(__x86_64__)
int main() {
    // The probe macro binds rdi/rsi/rdx by name, so reading the arguments out
    // of a trapped context is as architecture-specific as the ABI itself.
    fprintf(stderr, "probe_order_test: skipped, not x86-64\n");
    return 0;
}
#else

#include <ucontext.h>

// The producer under test. stub/stub.cc is compiled into this binary with
// PERFAGENT_STUB_NO_MAIN, so this is the very code the gate runs.
extern "C" void perfagent_stub_run(unsigned launches, unsigned period_us,
                                   unsigned sample_period);

// stub.cc's probe semaphores. Hidden visibility, so this only links because
// the producer and the test are one binary - which is the point: nothing here
// asks the producer to behave differently for a test.
extern "C" unsigned short perfagent_gpu_launch_v1_semaphore
    __attribute__((visibility("hidden")));
extern "C" unsigned short perfagent_gpu_exec_v1_semaphore
    __attribute__((visibility("hidden")));
extern "C" unsigned short perfagent_gpu_launch_sampled_v1_semaphore
    __attribute__((visibility("hidden")));
extern "C" unsigned short perfagent_gpu_kernel_name_v1_semaphore
    __attribute__((visibility("hidden")));

// The address every stapsdt note is relative to; comparing it with the note's
// own base field yields the load bias, PIE or not.
extern "C" char perfagent_stapsdt_base_sym __asm__("_.stapsdt.base");

// ------------------------------------------------------------ the recorder

// Correlations in stub.cc are 1..launches, so a flat array indexed by
// correlation is exact and needs no hashing inside a signal handler.
static constexpr unsigned kMaxCorrelation = 4096;

// Tick of the probe fire that carried each correlation. Zero means "not seen
// yet". Written from the launch thread and from the Drainer's thread, at
// distinct indices in the common case and idempotently otherwise, through
// atomics because a signal handler racing another thread is still a race.
static unsigned long g_launch_at[kMaxCorrelation];
static unsigned long g_sampled_at[kMaxCorrelation];
static unsigned long g_tick;
static unsigned long g_full_batches;   // launch batches that flushed at N==32
static unsigned long g_launch_records;
static unsigned long g_sampled_records;
static unsigned long g_overflow;       // a correlation past kMaxCorrelation

enum { kProbeLaunch, kProbeSampled, kProbeCount };
static uintptr_t g_probe_site[kProbeCount];

static void note_first(unsigned long *slot, unsigned long tick) {
    unsigned long zero = 0;
    // First fire wins. A batch is flushed once, so this only ever matters if
    // a correlation were somehow emitted twice - in which case the EARLIER
    // record is the one the consumer joins against, and the later one must
    // not overwrite it.
    __atomic_compare_exchange_n(slot, &zero, tick, false, __ATOMIC_SEQ_CST,
                                __ATOMIC_SEQ_CST);
}

static void on_trap(int, siginfo_t *, void *ucv) {
    ucontext_t *uc = (ucontext_t *)ucv;
    // int3 leaves %rip one byte past the patched nop, which is where the nop
    // itself would have left it: returning from the handler resumes correctly
    // with no single-stepping and no byte to restore.
    const uintptr_t site = (uintptr_t)uc->uc_mcontext.gregs[REG_RIP] - 1;
    const void *ptr = (const void *)uc->uc_mcontext.gregs[REG_RDI];
    const unsigned long count = (unsigned long)uc->uc_mcontext.gregs[REG_RSI];
    const unsigned long tick = __atomic_add_fetch(&g_tick, 1, __ATOMIC_SEQ_CST);

    if (site == g_probe_site[kProbeLaunch]) {
        const struct gpu_launch_v1 *recs = (const struct gpu_launch_v1 *)ptr;
        if (count == 32) __atomic_add_fetch(&g_full_batches, 1, __ATOMIC_SEQ_CST);
        for (unsigned long i = 0; i < count; i++) {
            const uint64_t corr = recs[i].correlation;
            __atomic_add_fetch(&g_launch_records, 1, __ATOMIC_SEQ_CST);
            if (corr < kMaxCorrelation) note_first(&g_launch_at[corr], tick);
            else __atomic_add_fetch(&g_overflow, 1, __ATOMIC_SEQ_CST);
        }
        return;
    }
    if (site == g_probe_site[kProbeSampled]) {
        const struct gpu_launch_sampled_v1 *rec =
            (const struct gpu_launch_sampled_v1 *)ptr;
        const uint64_t corr = rec->correlation;
        __atomic_add_fetch(&g_sampled_records, 1, __ATOMIC_SEQ_CST);
        if (corr < kMaxCorrelation) note_first(&g_sampled_at[corr], tick);
        else __atomic_add_fetch(&g_overflow, 1, __ATOMIC_SEQ_CST);
    }
}

// --------------------------------------------------------------- the notes

// probe_address returns the run-time address of the named probe's nop, read
// out of this binary's own .note.stapsdt - the same note internal/usdt parses
// and the same one the kernel is pointed at.
static uintptr_t probe_address(const char *want) {
    int fd = open("/proc/self/exe", O_RDONLY);
    assert(fd >= 0 && "open /proc/self/exe");
    struct stat st;
    assert(fstat(fd, &st) == 0);
    const uint8_t *m = (const uint8_t *)mmap(nullptr, (size_t)st.st_size, PROT_READ,
                                             MAP_PRIVATE, fd, 0);
    assert(m != MAP_FAILED);
    close(fd);

    const Elf64_Ehdr *eh = (const Elf64_Ehdr *)m;
    const Elf64_Shdr *sh = (const Elf64_Shdr *)(m + eh->e_shoff);
    const char *shstr = (const char *)(m + sh[eh->e_shstrndx].sh_offset);

    uintptr_t found = 0;
    for (unsigned i = 0; i < eh->e_shnum && !found; i++) {
        if (sh[i].sh_type != SHT_NOTE) continue;
        if (strcmp(shstr + sh[i].sh_name, ".note.stapsdt") != 0) continue;
        const uint8_t *p = m + sh[i].sh_offset;
        const uint8_t *end = p + sh[i].sh_size;
        while (p + sizeof(Elf64_Nhdr) <= end) {
            const Elf64_Nhdr *nh = (const Elf64_Nhdr *)p;
            const uint8_t *desc = p + sizeof(Elf64_Nhdr) + ((nh->n_namesz + 3) & ~3u);
            uint64_t pc, base;
            memcpy(&pc, desc, 8);
            memcpy(&base, desc + 8, 8);
            const char *provider = (const char *)(desc + 24);
            const char *name = provider + strlen(provider) + 1;
            if (strcmp(provider, "perfagent") == 0 && strcmp(name, want) == 0) {
                found = (uintptr_t)pc +
                        ((uintptr_t)&perfagent_stapsdt_base_sym - (uintptr_t)base);
                break;
            }
            p = desc + ((nh->n_descsz + 3) & ~3u);
        }
    }
    munmap((void *)m, (size_t)st.st_size);
    return found;
}

// Become the uprobe: replace the probe's one-byte nop with int3. The mapping
// is private, so this is a copy-on-write of our own text.
static void patch(uintptr_t probe) {
    const long pagesize = sysconf(_SC_PAGESIZE);
    void *page = (void *)(probe & ~(uintptr_t)(pagesize - 1));
    assert(mprotect(page, (size_t)pagesize * 2, PROT_READ | PROT_WRITE | PROT_EXEC) == 0);
    assert(*(volatile uint8_t *)probe == 0x90 && "probe site is not the expected nop");
    *(volatile uint8_t *)probe = 0xCC;
    assert(mprotect(page, (size_t)pagesize * 2, PROT_READ | PROT_EXEC) == 0);
}

// ---------------------------------------------------------------- the pass

static void reset() {
    memset((void *)g_launch_at, 0, sizeof(g_launch_at));
    memset((void *)g_sampled_at, 0, sizeof(g_sampled_at));
    g_tick = 0;
    g_full_batches = 0;
    g_launch_records = 0;
    g_sampled_records = 0;
    g_overflow = 0;
}

// Runs the producer and checks the one invariant the consumer's join rests
// on: for every launch the sampler picked, the sampled record reached the
// wire strictly before the batched record carrying that same correlation.
static int run_pass(const char *what, unsigned launches, unsigned sample_period) {
    assert(launches < kMaxCorrelation);
    reset();
    perfagent_stub_run(launches, 0, sample_period);

    int bad = 0;
    unsigned checked = 0, first_bad = 0;
    for (unsigned corr = 1; corr <= launches; corr++) {
        const unsigned long s = g_sampled_at[corr];
        if (!s) continue;                 // this launch was not sampled
        checked++;
        const unsigned long l = g_launch_at[corr];
        if (l == 0) {
            fprintf(stderr, "%s: correlation %u was sampled but its batched "
                            "launch record never reached the wire\n", what, corr);
            bad++;
            continue;
        }
        if (s > l) {
            if (!first_bad) first_bad = corr;
            bad++;
        }
    }

    if (bad) {
        fprintf(stderr,
                "%s: %d of %u sampled launches had their BATCHED record emitted "
                "before their sampled twin (first: correlation %u, batched at "
                "tick %lu, sampled at tick %lu).\n"
                "  The consumer holds that launch in deferredLaunches, the exec "
                "batch of the same loop iteration releases it stackless, and the "
                "stack parks in pendingStacks with nothing to join (issue #67).\n"
                "  Fire the sampled probe BEFORE the batched add().\n",
                what, bad, checked, first_bad,
                g_launch_at[first_bad], g_sampled_at[first_bad]);
        return 1;
    }

    // Neither half may pass vacuously. Without a full batch the collision
    // this test exists for cannot occur at all, and without sampled records
    // there is nothing to order.
    if (!g_full_batches) {
        fprintf(stderr, "%s: no launch batch ever flushed full, so the "
                        "batch-boundary collision was never reachable\n", what);
        return 1;
    }
    if (!checked) {
        fprintf(stderr, "%s: no sampled launch was observed at all\n", what);
        return 1;
    }
    if (g_overflow) {
        fprintf(stderr, "%s: %lu records carried a correlation past the table\n",
                what, g_overflow);
        return 1;
    }
    printf("probe_order_test: %s ok - %u sampled launches, all emitted before "
           "their batched twin; %lu launch records in %lu full batches\n",
           what, checked, g_launch_records, g_full_batches);
    return 0;
}

int main() {
    // The rendezvous is a consumer-side service and there is no consumer
    // here; disabling it outright keeps this test from spending its budget
    // discovering that (shim/core/enroll.h).
    setenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS", "0", 1);

    g_probe_site[kProbeLaunch] = probe_address("gpu_launch_v1");
    g_probe_site[kProbeSampled] = probe_address("gpu_launch_sampled_v1");
    if (!g_probe_site[kProbeLaunch] || !g_probe_site[kProbeSampled]) {
        fprintf(stderr, "probe_order_test: missing a perfagent probe note\n");
        return 1;
    }

    struct sigaction sa {};
    sa.sa_sigaction = on_trap;
    sa.sa_flags = SA_SIGINFO;
    sigemptyset(&sa.sa_mask);
    assert(sigaction(SIGTRAP, &sa, nullptr) == 0);

    patch(g_probe_site[kProbeLaunch]);
    patch(g_probe_site[kProbeSampled]);

    // Arm every semaphore, not just the two that are patched: the producer
    // takes a different path when a probe is unattached (Batch::add counts
    // and discards), and this test must exercise the attached one. The exec
    // and kernel-name probes fire their unpatched nops and cost nothing,
    // while still putting the exec batch on the wire between the twins,
    // which is the record that releases the deferred queue.
    perfagent_gpu_launch_v1_semaphore = 1;
    perfagent_gpu_exec_v1_semaphore = 1;
    perfagent_gpu_launch_sampled_v1_semaphore = 1;
    perfagent_gpu_kernel_name_v1_semaphore = 1;

    int bad = 0;
    // Pass 1, deterministic. At sample_period 1 every launch is sampled, so
    // the launch that fills the 32-record batch is sampled BY CONSTRUCTION
    // and the collision is not left to the sampler's schedule. This is the
    // pass that must fail on unfixed code, on every machine, every run.
    bad |= run_pass("period=1 launches=64", 64, 1);
    // Pass 2, the shipped configuration. The gate runs 500 launches at
    // period 8; 2048 gives the jittered schedule enough sample points to
    // land on a batch boundary the way the privileged gate did. It can only
    // ever detect a violation, never invent one, so a run in which the
    // schedule happens to miss every boundary still passes - pass 1 is what
    // makes the test deterministic.
    bad |= run_pass("period=8 launches=2048", 2048, 8);
    return bad;
}
#endif
