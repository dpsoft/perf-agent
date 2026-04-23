SHELL := /bin/bash
all: build
.PHONY: all

LIBBLAZESYM_SRC := $(abspath /home/diego/github/blazesym/)
LIBBLAZESYM_INC := $(abspath $(LIBBLAZESYM_SRC)/capi/include)
LIBBLAZESYM_OBJ := $(abspath $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a)
ALL_LDFLAGS := $(LDFLAGS) $(EXTRA_LDFLAGS)

build: $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a
	CGO_LDFLAGS=" -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release) -lblazesym_c -static " CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release)" go build .

# Diagnostic CLI that loads perf_dwarf.bpf.c and prints ringbuf samples.
# Static-linked so that capped binaries (which have LD_LIBRARY_PATH stripped
# for security) still work. Output to /home — /tmp is nosuid on Fedora so
# file capabilities are ignored there at exec time.
.PHONY: perf-dwarf-test
perf-dwarf-test: $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a
	CGO_LDFLAGS=" -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release) -lblazesym_c -static " CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release)" go build -o $(HOME)/bin/perf-dwarf-test ./cmd/perf-dwarf-test
	@echo ""
	@echo "Built $(HOME)/bin/perf-dwarf-test. To run without sudo, grant caps:"
	@echo "  sudo setcap cap_sys_admin,cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep $(HOME)/bin/perf-dwarf-test"

.PHONY: generate
generate:
	go generate ./...

.PHONY: test-workloads
test-workloads:
	cd test/workloads/go && go build -o cpu_bound cpu_bound.go
	cd test/workloads/go && go build -o io_bound io_bound.go
	@if command -v cargo >/dev/null 2>&1; then \
		cd test/workloads/rust && cargo build --release; \
	else \
		echo "Rust/Cargo not found, skipping Rust workload"; \
	fi
	chmod +x test/workloads/python/*.py

.PHONY: test-unit
test-unit: generate
	LD_LIBRARY_PATH="$(abspath $(LIBBLAZESYM_SRC)/target/release):$$LD_LIBRARY_PATH" \
	CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC)" \
	CGO_LDFLAGS="-L$(abspath $(LIBBLAZESYM_SRC)/target/release) -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
	go test -v ./cpu/... ./profile/... ./offcpu/... ./unwind/...

.PHONY: test-integration
test-integration: build test-workloads
	cd test && LD_LIBRARY_PATH="$(abspath $(LIBBLAZESYM_SRC)/target/release):$$LD_LIBRARY_PATH" \
		CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I$(LIBBLAZESYM_INC)" \
		CGO_LDFLAGS="-L$(abspath $(LIBBLAZESYM_SRC)/target/release) -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
		bash run_tests.sh

.PHONY: test
test: test-unit test-integration

.PHONY: clean
clean:
	rm -f perf-agent
	rm -f profile.pb.gz offcpu.pb.gz
	rm -f test/workloads/go/cpu_bound test/workloads/go/io_bound
	rm -rf test/workloads/rust/target
	rm -f /tmp/perf-agent-test-*.dat
