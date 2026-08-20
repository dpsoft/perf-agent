// Proves the record a probe points at is actually IN MEMORY when the probe
// fires.
//
// Every other test in this directory checks what the producer *intends* to
// emit: the ABI header's layouts, Batch's buffering, the Sampler's arithmetic.
// None of them can see the one thing that actually matters at a probe site --
// that the stores which filled the record happened BEFORE the trap, and were
// not sunk past it or deleted outright by the optimizer. A USDT probe is a
// one-byte nop; its argument is a pointer; nothing in the C++ abstract
// machine reads through that pointer. So the compiler is free to conclude the
// record is dead, and at -O2 g++ did exactly that: it dropped the
// sample_period and launch_seq stores of shim/stub/stub.cc's sampled-launch
// record, and every sampled launch arrived at the consumer with
// sample_period == 0. The producer was correct, the ABI was correct, the
// consumer was correct, and the wire bytes were wrong.
//
// This test looks at the wire bytes, by being a uprobe. It reads its own
// .note.stapsdt to find the probe address (the same note the kernel uses),
// patches the nop with int3 (what a uprobe does), and in the SIGTRAP handler
// reads %rdi/%rsi/%rdx out of the trapped context and copies the record --
// exactly what bpf_probe_read_user does in bpf/gpu_usdt.bpf.c. No privileges
// are required for any of it.
//
// It MUST be compiled with optimization on. At -O0 every store is emitted and
// the test passes against a broken macro; the Makefile passes -O2 for this
// reason and the check below refuses to pass silently without it.
#include "usdt_abi.h"
#include "usdt_probe.h"

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
#include <sys/syscall.h>
#include <unistd.h>

#if !defined(__x86_64__)
int main() {
    // The probe macro binds rdi/rsi/rdx by name, so this test is as
    // architecture-specific as the ABI it checks.
    fprintf(stderr, "probe_args_test: skipped, not x86-64\n");
    return 0;
}
#else

#ifndef __OPTIMIZE__
#error "probe_args_test must be built optimized; at -O0 it cannot fail"
#endif

#include <ucontext.h>

PERFAGENT_USDT_EMITTER(gpu_launch_sampled_v1, 56);

// The address every stapsdt note is relative to. Comparing it against the
// note's own base field yields the load bias, so this works for a PIE and a
// non-PIE build alike without asking either one which it is.
extern "C" char perfagent_stapsdt_base_sym __asm__("_.stapsdt.base");

// What the trap saw: the arguments in registers, and the bytes behind the
// pointer, copied at trap time.
static volatile sig_atomic_t g_fired = 0;
static gpu_launch_sampled_v1 g_seen;
static unsigned long g_count, g_seq;

static void on_trap(int, siginfo_t *, void *ucv) {
    ucontext_t *uc = (ucontext_t *)ucv;
    // int3 leaves %rip after the patched byte, which is where the nop would
    // have left it, so returning from the handler resumes correctly with no
    // single-stepping and no need to restore the original byte.
    const void *ptr = (const void *)uc->uc_mcontext.gregs[REG_RDI];
    g_count = (unsigned long)uc->uc_mcontext.gregs[REG_RSI];
    g_seq = (unsigned long)uc->uc_mcontext.gregs[REG_RDX];
    memcpy(&g_seen, ptr, sizeof(g_seen));
    g_fired = 1;
}

// Sources the compiler cannot constant-fold, so the record must genuinely be
// assembled at run time the way the shim assembles it.
static volatile uint64_t v_correlation = 0x1234;
static volatile uint64_t v_kernel_id = 0x1111;
static volatile uint64_t v_time_ns = 0x5555;
static volatile uint32_t v_sample_period = 8;
static volatile uint64_t v_launch_seq = 499;

static inline uint32_t current_tid() { return (uint32_t)syscall(SYS_gettid); }

