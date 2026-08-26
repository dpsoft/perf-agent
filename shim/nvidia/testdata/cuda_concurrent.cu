// The CONCURRENT CUDA workload the PC-sampling overhead benchmark is measured
// against (plan Task 12). It is a second workload, not a modification of
// cuda_workload.cu, and the reason is the whole point of this file.
//
// Why not extend cuda_workload.cu
// -------------------------------
// cuda_workload.cu is a two-kernel SERIAL loop on the default stream with a
// usleep between iterations. Its kernels are ~64k elements of one FMA each --
// microseconds -- and nothing ever overlaps anything. That shape is exactly
// right for what it exists for: it is the fixture the adapter, the join, the
// kernel-name table and the -lineinfo source resolution are all proven
// against, and the hardware half of the phase gate asserts against ITS source
// lines. Changing its shape would change what those gates measure.
//
// It is also exactly the WRONG instrument for measuring Tier A. The plan says
// so directly: a saturating stream of trivial kernels exaggerates per-launch
// costs and UNDERSTATES serialization costs, because serialization hurts in
// proportion to the concurrency it destroys and a serial loop has none to
// destroy. A benchmark run on cuda_workload.cu would report a small Tier A
// number for the same reason a stopped clock reports the right time twice a
// day.
//
// What this workload does instead
// -------------------------------
// S independent CUDA streams, each carrying its own buffer, each iteration
// launching one non-trivial kernel per stream with NO synchronization between
// the streams. The kernels are sized to occupy a FRACTION of the device
// (blocks x threads well under the SM count x occupancy), so several of them
// genuinely co-reside -- which is the property Tier A's KERNEL_SERIALIZED
// mode destroys, and therefore the property the measurement needs to have in
// the first place.
//
// The concurrency is not asserted here; it is MEASURED, by the benchmark, out
// of the profile the adapter produces:
//
//     concurrency = sum(exec duration) / (max end - min start)
//
// and the benchmark refuses to report any overhead number if that comes out
// near 1 on the baseline arm. A workload that turned out to be serial anyway
// would otherwise produce a small, green, meaningless Tier A cost.
//
// A periodic device sync every --sync-every iterations is deliberate, not
// housekeeping: it bounds queue depth, and it makes the pipeline REFILL after
// every drain. Refill cost is precisely what makes Tier A's damage outlast its
// burst, which is what the cost-over-duty ratio exists to detect.
//
// The exact self-check
// --------------------
// Each kernel adds +1 to every element `rounds` times and then adds -1 to
// every element `rounds` times. With rounds <= 2^23 every partial sum is
// exactly representable in float, so the composition is the IDENTITY: every
// element must come back bit-exact. `up` and `down` are runtime kernel
// arguments, so the compiler cannot fold the loops away, and `#pragma unroll
// 1` keeps them from being unrolled into a reassociated sum.
//
// max_abs_err must therefore be exactly 0.0, not "small". An arm that
// perturbed the computation fails the run rather than contributing a number.
//
// Built with -lineinfo -g like cuda_workload, so a PC sample taken during a
// burst resolves to a line of this file rather than to nothing.
#include <cuda_runtime.h>

#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <ctime>
#include <poll.h>
#include <unistd.h>

#define CHECK(call)                                                            \
    do {                                                                       \
        const cudaError_t _e = (call);                                         \
        if (_e != cudaSuccess) {                                               \
            fprintf(stderr, "concurrent: %s failed: %s\n", #call,              \
                    cudaGetErrorString(_e));                                   \
            exit(2);                                                           \
        }                                                                      \
    } while (0)

// One kernel: `rounds` dependent FMAs up, then `rounds` dependent FMAs down.
//
// The dependency chain is what gives the kernel a duration measured in
// hundreds of microseconds out of a tiny grid -- the alternative (a huge grid
// of trivial work) would saturate the device and leave nothing for a second
// stream to overlap with, which would quietly turn this back into the serial
// workload it exists not to be.
__global__ void perfagent_conc_chain(float *buf, int n, int rounds, float up,
                                     float down) {
    const int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= n) return;
    float v = buf[i];
#pragma unroll 1
    for (int r = 0; r < rounds; r++) v = fmaf(v, 1.0f, up);
