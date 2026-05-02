#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/run-real-rust-hip-flamegraph.sh [--dry-run] [--outdir <dir>] [--hip-library <path>] [--duration <dur>] [--join-window <dur>] [--iterations <n>] [--sleep-before-ms <ms>] [--sleep-between-ms <ms>] [--cpu-spin <n>]

Builds a real Rust HIP workload, runs it, profiles it with perf-agent using:
  --profile --gpu-linux-kfd --gpu-host-hip-library
and renders an HTML/SVG flamegraph from the resulting folded output.
EOF
}

quote_cmd() {
    local parts=()
    local arg
    for arg in "$@"; do
        parts+=("$(printf '%q' "${arg}")")
    done
    printf '%s\n' "${parts[*]}"
}

discover_hip_library() {
    local candidate
    for candidate in \
        "${PERF_AGENT_HIP_LIBRARY:-}" \
        "/usr/local/lib/ollama/rocm/libamdhip64.so.6.3.60303" \
        "/usr/local/lib/ollama/rocm/libamdhip64.so.6" \
        "/opt/rocm/lib/libamdhip64.so"
    do
        if [[ -n "${candidate}" && -e "${candidate}" ]]; then
            printf '%s\n' "${candidate}"
            return 0
        fi
    done
    return 1
}

DRY_RUN=0
OUTDIR="/tmp/perf-agent-real-rust-hip"
HIP_LIBRARY=""
DURATION=""
JOIN_WINDOW="5ms"
ITERATIONS="12"
SLEEP_BEFORE_MS="5000"
SLEEP_BETWEEN_MS="40"
CPU_SPIN="1500000"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --dry-run)
            DRY_RUN=1
            shift
            ;;
        --outdir)
            OUTDIR="${2:-}"
            shift 2
            ;;
        --hip-library)
            HIP_LIBRARY="${2:-}"
            shift 2
            ;;
        --duration)
            DURATION="${2:-}"
            shift 2
            ;;
        --join-window)
            JOIN_WINDOW="${2:-}"
            shift 2
            ;;
        --iterations)
            ITERATIONS="${2:-}"
            shift 2
            ;;
        --sleep-before-ms)
            SLEEP_BEFORE_MS="${2:-}"
            shift 2
            ;;
        --sleep-between-ms)
            SLEEP_BETWEEN_MS="${2:-}"
            shift 2
            ;;
        --cpu-spin)
            CPU_SPIN="${2:-}"
            shift 2
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown argument: $1" >&2
            usage
            exit 1
            ;;
    esac
done

if [[ -z "${HIP_LIBRARY}" ]]; then
    HIP_LIBRARY="$(discover_hip_library || true)"
fi
if [[ -z "${HIP_LIBRARY}" ]]; then
    echo "could not discover HIP library; pass --hip-library or set PERF_AGENT_HIP_LIBRARY" >&2
    exit 1
fi

if [[ -z "${DURATION}" ]]; then
    # Cover the pre-launch warmup, the token loop, and some extra slack so the
    # live attach window actually overlaps the HIP launches we want to render.
    LOOP_BUDGET_MS=$((ITERATIONS * SLEEP_BETWEEN_MS))
    PROFILE_DURATION_MS=$((SLEEP_BEFORE_MS + LOOP_BUDGET_MS + 3000))
    DURATION="${PROFILE_DURATION_MS}ms"
fi

APP_DIR="${REPO_ROOT}/.tmp/real-rust-hip"
APP_BIN="${APP_DIR}/real-hip-attention-workload"
AGENT_BIN="${APP_DIR}/perf-agent"
FLAMEGRAPH_BIN="${APP_DIR}/flamegraph-svg"
CPU_PROFILE="${OUTDIR}/real_rust_hip_attention.oncpu.pb.gz"
GPU_RAW="${OUTDIR}/real_rust_hip_attention.raw.json"
GPU_ATTR="${OUTDIR}/real_rust_hip_attention.attributions.json"
GPU_FOLDED="${OUTDIR}/real_rust_hip_attention.folded"
GPU_PPROF="${OUTDIR}/real_rust_hip_attention.pb.gz"
GPU_SVG="${OUTDIR}/real_rust_hip_attention.svg"
GPU_HTML="${OUTDIR}/real_rust_hip_attention.html"
APP_LOG="${OUTDIR}/real_rust_hip_attention.app.log"
RUNNER_LOG="${OUTDIR}/real_rust_hip_attention.runner.log"
OWNER_UID=$(id -u)
OWNER_GID=$(id -g)

declare -a BUILD_CMD=(
    rustc
    examples/real_hip_attention_workload.rs
    -o
    "${APP_BIN}"
    -C
    debuginfo=2
    -C
    force-frame-pointers=yes
    -C
    opt-level=0
)

declare -a BUILD_AGENT_CMD=(
    env
    "LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release"
    "GOCACHE=/tmp/perf-agent-gocache"
    "GOMODCACHE=/tmp/perf-agent-gomodcache"
    "GOTOOLCHAIN=auto"
    "CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
    "CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
    go
    build
    -o
    "${AGENT_BIN}"
    .
)

declare -a BUILD_RENDER_CMD=(
    env
    "LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release"
    "GOCACHE=/tmp/perf-agent-gocache"
    "GOMODCACHE=/tmp/perf-agent-gomodcache"
    "GOTOOLCHAIN=auto"
    "CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
    "CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
    go
    build
    -o
    "${FLAMEGRAPH_BIN}"
    ./cmd/flamegraph-svg
)

