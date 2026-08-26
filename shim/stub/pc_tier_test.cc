// Proves that PC-sampling tier "off" means OFF at the wire: with
// PERFAGENT_GPU_PC_SAMPLING unset or naming no tier, NOT ONE PC-sampling probe
// fires -- however loudly the stub's own PC knobs are turned up.
//
// Why this is a probe test and not a counter test
// -----------------------------------------------
// "off results in no PC-sampling calls at all" is a claim about what leaves
// the producer, and every cheaper way of checking it is a way of checking
// something else. A counter says what the producer THINKS it emitted. A
// consumer-side assertion says what survived a ringbuf. Reading the env in a
// unit test says what the parser returned. Seventeen defects on this project
// have been counters and checks reading green exactly when things were worst,
// so the assertion here is made where a uprobe would make it: at the probe
// site itself, in the optimized binary the gate runs.
//
// How it sees the wire without any privilege
// ------------------------------------------
// The same trick core/probe_args_test.cc and stub/probe_order_test.cc use:
// read our own .note.stapsdt to find the probe sites, patch their one-byte
// nops with int3 (which is all a uprobe does), and count the traps in the
// SIGTRAP handler. No CAP_BPF, no consumer, no GPU.
//
// The four probes trapped are the ones PC sampling owns:
//
//   gpu_pc_sample_batch_v1     the samples themselves
//   gpu_stall_reason_map_v1    the device's stall table
//   gpu_config_v1              the sampling configuration in force
//   gpu_sampling_window_v1     Tier A's serialization disclosure
//
// Non-vacuity is the other half of the test and it is asserted in three ways,
// because an "off" pass that trapped nothing would be equally green if the
// producer had simply failed to run:
//
//   1. the launch and exec probes ARE trapped too, and must fire in every
//      pass including the off ones;
//   2. pass 2 (continuous) must fire the sample, stall and config probes, so
//      the trapped sites are demonstrably reachable in this very binary;
//   3. pass 3 (serialized) must fire the window probe, likewise.
//
// A fourth and fifth pass cover the refusals: an unknown value and a value
// naming BOTH tiers must each fall closed to off, not to a tier nobody chose.
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
    fprintf(stderr, "pc_tier_test: skipped, not x86-64\n");
    return 0;
}
#else

#include <ucontext.h>

extern "C" void perfagent_stub_run(unsigned launches, unsigned period_us,
                                   unsigned sample_period);

// stub.cc's probe semaphores. Hidden visibility, so this only links because
// the producer and the test are one binary -- which is the point: nothing here
// asks the producer to behave differently for a test.
#define SEM(name) \
    extern "C" unsigned short perfagent_##name##_semaphore __attribute__((visibility("hidden")))
SEM(gpu_launch_v1);
SEM(gpu_exec_v1);
SEM(gpu_launch_sampled_v1);
SEM(gpu_kernel_name_v1);
SEM(gpu_module_load_v1);
SEM(gpu_pc_sample_batch_v1);
SEM(gpu_stall_reason_map_v1);
SEM(gpu_config_v1);
SEM(gpu_dropped_v1);
SEM(gpu_sampling_window_v1);
#undef SEM

extern "C" char perfagent_stapsdt_base_sym __asm__("_.stapsdt.base");

// ------------------------------------------------------------ the recorder

enum {
    kProbePCSample,
    kProbeStallMap,
    kProbeConfig,
    kProbeWindow,
    // The two controls. They are not PC-sampling probes; they are here so an
    // "off" pass that fired nothing at all fails instead of passing.
    kProbeLaunch,
    kProbeExec,
    kProbeCount,
};

static const char *const kProbeName[kProbeCount] = {
    "gpu_pc_sample_batch_v1", "gpu_stall_reason_map_v1", "gpu_config_v1",
    "gpu_sampling_window_v1", "gpu_launch_v1",           "gpu_exec_v1",
};

// The first four are the ones "off" must silence. Kept as a count rather than
// a second list so the two cannot drift apart.
static const int kPCProbes = 4;

static uintptr_t g_probe_site[kProbeCount];
static unsigned long g_fires[kProbeCount];
static unsigned long g_records[kProbeCount];

static void on_trap(int, siginfo_t *, void *ucv) {
    ucontext_t *uc = (ucontext_t *)ucv;
    // int3 leaves %rip one byte past the patched nop, which is where the nop
    // itself would have left it: returning resumes correctly with no
    // single-stepping and no byte to restore.
    const uintptr_t site = (uintptr_t)uc->uc_mcontext.gregs[REG_RIP] - 1;
    const unsigned long count = (unsigned long)uc->uc_mcontext.gregs[REG_RSI];
    for (int i = 0; i < kProbeCount; i++) {
        if (site != g_probe_site[i]) continue;
        __atomic_add_fetch(&g_fires[i], 1, __ATOMIC_SEQ_CST);
        __atomic_add_fetch(&g_records[i], count, __ATOMIC_SEQ_CST);
        return;
    }
}

// --------------------------------------------------------------- the notes

// probe_address returns the run-time address of the named probe's nop, read
// out of this binary's own .note.stapsdt -- the same note internal/usdt parses
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

// ---------------------------------------------------------------- the passes

// want: -1 "must not fire", 1 "must fire at least once", 0 "do not care".
struct Expect {
    int pc_sample, stall_map, config, window;
};

