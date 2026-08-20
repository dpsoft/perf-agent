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

test: core/batch_test.cc core/clock_test.cc $(CORE_SRC)
	$(CXX) -std=c++17 -I core -o /tmp/batch_test core/batch_test.cc core/batch.cc && /tmp/batch_test
	$(CXX) -std=c++17 -I core -o /tmp/clock_test core/clock_test.cc core/clock.cc && /tmp/clock_test

clean:
	rm -f $(CORE_OBJ) libperfagent-gpu-core.a
