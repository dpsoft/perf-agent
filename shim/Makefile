# Targets:
#   all         libperfagent-gpu-core.a
#   test        fast unit tests: batch, clock, drain, and the C11 ABI header
#   test-tsan   the concurrency case under ThreadSanitizer (clang++ only)
#   clean
CXX ?= c++
CC ?= cc
# GCC's TSan runtime is not installed on the reference machine; clang++ is the
# one that links -fsanitize=thread here.
TSAN_CXX ?= clang++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra -fPIC -fvisibility=hidden -I core

CORE_SRC := core/batch.cc core/clock.cc core/drain.cc
CORE_OBJ := $(CORE_SRC:.cc=.o)

.PHONY: all test test-tsan clean
all: libperfagent-gpu-core.a

libperfagent-gpu-core.a: $(CORE_OBJ)
	ar rcs $@ $^

%.o: %.cc
	$(CXX) $(CXXFLAGS) -c $< -o $@

perfagent-gpu-stub: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -o $@ stub/stub.cc $(CORE_SRC)

libperfagent-gpu-stub.so: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -shared -o $@ stub/stub.cc $(CORE_SRC)

# core/usdt_abi_test.c is built with a C compiler on purpose: usdt_abi.h is the
# ABI both the C++ shim and any C vendor adapter include, and its
# _Static_asserts are the test. Compiling it only as C++ leaves the C11 spelling
# unguarded -- which is how the header's validity broke earlier in this branch,
# when a C++ translation unit included it for the first time.
test: core/batch_test.cc core/clock_test.cc core/drain_test.cc core/usdt_abi_test.c $(CORE_SRC)
	$(CXX) -std=c++17 -pthread -I core -o /tmp/batch_test core/batch_test.cc core/batch.cc && /tmp/batch_test
	$(CXX) -std=c++17 -I core -o /tmp/clock_test core/clock_test.cc core/clock.cc && /tmp/clock_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/drain_test core/drain_test.cc core/drain.cc && /tmp/drain_test
	$(CC) -std=c11 -Wall -Werror -I core -o /tmp/usdt_abi_test core/usdt_abi_test.c && /tmp/usdt_abi_test

# Not a prerequisite of `test`: a TSan build is ~10x slower and `test` is the
# fast path. batch_test.cc's concurrent case asserts emitted + dropped == kAdds,
# which passes on most runs even with Batch's mutex deleted -- only the
# sanitizer actually proves the absence of the race.
test-tsan: core/batch_test.cc core/batch.cc core/batch.h
	$(TSAN_CXX) -std=c++17 -g -O1 -fsanitize=thread -pthread -I core \
		-o /tmp/batch_test_tsan core/batch_test.cc core/batch.cc && /tmp/batch_test_tsan

clean:
	rm -f $(CORE_OBJ) libperfagent-gpu-core.a perfagent-gpu-stub libperfagent-gpu-stub.so
