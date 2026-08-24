// The CUDA shape without CUDA: a producer .so loaded with dlopen(3).
//
// # Why this exists
//
// Issue #49's first fix passed the GPU-free gate with no-tables=0 and then
// did nothing at all on the RTX 3090 - UnwindEnrollRequests was 0 across
// three runs. The gate could not see it because the two paths differ in the
// one respect that mattered:
//
//   perfagent-gpu-fpless   EXEC'd.    The kernel arms the probe semaphores
//                                     while it builds the new mm, so by the
//                                     time main runs they read non-zero.
//   the CUPTI adapter      DLOPEN'd.  libcuda maps it and calls
//                                     InitializeInjection essentially at
//                                     once, and the semaphore had not armed
//                                     yet - so a rendezvous gated on it never
//                                     ran, while the probes fired fine later.
//
// A gate driven only by an exec'd producer cannot tell those apart, which is
// how a mechanism that does nothing under CUDA went green. This host closes
// that hole: it exec's, then dlopens libperfagent-gpu-stub.so and calls into
// it, so the producer's initialisation happens on the SAME kernel path the
// CUDA adapter's does.
//
// It deliberately does NOT link the .so at load time. A DT_NEEDED dependency
// is mapped by the loader before main, which is the exec path again wearing a
// different hat.
#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <ctime>

#include <dlfcn.h>
#include <poll.h>
#include <unistd.h>

namespace {

uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

// The same release contract as perfagent_stub_linger and the CUDA workload:
// a sampled launch's stack is symbolized against THIS process's
// /proc/<pid>/maps, which the kernel destroys the instant it exits, so the
// consumer holds us open by keeping our stdin open. Reimplemented here rather
// than dlsym'd because perfagent_stub_linger is hidden in the .so build.
void linger(unsigned linger_ms) {
    if (!linger_ms) return;
    struct pollfd p{};
    p.fd = STDIN_FILENO;
    p.events = POLLIN;
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
        if (rc == 0) return;
        char buf[256];
        if (read(STDIN_FILENO, buf, sizeof(buf)) <= 0) return;
    }
}

}  // namespace

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: %s <producer.so> [launches] [period_us] [sample_period] [linger_ms]\n",
                argv[0]);
        return 2;
    }
    const char *so = argv[1];
    const unsigned n = argc > 2 ? (unsigned)atoi(argv[2]) : 500;
    const unsigned us = argc > 3 ? (unsigned)atoi(argv[3]) : 1000;
    const unsigned sp = argc > 4 ? (unsigned)atoi(argv[4]) : 8;
    const unsigned lm = argc > 5 ? (unsigned)atoi(argv[5]) : 0;

    // RTLD_NOW so relocation happens here rather than lazily inside the first
    // call, keeping this as close to the driver's own dlopen as possible.
    void *h = dlopen(so, RTLD_NOW | RTLD_LOCAL);
    if (!h) {
        fprintf(stderr, "dlopen-host: dlopen %s: %s\n", so, dlerror());
        return 3;
    }
    // The one symbol the producer .so exports, same as the adapter's
    // InitializeInjection. Called immediately after the mapping appears,
    // which is the timing the rendezvous has to survive.
    // perfagent_fpless_caller first: it reaches perfagent_stub_run through two
    // frames compiled -fomit-frame-pointer, so the stack between the probe and
    // this executable's main can only be walked with CFI - the CUDA shape,
    // where libcupti and libcudart sit in exactly that position. Falling back
    // to the plain entry point keeps this host usable with
    // libperfagent-gpu-stub.so, whose chain has frame pointers throughout and
    // therefore exercises no DWARF at all.
    using fpless_fn = unsigned long (*)(unsigned, unsigned, unsigned);
    using run_fn = void (*)(unsigned, unsigned, unsigned);
    auto fpless = (fpless_fn)dlsym(h, "perfagent_fpless_caller");
    auto run = (run_fn)dlsym(h, "perfagent_stub_run");
    if (!fpless && !run) {
        fprintf(stderr, "dlopen-host: no entry point in %s: %s\n", so, dlerror());
        return 4;
    }
    fprintf(stderr, "dlopen-host: loaded %s pid=%d entry=%s\n", so, (int)getpid(),
            fpless ? "perfagent_fpless_caller" : "perfagent_stub_run");
    if (fpless) {
        // The result is used, not discarded: a discarded value would let the
        // optimizer prove the post-call work in the bridge chain is dead and
        // restore the sibling calls those locals exist to prevent.
        const unsigned long sink = fpless(n, us, sp);
        if (sink == 0) fprintf(stderr, "dlopen-host: bridge returned 0\n");
    } else {
        run(n, us, sp);
    }
    linger(lm);
    // Not dlclose(): unmapping the producer would take its probes out from
    // under a consumer that is still draining, and the real adapter is never
    // unloaded either.
    return 0;
}
