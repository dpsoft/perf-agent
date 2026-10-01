#!/usr/bin/env bash
# examples/cpp-pgo/pgo-cycle.sh — same shape as the Rust demo.
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
            "$bin $ITER" >/dev/null
        awk -F'"mean": ' '/mean/{print $2+0}' "${label}.json" | head -1
    else
        /usr/bin/time -p "$bin" "$ITER" 2>&1 | awk '/real/ {print $2}'
    fi
}

echo "==> 1. Baseline build (clang++ -O2 -g)"
make -s baseline

echo "==> 2. Baseline benchmark"
BASELINE=$(bench baseline ./workload-baseline)
echo "    baseline: ${BASELINE}s"

echo "==> 3. Capture profile via perf-agent"
./workload-baseline "$ITER" &
WL_PID=$!
sleep 1
# The workload must outlive the capture window, and on fast hardware the
# default ITER does not: 200M iterations finishes in well under a second
# here, so the agent attached to a pid that was already gone and failed
# with "read /proc/<pid>/status: no such file or directory", which reads
# like a permissions or namespace problem rather than "raise ITER" (#167).
if ! kill -0 "$WL_PID" 2>/dev/null; then
    echo "ERROR: the workload finished before the profiler could attach." >&2
    echo "  ITER=$ITER is too small for this machine. Raise it until the" >&2
    echo "  baseline run above takes comfortably longer than DURATION=$DURATION," >&2
    echo "  e.g. ITER=\$(( $ITER * 100 ))." >&2
    exit 1
fi
"$AGENT" --profile --pid "$WL_PID" --duration "$DURATION" \
         --perf-data-output train.perf.data
wait "$WL_PID" || true

echo "==> 4. Convert perf.data → LLVM .prof"
create_llvm_prof \
    --binary=./workload-baseline \
    --profile=train.perf.data \
    --out=train.prof \
    --use_lbr=false

echo "==> 5. PGO build (-fprofile-sample-use=train.prof)"
make -s pgo

echo "==> 6. Strip the optimised binary"
make -s strip

echo "==> 7. PGO-optimised benchmark"
OPT=$(bench optimized ./workload-pgo)
echo "    optimized: ${OPT}s"

echo
echo "==> Speedup"
awk -v b="$BASELINE" -v o="$OPT" \
    'BEGIN { printf "    %.2fx faster (%.1f%% improvement)\n", b/o, (b-o)/b*100 }'

echo "==> Final stripped binary:"
ls -la ./workload-pgo
file  ./workload-pgo