declare -a PROFILE_CMD=(
    sudo
    /usr/bin/env
    "PERF_AGENT_DEBUG_GPU_LIVE=${PERF_AGENT_DEBUG_GPU_LIVE:-}"
    "LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release"
    "GOCACHE=/tmp/perf-agent-gocache"
    "GOMODCACHE=/tmp/perf-agent-gomodcache"
    "GOTOOLCHAIN=auto"
    "CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
    "CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
    "${AGENT_BIN}"
    --profile
    --pid
    "<pid>"
    --duration
    "${DURATION}"
    --unwind
    fp
    --profile-output
    "${CPU_PROFILE}"
    --gpu-linux-kfd
    --gpu-host-hip-library
    "${HIP_LIBRARY}"
    --gpu-host-hip-symbol
    hipLaunchKernel
    --gpu-hip-linuxdrm-join-window
    "${JOIN_WINDOW}"
    --gpu-raw-output
    "${GPU_RAW}"
    --gpu-attribution-output
    "${GPU_ATTR}"
    --gpu-folded-output
    "${GPU_FOLDED}"
    --gpu-profile-output
    "${GPU_PPROF}"
)

declare -a RENDER_CMD=(
    env
    "LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release"
    "GOCACHE=/tmp/perf-agent-gocache"
    "GOMODCACHE=/tmp/perf-agent-gomodcache"
    "GOTOOLCHAIN=auto"
    "CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
    "CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
    "${FLAMEGRAPH_BIN}"
    --title
    "CPU + GPU Flame Graph: real_rust_hip_attention"
    --input
    "${GPU_FOLDED}"
    --output
    "${GPU_SVG}"
    --html-output
    "${GPU_HTML}"
)

if [[ "${DRY_RUN}" == "1" ]]; then
    echo "build:"
    quote_cmd "${BUILD_CMD[@]}"
    echo
    echo "build perf-agent:"
    quote_cmd "${BUILD_AGENT_CMD[@]}"
    echo
    echo "build flamegraph renderer:"
    quote_cmd "${BUILD_RENDER_CMD[@]}"
    echo
    echo "run app:"
    quote_cmd env "REAL_HIP_ATTENTION_LIBRARY=${HIP_LIBRARY}" "REAL_HIP_ATTENTION_ITERATIONS=${ITERATIONS}" "REAL_HIP_ATTENTION_SLEEP_BEFORE_MS=${SLEEP_BEFORE_MS}" "REAL_HIP_ATTENTION_SLEEP_BETWEEN_MS=${SLEEP_BETWEEN_MS}" "REAL_HIP_ATTENTION_CPU_SPIN=${CPU_SPIN}" "${APP_BIN}"
    echo
    echo "profile:"
    quote_cmd "${PROFILE_CMD[@]}"
    echo
    echo "render:"
    quote_cmd "${RENDER_CMD[@]}"
    exit 0
fi

mkdir -p "${APP_DIR}" "${OUTDIR}"

(
    cd "${REPO_ROOT}"
    "${BUILD_CMD[@]}"
    "${BUILD_AGENT_CMD[@]}"
    "${BUILD_RENDER_CMD[@]}"
)

set +e
(
    cd "${REPO_ROOT}"
    REAL_HIP_ATTENTION_LIBRARY="${HIP_LIBRARY}" \
    REAL_HIP_ATTENTION_ITERATIONS="${ITERATIONS}" \
    REAL_HIP_ATTENTION_SLEEP_BEFORE_MS="${SLEEP_BEFORE_MS}" \
    REAL_HIP_ATTENTION_SLEEP_BETWEEN_MS="${SLEEP_BETWEEN_MS}" \
    REAL_HIP_ATTENTION_CPU_SPIN="${CPU_SPIN}" \
    "${APP_BIN}" >"${APP_LOG}" 2>&1 &
    APP_PID=$!
    trap 'kill "${APP_PID}" 2>/dev/null || true' EXIT

    sleep 1
    for i in "${!PROFILE_CMD[@]}"; do
        if [[ "${PROFILE_CMD[$i]}" == "<pid>" ]]; then
            PROFILE_CMD[$i]="${APP_PID}"
            break
        fi
    done
    : >"${RUNNER_LOG}"
    printf 'profile command: ' >>"${RUNNER_LOG}"
    quote_cmd "${PROFILE_CMD[@]}" >>"${RUNNER_LOG}"
    "${PROFILE_CMD[@]}" >>"${RUNNER_LOG}" 2>&1
    PROFILE_STATUS=$?
    wait "${APP_PID}"
    APP_STATUS=$?
    trap - EXIT
    printf 'profile exit status: %d\n' "${PROFILE_STATUS}" >>"${RUNNER_LOG}"
    printf 'app exit status: %d\n' "${APP_STATUS}" >>"${RUNNER_LOG}"
    if [[ "${PROFILE_STATUS}" -ne 0 ]]; then
        exit "${PROFILE_STATUS}"
    fi
    if [[ "${APP_STATUS}" -ne 0 ]]; then
        exit "${APP_STATUS}"
    fi
)
STATUS=$?
set -e
if [[ "${STATUS}" -ne 0 ]]; then
    exit "${STATUS}"
fi

sudo chown "${OWNER_UID}:${OWNER_GID}" \
    "${CPU_PROFILE}" \
    "${GPU_RAW}" \
    "${GPU_ATTR}" \
    "${GPU_FOLDED}" \
    "${GPU_PPROF}" \
    "${GPU_SVG}" \
    "${GPU_HTML}" 2>/dev/null || true

(
    cd "${REPO_ROOT}"
    "${RENDER_CMD[@]}"
)

echo "Wrote:"
echo "  ${CPU_PROFILE}"
echo "  ${GPU_RAW}"
echo "  ${GPU_ATTR}"
echo "  ${GPU_FOLDED}"
echo "  ${GPU_PPROF}"
echo "  ${GPU_SVG}"
echo "  ${GPU_HTML}"
echo "  ${APP_LOG}"
echo "  ${RUNNER_LOG}"
