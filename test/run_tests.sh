#!/bin/bash
set -e

cd "$(dirname "$0")"

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
go generate ./...
go build -o perf-agent
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
    sudo -E go test -v -timeout 5m ./...
else
    go test -v -timeout 5m ./...
fi

echo ""
echo "=== All tests passed! ==="
