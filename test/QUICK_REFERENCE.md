# Test Suite Quick Reference

## One-Line Commands

```bash
# Run everything
make test

# Unit tests (no root)
make test-unit

# Integration tests (requires root)
sudo make test-integration

# Build workloads
make test-workloads

# Clean all
make clean
```

## Test Workloads

```bash
# Go CPU (4 threads, 30 seconds)
test/workloads/go/cpu_bound -duration=30s -threads=4

# Go I/O (2 threads, 30 seconds)
test/workloads/go/io_bound -duration=30s -threads=2

# Rust CPU (4 threads, 30 seconds)
test/workloads/rust/target/release/rust-workload 30 4

# Python CPU (4 threads, 30 seconds)
python3 test/workloads/python/cpu_bound.py 30 4

# Python I/O (2 threads, 30 seconds)
python3 test/workloads/python/io_bound.py 30 2
```

## Manual Testing

```bash
# 1. Start workload, capture PID
./test/workloads/go/cpu_bound -duration=120s &
PID=$!
echo "Testing PID: $PID"

# 2. Profile (choose one or combine)
sudo ./perf-agent --profile --pid $PID --duration 30s
sudo ./perf-agent --offcpu --pid $PID --duration 30s
sudo ./perf-agent --pmu --pid $PID --duration 30s
sudo ./perf-agent --profile --offcpu --pmu --pid $PID --duration 30s

# 3. Analyze
go tool pprof profile.pb.gz
go tool pprof offcpu.pb.gz
```

## Test Outputs

| Mode | Output File | Analysis Command |
|------|-------------|------------------|
| --profile | profile.pb.gz | `go tool pprof profile.pb.gz` |
| --offcpu | offcpu.pb.gz | `go tool pprof offcpu.pb.gz` |
| --pmu | stdout | View console output |

## Common Issues

| Error | Solution |
|-------|----------|
| Test requires root | `sudo -E go test -v ./test/...` |
| perf-agent not found | `make build` |
| Rust build fails | Install Rust or skip: tests auto-skip |
| Hardware counters unavailable | Expected in VMs, tests pass anyway |

## File Locations

```
test/
├── workloads/
│   ├── go/{cpu_bound,io_bound}.go
│   ├── rust/src/main.rs
│   └── python/{cpu_bound,io_bound}.py
├── integration_test.go
└── run_tests.sh

cpu/
└── cpu_usage_collector_test.go
```

## CI/CD

GitHub Actions: `.github/workflows/tests.yml`
- Runs on push/PR
- Unit tests, integration tests, lint, build
- Multi-arch builds (amd64, arm64)

## Documentation

- `README.md` - Main project README
- `TESTING.md` - Comprehensive testing guide
- `test/README.md` - Test suite overview
- `test/QUICK_REFERENCE.md` - This file
- `test/TEST_IMPLEMENTATION_SUMMARY.md` - Implementation details
