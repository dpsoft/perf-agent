#!/bin/bash
# Synthetic test for Node.js profiling with perf-agent + --perf-basic-prof
# Run from project root: ./test/test_node_profile.sh
set -e

# Change to project root
cd "$(dirname "$0")/.."

# Cleanup on exit
cleanup() {
    [ -n "$NODE_PID" ] && kill "$NODE_PID" 2>/dev/null || true
    rm -f profile.pb.gz
}
trap cleanup EXIT

echo "=== Starting Node.js workload with --perf-basic-prof ==="
node --perf-basic-prof test/workloads/node/cpu_bound.js 20 4 &
NODE_PID=$!
echo "Node PID: $NODE_PID"

# Wait for warmup / JIT and map generation
sleep 3

# Check perf map exists
PERF_MAP="/tmp/perf-${NODE_PID}.map"
if [ -f "$PERF_MAP" ]; then
    echo "✓ Perf map found: $PERF_MAP"
    echo "  Sample entries:"
    head -3 "$PERF_MAP" || true
else
    echo "✗ Perf map not found at $PERF_MAP"
    echo "  Ensure your Node binary supports --perf-basic-prof"
    echo "  You can check with: node --v8-options | grep perf-basic-prof"
    exit 1
fi

echo ""
echo "=== Running perf-agent for 10s ==="
PROFILE_OUTPUT="profile.pb.gz"
sudo ./perf-agent --profile --profile-output "$PROFILE_OUTPUT" --pid "$NODE_PID" --duration 10s --sample-rate 99 \
    --tag test=node_synthetic

# Wait for Node to finish (or ignore errors if it already exited)
wait "$NODE_PID" 2>/dev/null || true
NODE_PID=""

echo ""
echo "=== Validating profile output ==="
if [ -f "$PROFILE_OUTPUT" ]; then
    echo "✓ Profile created: $PROFILE_OUTPUT"

    echo ""
    echo "Top functions:"
    go tool pprof -top -nodecount=20 "$PROFILE_OUTPUT" 2>/dev/null | head -25

    echo ""
    echo "Looking for Node.js symbols..."
    # Adjust the patterns depending on your workload; look for JS function names or module paths
    if go tool pprof -top "$PROFILE_OUTPUT" 2>/dev/null | grep -qiE "cpuWork|Node\\.js|node|v8::"; then
        echo "✓ Node.js / V8 symbols found!"
    else
        echo "⚠ No clear Node.js symbols found (may still be only native frames)"
        echo "  Check the perf map content and that --perf-basic-prof is working as expected."
    fi

    echo ""
    echo "Profile comments (tags):"
    go tool pprof -comments "$PROFILE_OUTPUT" 2>/dev/null || echo "(no comments)"
else
    echo "✗ Profile not created!"
    exit 1
fi

echo ""
echo "=== Node.js test complete ==="

