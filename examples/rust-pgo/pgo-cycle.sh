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
        # Three runs, report the MINIMUM. The fallback used to time a single
        # run with no warmup, which cannot resolve the few percent this example
        # used to claim: the first run pays page-cache and CPU-frequency
        # warm-up, and the minimum is the statistic least contaminated by
        # whatever else the machine is doing.
        local best=""
        for _ in 1 2 3; do
            local t
            t=$(/usr/bin/time -p "$bin" "$ITER" 2>&1 >/dev/null | awk '/real/ {print $2}')
            if [ -z "$best" ] || awk -v a="$t" -v b="$best" 'BEGIN{exit !(a<b)}'; then
                best=$t
            fi
        done
        echo "$best"
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
# The workload must outlive the capture window, and on fast hardware the
# default ITER does not, leaving the agent to attach to a pid that is
# already gone and fail with "read /proc/<pid>/status: no such file or
# directory" -- which reads like a permissions problem rather than "raise
# ITER" (#167).
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

echo "==> 4. Convert perf.data → LLVM .prof via create_llvm_prof"
create_llvm_prof \
    --binary=./target/release/rust-pgo-example \
    --profile=train.perf.data \
    --out=train.prof \
    --use_lbr=false

echo "==> 5. PGO build (uses train.prof; strips symbols on the final artefact)"
# -Z profile-sample-use is rustc's AutoFDO flag. This script used to pass
# -Cllvm-args=-sample-profile-file instead, reaching for LLVM's raw cl::opt --
# but rustc builds its sample-profile pipeline from its own PGOOptions, so that
# option was parsed and then ignored. The effect was total: a real profile, a
# nonexistent profile and no profile at all produced byte-identical binaries,
# the build exited 0 either way, and the measured speedup was -0.0% (#168).
#
# It is a -Z flag, so it needs nightly. RUSTC_BOOTSTRAP=1 unlocks it on a
# stable toolchain; that is an unsupported escape hatch rather than a promise,
# so prefer a real nightly when one is installed.
if cargo +nightly --version >/dev/null 2>&1; then
    PGO_CARGO=(cargo +nightly)
    PGO_ENV=()
    echo "    using nightly for -Z profile-sample-use"
else
    PGO_CARGO=(cargo)
    PGO_ENV=(RUSTC_BOOTSTRAP=1)
    echo "    no nightly toolchain; using RUSTC_BOOTSTRAP=1 to unlock -Z on stable"
fi

# Two builds with IDENTICAL flags but for the profile, so the comparison below
# isolates the profile and nothing else. Neither is stripped: -C strip=symbols
# is applied to the final artefact afterwards, and stripping here would make
# the two binaries differ for a reason that has nothing to do with PGO.
COMMON="-Cdebuginfo=2"
touch src/main.rs
env "${PGO_ENV[@]}" RUSTFLAGS="$COMMON" "${PGO_CARGO[@]}" build --release --quiet
CONTROL_HASH=$(sha256sum ./target/release/rust-pgo-example | cut -d" " -f1)

touch src/main.rs
env "${PGO_ENV[@]}" \
    RUSTFLAGS="-Zprofile-sample-use=$WORKDIR/train.prof $COMMON" \
    "${PGO_CARGO[@]}" build --release --quiet
PGO_HASH=$(sha256sum ./target/release/rust-pgo-example | cut -d" " -f1)

# The gate the old invocation would have failed. A profile that changes nothing
# leaves the two builds identical, and the benchmark below would then be
# measuring one binary against itself and reporting it as a PGO outcome --
# which is exactly what #168 recorded as "0.0% improvement".
if [ "$CONTROL_HASH" = "$PGO_HASH" ]; then
    echo "ERROR: the sample profile changed nothing - control and PGO builds are byte-identical." >&2
    echo "  $CONTROL_HASH" >&2
    echo "  The profile was not applied, so there is no PGO result to measure." >&2
    echo "  Check that train.prof is non-empty and that -Z profile-sample-use is accepted" >&2
    echo "  by this toolchain (rustc --version; cargo +nightly --version)." >&2
    exit 1
fi
echo "    profile applied: control ${CONTROL_HASH:0:16} -> pgo ${PGO_HASH:0:16}"

# Strip only now, for the final artefact the user inspects at the end.
touch src/main.rs
env "${PGO_ENV[@]}" \
    RUSTFLAGS="-Zprofile-sample-use=$WORKDIR/train.prof -C strip=symbols" \
    "${PGO_CARGO[@]}" build --release --quiet

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
