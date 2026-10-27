// The issue #214 reproducer: two threads, each looping
// cuModuleLoadData -> launch -> sync -> cuModuleUnload on one shared context.
//
// Every unload fires CUPTI's MODULE_UNLOAD_STARTING callback on the unloading
// thread, inside the driver and holding one of its locks, and under continuous
// PC sampling the adapter must drain PC data there before returning. With the
// adapter's timer holding the CUPTI guard across cuptiActivityFlushAll, that
// hung 15 of 15 runs: the drain waited for the guard, the flush waited for a
// CUPTI pool thread, and the pool thread waited for the driver lock.
//
// Two threads, not one: the second keeps the driver busy loading and launching
// while the first is inside its unload callback, which is what makes the
// window wide enough to hit on every run instead of occasionally.
//
// Usage: cuda_unload <kernel.cubin> [iterations per thread, default 300]
// Prints "done" and exits 0 on completion. A hang is the failure, so the
// caller runs it under a timeout; see `make -C shim check-nvidia-unload`.
#include <cuda.h>

#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <iterator>
#include <thread>
#include <vector>

#define CK(x)                                                         \
    do {                                                              \
        CUresult r_ = (x);                                            \
        if (r_ != CUDA_SUCCESS) {                                     \
            const char *s_ = "?";                                     \
            cuGetErrorString(r_, &s_);                                \
            fprintf(stderr, "cuda_unload: %s: %s\n", #x, s_);         \
            exit(1);                                                  \
        }                                                             \
    } while (0)

static std::vector<char> g_cubin;

static void worker(CUcontext ctx, int iters) {
    CK(cuCtxSetCurrent(ctx));
    const int n = 1 << 16;
    CUdeviceptr buf;
    CK(cuMemAlloc(&buf, n * sizeof(float)));
    for (int it = 0; it < iters; it++) {
        CUmodule m;
        CUfunction f;
        CK(cuModuleLoadData(&m, g_cubin.data()));
        CK(cuModuleGetFunction(&f, m, "spin"));
        int nn = n;
        void *args[] = {&buf, &nn};
        CK(cuLaunchKernel(f, n / 256, 1, 1, 256, 1, 1, 0, nullptr, args, nullptr));
        CK(cuCtxSynchronize());
        CK(cuModuleUnload(m));
    }
    CK(cuMemFree(buf));
}

int main(int argc, char **argv) {
    if (argc < 2) {
        fprintf(stderr, "usage: cuda_unload <kernel.cubin> [iterations per thread]\n");
        return 2;
    }
    const int iters = argc > 2 ? atoi(argv[2]) : 300;
    std::ifstream f(argv[1], std::ios::binary);
    g_cubin.assign(std::istreambuf_iterator<char>(f), {});
    if (g_cubin.empty()) {
        fprintf(stderr, "cuda_unload: cannot read %s\n", argv[1]);
        return 2;
    }
    CK(cuInit(0));
    CUdevice d;
    CK(cuDeviceGet(&d, 0));
    CUcontext ctx;
    CK(cuDevicePrimaryCtxRetain(&ctx, d));
    std::thread a(worker, ctx, iters), b(worker, ctx, iters);
    a.join();
    b.join();
    CK(cuDevicePrimaryCtxRelease(d));
    printf("done\n");
    return 0;
}
