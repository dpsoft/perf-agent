# perf-agent Test Suite

Comprehensive test suite for perf-agent including unit tests, integration tests, and multi-language test workloads.

## Test Structure

```
test/
├── workloads/           # Test workload applications
│   ├── go/             # Go CPU and I/O bound workloads
│   ├── rust/           # Rust CPU bound workload
│   └── python/         # Python CPU and I/O bound workloads
├── integration_test.go  # Integration tests
└── run_tests.sh        # Test runner script
```

## Running Tests

### All Tests (Unit + Integration)

```bash
make test
```

### Unit Tests Only

```bash
make test-unit
# or
go test -v ./cpu/... ./profile/... ./offcpu/...
```

### Integration Tests Only

```bash
make test-integration
# or
sudo bash test/run_tests.sh
```

**Note:** Integration tests require root privileges to attach eBPF programs.

## Test Workloads

Test workloads are used to generate realistic CPU and I/O activity for profiling:

### Go Workloads

**CPU-bound:** Multi-threaded mathematical computation
```bash
cd test/workloads/go
go build -o cpu_bound cpu_bound.go
./cpu_bound -duration=30s -threads=4
```

**I/O-bound:** Multi-threaded file I/O operations
```bash
cd test/workloads/go
go build -o io_bound io_bound.go
./io_bound -duration=30s -threads=2
```

### Rust Workload

**Important:** The Rust workload is configured to include debug symbols in release builds for proper symbolization. See [RUST_PROFILING.md](RUST_PROFILING.md) for details.

**CPU-bound:** Multi-threaded computation with atomic operations
```bash
cd test/workloads/rust
cargo build --release  # Builds with optimizations + debug symbols
./target/release/rust-workload 30 4  # 30 seconds, 4 threads
```

The `Cargo.toml` includes:
```toml
[profile.release]
debug = true   # Enables symbolization in profiles
strip = false
```

### Python Workloads

**Important:** Python 3.12+ requires the `-X perf` flag for proper symbolization in profiles. See [PYTHON_PROFILING.md](PYTHON_PROFILING.md) for details.

**CPU-bound:** Multi-threaded mathematical computation
```bash
# With perf support (recommended, Python 3.12+)
python3 -X perf test/workloads/python/cpu_bound.py 30 4  # 30 seconds, 4 threads

# Without perf support (limited symbolization)
python3 test/workloads/python/cpu_bound.py 30 4
```

**I/O-bound:** Multi-threaded file I/O operations
```bash
# With perf support (recommended, Python 3.12+)
python3 -X perf test/workloads/python/io_bound.py 30 2  # 30 seconds, 2 threads

# Without perf support (limited symbolization)
python3 test/workloads/python/io_bound.py 30 2
```

**Check Python perf support:**
```bash
bash test/check_python_perf.sh
```

## Integration Test Coverage

### TestProfileMode
- Tests CPU profiling (--profile) against all workloads
- Verifies profile.pb.gz generation
- Validates stack trace collection
- Checks symbolization of functions
- **Languages tested:** Go, Rust, Python

### TestOffCPUMode
- Tests off-CPU profiling (--offcpu) against I/O workloads
- Verifies offcpu.pb.gz generation
- Validates blocking operation detection
- **Languages tested:** Go, Python

### TestPMUMode
- Tests PMU hardware counter collection (--pmu)
- Verifies scheduling latency metrics
- Validates hardware counter output (cycles, instructions, cache misses)
- Checks percentile calculation (P50, P95, P99)
- **Note:** Hardware counters may not be available in VMs

### TestCombinedMode
- Tests all features together (--profile --offcpu --pmu)
- Verifies simultaneous operation of all modes
- Validates output from all collectors

## Unit Test Coverage

### cpu/cpu_usage_collector_test.go

**TestStructAlignment:**
- Verifies Go struct sizes match BPF expectations
- Checks field offset alignment for zero-copy parsing
- Critical for correct ring buffer event parsing

**TestHistogramConfiguration:**
- Tests HDR histogram with various time ranges
- Validates percentile calculations
- Ensures proper handling of nanosecond precision

**TestPidMetrics:**
- Tests PidMetrics structure and histogram recording
- Validates sample counting and aggregation
- Checks latency calculations

## Requirements

- **Go 1.21+**
- **Python 3.8+** (for Python workloads)
  - Python 3.12+ recommended for full symbolization (`-X perf` support)
  - See [PYTHON_PROFILING.md](PYTHON_PROFILING.md) for details
- **Rust/Cargo** (optional, for Rust workload)
- **Root privileges** (for integration tests)
- **Linux kernel 5.8+** with BTF support

## Test Dependencies

The test module uses:
- `github.com/google/pprof` - For parsing and validating profile outputs
- `github.com/stretchr/testify` - For test assertions
- `github.com/HdrHistogram/hdrhistogram-go` - For latency distribution analysis

## Continuous Integration

For CI environments, you can skip Rust workload if cargo is not available:

```bash
# Build workloads (auto-skips Rust if not available)
make test-workloads

# Run tests with timeout
go test -v -timeout 5m ./test/...
```

## Troubleshooting

### "Test requires root privileges"
Integration tests need root to attach eBPF programs. Run with sudo:
```bash
sudo -E go test -v ./test/...
```

### "perf-agent binary not found"
Build the agent first:
```bash
go generate ./...
go build
```

### "Hardware Counters: not available"
This is expected in VMs. Tests will still pass, just without hardware counter validation.

### Rust workload build fails
Rust workload is optional. Install from https://rustup.rs/ or skip Rust tests.

## Manual Testing

For manual testing with custom workloads:

```bash
# 1. Start your workload and note its PID
./my_application &
PID=$!

# 2. Run perf-agent
sudo ./perf-agent --profile --offcpu --pmu --pid $PID --duration 30s

# 3. Analyze results
go tool pprof profile.pb.gz
go tool pprof offcpu.pb.gz
```

## Test Output Examples

### Successful PMU Test
```
=== PID 12345 Metrics ===
Samples: 1234

Scheduling Latency (time on CPU per switch):
  Min:    0.123 ms
  Max:    45.678 ms
  Mean:   5.432 ms
  P50:    4.567 ms
  P95:    12.345 ms
  P99:    23.456 ms
  P99.9:  40.123 ms

Hardware Counters:
  Total Cycles:       123456789
  Total Instructions: 234567890
  Total Cache Misses: 1234567
  IPC (Instr/Cycle):  1.899
  Cache Misses/1K Instr: 5.263
```

### Successful Profile Test
```
Profile written to profile.pb.gz
Off-CPU profile written to offcpu.pb.gz
```

## Adding New Tests

To add a new integration test:

1. Create a test workload in the appropriate language directory
2. Add workload definition to `workloads` slice in `integration_test.go`
3. Add test function following existing patterns
4. Run `make test` to verify

To add a new unit test:

1. Create `*_test.go` file in the appropriate package
2. Follow Go testing conventions
3. Run `go test -v ./<package>/` to verify
