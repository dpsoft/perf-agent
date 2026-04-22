// unwinder.c — DWARF-based stack unwinder built on libunwind.
//
// Shape: we create a libunwind address space with custom accessors. The
// caller's captured (regs, stack_bytes) become the starting state. For
// memory reads outside the captured stack (code, .eh_frame, ELF headers),
// we fall back to locally-mmap'd ELFs of the target — discovered by
// parsing /proc/<pid>/maps and mmap'ing the same file paths into our
// process, read-only. This avoids ptrace entirely.
//
// Scope note: this file is the working spike. It deliberately omits:
//   - caching across unwind calls (re-reads /proc/<pid>/maps per call)
//   - eviction on target mmap/munmap
//   - thread safety (a single Unwinder should be used from one goroutine)
//   - arm64 register index translation (amd64 only for now)
//   - dynamic code (JIT regions with no ELF backing)
// Each of those is a planned follow-up commit.

#include "unwinder.h"

#include <libunwind.h>
#include <libunwind-x86_64.h>

#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

// ---- per-PID ELF mapping table -------------------------------------------

typedef struct elf_mapping {
    uint64_t target_start;  // address in target's VA space
    uint64_t target_end;
    uint64_t offset;        // file offset of the mapping in the ELF
    void    *local;         // mmap'd into OUR process, covers [target_start, target_end)
    size_t   local_size;    // bytes mmap'd locally
    char     path[256];
} elf_mapping_t;

#define MAX_MAPPINGS 256

typedef struct {
    elf_mapping_t items[MAX_MAPPINGS];
    int           count;
    int32_t       pid;         // pid these mappings belong to
    int           procmem_fd;  // /proc/<pid>/mem, opened lazily
} pid_mappings_t;

// ---- per-sample context passed to libunwind accessors --------------------

typedef struct {
    // Captured sample state
    const uint64_t *regs;
    size_t          regs_len;
    uint64_t        stack_base;
    const uint8_t  *stack;
    size_t          stack_len;

    // PID + ELF mapping index
    pid_mappings_t *maps;

    // Writable mirror of captured regs, updated as libunwind restores frames.
    // Indexed by our logical register id (REG_RIP, REG_RSP, REG_RBP, ...),
    // NOT by libunwind's unw_regnum_t.
    uint64_t        live_regs[24];
} unwind_ctx_t;

// ---- register mapping (amd64) --------------------------------------------
//
// Our captured regs[] layout comes from perfreader's SampleRegsUser mask.
// The dense order matches kernel's <asm/perf_regs.h>: AX=0 BX=1 CX=2 DX=3
// SI=4 DI=5 BP=6 SP=7 IP=8 ... R8=16 ... R15=23. Bits for FLAGS/CS/SS/DS/
// ES/FS/GS (9..15) are NOT set in SampleRegsUser, so those positions are
// SKIPPED in the dense array.
//
// The table below maps from the DENSE captured-regs index to a logical
// slot in live_regs[]. Logical slots use the SAME indices as <asm/perf_regs>
// so subsequent code can reference PERF_REG_X86_IP etc. directly.
#define NUM_LIVE_REGS 24

// perfreader/regs_amd64.go mirror: dense index -> perf_regs.h index
static const int k_regs_dense_to_perf[] = {
    0,  // AX
    1,  // BX
    2,  // CX
    3,  // DX
    4,  // SI
    5,  // DI
    6,  // BP
    7,  // SP
    8,  // IP
    16, // R8
    17, // R9
    18, // R10
    19, // R11
    20, // R12
    21, // R13
    22, // R14
    23, // R15
};
#define NUM_CAPTURED_REGS \
    (sizeof(k_regs_dense_to_perf) / sizeof(k_regs_dense_to_perf[0]))

