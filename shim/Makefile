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

CORE_SRC := core/batch.cc core/clock.cc core/cubin.cc core/cubinqueue.cc core/drain.cc core/enroll.cc core/sampler.cc
CORE_OBJ := $(CORE_SRC:.cc=.o)

.PHONY: all test test-tsan check-cubin-defer nvidia nvidia-workload clean
all: libperfagent-gpu-core.a

libperfagent-gpu-core.a: $(CORE_OBJ)
	ar rcs $@ $^

%.o: %.cc
	$(CXX) $(CXXFLAGS) -c $< -o $@

perfagent-gpu-stub: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -o $@ stub/stub.cc $(CORE_SRC)

libperfagent-gpu-stub.so: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -shared -o $@ stub/stub.cc $(CORE_SRC)

# The frame-pointer-less producer: the same records as perfagent-gpu-stub,
# reached through two frames with no FP link, so the consumer's walk has to
# cross FP-less frames exactly as it does in a CUDA process. gate_test.go
# drives this, not perfagent-gpu-stub, because the plain stub's stack is one
# a frame-pointer walk can follow -- a gate driven by it passes whether or not
# the DWARF walker works.
#
# The per-file flags ARE the test, not tidiness:
#   stub/fpless_bridge.cc   -fomit-frame-pointer      the FP-less frames
#   stub/fpless_main.cc     -fno-omit-frame-pointer   the frame the walk must
#                                                     reach across them
#   stub/stub.cc            -fno-omit-frame-pointer   the frame the probe
#                                                     fires in; its FP link is
#                                                     the walk's first step
# -fasynchronous-unwind-tables is explicit rather than left to the x86-64
# psABI default: .eh_frame is the ONLY way the walker can cross an FP-less
# frame, so a toolchain that quietly dropped it would turn this producer into
# one that proves nothing. -g is for the gate's by-name assertions.
#
# `make check-fpless` proves the flags took effect instead of assuming it.
perfagent-gpu-fpless: stub/fpless_bridge.cc stub/fpless_main.cc stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -g -pthread -I core -fomit-frame-pointer \
		-fasynchronous-unwind-tables -c stub/fpless_bridge.cc -o stub/fpless_bridge.o
	$(CXX) $(CXXFLAGS) -g -pthread -I core -fno-omit-frame-pointer \
		-fasynchronous-unwind-tables -c stub/fpless_main.cc -o stub/fpless_main.o
	$(CXX) $(CXXFLAGS) -g -pthread -I core -fno-omit-frame-pointer \
		-fasynchronous-unwind-tables -DPERFAGENT_STUB_NO_MAIN \
		-c stub/stub.cc -o stub/fpless_stub.o
	$(CXX) $(CXXFLAGS) -g -pthread -fno-omit-frame-pointer -o $@ \
		stub/fpless_bridge.o stub/fpless_main.o stub/fpless_stub.o $(CORE_SRC)

