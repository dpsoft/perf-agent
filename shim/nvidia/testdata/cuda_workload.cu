// The CUDA workload the NVIDIA adapter is proven against.
//
// Two distinct kernels, launched in a loop from two named host functions, so
// that a sampled launch's captured CPU stack has something to resolve to and
// the kernel-name table has more than one entry to intern. The result is
// checked on the host: the point of the unattached run is that the workload
// still computes the right answer with the adapter injected.
#include <cuda_runtime.h>

#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <ctime>
#include <poll.h>
#include <unistd.h>

#define CHECK(call)                                                            \
    do {                                                                       \
        const cudaError_t _e = (call);                                         \
        if (_e != cudaSuccess) {                                               \
            fprintf(stderr, "workload: %s failed: %s\n", #call,                \
                    cudaGetErrorString(_e));                                   \
            exit(2);                                                           \
        }                                                                      \
    } while (0)

static const int kN = 1 << 16;

__global__ void perfagent_axpy(float a, const float *x, float *y, int n) {
    const int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < n) y[i] = a * x[i] + y[i];
}

__global__ void perfagent_scale(float *y, float s, int n) {
    const int i = blockIdx.x * blockDim.x + threadIdx.x;
    if (i < n) y[i] = y[i] * s;
}

// noinline so the sampled stack has a frame that names which kernel was
// being submitted, rather than one inlined blob of the loop body.
static __attribute__((noinline)) void launch_axpy(const float *x, float *y) {
    perfagent_axpy<<<(kN + 255) / 256, 256>>>(2.0f, x, y, kN);
}

static __attribute__((noinline)) void launch_scale(float *y) {
    perfagent_scale<<<(kN + 255) / 256, 256>>>(y, 0.5f, kN);
}

static uint64_t mono_ns() {
    struct timespec t;
    clock_gettime(CLOCK_MONOTONIC, &t);
    return (uint64_t)t.tv_sec * 1000000000ULL + (uint64_t)t.tv_nsec;
}

// Same contract as the stub's linger (shim/stub/stub.cc): a sampled launch's
// CPU stack is symbolized against /proc/<pid>/maps of THIS process, which the
// kernel destroys the instant it exits. The consumer releases us by closing
// our stdin; linger_ms is the backstop for a run by hand from a terminal,
// where stdin never reaches EOF.
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

int main(int argc, char **argv) {
    const unsigned iters = argc > 1 ? (unsigned)atoi(argv[1]) : 2000;
    const unsigned sleep_us = argc > 2 ? (unsigned)atoi(argv[2]) : 200;
    const unsigned linger_ms = argc > 3 ? (unsigned)atoi(argv[3]) : 0;

    float *x = nullptr, *y = nullptr;
    CHECK(cudaMalloc(&x, kN * sizeof(float)));
    CHECK(cudaMalloc(&y, kN * sizeof(float)));

    float *hx = (float *)malloc(kN * sizeof(float));
    float *hy = (float *)malloc(kN * sizeof(float));
    for (int i = 0; i < kN; i++) { hx[i] = 1.0f; hy[i] = 0.0f; }
    CHECK(cudaMemcpy(x, hx, kN * sizeof(float), cudaMemcpyHostToDevice));
    CHECK(cudaMemcpy(y, hy, kN * sizeof(float), cudaMemcpyHostToDevice));

    const uint64_t t0 = mono_ns();
    for (unsigned i = 0; i < iters; i++) {
        launch_axpy(x, y);      // y = 2x + y
        launch_scale(y);        // y = y/2
        if ((i % 64) == 63) CHECK(cudaDeviceSynchronize());
        if (sleep_us) usleep(sleep_us);
    }
    CHECK(cudaDeviceSynchronize());
    const uint64_t t1 = mono_ns();
    CHECK(cudaGetLastError());

    CHECK(cudaMemcpy(hy, y, kN * sizeof(float), cudaMemcpyDeviceToHost));
    // Each iteration is y = (2x + y)/2 with x == 1, whose fixed point is
    // y == 2 -- reached to float precision within ~25 iterations. That single
    // number checks BOTH kernels: drop the axpy and y decays to 0, drop the
    // scale and y grows to 2*iters. It is the workload's proof that the
    // injected adapter did not perturb the computation.
    const double want = 2.0;
    double err = 0.0;
    for (int i = 0; i < kN; i++) {
        const double d = (double)hy[i] - want;
        err += d < 0 ? -d : d;
    }
    printf("workload: iters=%u launches=%u elapsed_ms=%.1f abs_err=%.6f y[0]=%.6f\n",
           iters, iters * 2, (double)(t1 - t0) / 1e6, err, (double)hy[0]);
    fflush(stdout);

    linger(linger_ms);

    CHECK(cudaFree(x));
    CHECK(cudaFree(y));
    free(hx);
    free(hy);
    // Not cudaDeviceReset(): it tears the context down before the adapter's
    // atexit flush runs, and the last activity records go with it.
    return err < 1e-3 ? 0 : 1;
}
