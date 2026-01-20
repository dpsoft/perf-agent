#!/bin/bash
set -e

cd "$(dirname "$0")"

# Set CGO flags for blazesym if not already set
# Users can override these by setting them before running this script
if [ -z "$CGO_CFLAGS" ]; then
    # Check common locations for blazesym
    if [ -f "/usr/local/include/blazesym.h" ]; then
        export CGO_CFLAGS="-I/usr/local/include"
    elif [ -d "$HOME/github/blazesym/capi/include" ]; then
        export CGO_CFLAGS="-I$HOME/github/blazesym/capi/include"
    fi
fi

if [ -z "$CGO_LDFLAGS" ]; then
    if [ -f "/usr/local/lib/libblazesym_c.a" ]; then
        export CGO_LDFLAGS="-L/usr/local/lib"
    elif [ -d "$HOME/github/blazesym/target/release" ]; then
        export CGO_LDFLAGS="-L$HOME/github/blazesym/target/release"
    fi
fi

# Export for child processes
export CGO_CFLAGS
export CGO_LDFLAGS

echo "Using CGO_CFLAGS: $CGO_CFLAGS"
echo "Using CGO_LDFLAGS: $CGO_LDFLAGS"
echo ""

echo "=== Building test workloads ==="

# Build Go workloads
echo "Building Go workloads..."
cd workloads/go
go build -o cpu_bound cpu_bound.go
go build -o io_bound io_bound.go
cd ../..

# Build Rust workload
echo "Building Rust workload..."
cd workloads/rust
if ! command -v cargo &> /dev/null; then
    echo "Rust/Cargo not found, skipping Rust workload"
    echo "Install from: https://rustup.rs/"
else
    cargo build --release
fi
cd ../..

# Make Python scripts executable
chmod +x workloads/python/*.py

echo ""
echo "=== Building perf-agent ==="
cd ..
if [ -f "perf-agent" ] && [ "perf-agent" -nt "main.go" ]; then
    echo "perf-agent already built and up to date, skipping..."
else
    go generate ./...
    go build -o perf-agent
fi
cd test

echo ""
echo "=== Running unit tests ==="
cd ..
go test -v ./cpu/
cd test

echo ""
echo "=== Running integration tests ==="
if [ "$(id -u)" -ne 0 ]; then
    echo "Integration tests require root. Running with sudo..."
    sudo CGO_CFLAGS="$CGO_CFLAGS" CGO_LDFLAGS="$CGO_LDFLAGS" $(which go) test -v -timeout 5m ./...
else
    go test -v -timeout 5m ./...
fi

echo ""
echo "=== All tests passed! ==="
