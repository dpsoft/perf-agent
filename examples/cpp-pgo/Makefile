CXX      ?= clang++
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