# Fails the BUILD rather than the gate if the toolchain did not do what the
# flags asked. Three separate things are checked, because three separate
# things would each silently turn the gate green while proving nothing:
#
#   1. The two bridge functions must not open with `push %rbp; mov %rsp,%rbp`.
#      Fedora ships -fno-omit-frame-pointer in its RPM build flags, so a
#      toolchain that re-enables frame pointers is a real failure mode here,
#      not a theoretical one. With frame pointers they are followable by the
#      walker this phase replaces.
#   2. They must not WRITE %rbp at all. -fomit-frame-pointer frees rbp as a
#      general-purpose register; if either function used it, perfagent_stub_run
#      would save that garbage in its prologue and the walk would die at its
#      first FP step, never reaching an FP-less frame and never taking the
#      DWARF path.
#   3. perfagent_stub_run must KEEP its frame pointer, since the walk's first
#      step is an FP step out of its frame.
.PHONY: check-fpless
check-fpless: perfagent-gpu-fpless libperfagent-gpu-fpless.so
	@rc=0; for img in perfagent-gpu-fpless libperfagent-gpu-fpless.so; do \
	 echo "== $$img"; \
	 for fn in perfagent_fpless_bridge perfagent_fpless_caller; do \
	  objdump -d --no-show-raw-insn $$img \
	    | awk -v f="<$$fn>:" 'index($$0,f){g=1;next} g&&/^$$/{exit} g' > /tmp/fpless_$$fn.asm; \
	  if [ ! -s /tmp/fpless_$$fn.asm ]; then echo "$$img: $$fn: not found in the binary"; rc=1; continue; fi; \
	  if grep -q 'mov  *%rsp,%rbp' /tmp/fpless_$$fn.asm; then \
	    echo "$$fn: establishes a frame pointer; -fomit-frame-pointer did not take"; rc=1; fi; \
	  if grep -qE ',%rbp|%rbp,|pop  *%rbp|push  *%rbp' /tmp/fpless_$$fn.asm; then \
	    echo "$$fn: touches %rbp; the FP chain into perfagent_stub_run would be poisoned"; \
	    grep -nE ',%rbp|%rbp,|%rbp' /tmp/fpless_$$fn.asm; rc=1; fi; \
	  echo "$$img: $$fn: no frame pointer, %rbp untouched"; \
	 done; \
	 objdump -d --no-show-raw-insn $$img \
	   | awk 'index($$0,"<perfagent_stub_run>:"){g=1;next} g&&/^$$/{exit} g' | head -4 > /tmp/fpless_stubrun.asm; \
	 grep -q 'mov  *%rsp,%rbp' /tmp/fpless_stubrun.asm || { \
	   echo "$$img: perfagent_stub_run: no frame pointer; the walk's first FP step would fail"; rc=1; }; \
	 readelf -S $$img | grep -q '\.eh_frame' || { \
	   echo "$$img: no .eh_frame: the DWARF walker has nothing to read"; rc=1; }; \
	done; \
	[ $$rc -eq 0 ] && echo "check-fpless: OK"; exit $$rc

# The CUDA shape without CUDA: a host that DLOPENS the producer .so instead of
# being it. stub/dlopen_host.cc explains why the gate needs this as well as
# perfagent-gpu-fpless -- an exec'd producer cannot exercise the kernel path on
# which issue #49's first fix silently did nothing.
#
# -fno-omit-frame-pointer and -g on both halves: the walk has to cross from the
# producer .so into the host executable for the #39 attribution guard to pass,
# which is the same boundary the CUDA run crosses and the plain stub has no
# equivalent of.
# The producer .so the dlopen host loads, with FP-LESS frames between the probe
# and the host's main -- the CUDA shape, and the reason this is a separate .so
# from libperfagent-gpu-stub.so.
#
# The first version of the dlopen gate loaded libperfagent-gpu-stub.so, which is
# built with frame pointers throughout, and reported dwarf=0 fp-only=63 with
# no-tables=0: the tables were installed and NO walk ever needed them. That is
# legitimate for that producer -- walk_step only sets WALKER_FLAG_DWARF_USED for
# a frame it classified FP_LESS -- but it means the gate proved the rendezvous
# fired and proved nothing about the DWARF unwinding the rendezvous exists to
# feed. A real CUDA stack crosses libcupti and libcudart, which have no frame
# pointers; this .so reproduces that with stub/fpless_bridge.cc.
#
#   host main                 frame pointer      <- the walk must reach here
#   perfagent_fpless_caller   NO frame pointer      DWARF only
#   perfagent_fpless_bridge   NO frame pointer      DWARF only
#   perfagent_stub_run        frame pointer      <- the probe fires here
#
# check-fpless proves the toolchain actually omitted those frame pointers, for
# this .so as well as for the executable.
libperfagent-gpu-fpless.so: stub/fpless_bridge.cc stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -g -pthread -I core -fomit-frame-pointer \
		-fasynchronous-unwind-tables -c stub/fpless_bridge.cc -o stub/so_fpless_bridge.o
	$(CXX) $(CXXFLAGS) -g -pthread -I core -fno-omit-frame-pointer \
		-fasynchronous-unwind-tables -DPERFAGENT_STUB_NO_MAIN \
		-c stub/stub.cc -o stub/so_fpless_stub.o
	$(CXX) $(CXXFLAGS) -g -pthread -fno-omit-frame-pointer -shared -o $@ \
		stub/so_fpless_bridge.o stub/so_fpless_stub.o $(CORE_SRC)

perfagent-gpu-dlopen-host: stub/dlopen_host.cc
	$(CXX) $(CXXFLAGS) -g -fno-omit-frame-pointer -fasynchronous-unwind-tables \
		-o $@ stub/dlopen_host.cc -ldl

