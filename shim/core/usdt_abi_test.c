// usdt_abi.h is the ABI both the C++ shim and any C vendor adapter include.
// Compiling it as C11 keeps that spelling guarded; the _Static_asserts in the
// header are the bulk of the test.
//
// The runtime checks below cover the one thing a static assert cannot: that a
// record written through the struct lands, byte for byte, where the Go
// decoders in internal/gpuabi read it by hard-coded offset. offsetof proves
// the compiler's layout; this proves the *bytes*, including endianness and
// the padding the decoders skip over.
#include "usdt_abi.h"

#include <stdio.h>
#include <string.h>

static int failures;

#define CHECK(cond)                                                        \
    do {                                                                   \
        if (!(cond)) {                                                     \
            fprintf(stderr, "%s:%d: FAILED: %s\n", __FILE__, __LINE__, #cond); \
            failures++;                                                    \
        }                                                                  \
    } while (0)

// Little-endian readers, matching what internal/gpuabi does to the wire.
static uint32_t rd32(const unsigned char *b)
{
    return (uint32_t)b[0] | ((uint32_t)b[1] << 8) | ((uint32_t)b[2] << 16) | ((uint32_t)b[3] << 24);
}

static uint64_t rd64(const unsigned char *b)
{
    return (uint64_t)rd32(b) | ((uint64_t)rd32(b + 4) << 32);
}

static uint16_t rd16(const unsigned char *b)
{
    return (uint16_t)((uint32_t)b[0] | ((uint32_t)b[1] << 8));
}

static void stall_reason_map(void)
{
    struct gpu_stall_reason_map_v1 r;
    memset(&r, 0, sizeof(r));
    r.index = 0x11223344;
    r.name_len = 17;
    r.truncated = 1;
    memcpy(r.name, "long_scoreboard__", 17);

    const unsigned char *b = (const unsigned char *)&r;
    CHECK(sizeof(r) == 136);
    CHECK(rd32(b + 0) == 0x11223344u);
    CHECK(rd16(b + 4) == 17);
    CHECK(b[6] == 1);
    CHECK(memcmp(b + 8, "long_scoreboard__", 17) == 0);
    // name_len is authoritative and the buffer is fixed-size: the bytes past
    // it are not part of the name and the record does not shrink.
    CHECK(b[8 + 17] == 0);
}

static void sampling_window(void)
{
    struct gpu_sampling_window_v1 w;
    memset(&w, 0, sizeof(w));
    w.start_ns = 0x0102030405060708ull;
    w.end_ns = 0x1112131415161718ull;
    w.mode = GPU_SAMPLING_MODE_KERNEL_SERIALIZED;

    const unsigned char *b = (const unsigned char *)&w;
    CHECK(sizeof(w) == 24);
    CHECK(rd64(b + 0) == 0x0102030405060708ull);
    CHECK(rd64(b + 8) == 0x1112131415161718ull);
    CHECK(b[16] == 2);

    // end_ns == 0 is "still open when the producer stopped reporting", a
    // value the consumer must be able to tell apart from a closed window.
    // Nothing in the encoding reserves it, so it has to stay representable.
    w.end_ns = 0;
    CHECK(rd64(b + 8) == 0ull);
    CHECK(rd64(b + 0) == 0x0102030405060708ull);

    CHECK(GPU_SAMPLING_MODE_CONTINUOUS == 1);
    CHECK(GPU_SAMPLING_MODE_KERNEL_SERIALIZED == 2);
}

static void config(void)
{
    struct gpu_config_v1 c;
    memset(&c, 0, sizeof(c));
    c.clock_hz = 1000000000ull;
    c.sampling_factor = 5;
    c.sm_count = 82;   // GA102, the RTX 3090 this is proven on
    c.vendor = 1;

    const unsigned char *b = (const unsigned char *)&c;
    CHECK(sizeof(c) == 24);
    CHECK(rd64(b + 0) == 1000000000ull);
    CHECK(rd32(b + 8) == 5u);
    CHECK(rd32(b + 12) == 82u);
    CHECK(b[16] == 1);
}

int main(void)
{
    stall_reason_map();
    sampling_window();
    config();
    if (failures) {
        fprintf(stderr, "usdt_abi_test: %d failure(s)\n", failures);
        return 1;
    }
    printf("usdt_abi_test: OK\n");
    return 0;
}