#pragma unroll 1
    for (int r = 0; r < rounds; r++) v = fmaf(v, 1.0f, down);
    buf[i] = v;
}

// noinline so a sampled launch's captured CPU stack has a frame naming the
// submitting function rather than one inlined blob of the loop body -- the
// same reason cuda_workload.cu marks its two launchers noinline.
static __attribute__((noinline)) void launch_chain(float *buf, int n, int rounds,
                                                   int blocks, int threads,
                                                   cudaStream_t s) {
    perfagent_conc_chain<<<blocks, threads, 0, s>>>(buf, n, rounds, 1.0f, -1.0f);
}

static uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

// Same release contract as cuda_workload.cu and the stub: a sampled launch's
// CPU stack is symbolized against /proc/<pid>/maps of THIS process, which the
// kernel destroys the instant it exits. The consumer releases us by closing
// our stdin; linger_ms is the backstop for a run by hand from a terminal.
// The overhead benchmark passes 0 -- it needs no stacks -- but a run by hand
// under cmd/gpu-cuda-profile does.
static void linger(unsigned linger_ms) {
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
        const ssize_t got = read(STDIN_FILENO, buf, sizeof(buf));
        if (got <= 0) return;
    }
}

struct Opts {
    unsigned iters = 20000;      // timed iterations; each launches `streams` kernels
    unsigned warmup = 64;        // untimed iterations before the clock starts
    unsigned streams = 4;        // concurrent streams == concurrent kernels
    unsigned rounds = 64000;     // FMA rounds per direction; sets kernel duration
    unsigned blocks = 16;        // grid; deliberately a FRACTION of the device
    unsigned threads = 256;
    unsigned sync_every = 4;     // device sync every N iterations; bounds queue depth
    unsigned linger_ms = 0;
};

static bool parse_uint(const char *arg, const char *name, unsigned *out) {
    const size_t n = strlen(name);
    if (strncmp(arg, name, n) != 0 || arg[n] != '=') return false;
    char *end = nullptr;
    const unsigned long v = strtoul(arg + n + 1, &end, 10);
    if (end == arg + n + 1 || (end && *end)) {
        fprintf(stderr, "concurrent: %s: not a number\n", arg);
        exit(2);
    }
    *out = (unsigned)v;
    return true;
}

