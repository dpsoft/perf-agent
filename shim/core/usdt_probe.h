// USDT probes without a systemtap build dependency: the .note.stapsdt notes
// are emitted directly. Decided on evidence in spec §15 — a hand-written note
// round-trips through readelf and through internal/usdt.
//
// The three arguments are bound to explicit registers so the note's argument
// descriptor is a constant of the ABI. Letting the compiler choose produced
// "8@%rax 8@%rdx 8@%rcx" in the spike; the consumer reads fixed registers.
//
// The probe's first argument is a POINTER, and the consumer dereferences it
// with bpf_probe_read_user the instant the trap fires. That makes the record
// buffer an input to the asm even though no operand names it, and the
// compiler has to be told: without the "memory" clobber it is free to assume
// nothing ever reads the buffer, and it does exactly that. See
// PERFAGENT_USDT_PROBE3.
#ifndef PERFAGENT_USDT_PROBE_H
#define PERFAGENT_USDT_PROBE_H

#define PERFAGENT_USDT_BASE                                                 \
    ".ifndef _.stapsdt.base\n"                                              \
    ".pushsection .stapsdt.base,\"aG\",\"progbits\",.stapsdt.base,comdat\n" \
    ".weak _.stapsdt.base\n"                                                \
    ".hidden _.stapsdt.base\n"                                              \
    "_.stapsdt.base: .space 1\n"                                            \
    ".size _.stapsdt.base,1\n"                                              \
    ".popsection\n"                                                         \
    ".endif\n"

// The semaphore the kernel maintains through link.UprobeOptions RefCtrOffset.
// Hidden visibility: core/ must not leak symbols into the application it is
// injected into (spec §6.1).
#define PERFAGENT_USDT_SEMAPHORE(name)                                      \
    __asm__ (                                                               \
        ".pushsection .probes,\"aw\",\"progbits\"\n"                        \
        ".balign 2\n"                                                       \
        ".globl perfagent_" #name "_semaphore\n"                            \
        ".hidden perfagent_" #name "_semaphore\n"                           \
        ".type perfagent_" #name "_semaphore,@object\n"                     \
        ".size perfagent_" #name "_semaphore,2\n"                           \
        "perfagent_" #name "_semaphore: .zero 2\n"                          \
        ".popsection\n");                                                   \
    extern "C" unsigned short perfagent_##name##_semaphore                  \
        __attribute__((visibility("hidden")))

// True when a consumer is attached. Every emit path checks this first.
#define PERFAGENT_USDT_ENABLED(name) (perfagent_##name##_semaphore != 0)

// The "memory" clobber is load-bearing, not defensive. `ptr` reaches the asm
// as a plain integer in a register: an operand list without it says the asm
// touches no memory at all, so the compiler may sink — or delete outright —
// the stores that filled the record, and the consumer then reads a buffer
// that was never written. That is not hypothetical. At -O2, g++ dropped the
// last two field stores of shim/stub/stub.cc's sampled-launch record
// (sample_period and launch_seq, the two nothing read again), and every
// sampled launch reached the consumer with sample_period == 0 — rejected by
// gpuabi.DecodeLaunchSampled, one hundred percent of them.
//
// The clobber costs a scheduling barrier at the probe site, which is
// nothing next to the ~1-2us uprobe trap the probe is about to take, and it
// is paid only on the path where the semaphore is already armed.
#define PERFAGENT_USDT_PROBE3(name, ptr, count, seq)                        \
  do {                                                                      \
    register unsigned long _a0 __asm__("rdi") = (unsigned long)(ptr);       \
    register unsigned long _a1 __asm__("rsi") = (unsigned long)(count);     \
    register unsigned long _a2 __asm__("rdx") = (unsigned long)(seq);       \
    __asm__ __volatile__ (                                                  \
      "990: nop\n"                                                          \
      PERFAGENT_USDT_BASE                                                   \
      ".pushsection .note.stapsdt,\"\",\"note\"\n"                          \
      ".balign 4\n"                                                         \
      ".4byte 992f-991f, 994f-993f, 3\n"                                    \
      "991: .asciz \"stapsdt\"\n"                                           \
      "992: .balign 4\n"                                                    \
      "993: .8byte 990b\n"                                                  \
      ".8byte _.stapsdt.base\n"                                             \
      ".8byte perfagent_" #name "_semaphore\n"                              \
      ".asciz \"perfagent\"\n"                                              \
      ".asciz \"" #name "\"\n"                                              \
      ".asciz \"8@%%rdi 8@%%rsi 8@%%rdx\"\n"                                \
      "994: .balign 4\n"                                                    \
      ".popsection\n"                                                       \
      :: "r"(_a0), "r"(_a1), "r"(_a2) : "memory");                          \
  } while (0)

// Declares the semaphore for probe `name` plus the two thunks a Batch needs,
// and pins the record's wire size.
//
// Every perfagent probe is named after the record it carries: probe
// gpu_launch_v1 carries struct gpu_launch_v1. That pairing is not decoration
// — the eBPF consumer derives the record size from an attach cookie keyed on
// the probe name (record_size() in bpf/gpu_usdt.bpf.c), so a probe fired over
// a buffer of some other record makes the kernel read past the end of it.
//
// Generating the probe fire and the thunk's parameter type from the same
// token, together with Batch's emit callback being typed on its record, makes
// that mismatch a compile error: `Batch<gpu_module_load_v1, N>` will not
// accept gpu_launch_v1_emit. `wire_size` pins the frozen size the consumer's
// cookie assumes (spec §6.3), so a record that grows is caught here too.
#define PERFAGENT_USDT_EMITTER(name, wire_size)                             \
    PERFAGENT_USDT_SEMAPHORE(name);                                         \
    static_assert(sizeof(struct name) == (wire_size),                       \
                  #name " record size is frozen; the BPF cookie assumes it");\
    static bool name##_enabled() { return PERFAGENT_USDT_ENABLED(name); }   \
    static void name##_emit(const struct name *ptr, unsigned long count,    \
                            unsigned long seq) {                            \
        PERFAGENT_USDT_PROBE3(name, ptr, count, seq);                       \
    }                                                                       \
    static_assert(true, "swallow the trailing semicolon")

#endif