libperfagent-gpu-stub.so: CXXFLAGS += -g -fno-omit-frame-pointer -fasynchronous-unwind-tables

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

# Task 5's structural assertion, run as a BUILD step rather than asserted in a
# comment: the copy of a cubin must happen inside the vendor callback, so no
# deferral may sit between callback entry and the memcpy. core/cubinqueue.h
# enforces that by shape - CubinView has no copy, no move, no assignment, no
# operator new and NO ACCESSOR FOR ITS POINTER - and this target is the proof
# that the enforcement works, by trying to violate it five ways and requiring
# every one to be rejected by the compiler.
#
# Mode 0 is the control and it MUST compile. Without it the whole check would
# pass just as happily on a file that fails to build for an unrelated reason -
# a typo, a renamed header - and would then be proving nothing, which is the
# exact shape of the eleven counters this project has shipped reading green at
# the worst possible moment.
#
# 2>/dev/null on the negative arms: the compiler errors ARE the pass, and
# printing five screens of them would bury the one line that matters.
.PHONY: check-cubin-defer
check-cubin-defer: core/cubin_defer_test.cc core/cubinqueue.h
	@$(CXX) -std=c++17 -Wall -Werror -I core -fsyntax-only \
		-DPERFAGENT_CUBIN_DEFER=0 core/cubin_defer_test.cc || { \
	  echo "check-cubin-defer: the COMPLIANT capture does not compile; this check would"; \
	  echo "                   otherwise pass for the wrong reason. Fix core/cubinqueue.h."; \
	  exit 1; }
	@rc=0; for m in 1 2 3 4 5; do \
	  if $(CXX) -std=c++17 -Wall -Werror -I core -fsyntax-only \
		-DPERFAGENT_CUBIN_DEFER=$$m core/cubin_defer_test.cc 2>/dev/null; then \
	    echo "check-cubin-defer: deferral mode $$m COMPILED. The vendor's pointer can escape"; \
	    echo "                   the callback, so the copy can be deferred past the buffer's"; \
	    echo "                   lifetime and a late reader gets silently WRONG bytes (spec"; \
	    echo "                   6.3 finding 2). See core/cubin_defer_test.cc mode $$m."; \
	    rc=1; \
	  fi; \
	done; \
	[ $$rc -eq 0 ] && echo "check-cubin-defer: OK - the compliant capture compiles, all 5 deferrals do not"; \
	exit $$rc