int main(int argc, char **argv) {
    Opts o;
    for (int i = 1; i < argc; i++) {
        const char *a = argv[i];
        if (parse_uint(a, "--iters", &o.iters)) continue;
        if (parse_uint(a, "--warmup", &o.warmup)) continue;
        if (parse_uint(a, "--streams", &o.streams)) continue;
        if (parse_uint(a, "--rounds", &o.rounds)) continue;
        if (parse_uint(a, "--blocks", &o.blocks)) continue;
        if (parse_uint(a, "--threads", &o.threads)) continue;
        if (parse_uint(a, "--sync-every", &o.sync_every)) continue;
        if (parse_uint(a, "--linger-ms", &o.linger_ms)) continue;
        fprintf(stderr,
                "concurrent: unknown argument %s\n"
                "usage: cuda_concurrent [--iters=N] [--warmup=N] [--streams=N] "
                "[--rounds=N] [--blocks=N] [--threads=N] [--sync-every=N] "
                "[--linger-ms=N]\n",
                a);
        return 2;
    }
    if (!o.iters || !o.streams || !o.rounds || !o.blocks || !o.threads) {
        fprintf(stderr, "concurrent: iters, streams, rounds, blocks and threads "
                        "must all be positive\n");
        return 2;
    }
    if (!o.sync_every) o.sync_every = 1;
    // 2^23: past this the +1/-1 partial sums stop being exactly representable
    // in float and the identity check below would start reporting a rounding
    // artefact as a corrupted computation.
    if (o.rounds > (1u << 23)) {
        fprintf(stderr, "concurrent: --rounds=%u exceeds 2^23; the exact "
                        "identity check would no longer hold\n", o.rounds);
        return 2;
    }

    const int n = (int)(o.blocks * o.threads);
    const size_t bytes = (size_t)n * sizeof(float);

    float **d = (float **)calloc(o.streams, sizeof(float *));
    cudaStream_t *s = (cudaStream_t *)calloc(o.streams, sizeof(cudaStream_t));
    if (!d || !s) { fprintf(stderr, "concurrent: out of memory\n"); return 2; }

    float *h = (float *)malloc(bytes);
    if (!h) { fprintf(stderr, "concurrent: out of memory\n"); return 2; }
    for (int i = 0; i < n; i++) h[i] = 1.0f;

    for (unsigned k = 0; k < o.streams; k++) {
        // Non-blocking so a stream never implicitly synchronizes against the
        // legacy default stream. With the default (blocking) flag every one of
        // these streams would serialize against stream 0 and the workload
        // would have no concurrency at all -- which is the exact failure this
        // file exists to avoid, so it is spelled out rather than defaulted.
        CHECK(cudaStreamCreateWithFlags(&s[k], cudaStreamNonBlocking));
        CHECK(cudaMalloc(&d[k], bytes));
        CHECK(cudaMemcpy(d[k], h, bytes, cudaMemcpyHostToDevice));
    }

    // Warm-up, OUTSIDE the timed region: module load, JIT, first-touch
    // allocation and the driver's own lazy initialization all land here rather
    // than in the fixed-work measurement. It also gives the adapter's drain
    // timer and (in Tier A) its burst controller a few cycles to reach steady
    // state before the clock starts.
    for (unsigned i = 0; i < o.warmup; i++)
        for (unsigned k = 0; k < o.streams; k++)
            launch_chain(d[k], n, (int)o.rounds, (int)o.blocks, (int)o.threads, s[k]);
    CHECK(cudaDeviceSynchronize());
    CHECK(cudaGetLastError());

    // FIXED WORK, not fixed time: iters x streams kernels, whatever it takes.
    // The wall clock over this region is the number the benchmark compares
    // across arms.
    const uint64_t t0 = mono_ns();
    for (unsigned i = 0; i < o.iters; i++) {
        for (unsigned k = 0; k < o.streams; k++)
            launch_chain(d[k], n, (int)o.rounds, (int)o.blocks, (int)o.threads, s[k]);
        if ((i % o.sync_every) == o.sync_every - 1) CHECK(cudaDeviceSynchronize());
    }
    CHECK(cudaDeviceSynchronize());
    const uint64_t t1 = mono_ns();
    CHECK(cudaGetLastError());

    const double elapsed_ms = (double)(t1 - t0) / 1e6;
    const unsigned long long kernels = (unsigned long long)o.iters * o.streams;

    // Exactly zero, not "small". Every kernel is the identity on its buffer,
    // so any non-zero here means the computation was corrupted and this run's
    // timing must not be used.
    double max_abs_err = 0.0;
    for (unsigned k = 0; k < o.streams; k++) {
        CHECK(cudaMemcpy(h, d[k], bytes, cudaMemcpyDeviceToHost));
        for (int i = 0; i < n; i++) {
            const double e = (double)h[i] - 1.0;
            const double a = e < 0 ? -e : e;
            if (a > max_abs_err) max_abs_err = a;
        }
    }

    printf("concurrent: iters=%u warmup=%u streams=%u rounds=%u blocks=%u "
           "threads=%u sync_every=%u kernels=%llu elapsed_ms=%.3f "
           "kernels_per_s=%.1f max_abs_err=%.9f\n",
           o.iters, o.warmup, o.streams, o.rounds, o.blocks, o.threads,
           o.sync_every, kernels, elapsed_ms,
           elapsed_ms > 0 ? (double)kernels * 1000.0 / elapsed_ms : 0.0,
           max_abs_err);
    fflush(stdout);

    linger(o.linger_ms);

    for (unsigned k = 0; k < o.streams; k++) {
        CHECK(cudaFree(d[k]));
        CHECK(cudaStreamDestroy(s[k]));
    }
    free(h);
    free(d);
    free(s);
    // Not cudaDeviceReset(), for the reason cuda_workload.cu gives: it tears
    // the context down before the adapter's atexit flush runs and the last
    // activity records go with it.
    return max_abs_err == 0.0 ? 0 : 1;
}
