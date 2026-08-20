// USDT probes without a systemtap build dependency: the .note.stapsdt notes
// are emitted directly. Decided on evidence in spec §15 — a hand-written note
// round-trips through readelf and through internal/usdt.
//
// The three arguments are bound to explicit registers so the note's argument
// descriptor is a constant of the ABI. Letting the compiler choose produced
// "8@%rax 8@%rdx 8@%rcx" in the spike; the consumer reads fixed registers.
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
      :: "r"(_a0), "r"(_a1), "r"(_a2));                                     \
  } while (0)

#endif
