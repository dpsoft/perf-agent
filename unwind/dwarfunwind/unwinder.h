// unwinder.h — C surface for the DWARF-based stack unwinder. Called from
// Go via CGO. See unwinder.c for implementation notes.
#ifndef PERFAGENT_DWARFUNWIND_H
#define PERFAGENT_DWARFUNWIND_H

#include <stdint.h>
#include <stddef.h>

// pa_unwinder_t is an opaque handle returned by pa_unwinder_new and freed
// by pa_unwinder_free. One per process is sufficient.
typedef struct pa_unwinder pa_unwinder_t;

// pa_unwinder_new allocates a new unwinder and its libunwind address space.
// Returns NULL on failure (out of memory, libunwind init failure).
pa_unwinder_t *pa_unwinder_new(void);

// pa_unwinder_free releases all resources: address space, open /proc fds,
// locally-mmap'd ELF regions.
void pa_unwinder_free(pa_unwinder_t *u);

// pa_unwind walks a captured stack and fills out_pcs (leaf-first) with up
// to max_pcs return addresses including the initial IP.
//
// Inputs:
//   pid:        target process ID (required to locate /proc/<pid>/maps
//               and read code memory outside the captured stack).
//   regs:       register values in the order implied by SampleRegsUser
//               (see unwind/perfreader/regs_amd64.go). Must have at least
//               3 entries exposing IP, SP, BP at the indices baked into
//               the C side (see ua_regs_t layout).
//   regs_len:   number of valid entries in regs.
//   stack_base: target address of stack[0] (RSP at sample time).
//   stack:      captured stack bytes.
//   stack_len:  length of stack in bytes.
//   out_pcs:    caller-allocated buffer for result PCs.
//   max_pcs:    capacity of out_pcs.
//
// Returns: number of PCs written (>= 1 on success, 0 if IP is invalid),
//          or negative on error (see pa_unwind_err_* codes in .c).
int pa_unwind(pa_unwinder_t *u,
              int32_t pid,
              const uint64_t *regs, size_t regs_len,
              uint64_t stack_base,
              const uint8_t *stack, size_t stack_len,
              uint64_t *out_pcs, size_t max_pcs);

#endif // PERFAGENT_DWARFUNWIND_H
