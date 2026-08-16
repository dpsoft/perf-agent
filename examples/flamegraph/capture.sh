#!/usr/bin/env bash
# Capture a perf-agent profile against a running PID and render a
# Brendan-Gregg-style flame graph to flame.svg.
#
# Requires:
#   - perf-agent (caps set)
#   - perf binary (for `perf script`)
#   - FlameGraph scripts (stackcollapse-perf.pl, flamegraph.pl).
#     Either on PATH, or set FLAMEGRAPH_DIR=/path/to/FlameGraph.
#
# Usage:
#   ./capture.sh <PID> [DURATION]
#       e.g. ./capture.sh $(pgrep my-app) 30s

set -euo pipefail

PID=${1:?usage: $0 <PID> [DURATION]}
DURATION=${2:-30s}
AGENT=${AGENT:-perf-agent}

if [[ -n "${FLAMEGRAPH_DIR:-}" ]]; then
    SC="$FLAMEGRAPH_DIR/stackcollapse-perf.pl"
    FG="$FLAMEGRAPH_DIR/flamegraph.pl"
elif command -v stackcollapse-perf.pl >/dev/null 2>&1; then
    SC=$(command -v stackcollapse-perf.pl)
    FG=$(command -v flamegraph.pl)
else
    cat >&2 <<EOF
error: FlameGraph scripts not found.
  Either:
    git clone https://github.com/brendangregg/FlameGraph
    export FLAMEGRAPH_DIR=\$(pwd)/FlameGraph
  or install the scripts on PATH.
EOF
    exit 1
fi

WORKDIR=$(cd "$(dirname "$0")" && pwd)
cd "$WORKDIR"

echo "==> 1. Capture profile (PID=$PID, duration=$DURATION)"
"$AGENT" --profile --pid "$PID" --duration "$DURATION" \
         --perf-data-output capture.perf.data

echo "==> 2. perf script | stackcollapse-perf.pl | flamegraph.pl"
perf script -i capture.perf.data | "$SC" | "$FG" \
    --title "perf-agent flame graph (pid $PID, $DURATION)" \
    > flame.svg

echo "==> Wrote flame.svg ($(stat -c %s flame.svg) bytes)"
echo "    Open it in a browser, or with: xdg-open flame.svg"
