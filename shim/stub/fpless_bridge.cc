// The frame-pointer-less half of the FP-less producer.
//
// This file, and only this file, is compiled -fomit-frame-pointer (see
// shim/Makefile's perfagent-gpu-fpless target). Everything else in that
// binary -- shim/stub/stub.cc, shim/core/, and stub/fpless_main.cc -- keeps
// frame pointers, so the stack the consumer walks is a genuine hybrid:
//
//   main                      frame pointer      FP walk
//   perfagent_fpless_caller   NO frame pointer   DWARF only
//   perfagent_fpless_bridge   NO frame pointer   DWARF only
//   perfagent_stub_run        frame pointer      FP walk   <- probe fires here
//
// That is the shape of a real CUDA stack: the probe fires inside the vendor
// shim, and between it and the application sit libcupti/libcuda frames built
// without frame pointers.
//
// # Why this is not decoration
//
// A saved-RBP walk cannot produce `perfagent_fpless_caller`. Neither function
// below establishes rbp, and neither clobbers it, so rbp throughout both of
// them still holds *main's* frame pointer. The FP walk therefore steps from
// perfagent_stub_run's frame straight to main's, skipping both:
//
//   FP-only walk:  perfagent_stub_run, perfagent_fpless_bridge, main, ...
//   hybrid walk:   perfagent_stub_run, perfagent_fpless_bridge,
//                  perfagent_fpless_caller, main
//
// (perfagent_fpless_bridge's PC appears in both, because it is the return
// address stored in perfagent_stub_run's own frame -- reaching a frame's
// return address is not the same as unwinding that frame.)
//
// So `perfagent_fpless_caller` appearing in a resolved stack is a *positive*
// witness that the walker read this binary's .eh_frame and unwound an FP-less
// frame with it. That is what gpuprobe/gate_test.go asserts on, and it is why
// the assertion cannot pass under the frame-pointer walker this phase
// replaces.
//
// # Two constraints on the code below
//
// 1. Neither function may touch %rbp. If either used it as a general-purpose
//    register (which -fomit-frame-pointer permits), perfagent_stub_run would
//    save that garbage in its prologue and the walk would die at its very
//    first FP step -- before ever reaching an FP-less frame, so the DWARF
//    path would never be exercised at all. `make check-fpless` fails the
//    build if that ever happens.
// 2. Neither call may become a tail call. A sibling call is a `jmp`: the
//    frame never exists, and the walk skips it. The volatile guard local and
//    the value returned through it are what force work *after* the call
//    returns, which is what forbids the tail call.

extern "C" void perfagent_stub_run(unsigned launches, unsigned period_us, unsigned sample_period);

// noinline: an inlined function is not a frame, and a frame is the point.
// default visibility survives the Makefile's -fvisibility=hidden, so both
// names land in .dynsym as well as .symtab and the gate can assert on them.
extern "C" __attribute__((noinline)) __attribute__((visibility("default")))
unsigned long perfagent_fpless_bridge(unsigned launches, unsigned period_us,
                                      unsigned sample_period) {
    volatile unsigned long guard = 0x5f5f5f5f5f5f5f5fUL;
    perfagent_stub_run(launches, period_us, sample_period);
    return guard ^ (unsigned long)launches;
}

extern "C" __attribute__((noinline)) __attribute__((visibility("default")))
unsigned long perfagent_fpless_caller(unsigned launches, unsigned period_us,
                                      unsigned sample_period) {
    volatile unsigned long guard = 0x3d3d3d3d3d3d3d3dUL;
    const unsigned long inner = perfagent_fpless_bridge(launches, period_us, sample_period);
    return guard ^ inner;
}
