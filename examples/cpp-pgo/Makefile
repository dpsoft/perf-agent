# `?=` doesn't help here: GNU Make's built-in CXX default is g++, and
# `?=` only assigns when undefined — built-ins count as defined. Using
# `=` overrides that while still allowing `make CXX=...` on the command
# line. clang is required: create_llvm_prof's AutoFDO output uses
# LLVM's sample-profile format, consumed only by clang's
# -fprofile-sample-use. (gcc would need create_gcov + -fauto-profile.)
CXX      = clang++
CXXFLAGS ?= -O2 -g -std=c++17

.PHONY: baseline pgo strip clean
baseline:
	$(CXX) $(CXXFLAGS) workload.cpp -o workload-baseline

pgo:
	$(CXX) $(CXXFLAGS) -fprofile-sample-use=$(PWD)/train.prof workload.cpp -o workload-pgo

strip:
	strip --strip-all workload-pgo

clean:
	rm -f workload-baseline workload-pgo train.perf.data train.prof baseline.json optimized.json
