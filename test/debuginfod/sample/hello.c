#include <stdio.h>
#include <unistd.h>

void deep_function(int depth) {
    if (depth > 0) {
        deep_function(depth - 1);
    } else {
        printf("Hit bottom\n");
    }
}

void middle_function(int n) {
    deep_function(n);
}

int main(int argc, char **argv) {
    while (1) {
        middle_function(5);
        sleep(1);
    }
    return 0;
}
