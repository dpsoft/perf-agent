// CPU-bound: 99% of dispatched ops are Add. clang -fprofile-sample-use
// will move the Add arm to fall-through and inline it through the loop.
//
// Build with -g (-C debuginfo=2 equivalent) so create_llvm_prof can resolve
// symbols. Run: ./workload <iterations>

#include <cstdint>
#include <cstdio>
#include <cstdlib>

enum class Op { Add, Sub, Mul, Div };

__attribute__((noinline))
static uint64_t dispatch(Op op, uint64_t a, uint64_t b) {
    switch (op) {
        case Op::Add: return a + b;
        case Op::Sub: return a - b;
        case Op::Mul: return a * b;
        case Op::Div: return b == 0 ? 0 : a / b;
    }
    __builtin_unreachable();
}

int main(int argc, char** argv) {
    uint64_t n = (argc > 1) ? std::strtoull(argv[1], nullptr, 10) : 200000000;
    static const Op ops[] = {Op::Add, Op::Sub, Op::Mul, Op::Div};
    uint64_t total = 1;
    for (uint64_t i = 0; i < n; ++i) {
        Op op = (i % 100 == 0) ? ops[(i / 100) % 4] : Op::Add;
        total += dispatch(op, i, total);
    }
    std::printf("%llu\n", (unsigned long long)total);
    return 0;
}
