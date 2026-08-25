// A compile-time test with no runtime: it passes by FAILING to compile.
//
// The plan's structural assertion for Task 5 is that "no deferral may sit
// between callback entry and the memcpy", enforced by the type system rather
// than by a comment. A guarantee nobody has tried to violate is a claim, so
// this file tries, five ways, and shim/Makefile's `test` target fails the
// build if any of them succeeds.
//
// Build it with -DPERFAGENT_CUBIN_DEFER=<n>:
//
//   0  the COMPLIANT capture. Must compile. Without this control the check
//      would pass just as happily on a file that fails to compile for some
//      unrelated reason - a typo, a missing include, a renamed header - and
//      would then be proving nothing at all.
//   1  store the view in a struct that outlives the callback
//   2  heap-allocate the view and keep the address
//   3  move the view into a container
//   4  read the borrowed pointer straight out of the view
//   5  assign one view over another
//
// Each of 1-5 is a real thing an implementer would reach for while "just
// deferring the copy to the drain thread"; each is a compile error, and the
// Makefile prints which.
#include "cubinqueue.h"

#include <cstdint>
#include <cstdlib>
#include <utility>
#include <vector>

#ifndef PERFAGENT_CUBIN_DEFER
#define PERFAGENT_CUBIN_DEFER 0
#endif

namespace {

uint64_t fake_crc(const void *bytes, size_t len) {
    (void)bytes;
    return len ? 0x1234u : 0u;
}

void fake_captured(void *, uint64_t, const void *, size_t) {}

}  // namespace

#if PERFAGENT_CUBIN_DEFER == 0

// The compliant shape: the view is created at the callback, handed to
// capture, and dies there. The copy is taken before the function returns.
bool compliant(perfagent::CubinQueue &q, const void *vendor_bytes, size_t len) {
    const perfagent::CubinView view(vendor_bytes, len);
    return q.capture(view, fake_crc, fake_captured, nullptr);
}

#elif PERFAGENT_CUBIN_DEFER == 1

// "I'll stash it and copy on the drain thread." The view has no copy
// constructor and no default constructor, so it cannot be a member that is
// filled in later.
struct Deferred {
    perfagent::CubinView view;
};

Deferred defer_by_storing(const void *vendor_bytes, size_t len) {
    const perfagent::CubinView view(vendor_bytes, len);
    return Deferred{view};
}

#elif PERFAGENT_CUBIN_DEFER == 2

// "I'll put it on the heap and keep the pointer." operator new is deleted.
perfagent::CubinView *defer_by_heap(const void *vendor_bytes, size_t len) {
    return new perfagent::CubinView(vendor_bytes, len);
}

#elif PERFAGENT_CUBIN_DEFER == 3

// "I'll move it into the queue." No move constructor either, so a container
// cannot hold one.
void defer_by_moving(std::vector<perfagent::CubinView> &sink, const void *vendor_bytes,
                     size_t len) {
    perfagent::CubinView view(vendor_bytes, len);
    sink.push_back(std::move(view));
}

#elif PERFAGENT_CUBIN_DEFER == 4

// The direct route: take the pointer out and keep THAT. There is no accessor
// for it - copy_to() is the only member that can reach bytes_ - so this does
// not compile no matter how the caller then stores the result.
const void *defer_by_stealing_the_pointer(const perfagent::CubinView &view) {
    return view.bytes();
}

#elif PERFAGENT_CUBIN_DEFER == 5

// "I'll keep one around and re-point it at each new buffer." Assignment is
// deleted, so a long-lived view slot does not exist.
void defer_by_reassigning(perfagent::CubinView &slot, const void *vendor_bytes, size_t len) {
    slot = perfagent::CubinView(vendor_bytes, len);
}

#else
#error "PERFAGENT_CUBIN_DEFER must be 0..5"
#endif
