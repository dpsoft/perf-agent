#!/bin/bash
# Synthetic test for Python profiling with perf-agent
# Run from project root: ./test/test_python_profile.sh
set -e

# Change to project root
cd "$(dirname "$0")/.."

# Cleanup on exit
cleanup() {
    [ -n "$PYTHON_PID" ] && kill $PYTHON_PID 2>/dev/null || true
    rm -f /tmp/test_profile.pb.gz
}
trap cleanup EXIT

echo "=== Starting Python workload with -X perf ==="
python3 -X perf test/workloads/python/cpu_bound.py 20 2 &
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
    echo "  Looking for cpu_work..."
    grep -c "cpu_work" "$PERF_MAP" && echo "✓ cpu_work found in perf map" || echo "⚠ cpu_work not found"
else
    echo "✗ Perf map not found!"
fi

echo ""
echo "=== Running perf-agent for 10s ==="
sudo ./perf-agent --profile --pid $PYTHON_PID --duration 10s --sample-rate 99 \
    --tag test=python_synthetic

# Wait for Python to finish
wait $PYTHON_PID 2>/dev/null || true
PYTHON_PID=""

echo ""
echo "=== Validating profile output ==="
if [ -f "profile.pb.gz" ]; then
    echo "✓ Profile created: profile.pb.gz"
    
    # Check profile with pprof
    echo ""
    echo "Top functions:"
    go tool pprof -top -nodecount=15 profile.pb.gz 2>/dev/null | head -20
    
    echo ""
    echo "Looking for Python symbols..."
    if go tool pprof -top profile.pb.gz 2>/dev/null | grep -qE "cpu_work|main|warmup"; then
        echo "✓ Python user symbols found!"
    else
        echo "⚠ No Python user symbols found (only native symbols)"
        echo "  This may be expected - see test/PYTHON_PERF_KNOWN_ISSUES.md"
    fi
    
    echo ""
    echo "Profile comments (tags):"
    go tool pprof -comments profile.pb.gz 2>/dev/null || echo "(no comments)"
else
    echo "✗ Profile not created!"
    exit 1
fi

echo ""
echo "=== Test complete ==="