// The exact shape of shim/stub/stub.cc's sampled-launch emit, and the shape
// matters more than it looks. A record is built in a local, fired at
// unbatched, and never read again -- which is what licenses the optimizer to
// drop the stores -- and an opaque call (current_tid) sits in the middle of
// the fill. That call is why the bug was PARTIAL rather than total: a call
// may read memory whose address escaped, so the stores before it survive,
// while the ones after it, with nothing but a nop between them and the end of
// the record's life, do not. Reproducing that shape is what makes this test
// able to fail.
__attribute__((noinline)) static uint32_t fill_and_fire() {
    gpu_launch_sampled_v1 sl{};
    sl.correlation = v_correlation;
    sl.kernel_id = v_kernel_id;
    sl.queue_id = 1;
    sl.context_id = 1;
    sl.time_ns = v_time_ns;
    const uint32_t tid = current_tid();
    sl.tid = tid;
    sl.sample_period = v_sample_period;
    sl.launch_seq = v_launch_seq;
    if (gpu_launch_sampled_v1_enabled())
        gpu_launch_sampled_v1_emit(&sl, 1, 7);
    // The tid is returned, not re-read out of the record: reading sl.tid back
    // would make that one store live and quietly narrow what this test can
    // catch.
    return tid;
}

// probe_address returns the run-time address of the named probe's nop, read
// out of this binary's own .note.stapsdt -- the same note internal/usdt
// parses and the same one the kernel is pointed at.
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
            // desc: probe pc, base, semaphore, then provider\0 name\0 args\0.
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

int main() {
    uintptr_t probe = probe_address("gpu_launch_sampled_v1");
    if (!probe) {
        fprintf(stderr, "probe_args_test: no perfagent/gpu_launch_sampled_v1 note\n");
        return 1;
    }

    struct sigaction sa {};
    sa.sa_sigaction = on_trap;
    sa.sa_flags = SA_SIGINFO;
    sigemptyset(&sa.sa_mask);
    assert(sigaction(SIGTRAP, &sa, nullptr) == 0);

    // Become the uprobe: replace the one-byte nop with int3. The mapping is
    // private, so this is a copy-on-write of our own text and affects nothing
    // else on the system.
    const long pagesize = sysconf(_SC_PAGESIZE);
    void *page = (void *)(probe & ~(uintptr_t)(pagesize - 1));
    assert(mprotect(page, (size_t)pagesize * 2, PROT_READ | PROT_WRITE | PROT_EXEC) == 0);
    assert(*(volatile uint8_t *)probe == 0x90 && "probe site is not the expected nop");
    *(volatile uint8_t *)probe = 0xCC;
    assert(mprotect(page, (size_t)pagesize * 2, PROT_READ | PROT_EXEC) == 0);

    // Arm the semaphore, the way an attaching consumer does.
    perfagent_gpu_launch_sampled_v1_semaphore = 1;

    const uint32_t tid = fill_and_fire();

    assert(g_fired && "the probe never fired");
    assert(g_count == 1 && "the probe's count argument (%rsi) did not arrive");
    assert(g_seq == 7 && "the probe's seq argument (%rdx) did not arrive");

    // Every field, because a partial store elision is as wrong as a total
    // one and the failing one was the second to last.
    int bad = 0;
#define CHECK(field, want)                                                    \
    do {                                                                      \
        if ((uint64_t)g_seen.field != (uint64_t)(want)) {                     \
            fprintf(stderr,                                                   \
                    "probe_args_test: %s reached the probe as %llu, want %llu" \
                    " -- the store was sunk past the probe or deleted\n",     \
                    #field, (unsigned long long)g_seen.field,                 \
                    (unsigned long long)(want));                              \
            bad = 1;                                                          \
        }                                                                     \
    } while (0)
    CHECK(correlation, 0x1234);
    CHECK(kernel_id, 0x1111);
    CHECK(queue_id, 1);
    CHECK(context_id, 1);
    CHECK(time_ns, 0x5555);
    CHECK(tid, tid);
    CHECK(sample_period, 8);
    CHECK(launch_seq, 499);
#undef CHECK
    if (bad) return 1;

    printf("probe_args_test: ok\n");
    return 0;
}
#endif
