# Building perf-agent

## Prerequisites

### Required Tools
- Go 1.25+
- Clang/LLVM
- Linux headers
- Rust toolchain (for blazesym)

### Install System Dependencies

**Fedora/RHEL:**
```bash
sudo dnf install -y clang llvm elfutils-libelf-devel kernel-devel
```

**Ubuntu/Debian:**
```bash
sudo apt-get install -y clang llvm libelf-dev linux-headers-$(uname -r)
```

## Building Blazesym

perf-agent uses the official Go bindings from `github.com/libbpf/blazesym/go`. You need to build the blazesym C library before building perf-agent.

### Option 1: Local Development Setup

```bash
# Clone blazesym to your preferred location
cd ~/github  # or any directory you prefer
git clone https://github.com/libbpf/blazesym.git
cd blazesym

# Build the C library
cargo build --release --package blazesym-c

# The library will be at: target/release/libblazesym_c.a
```

Then build perf-agent using `CGO_CFLAGS` and `CGO_LDFLAGS`:
```bash
CGO_CFLAGS="-I$HOME/github/blazesym/capi/include" \
CGO_LDFLAGS="-L$HOME/github/blazesym/target/release" \
go build .
```

### Option 2: System-Wide Installation

```bash
# Clone and build
git clone https://github.com/libbpf/blazesym.git
cd blazesym
cargo build --release --package blazesym-c

# Install system-wide
sudo mkdir -p /usr/local/lib /usr/local/include
sudo cp target/release/libblazesym_c.a /usr/local/lib/
sudo cp capi/include/blazesym.h /usr/local/include/
sudo ldconfig
```

With this option, you don't need to set `CGO_CFLAGS` or `CGO_LDFLAGS` - the compiler will find them in the default system paths.

## Building perf-agent

Once blazesym is set up:

```bash
# Generate BPF code
go generate ./...

# Build the agent (use CGO_CFLAGS/CGO_LDFLAGS if blazesym is not installed system-wide)
CGO_CFLAGS="-I/path/to/blazesym/capi/include" \
CGO_LDFLAGS="-L/path/to/blazesym/target/release" \
go build -o perf-agent

# Or if blazesym is installed system-wide:
go build -o perf-agent

# Run tests
go test -v ./cpu/... ./profile/... ./offcpu/...
```

## Building Test Workloads

```bash
# Go workloads
cd test/workloads/go
go build -o cpu_bound cpu_bound.go
go build -o io_bound io_bound.go

# Rust workload (with debug symbols)
cd ../rust
cargo build --release

# Python workloads (already executable)
cd ../python
chmod +x *.py
```

## Running Tests

### Unit Tests
```bash
go test -v ./cpu/... ./profile/... ./offcpu/...
```

### Integration Tests (Requires Root)
```bash
cd test
sudo -E go test -v ./...
```

## CI/CD

The GitHub Actions workflow (`.github/workflows/ci.yml`) automatically:
1. Installs all dependencies
2. Builds blazesym
3. Generates BPF code
4. Builds perf-agent using `CGO_CFLAGS` and `CGO_LDFLAGS`
5. Runs unit tests
6. Builds test workloads

## Troubleshooting

### Error: "cannot find -lblazesym_c"

This means the linker can't find the blazesym library. Solutions:

1. **Set CGO_CFLAGS and CGO_LDFLAGS:**
   ```bash
   CGO_CFLAGS="-I/path/to/blazesym/capi/include" \
   CGO_LDFLAGS="-L/path/to/blazesym/target/release" \
   go build .
   ```

2. **Verify blazesym is built:**
   ```bash
   ls ~/github/blazesym/target/release/libblazesym_c.a
   ls ~/github/blazesym/capi/include/blazesym.h
   ```

3. **Or install system-wide:**
   ```bash
   sudo cp ~/github/blazesym/target/release/libblazesym_c.a /usr/local/lib/
   sudo cp ~/github/blazesym/capi/include/blazesym.h /usr/local/include/
   sudo ldconfig
   ```

### Error: "blazesym.h: No such file or directory"

This means the compiler can't find the blazesym header file. Set `CGO_CFLAGS`:
```bash
CGO_CFLAGS="-I/path/to/blazesym/capi/include" go build .
```

Or install the header system-wide:
```bash
sudo cp ~/github/blazesym/capi/include/blazesym.h /usr/local/include/
```

### Error: "kernel.org/pub/linux/libs/security/libcap" issues

Install libcap development files:
```bash
# Fedora/RHEL
sudo dnf install libcap-devel

# Ubuntu/Debian
sudo apt-get install libcap-dev
```

### BPF Compilation Errors

Ensure you have:
- Clang/LLVM installed
- Linux headers for your kernel version
- bpftool (optional but helpful)

```bash
# Check clang
clang --version

# Check kernel headers
ls /usr/src/kernels/$(uname -r)  # Fedora/RHEL
ls /usr/src/linux-headers-$(uname -r)  # Ubuntu/Debian
```

## Quick Start (Local Development)

```bash
# 1. Clone the project
git clone https://github.com/your-org/perf-agent.git
cd perf-agent

# 2. Build blazesym (one-time setup)
git clone https://github.com/libbpf/blazesym.git /tmp/blazesym
cd /tmp/blazesym
cargo build --release --package blazesym-c
cd -

# 3. Build perf-agent
go generate ./...
CGO_CFLAGS="-I/tmp/blazesym/capi/include" \
CGO_LDFLAGS="-L/tmp/blazesym/target/release" \
go build -o perf-agent

# 4. Run it!
sudo ./perf-agent --profile --pid <PID> --duration 30s
```

Or install blazesym system-wide for simpler builds:
```bash
sudo mkdir -p /usr/local/lib /usr/local/include
sudo cp /tmp/blazesym/target/release/libblazesym_c.a /usr/local/lib/
sudo cp /tmp/blazesym/capi/include/blazesym.h /usr/local/include/
sudo ldconfig
go build -o perf-agent
```

## Platform Support

- **Linux:** x86_64, ARM64
- **Kernel:** 5.10+ recommended (for all eBPF features)
- **Python:** 3.12+ for perf profiling with `-X perf` flag
- **Go:** 1.24+ required
- **Rust:** Latest stable (for building blazesym and test workloads)

## See Also

- [TESTING.md](TESTING.md) - Comprehensive testing guide
- [test/QUICK_REFERENCE.md](test/QUICK_REFERENCE.md) - Quick command reference
- [test/PYTHON_PROFILING.md](test/PYTHON_PROFILING.md) - Python profiling setup
- [test/RUST_PROFILING.md](test/RUST_PROFILING.md) - Rust profiling setup
