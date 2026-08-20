CXX ?= c++
CXXFLAGS ?= -std=c++17 -O2 -Wall -Wextra -fPIC -fvisibility=hidden -I core

CORE_SRC := core/batch.cc core/clock.cc core/drain.cc
CORE_OBJ := $(CORE_SRC:.cc=.o)

.PHONY: all test clean
all: libperfagent-gpu-core.a

libperfagent-gpu-core.a: $(CORE_OBJ)
	ar rcs $@ $^

%.o: %.cc
	$(CXX) $(CXXFLAGS) -c $< -o $@

perfagent-gpu-stub: stub/stub.cc $(CORE_SRC)
	$(CXX) $(CXXFLAGS) -pthread -I core -o $@ stub/stub.cc $(CORE_SRC)

test: core/batch_test.cc core/clock_test.cc core/drain_test.cc $(CORE_SRC)
	$(CXX) -std=c++17 -I core -o /tmp/batch_test core/batch_test.cc core/batch.cc && /tmp/batch_test
	$(CXX) -std=c++17 -I core -o /tmp/clock_test core/clock_test.cc core/clock.cc && /tmp/clock_test
	$(CXX) -std=c++17 -pthread -I core -o /tmp/drain_test core/drain_test.cc core/drain.cc && /tmp/drain_test

clean:
	rm -f $(CORE_OBJ) libperfagent-gpu-core.a perfagent-gpu-stub
