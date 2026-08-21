// The entry point of the FP-less producer. Compiled -fno-omit-frame-pointer
// on purpose: main must be the frame the walk lands on *after* it has crossed
// the two FP-less frames in stub/fpless_bridge.cc, and it can only be that if
// it has a frame pointer of its own.
//
// It defines no records and no probes. perfagent_stub_run and the linger
// contract come from stub/stub.cc, compiled here with
// -DPERFAGENT_STUB_NO_MAIN, so this binary emits byte-for-byte the records
// perfagent-gpu-stub emits and every gate assertion about them keeps its
// meaning.
#include <cstdlib>

extern "C" unsigned long perfagent_fpless_caller(unsigned launches, unsigned period_us,
                                                 unsigned sample_period);
extern "C" void perfagent_stub_linger(unsigned linger_ms);

int main(int argc, char **argv) {
    const unsigned n = argc > 1 ? (unsigned)atoi(argv[1]) : 1000;
    const unsigned us = argc > 2 ? (unsigned)atoi(argv[2]) : 100;
    const unsigned sp = argc > 3 ? (unsigned)atoi(argv[3]) : 8;
    const unsigned lm = argc > 4 ? (unsigned)atoi(argv[4]) : 0;
    // The result is used, not discarded: a discarded value would let the
    // optimizer prove the post-call work in the bridge chain is dead and
    // restore the sibling calls those locals exist to prevent.
    const unsigned long sink = perfagent_fpless_caller(n, us, sp);
    perfagent_stub_linger(lm);
    return sink == 0 ? 1 : 0;
}
