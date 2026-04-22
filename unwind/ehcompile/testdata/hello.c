// Fixture for ehcompile snapshot tests. Deliberately simple — one main
// that calls a leaf function — so the resulting .eh_frame is small and
// the golden file stays readable.
#include <stdio.h>

__attribute__((noinline))
int leaf(int x) {
    return x * 2 + 1;
}

int main(int argc, char **argv) {
    (void)argv;
    return leaf(argc);
}