static int run_pass(const char *what, const char *tier_value, Expect want) {
    memset(g_fires, 0, sizeof(g_fires));
    memset(g_records, 0, sizeof(g_records));
    if (tier_value) setenv("PERFAGENT_GPU_PC_SAMPLING", tier_value, 1);
    else unsetenv("PERFAGENT_GPU_PC_SAMPLING");

    // 64 launches, no sleep: enough to flush a full 32-record launch batch
    // twice, so the control probes cannot fail to fire for want of volume.
    perfagent_stub_run(64, 0, 1);

    const int expect[kPCProbes] = {want.pc_sample, want.stall_map, want.config, want.window};
    int bad = 0;
    for (int i = 0; i < kPCProbes; i++) {
        const unsigned long fires = g_fires[i];
        if (expect[i] < 0 && fires != 0) {
            fprintf(stderr,
                    "pc_tier_test: %s: %s FIRED %lu times (%lu records) with the tier off.\n"
                    "  \"off\" must mean no PC-sampling call at all -- not enabled-but-idle,\n"
                    "  not enabled-at-a-low-rate. A producer that emits a PC-sampling record\n"
                    "  the operator did not ask for is sampling a workload nobody consented\n"
                    "  to have sampled, and in Tier A's case perturbing it.\n",
                    what, kProbeName[i], fires, g_records[i]);
            bad = 1;
        }
        if (expect[i] > 0 && fires == 0) {
            fprintf(stderr,
                    "pc_tier_test: %s: %s never fired, so the negative passes above prove\n"
                    "  nothing -- this probe site is not reachable in this binary at all.\n",
                    what, kProbeName[i]);
            bad = 1;
        }
    }

    // The controls, on EVERY pass. Without them an "off" pass would be just as
    // green if perfagent_stub_run had returned immediately.
    if (!g_fires[kProbeLaunch] || !g_fires[kProbeExec]) {
        fprintf(stderr,
                "pc_tier_test: %s: the producer emitted no launch (%lu) or exec (%lu) record,\n"
                "  so it did not run and every assertion above passed vacuously.\n",
                what, g_fires[kProbeLaunch], g_fires[kProbeExec]);
        bad = 1;
    }

    if (!bad) {
        printf("pc_tier_test: %s ok - pc_sample=%lu stall_map=%lu config=%lu window=%lu "
               "(launch=%lu exec=%lu)\n",
               what, g_fires[kProbePCSample], g_fires[kProbeStallMap],
               g_fires[kProbeConfig], g_fires[kProbeWindow], g_fires[kProbeLaunch],
               g_fires[kProbeExec]);
    }
    return bad;
}

int main() {
    // The rendezvous is a consumer-side service and there is no consumer here;
    // disabling it keeps this test from spending its budget discovering that
    // (shim/core/enroll.h).
    setenv("PERFAGENT_GPU_ENROLL_TIMEOUT_MS", "0", 1);
    // The stub's own PC knobs, turned UP and left up for every pass including
    // the off ones. That is the whole point: the tier must silence the
    // producer even when everything else is asking it to speak.
    setenv("PERFAGENT_STUB_PC_SAMPLES", "128", 1);
    setenv("PERFAGENT_STUB_SAMPLING_WINDOWS", "4", 1);

    for (int i = 0; i < kProbeCount; i++) {
        g_probe_site[i] = probe_address(kProbeName[i]);
        if (!g_probe_site[i]) {
            fprintf(stderr, "pc_tier_test: no .note.stapsdt entry for %s\n", kProbeName[i]);
            return 1;
        }
    }

    struct sigaction sa {};
    sa.sa_sigaction = on_trap;
    sa.sa_flags = SA_SIGINFO;
    sigemptyset(&sa.sa_mask);
    assert(sigaction(SIGTRAP, &sa, nullptr) == 0);
    for (int i = 0; i < kProbeCount; i++) patch(g_probe_site[i]);

    // Arm every semaphore. The producer takes a different path when a probe is
    // unattached (Batch::add counts and discards), and a test that left the PC
    // semaphores at zero would be proving that the semaphore gate works, not
    // that the tier gate does.
    perfagent_gpu_launch_v1_semaphore = 1;
    perfagent_gpu_exec_v1_semaphore = 1;
    perfagent_gpu_launch_sampled_v1_semaphore = 1;
    perfagent_gpu_kernel_name_v1_semaphore = 1;
    perfagent_gpu_module_load_v1_semaphore = 1;
    perfagent_gpu_pc_sample_batch_v1_semaphore = 1;
    perfagent_gpu_stall_reason_map_v1_semaphore = 1;
    perfagent_gpu_config_v1_semaphore = 1;
    perfagent_gpu_dropped_v1_semaphore = 1;
    perfagent_gpu_sampling_window_v1_semaphore = 1;

    const Expect kSilent = {-1, -1, -1, -1};
    int bad = 0;
    // 1. Unset. The shipping default, and the one an operator gets by doing
    //    nothing at all.
    bad |= run_pass("tier unset", nullptr, kSilent);
    // 2. Explicitly off, both spellings the parser accepts.
    bad |= run_pass("tier=off", "off", kSilent);
    bad |= run_pass("tier=0", "0", kSilent);
    // 3. Tier B. The positive control for three of the four probes -- and the
    //    negative one for the window probe, since a CONTINUOUS producer that
    //    announced a serialization window would be claiming a perturbation it
    //    did not cause.
    bad |= run_pass("tier=continuous", "continuous", Expect{1, 1, 1, -1});
    // 4. Tier A. The positive control for the window probe.
    bad |= run_pass("tier=serialized", "serialized", Expect{1, 1, 1, 1});
    // 5. The refusals. Both must fall CLOSED to off. A parser that took the
    //    first token of "continuous,serialized" would turn a refused setting
    //    into a tier nobody chose, and would look exactly like a correct run.
    bad |= run_pass("tier=nonsense", "nonsense", kSilent);
    bad |= run_pass("tier=continuous,serialized", "continuous,serialized", kSilent);
    bad |= run_pass("tier=serialized,continuous", "serialized,continuous", kSilent);
    return bad;
}
#endif
