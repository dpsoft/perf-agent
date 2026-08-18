#include <sys/sdt.h>
#include <stdint.h>

/* A single USDT probe via the systemtap sys/sdt.h header
   (DTRACE_PROBE1, no _SDT_HAS_SEMAPHORES), so it carries a real
   .stapsdt.base section but Semaphore: 0x0 -- the "no semaphore" case. */
void emit_batch(uint64_t ptr) { DTRACE_PROBE1(perfagent, gpu_launch_v1, ptr); }
int main(void) { emit_batch(0xdeadbeef); return 0; }
