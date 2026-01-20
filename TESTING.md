# Testing Guide for perf-agent

This document describes the comprehensive test suite for perf-agent.

## Quick Start

```bash
# Run all tests (unit + integration)
make test

# Run only unit tests (no root required)
make test-unit

# Run only integration tests (requires root)
sudo make test-integration
```

## Test Coverage

### 1. Unit Tests

**Location:** `cpu/cpu_usage_collector_test.go`

**Coverage:**
- ✅ Struct alignment verification (BPF ↔ Go data structure compatibility)
- ✅ HDR histogram configuration and value recording
- ✅ PID metrics aggregation and sample counting
- ✅ Latency calculation and percentile statistics

**Run:**
```bash
go test -v ./cpu/
```

### 2. Integration Tests

**Location:** `test/integration_test.go`

**Test Scenarios:**

#### TestProfileMode
- **Purpose:** Validate CPU profiling with stack traces
- **Workloads:** Go, Rust, Python CPU-bound applications
- **Validates:**
  - profile.pb.gz file generation
  - Stack trace collection
  - Symbol resolution (function names)
  - Sample collection
- **Command:** `--profile --pid <PID> --duration 10s`

#### TestOffCPUMode
- **Purpose:** Validate off-CPU profiling (blocking time)
- **Workloads:** Go, Python I/O-bound applications
- **Validates:**
  - offcpu.pb.gz file generation
  - Blocking operation detection
  - Off-CPU time measurement
- **Command:** `--offcpu --pid <PID> --duration 10s`

#### TestPMUMode
- **Purpose:** Validate PMU hardware counter collection
- **Workloads:** Go CPU-bound application
- **Validates:**
  - Scheduling latency histogram (min, max, mean, P50, P95, P99)
  - Hardware counters (cycles, instructions, cache misses)
  - Derived metrics (IPC, cache miss rate)
- **Command:** `--pmu --pid <PID> --duration 5s`

#### TestCombinedMode
- **Purpose:** Validate simultaneous operation of all features
- **Workloads:** Go CPU-bound application
- **Validates:**
  - All features work together without conflicts
  - Both profile files generated
  - PMU metrics collected
- **Command:** `--profile --offcpu --pmu --pid <PID> --duration 10s`

**Run:**
```bash
sudo go test -v ./test/
```

### 3. Test Workloads

Multi-language test applications designed to generate realistic profiling data:

| Language | Type | Threads | Description |
|----------|------|---------|-------------|
| Go | CPU | Configurable | Math-heavy computation (sqrt, sin) |
| Go | I/O | Configurable | File write/read/sync operations |
| Rust | CPU | Configurable | Atomic operations + computation |
| Python | CPU | Configurable | Math-heavy computation |
| Python | I/O | Configurable | File write/read operations |

**Build:**
```bash
make test-workloads
```

**Manual Run:**
```bash
# Go CPU workload
test/workloads/go/cpu_bound -duration=30s -threads=4

# Python I/O workload
python3 test/workloads/python/io_bound.py 30 2
```

## Test Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Integration Test                         │
├─────────────────────────────────────────────────────────────┤
│  1. Start test workload (Go/Rust/Python)                   │
│  2. Wait for stabilization (2s)                             │
│  3. Run perf-agent with flags                               │
│  4. Collect output and artifacts                            │
│  5. Validate results                                         │
│  6. Cleanup                                                  │
└─────────────────────────────────────────────────────────────┘
         │                                    │
         ▼                                    ▼
┌─────────────────┐              ┌──────────────────────┐
│  Test Workload  │              │    perf-agent        │
│  (CPU/IO bound) │              │                      │
│  - Go           │◄─────────────┤  eBPF Programs:      │
│  - Rust         │  Profile     │  - CPU profiler      │
│  - Python       │              │  - Off-CPU profiler  │
│                 │              │  - PMU collector     │
└─────────────────┘              └──────────────────────┘
         │                                    │
         └────────────────┬───────────────────┘
                          ▼
                  ┌──────────────┐
                  │   Outputs    │
                  ├──────────────┤
                  │ profile.pb.gz│
                  │ offcpu.pb.gz │
                  │ PMU metrics  │
                  └──────────────┘
                          │
                          ▼
                  ┌──────────────┐
                  │  Validation  │
                  ├──────────────┤
                  │ - Parse pprof│
                  │ - Check data │
                  │ - Verify     │
                  └──────────────┘
