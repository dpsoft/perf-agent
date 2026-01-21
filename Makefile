SHELL := /bin/bash
all: build
.PHONY: all

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
		cd test/workloads/rust && cargo build --release; \
	else \
		echo "Rust/Cargo not found, skipping Rust workload"; \
	fi
	chmod +x test/workloads/python/*.py

.PHONY: test-unit
test-unit: generate
	go test -v ./cpu/... ./profile/... ./offcpu/...

.PHONY: test-integration
test-integration: build test-workloads
	cd test && CGO_CFLAGS="-I$(LIBBLAZESYM_INC)" CGO_LDFLAGS="-L$(abspath $(LIBBLAZESYM_SRC)/target/release)" bash run_tests.sh

.PHONY: test
test: test-unit test-integration

.PHONY: clean
clean:
	rm -f perf-agent
	rm -f profile.pb.gz offcpu.pb.gz
	rm -f test/workloads/go/cpu_bound test/workloads/go/io_bound
	rm -rf test/workloads/rust/target
	rm -f /tmp/perf-agent-test-*.dat