# stub/pc_tier_test.cc is the "off means off" assertion, and it is built the
# same way probe_order_test is and for the same reason: it drives the real
# producer (stub/stub.cc with PERFAGENT_STUB_NO_MAIN) and reads its probe fires
# by patching the nops with int3, so it has to be the optimized build the gate
# runs. It asserts NEGATIVELY -- with the tier off, not one of the four
# PC-sampling probes may fire -- with the launch and exec probes trapped as
# controls so an inert producer cannot make it pass.
#
# core/probe_args_test.cc and stub/probe_order_test.cc are the two tests here
# built with -O2, and that is not a detail. probe_args_test checks that the
# record a probe points at has actually been written to memory when the probe
# fires, which only an optimizer can get wrong; at -O0 it cannot fail, so the
# flag is part of the test. probe_order_test drives the real producer
# (stub/stub.cc, with PERFAGENT_STUB_NO_MAIN) and reads the wire ORDER of its
# probe fires by patching their nops with int3 - so it has to be the same
# optimized build the gate runs, not a debug one that the compiler laid out
# differently.
#
# core/usdt_abi_test.c is built with a C compiler on purpose: usdt_abi.h is the
# ABI both the C++ shim and any C vendor adapter include, and its
# _Static_asserts are the test. Compiling it only as C++ leaves the C11 spelling
# unguarded -- which is how the header's validity broke earlier in this branch,
# when a C++ translation unit included it for the first time.
test: check-cubin-defer core/batch_test.cc core/clock_test.cc core/cubin_test.cc core/cubinqueue_test.cc core/drain_test.cc core/pcdrain_test.cc core/burst_test.cc core/pctier_test.cc core/enroll_test.cc core/usdt_abi_test.c core/sampler_test.cc core/probe_args_test.cc stub/probe_order_test.cc stub/pc_tier_test.cc stub/stub.cc $(CORE_SRC)
	$(CXX) -std=c++17 -pthread -I core -o /tmp/batch_test core/batch_test.cc core/batch.cc && /tmp/batch_test
	$(CXX) -std=c++17 -I core -o /tmp/clock_test core/clock_test.cc core/clock.cc && /tmp/clock_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/drain_test core/drain_test.cc core/drain.cc && /tmp/drain_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/pcdrain_test core/pcdrain_test.cc && /tmp/pcdrain_test
	$(CXX) -std=c++17 -Wall -Werror -pthread -I core -o /tmp/burst_test core/burst_test.cc && /tmp/burst_test
	$(CXX) -std=c++17 -Wall -Werror -I core -o /tmp/pctier_test core/pctier_test.cc && /tmp/pctier_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/enroll_test core/enroll_test.cc core/enroll.cc && /tmp/enroll_test
	$(CXX) -std=c++17 -Wall -Werror -pthread -I core -o /tmp/cubin_test core/cubin_test.cc core/cubin.cc core/enroll.cc && /tmp/cubin_test
	$(CXX) -std=c++17 -Wall -Werror -pthread -I core -o /tmp/cubinqueue_test core/cubinqueue_test.cc core/cubinqueue.cc && /tmp/cubinqueue_test
	$(CC) -std=c11 -Wall -Werror -I core -o /tmp/usdt_abi_test core/usdt_abi_test.c && /tmp/usdt_abi_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/sampler_test core/sampler_test.cc core/sampler.cc && /tmp/sampler_test
	$(CXX) -std=c++17 -O2 -Wall -Werror -I core -o /tmp/probe_args_test core/probe_args_test.cc && /tmp/probe_args_test
	$(CXX) -std=c++17 -O2 -Wall -Werror -pthread -I core -DPERFAGENT_STUB_NO_MAIN \
		-o /tmp/probe_order_test stub/probe_order_test.cc stub/stub.cc $(CORE_SRC) && /tmp/probe_order_test
	$(CXX) -std=c++17 -O2 -Wall -Werror -pthread -I core -DPERFAGENT_STUB_NO_MAIN \
		-o /tmp/pc_tier_test stub/pc_tier_test.cc stub/stub.cc $(CORE_SRC) && /tmp/pc_tier_test

# Not a prerequisite of `test`: a TSan build is ~10x slower and `test` is the
# fast path. batch_test.cc's concurrent case asserts emitted + dropped == kAdds,
# which passes on most runs even with Batch's mutex deleted -- only the
# sanitizer actually proves the absence of the race. sampler_test.cc's
# concurrent case is the same shape, for Sampler's atomics.
#
# cubinqueue_test.cc belongs here for a third reason: its capture path runs on
# the APPLICATION's thread while drain() runs on the profiler's, and the whole
# design turns on drain() not holding the mutex across an offer. A test that
# only checks the counters passes with the lock held the wrong way round; only
# TSan sees the unsynchronized access if one is ever introduced.
test-tsan: core/batch_test.cc core/batch.cc core/batch.h core/sampler_test.cc core/sampler.cc core/sampler.h core/cubinqueue_test.cc core/cubinqueue.cc core/cubinqueue.h
	$(TSAN_CXX) -std=c++17 -g -O1 -fsanitize=thread -pthread -I core \
		-o /tmp/batch_test_tsan core/batch_test.cc core/batch.cc && /tmp/batch_test_tsan
	$(TSAN_CXX) -std=c++17 -g -O1 -fsanitize=thread -pthread -I core \
		-o /tmp/sampler_test_tsan core/sampler_test.cc core/sampler.cc && /tmp/sampler_test_tsan
	$(TSAN_CXX) -std=c++17 -g -O1 -fsanitize=thread -pthread -I core \
		-o /tmp/cubinqueue_test_tsan core/cubinqueue_test.cc core/cubinqueue.cc && /tmp/cubinqueue_test_tsan

clean:
	rm -f $(CORE_OBJ) libperfagent-gpu-core.a perfagent-gpu-stub libperfagent-gpu-stub.so \
		perfagent-gpu-dlopen-host libperfagent-gpu-fpless.so \
		stub/so_fpless_bridge.o stub/so_fpless_stub.o \
		perfagent-gpu-fpless stub/fpless_bridge.o stub/fpless_main.o stub/fpless_stub.o \
		libperfagent-gpu-nvidia.so nvidia/testdata/cuda_workload