// Translate libunwind x86_64 regnum -> perf_regs.h index. Returns -1 for
// regs we don't track.
static int ua_libunwind_to_perf(unw_regnum_t reg) {
    switch (reg) {
        case UNW_X86_64_RAX: return 0;
        case UNW_X86_64_RBX: return 1;
        case UNW_X86_64_RCX: return 2;
        case UNW_X86_64_RDX: return 3;
        case UNW_X86_64_RSI: return 4;
        case UNW_X86_64_RDI: return 5;
        case UNW_X86_64_RBP: return 6;
        case UNW_X86_64_RSP: return 7;
        case UNW_X86_64_R8:  return 16;
        case UNW_X86_64_R9:  return 17;
        case UNW_X86_64_R10: return 18;
        case UNW_X86_64_R11: return 19;
        case UNW_X86_64_R12: return 20;
        case UNW_X86_64_R13: return 21;
        case UNW_X86_64_R14: return 22;
        case UNW_X86_64_R15: return 23;
        // UNW_REG_IP aliases to UNW_X86_64_RIP on x86_64; since the arch-
        // specific RIP is not otherwise handled above, map it here.
        case UNW_REG_IP:     return 8;
        // UNW_REG_SP aliases to UNW_X86_64_RSP, which is already handled
        // in the RSP case above — no separate entry needed.
        default: return -1;
    }
}

// ---- /proc/<pid>/maps parsing --------------------------------------------
//
// We want only executable, file-backed mappings — those are where .eh_frame
// lives. Format lines look like:
//
//   7f8c4aa00000-7f8c4aa23000 r-xp 00000000 fd:00 12345  /usr/lib64/libfoo.so
//
// We parse start-end, perms (must contain 'x'), offset, and path.

static int maps_load(pid_mappings_t *m, int32_t pid) {
    char path[64];
    snprintf(path, sizeof(path), "/proc/%d/maps", pid);
    FILE *f = fopen(path, "r");
    if (!f) return -1;

    m->count = 0;
    char line[512];
    while (fgets(line, sizeof(line), f) && m->count < MAX_MAPPINGS) {
        uint64_t start, end, offset;
        char perms[8];
        int path_start = 0;
        // scanf up through the path offset, remember where path begins.
        int n = sscanf(line, "%lx-%lx %7s %lx %*x:%*x %*d %n",
                       &start, &end, perms, &offset, &path_start);
        if (n < 4 || path_start == 0) continue;
        if (strchr(perms, 'x') == NULL) continue;      // need executable
        const char *p = line + path_start;
        if (*p != '/') continue;                       // need real file path
        // strip trailing newline
        char *nl = strchr(p, '\n');
        if (nl) *nl = '\0';
        // skip mappings for deleted files / anon
        if (strstr(p, "(deleted)")) continue;

        elf_mapping_t *em = &m->items[m->count];
        em->target_start = start;
        em->target_end   = end;
        em->offset       = offset;
        em->local        = MAP_FAILED;
        em->local_size   = 0;
        strncpy(em->path, p, sizeof(em->path) - 1);
        em->path[sizeof(em->path) - 1] = '\0';
        m->count++;
    }
    fclose(f);
    m->pid = pid;
    return 0;
}

// maps_mmap_file opens `path` read-only and mmaps the region covering
// [offset, offset + (target_end - target_start)). Returns MAP_FAILED on
// error. Called lazily on first memory access into a given mapping.
static void *maps_mmap_file(elf_mapping_t *em) {
    if (em->local != MAP_FAILED) return em->local;
    int fd = open(em->path, O_RDONLY | O_CLOEXEC);
    if (fd < 0) return MAP_FAILED;
    size_t sz = (size_t)(em->target_end - em->target_start);
    // Round up to page, and ensure offset is page-aligned (it is — kernel
    // only maps ELF segments at page boundaries).
    void *addr = mmap(NULL, sz, PROT_READ, MAP_PRIVATE, fd, (off_t)em->offset);
    close(fd);
    if (addr == MAP_FAILED) return MAP_FAILED;
    em->local = addr;
    em->local_size = sz;
    return addr;
}

static void maps_free(pid_mappings_t *m) {
    for (int i = 0; i < m->count; i++) {
        if (m->items[i].local != MAP_FAILED && m->items[i].local != NULL) {
            munmap(m->items[i].local, m->items[i].local_size);
        }
    }
    m->count = 0;
    if (m->procmem_fd > 0) {
        close(m->procmem_fd);
        m->procmem_fd = -1;
    }
}

