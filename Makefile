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

.PHONY: blazesym-check
blazesym-check:
	@if ! grep -q 'process_dispatch' $(LIBBLAZESYM_INC)/blazesym.h; then \
		echo "*** blazesym header at $(LIBBLAZESYM_INC)/blazesym.h is too old"; \
		echo "*** missing process_dispatch — pull blazesym to a commit ≥ 8891e70"; \
		exit 1; \
	fi
	@if ! grep -q 'blaze_symbolize_kernel_abs_addrs' $(LIBBLAZESYM_INC)/blazesym.h; then \
		echo "*** blazesym header at $(LIBBLAZESYM_INC)/blazesym.h is too old"; \
		echo "*** missing blaze_symbolize_kernel_abs_addrs (kernel-mode + module DWARF symbolization)"; \
		echo "*** pull blazesym to a commit ≥ 29a609f"; \
		exit 1; \
	fi

build: blazesym-check $(LIBBLAZESYM_SRC)/target/release/libblazesym_c.a
	CGO_LDFLAGS=" -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release) -lblazesym_c -static " CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I $(LIBBLAZESYM_INC) -L /usr/lib -L $(abspath $(LIBBLAZESYM_SRC)/target/release)" go build .

.PHONY: generate
generate:
	go generate ./...

# The committed bpf2go outputs. Scoped deliberately: the check must fire on a
# stale generated object and stay silent on whatever else is dirty in a
# contributor's tree, or it becomes the kind of check people learn to ignore.
GENERATED_GLOBS := '*_bpfel.go' '*_bpfel.o'

# Regeneration must not change the committed objects. Issue #87.
#
# The .o files are committed build artifacts, so their bytes depend on the
# exact clang/LLVM that produced them. Running `make generate` on a different
# toolchain rewrites packages you did not touch, and the only ways out are to
# commit the noise or revert it by hand — which only works if you notice.
#
# This is the same check CI runs. A failure names the files and prints their
# sizes: same size with differing bytes is usually BTF type-ordering rather
# than a real code change, which is the distinction that decides whether the
# diff is worth committing or the toolchain is worth pinning.
.PHONY: generate-check
generate-check: generate
	@if git diff --quiet -- $(GENERATED_GLOBS); then \
		echo "✓ generated objects match the committed ones"; \
	else \
		echo "*** go generate changed committed files:"; \
		git diff --stat -- $(GENERATED_GLOBS); \
		git diff --name-only -- $(GENERATED_GLOBS) | while read -r f; do \
			printf '    %s committed=%s regenerated=%s\n' "$$f" \
				"$$(git show HEAD:"$$f" | wc -c)" "$$(wc -c < "$$f")"; \
		done; \
		echo "*** clang: $$(clang --version | head -1)"; \
		exit 1; \
	fi

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
# Deliberately NOT depending on `generate`. Issue #87.
#
# It used to, which meant running the unit tests silently rewrote the committed
# eBPF objects with whatever clang happened to be installed — so drift entered
# the tree without anyone running `make generate` on purpose, and the person
# who introduced it had no reason to look. Tests now run against the committed
# objects, which is what `go build` and every user get. `make generate-check`
# is what says those objects are current.
test-unit:
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

# Self-profile scenario: a second perf-agent profiles the first while
# it captures a CPU workload. Catches perf-agent overhead regressions
# AND the v1.2.0-class lockdown bug at PR time (the kernel-resolution
# canary would have surfaced empty kernel-side flames in CI).
# Defaults:
#   --self-duration 10s  --runs 3
#   --cpu-budget 0.10        (perf-agent ≤ 10% of workload CPU)
#   --resolution-budget 0.50 (≥ 50% of kernel locations named)
# Override on the command line if needed.
.PHONY: bench-self
# Note: depends only on bench-build + test-workloads. The perf-agent
# binary is expected to be built AND setcap'd manually beforehand —
# adding `build` as a dep would wipe the file caps every invocation.
bench-self: bench-build test-workloads
	@# The scenario binary doesn't need caps for "self" — it's pure
	@# orchestration. perf-agent subprocesses each carry their own
	@# file caps and need to be capped explicitly.
	@if ! getcap ./perf-agent | grep -q cap_perfmon; then \
		echo "*** perf-agent binary missing caps; run: sudo setcap cap_perfmon,cap_bpf,cap_sys_admin,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent"; \
		exit 1; \
	fi
	@# Budget gates calibrated to catch regressions, not enforce a
	@# specific overhead target on this codebase as-of-today:
	@#   --cpu-budget 1.5 means agent CPU <= 150% of workload CPU.
	@#   --resolution-budget 0.5 means >=50% of kernel locations named.
	@# Tighten as perf-agent gets leaner; loosen on slow CI runners.
	./bench/cmd/scenario/scenario --scenario self --runs 3 --self-duration 10s \
		--cpu-budget 1.5 --resolution-budget 0.5 \
		--out bench-self.json
	@echo "self-profile bench written to bench-self.json"

# GPU PC-sampling overhead (plan Task 12): the marginal cost of Tier B and of
# Tier A at three duty fractions, against the shipping Phase 4 configuration
# with PC sampling off. Needs an NVIDIA GPU, the CUPTI adapter and the
# concurrent CUDA workload; reports BENCH_SKIPPED and exits 0 without any of
# them.
#
# The capability set is gpuprobe's own and is SMALLER than the one
# bench-scenarios needs. cap_sys_admin is deliberately not in it.
#
# Exit codes: 0 when the measurement completed (whatever the verdict — an
# honest TIER_A_UNSHIPPABLE is a successful run of this benchmark), 3 when an
# arm could not prove it ran in the mode it claims, in which case the numbers
# are not a tier decision and must not be recorded as one.
.PHONY: bench-gpu-pc-overhead
bench-gpu-pc-overhead: bench-build
	@$(MAKE) -C shim nvidia nvidia-concurrent
	@if ! getcap ./bench/cmd/scenario/scenario | grep -q cap_bpf; then \
		echo "*** scenario binary missing caps; run: sudo setcap cap_bpf,cap_perfmon,cap_checkpoint_restore+ep ./bench/cmd/scenario/scenario"; \
		exit 1; \
	fi
	./bench/cmd/scenario/scenario --scenario gpu-pc-overhead --runs 5 \
		--out bench-gpu-pc-overhead.json
	@echo "gpu pc-sampling overhead written to bench-gpu-pc-overhead.json"
