#!/bin/bash
# Synthetic test for Off-CPU profiling with perf-agent
# Run from project root: ./test/test_offcpu_profile.sh
set -e

# Change to project root
cd "$(dirname "$0")/.."

# Cleanup on exit
cleanup() {
    [ -n "$PYTHON_PID" ] && kill $PYTHON_PID 2>/dev/null || true
}
trap cleanup EXIT

echo "=== Starting Python I/O workload with -X perf ==="
python3 -X perf test/workloads/python/io_bound.py 20 2 &
PYTHON_PID=$!
echo "Python PID: $PYTHON_PID"

# Wait for warmup (the script prints when done)
sleep 3

# Check perf map exists
PERF_MAP="/tmp/perf-${PYTHON_PID}.map"
if [ -f "$PERF_MAP" ]; then
    echo "✓ Perf map found: $PERF_MAP"
    echo "  Sample entries:"
    head -3 "$PERF_MAP"
    echo "  Looking for io_work..."
    grep -c "io_work" "$PERF_MAP" && echo "✓ io_work found in perf map" || echo "⚠ io_work not found"
else
    echo "✗ Perf map not found!"
fi

echo ""
echo "=== Running perf-agent --offcpu for 10s ==="
sudo ./perf-agent --offcpu --pid $PYTHON_PID --duration 10s \
    --tag test=offcpu_synthetic

# Wait for Python to finish
wait $PYTHON_PID 2>/dev/null || true
PYTHON_PID=""

echo ""
echo "=== Validating off-CPU profile output ==="
if [ -f "offcpu.pb.gz" ]; then
    echo "✓ Off-CPU profile created: offcpu.pb.gz"
    
    # Check profile with pprof
    echo ""
    echo "Top functions (off-CPU time in nanoseconds):"
    go tool pprof -top -nodecount=15 offcpu.pb.gz 2>/dev/null | head -20
    
    echo ""
    echo "Looking for blocking symbols..."
    if go tool pprof -top offcpu.pb.gz 2>/dev/null | grep -qiE "sleep|wait|read|write|fsync|io_work"; then
        echo "✓ Blocking/I/O symbols found!"
    else
        echo "⚠ No obvious blocking symbols found"
    fi
    
    echo ""
    echo "Profile comments (tags):"
    go tool pprof -comments offcpu.pb.gz 2>/dev/null || echo "(no comments)"
    
    echo ""
    echo "Profile sample type:"
    go tool pprof -sample_index offcpu.pb.gz 2>/dev/null | head -5 || true
else
    echo "✗ Off-CPU profile not created!"
    exit 1
fi

echo ""
echo "=== Test complete ==="
echo ""
echo "To explore the off-CPU profile interactively:"
echo "  go tool pprof offcpu.pb.gz"
echo ""
echo "To see flame graph:"
echo "  go tool pprof -http=:8080 offcpu.pb.gz"
