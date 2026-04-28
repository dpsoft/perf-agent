#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

typedef int hipError_t;
typedef void *hipStream_t;

typedef struct {
    unsigned int x;
    unsigned int y;
    unsigned int z;
} dim3;

typedef hipError_t (*hip_get_device_count_fn)(int *count);
typedef hipError_t (*hip_launch_kernel_fn)(
    const void *function_address,
    dim3 num_blocks,
    dim3 dim_blocks,
    void **args,
    size_t shared_mem_bytes,
    hipStream_t stream);

__attribute__((noinline)) static void hip_launch_shim_kernel(void)
{
    __asm__ __volatile__("" ::: "memory");
}

static const char *env_or_default(const char *name, const char *fallback)
{
    const char *value = getenv(name);
    if (value == NULL || value[0] == '\0') {
        return fallback;
    }
    return value;
}

static unsigned int parse_ms(const char *name, unsigned int fallback)
{
    const char *value = getenv(name);
    if (value == NULL || value[0] == '\0') {
        return fallback;
    }

    char *end = NULL;
    unsigned long parsed = strtoul(value, &end, 10);
    if (end == value || (end != NULL && *end != '\0')) {
        fprintf(stderr, "invalid %s: %s\n", name, value);
        return fallback;
    }
    return (unsigned int)parsed;
}

int main(void)
{
    const char *library_path = env_or_default("HIP_LAUNCH_SHIM_LIBRARY", "/opt/rocm/lib/libamdhip64.so");
    unsigned int sleep_before_ms = parse_ms("HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS", 2000);
    unsigned int sleep_after_ms = parse_ms("HIP_LAUNCH_SHIM_SLEEP_AFTER_MS", 4000);

    void *handle = dlopen(library_path, RTLD_NOW | RTLD_LOCAL);
    if (handle == NULL) {
        fprintf(stderr, "dlopen(%s): %s\n", library_path, dlerror());
        return 1;
    }

    hip_get_device_count_fn hip_get_device_count =
        (hip_get_device_count_fn)dlsym(handle, "hipGetDeviceCount");
    hip_launch_kernel_fn hip_launch_kernel =
        (hip_launch_kernel_fn)dlsym(handle, "hipLaunchKernel");
    if (hip_get_device_count == NULL || hip_launch_kernel == NULL) {
        fprintf(stderr, "required HIP symbols missing in %s\n", library_path);
        dlclose(handle);
        return 1;
    }

    int device_count = -1;
    hipError_t count_err = hip_get_device_count(&device_count);
    printf("hipGetDeviceCount -> err=%d count=%d\n", count_err, device_count);
    fflush(stdout);

    usleep((useconds_t)sleep_before_ms * 1000);

    dim3 blocks = {1, 1, 1};
    dim3 threads = {1, 1, 1};
    hipError_t launch_err = hip_launch_kernel(
        (const void *)&hip_launch_shim_kernel,
        blocks,
        threads,
        NULL,
        0,
        NULL);
    printf("hipLaunchKernel -> err=%d\n", launch_err);
    fflush(stdout);

    usleep((useconds_t)sleep_after_ms * 1000);

    dlclose(handle);
    return 0;
}
