#!/usr/bin/env bash
# End-to-end Rust AutoFDO demo. Requires:
#   - cargo (any recent stable)
#   - perf-agent built and on PATH (or pass --agent ./perf-agent)
#   - create_llvm_prof from https://github.com/google/autofdo
#   - hyperfine (optional; falls back to /usr/bin/time -p)
#
# Output: baseline vs PGO-optimised wall-clock time for the same workload.
set -euo pipefail

ITER=${ITER:-200000000}
DURATION=${DURATION:-30s}
AGENT=${AGENT:-perf-agent}
WORKDIR=$(cd "$(dirname "$0")" && pwd)
cd "$WORKDIR"

bench() {
    local label=$1 bin=$2
    if command -v hyperfine >/dev/null 2>&1; then
        hyperfine --warmup 1 --runs 3 --export-json "${label}.json" \
            "$bin $ITER" >"${label}.txt" 2>&1
        awk -F'"mean": ' '/mean/{print $2+0}' "${label}.json" | head -1
    else
        /usr/bin/time -p "$bin" "$ITER" 2>&1 | awk '/real/ {print $2}'
    fi
}

echo "==> 1. Baseline build (with debug info)"
RUSTFLAGS="-C debuginfo=2" cargo build --release --quiet

echo "==> 2. Baseline benchmark"
BASELINE=$(bench baseline ./target/release/rust-pgo-example)
echo "    baseline: ${BASELINE}s"

echo "==> 3. Capture profile via perf-agent"
./target/release/rust-pgo-example "$ITER" &
WL_PID=$!
sleep 1   # workload warmup
"$AGENT" --profile --pid "$WL_PID" --duration "$DURATION" \
         --perf-data-output train.perf.data
wait "$WL_PID" || true

echo "==> 4. Convert perf.data → LLVM .prof via create_llvm_prof"
create_llvm_prof \
    --binary=./target/release/rust-pgo-example \
    --profile=train.perf.data \
    --out=train.prof

echo "==> 5. PGO build (uses train.prof; strips symbols on the final artefact)"
RUSTFLAGS="-C profile-use=$WORKDIR/train.prof -C strip=symbols" \
    cargo build --release --quiet

echo "==> 6. PGO-optimised benchmark"
OPT=$(bench optimized ./target/release/rust-pgo-example)
echo "    optimized: ${OPT}s"

echo
echo "==> Speedup"
awk -v b="$BASELINE" -v o="$OPT" \
    'BEGIN { printf "    %.2fx faster (%.1f%% improvement)\n", b/o, (b-o)/b*100 }'

echo "==> Final stripped binary:"
ls -la ./target/release/rust-pgo-example
file  ./target/release/rust-pgo-example