// Find the mapping containing target address `addr`.
static elf_mapping_t *maps_lookup(pid_mappings_t *m, uint64_t addr) {
    for (int i = 0; i < m->count; i++) {
        if (addr >= m->items[i].target_start && addr < m->items[i].target_end) {
            return &m->items[i];
        }
    }
    return NULL;
}

// Read 8 bytes at target address `addr` by translating via the local ELF
// mmap. Returns 0 on success, -1 on failure.
static int maps_read8(pid_mappings_t *m, uint64_t addr, uint64_t *out) {
    elf_mapping_t *em = maps_lookup(m, addr);
    if (!em) return -1;
    void *local = maps_mmap_file(em);
    if (local == MAP_FAILED) return -1;
    size_t rel = (size_t)(addr - em->target_start);
    if (rel + 8 > em->local_size) return -1;
    memcpy(out, (const uint8_t *)local + rel, 8);
    return 0;
}

// Read `n` bytes starting at target `addr` into `dst`. Returns 0 on success.
// Used by libunwind to fetch ELF headers / .eh_frame through access_mem.
static int maps_read_range(pid_mappings_t *m, uint64_t addr, void *dst, size_t n) {
    elf_mapping_t *em = maps_lookup(m, addr);
    if (!em) return -1;
    void *local = maps_mmap_file(em);
    if (local == MAP_FAILED) return -1;
    size_t rel = (size_t)(addr - em->target_start);
    if (rel + n > em->local_size) return -1;
    memcpy(dst, (const uint8_t *)local + rel, n);
    return 0;
}

// Fallback: read via /proc/<pid>/mem (opened lazily). Slow path for data
// segments / heap / captured-stack misses.
static int procmem_read8(pid_mappings_t *m, uint64_t addr, uint64_t *out) {
    if (m->procmem_fd < 0) {
        char path[64];
        snprintf(path, sizeof(path), "/proc/%d/mem", m->pid);
        m->procmem_fd = open(path, O_RDONLY | O_CLOEXEC);
        if (m->procmem_fd < 0) return -1;
    }
    ssize_t r = pread(m->procmem_fd, out, 8, (off_t)addr);
    return (r == 8) ? 0 : -1;
}

// ---- libunwind accessor callbacks ----------------------------------------

static int our_find_proc_info(unw_addr_space_t as, unw_word_t ip,
                              unw_proc_info_t *pi, int need_unwind_info,
                              void *arg) {
    (void)as; (void)ip; (void)pi; (void)need_unwind_info; (void)arg;
    // Spike note: libunwind's default dwarf_find_proc_info requires
    // the address space to be initialized with ELF metadata it can walk
    // via access_mem. Our access_mem already serves ELF bytes through
    // the local file mmap, so this SHOULD work when hooked up. Leaving
    // as a stub for now — the next commit wires dwarf_find_proc_info in
    // after validating the rest of the pipeline builds.
    return -UNW_ENOINFO;
}

static void our_put_unwind_info(unw_addr_space_t as, unw_proc_info_t *pi, void *arg) {
    (void)as; (void)pi; (void)arg;
}

static int our_get_dyn_info_list_addr(unw_addr_space_t as, unw_word_t *dil_addr, void *arg) {
    (void)as; (void)dil_addr; (void)arg;
    return -UNW_ENOINFO;
}

static int our_access_mem(unw_addr_space_t as, unw_word_t addr,
                          unw_word_t *val, int write, void *arg) {
    (void)as;
    if (write) return -UNW_EINVAL;
    unwind_ctx_t *ctx = (unwind_ctx_t *)arg;

    // Fast path: captured stack.
    if (addr >= ctx->stack_base && addr + 8 <= ctx->stack_base + ctx->stack_len) {
        memcpy(val, ctx->stack + (addr - ctx->stack_base), 8);
        return 0;
    }
    // Code / .eh_frame via locally-mmap'd ELFs.
    uint64_t v;
    if (maps_read8(ctx->maps, addr, &v) == 0) {
        *val = (unw_word_t)v;
        return 0;
    }
    // Last resort: /proc/<pid>/mem.
    if (procmem_read8(ctx->maps, addr, &v) == 0) {
        *val = (unw_word_t)v;
        return 0;
    }
    return -UNW_EINVAL;
}

