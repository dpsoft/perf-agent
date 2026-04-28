SHELL := /bin/bash
all: build
.PHONY: all

# Allow Go to fetch the toolchain pinned in go.mod (1.26+) instead of failing
# when the system Go is older. Override with `GOTOOLCHAIN=local make build`
# to enforce the locally-installed toolchain.
export GOTOOLCHAIN ?= auto

LIBBLAZESYM_SRC := $(abspath /home/diego/github/blazesym/)
LIBBLAZESYM_INC := $(abspath $(LIBBLAZESYM_SRC)/capi/include)
LIBBLAZESYM_OBJ := $(abspath $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a)
ALL_LDFLAGS := $(LDFLAGS) $(EXTRA_LDFLAGS)

build: $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a
	CGO_LDFLAGS=" -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release) -lblazesym_c -static " CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release)" go build .

.PHONY: generate
generate:
	go generate ./...

.PHONY: test-workloads
test-workloads:
	cd test/workloads/go && go build -o cpu_bound cpu_bound.go
	cd test/workloads/go && go build -o io_bound io_bound.go
	@if command -v cargo >/dev/null 2>&1; then \
		(cd test/workloads/rust && cargo build --release); \
		(cd test/workloads/rust/probe && cargo build --release); \
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

.PHONY: bench-corpus bench-build bench-scenarios

bench-corpus:
	GOTOOLCHAIN=auto go test -bench=. -benchmem -run=^$$ ./unwind/ehcompile/...

bench-build:
	CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC)" \
		CGO_LDFLAGS="-L$(abspath $(LIBBLAZESYM_SRC)/target/release) -Wl,-rpath,$(abspath $(LIBBLAZESYM_SRC)/target/release) -lblazesym_c" \
		GOTOOLCHAIN=auto go build -o bench/cmd/scenario/scenario ./bench/cmd/scenario
	GOTOOLCHAIN=auto go build -o bench/cmd/report/report ./bench/cmd/report

bench-scenarios: bench-build test-workloads
	@if ! getcap ./bench/cmd/scenario/scenario | grep -q cap_perfmon; then \
		echo "*** scenario binary missing caps; run: sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario"; \
		exit 1; \
	fi
	./bench/cmd/scenario/scenario --scenario pid-large --runs 5 --out bench-pid-large.json
	./bench/cmd/scenario/scenario --scenario system-wide-mixed --processes 30 --runs 5 --out bench-system-wide-mixed.json
	./bench/cmd/report/report --in bench-pid-large.json bench-system-wide-mixed.json > bench-report.md
	@echo "report written to bench-report.md"
