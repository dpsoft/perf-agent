#include <stdint.h>
#include <stddef.h>

/* A USDT probe with no systemtap dependency: the .note.stapsdt note is just
   an ELF note with a documented layout. This is the inline-asm route
   docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md §6 flags as
   the alternative to systemtap-sdt-devel.

   Note: nothing stops the compiler from duplicating this call site when it
   inlines emit_batch, and it does here -- the resulting binary carries two
   NT_STAPSDT notes with identical provider/name and different locations.
   That is intentional and is exactly what internal/usdt must not
   deduplicate. Semaphore is 0x0 throughout: this route never declares one. */
#define STAP_NOTE(provider, name, arg1)                                  \
  __asm__ __volatile__ (                                                 \
    "990: nop\n"                                                         \
    ".pushsection .note.stapsdt,\"?\",\"note\"\n"                        \
    ".balign 4\n"                                                        \
    ".4byte 992f-991f, 994f-993f, 3\n"                                   \
    "991: .asciz \"stapsdt\"\n"                                          \
    "992: .balign 4\n"                                                   \
    "993: .8byte 990b\n"                                                 \
    "     .8byte 0\n"                                                    \
    "     .8byte 0\n"                                                    \
    "     .asciz \"" #provider "\"\n"                                    \
    "     .asciz \"" #name "\"\n"                                        \
    "     .asciz \"8@%0\"\n"                                             \
    "994: .balign 4\n"                                                   \
    ".popsection\n"                                                      \
    :: "r" ((uint64_t)(arg1)))

void emit_batch(uint64_t ptr) { STAP_NOTE(perfagent, gpu_launch_v1, ptr); }
int main(void) { emit_batch(0xdeadbeef); return 0; }