static int our_access_reg(unw_addr_space_t as, unw_regnum_t reg,
                          unw_word_t *val, int write, void *arg) {
    (void)as;
    unwind_ctx_t *ctx = (unwind_ctx_t *)arg;
    int idx = ua_libunwind_to_perf(reg);
    if (idx < 0 || idx >= NUM_LIVE_REGS) return -UNW_EBADREG;
    if (write) {
        ctx->live_regs[idx] = *val;
    } else {
        *val = ctx->live_regs[idx];
    }
    return 0;
}

static int our_access_fpreg(unw_addr_space_t as, unw_regnum_t reg,
                            unw_fpreg_t *val, int write, void *arg) {
    (void)as; (void)reg; (void)val; (void)write; (void)arg;
    return -UNW_EBADREG;
}

static int our_resume(unw_addr_space_t as, unw_cursor_t *c, void *arg) {
    (void)as; (void)c; (void)arg;
    return -UNW_EINVAL;
}

static int our_get_proc_name(unw_addr_space_t as, unw_word_t addr,
                             char *buf, size_t buf_len, unw_word_t *offset,
                             void *arg) {
    (void)as; (void)addr; (void)buf; (void)buf_len; (void)offset; (void)arg;
    return -UNW_ENOINFO;
}

static unw_accessors_t k_accessors = {
    .find_proc_info        = our_find_proc_info,
    .put_unwind_info       = our_put_unwind_info,
    .get_dyn_info_list_addr = our_get_dyn_info_list_addr,
    .access_mem            = our_access_mem,
    .access_reg            = our_access_reg,
    .access_fpreg          = our_access_fpreg,
    .resume                = our_resume,
    .get_proc_name         = our_get_proc_name,
};

// ---- public API ----------------------------------------------------------

struct pa_unwinder {
    unw_addr_space_t as;
};

pa_unwinder_t *pa_unwinder_new(void) {
    pa_unwinder_t *u = calloc(1, sizeof(*u));
    if (!u) return NULL;
    u->as = unw_create_addr_space(&k_accessors, __LITTLE_ENDIAN);
    if (!u->as) {
        free(u);
        return NULL;
    }
    return u;
}

void pa_unwinder_free(pa_unwinder_t *u) {
    if (!u) return;
    if (u->as) unw_destroy_addr_space(u->as);
    free(u);
}

// Populate live_regs[] from the dense captured regs slice.
static void hydrate_live_regs(unwind_ctx_t *ctx,
                              const uint64_t *regs, size_t regs_len) {
    memset(ctx->live_regs, 0, sizeof(ctx->live_regs));
    size_t n = regs_len < NUM_CAPTURED_REGS ? regs_len : NUM_CAPTURED_REGS;
    for (size_t i = 0; i < n; i++) {
        int slot = k_regs_dense_to_perf[i];
        if (slot >= 0 && slot < NUM_LIVE_REGS) {
            ctx->live_regs[slot] = regs[i];
        }
    }
}

int pa_unwind(pa_unwinder_t *u,
              int32_t pid,
              const uint64_t *regs, size_t regs_len,
              uint64_t stack_base,
              const uint8_t *stack, size_t stack_len,
              uint64_t *out_pcs, size_t max_pcs) {
    if (!u || !u->as || !regs || !out_pcs || max_pcs == 0) return -1;

    pid_mappings_t maps = { .procmem_fd = -1 };
    if (maps_load(&maps, pid) != 0) return -2;

    unwind_ctx_t ctx = {
        .regs        = regs,
        .regs_len    = regs_len,
        .stack_base  = stack_base,
        .stack       = stack,
        .stack_len   = stack_len,
        .maps        = &maps,
    };
    hydrate_live_regs(&ctx, regs, regs_len);

    unw_cursor_t cursor;
    int ret = unw_init_remote(&cursor, u->as, &ctx);
    if (ret != 0) {
        maps_free(&maps);
        return -3;
    }

    size_t n = 0;
    do {
        if (n >= max_pcs) break;
        unw_word_t ip;
        if (unw_get_reg(&cursor, UNW_REG_IP, &ip) != 0) break;
        out_pcs[n++] = (uint64_t)ip;
        ret = unw_step(&cursor);
    } while (ret > 0);

    maps_free(&maps);
    return (int)n;
}
