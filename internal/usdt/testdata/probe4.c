#define _SDT_HAS_SEMAPHORES 1
#include <sys/sdt.h>
#include <stdint.h>
#include <stdio.h>

/* The semaphore must be declared by the shim itself; sys/sdt.h only
   references it. A consumer attaching bumps it, which is how the shim
   learns someone is listening and can replay module/stall-map/config
   records. This is the "real semaphore" fixture: Semaphore != 0x0. */
__extension__ unsigned short perfagent_gpu_launch_v1_semaphore
    __attribute__((unused)) __attribute__((section(".probes")));

void emit_batch(uint64_t ptr) {
    if (perfagent_gpu_launch_v1_semaphore) {      /* nobody listening -> skip the work */
        STAP_PROBE1(perfagent, gpu_launch_v1, ptr);
    }
}
int main(void) { emit_batch(0xdeadbeef); printf("semaphore=%u\n", perfagent_gpu_launch_v1_semaphore); return 0; }