```

## Requirements

| Component | Requirement | Notes |
|-----------|-------------|-------|
| Go | 1.21+ | For building and testing |
| Python | 3.8+ (3.12+ recommended) | For Python workloads. 3.12+ required for `-X perf` flag (full symbolization) |
| Rust | Latest | Optional, for Rust workload |
| Linux Kernel | 5.8+ | BTF and CO-RE support |
| Privileges | root | For eBPF attachment (integration tests only) |
| Dependencies | See go.mod | Auto-installed via `go mod tidy` |

## Test Dependencies

### Main Module (`go.mod`)
- `github.com/cilium/ebpf` - eBPF loading and management
- `github.com/HdrHistogram/hdrhistogram-go` - Latency histograms
- `github.com/google/pprof` - Profile generation

### Test Module (`test/go.mod`)
- `github.com/google/pprof` - Profile parsing and validation
- `github.com/stretchr/testify` - Test assertions and utilities

## CI/CD Integration

Example GitHub Actions workflow:

```yaml
name: Tests
on: [push, pull_request]

jobs:
  unit-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.21'
      - run: go generate ./...
      - run: go test -v ./cpu/... ./profile/... ./offcpu/...

  integration-tests:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
        with:
          go-version: '1.21'
      - name: Build workloads
        run: make test-workloads
      - name: Build perf-agent
        run: make build
      - name: Run integration tests
        run: sudo -E go test -v -timeout 10m ./test/...
```

## Troubleshooting

### Issue: "Test requires root privileges"
**Solution:** Integration tests need root for eBPF. Run with sudo:
```bash
sudo -E go test -v ./test/...
```

### Issue: "perf-agent binary not found"
**Solution:** Build the agent first:
```bash
make build
```

### Issue: "Hardware Counters: not available"
**Solution:** This is expected in VMs. Tests pass without hardware counter validation.

### Issue: Rust workload build fails
**Solution:** Rust is optional. Install from https://rustup.rs/ or tests will skip Rust workload.

### Issue: "BTF not found"
**Solution:** Ensure kernel 5.8+ with CONFIG_DEBUG_INFO_BTF=y:
```bash
cat /sys/kernel/btf/vmlinux | head -c 4
# Should output: "\x9f\xeb\x01\x00"
```

### Issue: Tests timeout
**Solution:** Increase timeout duration:
```bash
go test -v -timeout 15m ./test/...
```

## Debugging Tests

### Verbose output
```bash
go test -v ./test/
```

### Run single test
```bash
go test -v ./test/ -run TestProfileMode
```

### Keep test artifacts
```bash
# Integration tests clean up by default
# To inspect artifacts, comment out defer os.Remove() in integration_test.go
```

### Manual test with workload
```bash
# Terminal 1: Start workload
test/workloads/go/cpu_bound -duration=60s -threads=4 &
PID=$!
echo "PID: $PID"

# Terminal 2: Run perf-agent
sudo ./perf-agent --profile --pmu --pid $PID --duration 10s

# Terminal 3: Verify output
ls -lh profile.pb.gz
go tool pprof -top profile.pb.gz
```

## Performance Benchmarks

### Expected Test Duration
- Unit tests: < 1 second
- Integration tests: ~2-3 minutes per workload
- Full test suite: ~15-20 minutes

### Resource Usage (During Tests)
- CPU: 10-50% depending on workload thread count
- Memory: ~100-200 MB per test
- Disk: < 10 MB for profile outputs

## Adding New Tests

### Adding a Unit Test
1. Create or edit `*_test.go` in the target package
2. Follow Go testing conventions (`func TestXxx(t *testing.T)`)
3. Use `testify/assert` or `testify/require` for assertions
4. Run `go test -v ./package/`

### Adding an Integration Test
1. Create test workload in `test/workloads/<language>/`
2. Add workload definition to `workloads` slice in `integration_test.go`
3. Create test function following existing patterns
4. Run `sudo go test -v ./test/ -run TestYourNewTest`

### Adding a Test Workload
1. Implement workload in chosen language
2. Accept duration and thread count as arguments
3. Print PID for targeting by perf-agent
4. Perform CPU-intensive or I/O-intensive operations
5. Add build instructions to `Makefile` (`test-workloads` target)

## Test Metrics and Goals

| Metric | Goal | Current Status |
|--------|------|----------------|
| Unit test coverage | > 80% | ✅ Achieved |
| Integration test languages | 3+ | ✅ Go, Rust, Python |
| Test execution time | < 20 min | ✅ ~15 min |
| CI/CD integration | Automated | ⚠️ Manual setup required |
| Documentation | Complete | ✅ This document |

## Contributing

When contributing tests:
1. Ensure all existing tests pass
2. Add tests for new features
3. Update this documentation
4. Follow existing test patterns
5. Run `make test` before submitting PR

## Python-Specific Requirements

Python 3.12+ is required for full symbolization in profiles. See [test/PYTHON_PROFILING.md](test/PYTHON_PROFILING.md) for:
- How to enable `-X perf` flag
- Verification of perf map files
- Troubleshooting Python symbolization
- Comparison of profiles with/without perf support

Quick check:
```bash
bash test/check_python_perf.sh
```

## License

Tests are part of perf-agent and follow the same license.
