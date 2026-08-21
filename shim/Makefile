# Targets:
#   all         libperfagent-gpu-core.a
#   test        fast unit tests: batch, clock, drain, and the C11 ABI header
#   test-tsan   the concurrency case under ThreadSanitizer (clang++ only)
#   nvidia      libperfagent-gpu-nvidia.so, the CUPTI adapter (needs the CUDA toolkit)
#   nvidia-workload  the CUDA test workload the adapter is proven against
#   clean
CXX ?= c++
CC ?= cc
# GCC's TSan runtime is not installed on the reference machine; clang++ is the
# one that links -fsanitize=thread here.
TSAN_CXX ?= clang++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra -fPIC -fvisibility=hidden -I core

# The CUDA toolkit is not a build dependency of perf-agent: only the `nvidia`
# targets need it, and they are not reachable from `all` or `test`.
CUDA_HOME ?= /usr/local/cuda-13.3
CUDA_INC := $(CUDA_HOME)/targets/x86_64-linux/include
CUDA_LIB := $(CUDA_HOME)/targets/x86_64-linux/lib
NVCC ?= $(CUDA_HOME)/bin/nvcc
# sm_86 is the RTX 3090 this adapter was proven on.
CUDA_ARCH ?= sm_86

CORE_SRC := core/batch.cc core/clock.cc core/drain.cc core/sampler.cc
CORE_OBJ := $(CORE_SRC:.cc=.o)

.PHONY: all test test-tsan nvidia nvidia-workload clean
all: libperfagent-gpu-core.a

libperfagent-gpu-core.a: $(CORE_OBJ)
	ar rcs $@ $^

%.o: %.cc
	$(CXX) $(CXXFLAGS) -c $< -o $@

perfagent-gpu-stub: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -o $@ stub/stub.cc $(CORE_SRC)

libperfagent-gpu-stub.so: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -shared -o $@ stub/stub.cc $(CORE_SRC)

# The CUPTI adapter. The CUDA driver dlopens this through CUDA_INJECTION64_PATH
# into a process that links no CUPTI of its own, so the rpath is load-bearing:
# libcupti.so is not on the default loader path and the injected process has no
# reason to have it there.
#
# -fvisibility=hidden comes from CXXFLAGS and is what keeps core/ from leaking
# symbols into somebody else's address space; InitializeInjection carries an
# explicit default-visibility attribute to survive it (see cupti_adapter.cc).
nvidia: libperfagent-gpu-nvidia.so
libperfagent-gpu-nvidia.so: nvidia/cupti_adapter.cc nvidia/export.map $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -I $(CUDA_INC) -shared -o $@ \
		nvidia/cupti_adapter.cc $(CORE_SRC) \
		-Wl,--version-script=nvidia/export.map \
		-L $(CUDA_LIB) -lcupti -Wl,-rpath,$(CUDA_LIB)

# Not stripped, and built with -g: a sampled launch's CPU stack is symbolized
# against this binary's own symbols, so a stripped workload would prove the
# pipeline runs and name nothing in it.
nvidia-workload: nvidia/testdata/cuda_workload
nvidia/testdata/cuda_workload: nvidia/testdata/cuda_workload.cu
	$(NVCC) -O2 -g -lineinfo -arch=$(CUDA_ARCH) \
		-Xcompiler -fno-omit-frame-pointer -Xcompiler -rdynamic -o $@ $<

# core/probe_args_test.cc is the one test here built with -O2, and that is not
# a detail: it checks that the record a probe points at has actually been
# written to memory when the probe fires, which only an optimizer can get
# wrong. At -O0 it cannot fail, so the flag is part of the test.
#
# core/usdt_abi_test.c is built with a C compiler on purpose: usdt_abi.h is the
# ABI both the C++ shim and any C vendor adapter include, and its
# _Static_asserts are the test. Compiling it only as C++ leaves the C11 spelling
# unguarded -- which is how the header's validity broke earlier in this branch,
# when a C++ translation unit included it for the first time.
test: core/batch_test.cc core/clock_test.cc core/drain_test.cc core/usdt_abi_test.c core/sampler_test.cc core/probe_args_test.cc $(CORE_SRC)
	$(CXX) -std=c++17 -pthread -I core -o /tmp/batch_test core/batch_test.cc core/batch.cc && /tmp/batch_test
	$(CXX) -std=c++17 -I core -o /tmp/clock_test core/clock_test.cc core/clock.cc && /tmp/clock_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/drain_test core/drain_test.cc core/drain.cc && /tmp/drain_test
	$(CC) -std=c11 -Wall -Werror -I core -o /tmp/usdt_abi_test core/usdt_abi_test.c && /tmp/usdt_abi_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/sampler_test core/sampler_test.cc core/sampler.cc && /tmp/sampler_test
	$(CXX) -std=c++17 -O2 -Wall -Werror -I core -o /tmp/probe_args_test core/probe_args_test.cc && /tmp/probe_args_test

# Not a prerequisite of `test`: a TSan build is ~10x slower and `test` is the
# fast path. batch_test.cc's concurrent case asserts emitted + dropped == kAdds,
# which passes on most runs even with Batch's mutex deleted -- only the
# sanitizer actually proves the absence of the race. sampler_test.cc's
# concurrent case is the same shape, for Sampler's atomics.
test-tsan: core/batch_test.cc core/batch.cc core/batch.h core/sampler_test.cc core/sampler.cc core/sampler.h
	$(TSAN_CXX) -std=c++17 -g -O1 -fsanitize=thread -pthread -I core \
		-o /tmp/batch_test_tsan core/batch_test.cc core/batch.cc && /tmp/batch_test_tsan
	$(TSAN_CXX) -std=c++17 -g -O1 -fsanitize=thread -pthread -I core \
		-o /tmp/sampler_test_tsan core/sampler_test.cc core/sampler.cc && /tmp/sampler_test_tsan

clean:
	rm -f $(CORE_OBJ) libperfagent-gpu-core.a perfagent-gpu-stub libperfagent-gpu-stub.so \
		libperfagent-gpu-nvidia.so nvidia/testdata/cuda_workload
