#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/run-real-rust-rocprofiler-sdk-flamegraph.sh [--dry-run] [--outdir <dir>] [--hip-library <path>] [--rocprofiler-sdk-library <path>] [--duration <dur>] [--iterations <n>] [--sleep-before-ms <ms>] [--sleep-between-ms <ms>] [--sleep-after-ms <ms>] [--cpu-spin <n>]

Builds a real Rust HIP workload, runs it, profiles CPU stacks plus HIP host
launches, and feeds a real rocprofiler-sdk native producer into
--gpu-amd-sample-stdin before rendering an HTML/SVG flamegraph.
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

discover_rocprofiler_sdk_library() {
    local candidate
    for candidate in \
        "${PERF_AGENT_ROCPROFILER_SDK_LIBRARY:-}" \
        "/home/diego/github/rocm-systems/rocprofiler-sdk-build/lib/librocprofiler-sdk.so" \
        "/usr/local/lib/librocprofiler-sdk.so" \
        "/opt/rocm/lib/librocprofiler-sdk.so"
    do
        if [[ -n "${candidate}" && -e "${candidate}" ]]; then
            printf '%s\n' "${candidate}"
            return 0
        fi
    done
    return 1
}

DRY_RUN=0
OUTDIR="/tmp/perf-agent-real-rust-hip-sdk"
HIP_LIBRARY=""
ROCPROFILER_SDK_LIBRARY=""
DURATION=""
ITERATIONS="12"
SLEEP_BEFORE_MS="5000"
SLEEP_BETWEEN_MS="40"
SLEEP_AFTER_MS="250"
CPU_SPIN="1500000"
LAUNCHES_PER_ITERATION="4"

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
        --rocprofiler-sdk-library)
            ROCPROFILER_SDK_LIBRARY="${2:-}"
            shift 2
            ;;
        --duration)
            DURATION="${2:-}"
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
        --sleep-after-ms)
            SLEEP_AFTER_MS="${2:-}"
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

if [[ -z "${ROCPROFILER_SDK_LIBRARY}" ]]; then
    ROCPROFILER_SDK_LIBRARY="$(discover_rocprofiler_sdk_library || true)"
fi
if [[ -z "${ROCPROFILER_SDK_LIBRARY}" ]]; then
    echo "could not discover rocprofiler-sdk library; pass --rocprofiler-sdk-library or set PERF_AGENT_ROCPROFILER_SDK_LIBRARY" >&2
    exit 1
fi
ROCPROFILER_SDK_LIB_DIR=$(dirname "${ROCPROFILER_SDK_LIBRARY}")

if [[ -z "${DURATION}" ]]; then
    LOOP_BUDGET_MS=$((ITERATIONS * LAUNCHES_PER_ITERATION * SLEEP_BETWEEN_MS))
    PROFILE_DURATION_MS=$((SLEEP_BEFORE_MS + LOOP_BUDGET_MS + SLEEP_AFTER_MS + 3000))
    DURATION="${PROFILE_DURATION_MS}ms"
fi

PRODUCER_SLEEP_BEFORE_MS=0
if (( SLEEP_BEFORE_MS > 1000 )); then
    PRODUCER_SLEEP_BEFORE_MS=$((SLEEP_BEFORE_MS - 1000 + 200))
fi

APP_DIR="${REPO_ROOT}/.tmp/real-rust-hip-sdk"
APP_BIN="${APP_DIR}/real-hip-attention-workload"
AGENT_BIN="${APP_DIR}/perf-agent"
COLLECTOR_BIN="${APP_DIR}/amd-sample-collector"
FLAMEGRAPH_BIN="${APP_DIR}/flamegraph-svg"
BRIDGE_SO="${APP_DIR}/libperf-agent-rocprofiler-sdk-preload.so"
CPU_PROFILE="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.oncpu.pb.gz"
GPU_RAW="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.raw.json"
GPU_ATTR="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.attributions.json"
GPU_FOLDED="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.folded"
GPU_PPROF="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.pb.gz"
GPU_SVG="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.svg"
GPU_HTML="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.html"
NATIVE_JSON="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.native.ndjson"
APP_LOG="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.app.log"
RUNNER_LOG="${OUTDIR}/real_rust_hip_attention_rocprofiler_sdk.runner.log"
OWNER_UID=$(id -u)
OWNER_GID=$(id -g)

declare -a BUILD_APP_CMD=(
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

declare -a BUILD_COLLECTOR_CMD=(
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
    "${COLLECTOR_BIN}"
    ./cmd/amd-sample-collector
)

declare -a BUILD_BRIDGE_CMD=(
    c++
    -shared
    -fPIC
    -std=c++17
    -D__HIP_PLATFORM_AMD__
    examples/rocprofiler_sdk_preload_bridge.cpp
    -I
    /home/diego/github/rocm-systems/projects/rocprofiler-sdk/source/include
    -I
    /home/diego/github/rocm-systems/rocprofiler-sdk-build/source/include
    -L
    "${ROCPROFILER_SDK_LIB_DIR}"
    -lrocprofiler-sdk
    "-Wl,-rpath,${ROCPROFILER_SDK_LIB_DIR}"
    -o
    "${BRIDGE_SO}"
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
    --gpu-amd-sample-stdin
    --gpu-host-hip-library
    "${HIP_LIBRARY}"
    --gpu-host-hip-symbol
    hipModuleLaunchKernel
    --gpu-raw-output
    "${GPU_RAW}"
    --gpu-attribution-output
    "${GPU_ATTR}"
    --gpu-folded-output
    "${GPU_FOLDED}"
    --gpu-profile-output
    "${GPU_PPROF}"
)

declare -a PRODUCER_CMD=(
    env
    "GOCACHE=/tmp/perf-agent-gocache"
    "GOMODCACHE=/tmp/perf-agent-gomodcache"
    "GOTOOLCHAIN=auto"
    "PERF_AGENT_ROCPROFILER_SDK_COMMAND=bash -lc 'while kill -0 <pid> 2>/dev/null; do sleep 0.1; done; cat ${NATIVE_JSON}'"
    "PERF_AGENT_HIP_PID=<pid>"
    "PERF_AGENT_GPU_DURATION=${DURATION}"
    "PERF_AGENT_GPU_KERNEL_NAME=flash_attn_decode_bf16_gfx11"
    "${COLLECTOR_BIN}"
    --mode
    real
    --real-source
    rocprofiler-sdk
    --sleep-before-ms
    0
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
    "CPU + GPU Flame Graph: real_rust_hip_attention_rocprofiler_sdk"
    --input
    "${GPU_FOLDED}"
    --output
    "${GPU_SVG}"
    --html-output
    "${GPU_HTML}"
)

if [[ "${DRY_RUN}" == "1" ]]; then
    echo "build app:"
    quote_cmd "${BUILD_APP_CMD[@]}"
    echo
    echo "build perf-agent:"
    quote_cmd "${BUILD_AGENT_CMD[@]}"
    echo
    echo "build amd sample collector:"
    quote_cmd "${BUILD_COLLECTOR_CMD[@]}"
    echo
    echo "build rocprofiler-sdk preload bridge:"
    quote_cmd "${BUILD_BRIDGE_CMD[@]}"
    echo
    echo "build flamegraph renderer:"
    quote_cmd "${BUILD_RENDER_CMD[@]}"
    echo
    echo "run app:"
    printf '%s 3>%q\n' "$(quote_cmd env "LD_PRELOAD=${BRIDGE_SO}" "LD_LIBRARY_PATH=${ROCPROFILER_SDK_LIB_DIR}" "PERF_AGENT_ROCPROFILER_SDK_OUTPUT_FD=3" "PERF_AGENT_ROCPROFILER_SDK_DEBUG=${PERF_AGENT_ROCPROFILER_SDK_DEBUG:-}" "REAL_HIP_ATTENTION_LIBRARY=${HIP_LIBRARY}" "REAL_HIP_ATTENTION_ITERATIONS=${ITERATIONS}" "REAL_HIP_ATTENTION_SLEEP_BEFORE_MS=${SLEEP_BEFORE_MS}" "REAL_HIP_ATTENTION_SLEEP_BETWEEN_MS=${SLEEP_BETWEEN_MS}" "REAL_HIP_ATTENTION_SLEEP_AFTER_MS=${SLEEP_AFTER_MS}" "REAL_HIP_ATTENTION_CPU_SPIN=${CPU_SPIN}" "${APP_BIN}")" "${NATIVE_JSON}"
    echo
    echo "producer:"
    quote_cmd "${PRODUCER_CMD[@]}"
    echo
    echo "profile:"
    quote_cmd "${PROFILE_CMD[@]}"
    echo
    echo "render:"
    quote_cmd "${RENDER_CMD[@]}"
    exit 0
fi

mkdir -p "${APP_DIR}" "${OUTDIR}"
rm -f \
    "${CPU_PROFILE}" \
    "${GPU_RAW}" \
    "${GPU_ATTR}" \
    "${GPU_FOLDED}" \
    "${GPU_PPROF}" \
    "${GPU_SVG}" \
    "${GPU_HTML}" \
    "${NATIVE_JSON}" \
    "${APP_LOG}" \
    "${RUNNER_LOG}"

(
    cd "${REPO_ROOT}"
    "${BUILD_APP_CMD[@]}"
    "${BUILD_AGENT_CMD[@]}"
    "${BUILD_COLLECTOR_CMD[@]}"
    "${BUILD_BRIDGE_CMD[@]}"
    "${BUILD_RENDER_CMD[@]}"
)

set +e
(
    cd "${REPO_ROOT}"
    rm -f "${NATIVE_JSON}"
    env \
    LD_PRELOAD="${BRIDGE_SO}" \
    LD_LIBRARY_PATH="${ROCPROFILER_SDK_LIB_DIR}${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}" \
    PERF_AGENT_ROCPROFILER_SDK_OUTPUT_FD=3 \
    PERF_AGENT_ROCPROFILER_SDK_DEBUG="${PERF_AGENT_ROCPROFILER_SDK_DEBUG:-}" \
    REAL_HIP_ATTENTION_LIBRARY="${HIP_LIBRARY}" \
    REAL_HIP_ATTENTION_ITERATIONS="${ITERATIONS}" \
    REAL_HIP_ATTENTION_SLEEP_BEFORE_MS="${SLEEP_BEFORE_MS}" \
    REAL_HIP_ATTENTION_SLEEP_BETWEEN_MS="${SLEEP_BETWEEN_MS}" \
    REAL_HIP_ATTENTION_SLEEP_AFTER_MS="${SLEEP_AFTER_MS}" \
    REAL_HIP_ATTENTION_CPU_SPIN="${CPU_SPIN}" \
    "${APP_BIN}" 3>"${NATIVE_JSON}" >"${APP_LOG}" 2>&1 &
    APP_PID=$!
    trap 'kill "${APP_PID}" 2>/dev/null || true' EXIT

    sleep 1
    for i in "${!PROFILE_CMD[@]}"; do
        if [[ "${PROFILE_CMD[$i]}" == "<pid>" ]]; then
            PROFILE_CMD[$i]="${APP_PID}"
            break
        fi
    done
    for i in "${!PRODUCER_CMD[@]}"; do
        if [[ "${PRODUCER_CMD[$i]}" == "PERF_AGENT_HIP_PID=<pid>" ]]; then
            PRODUCER_CMD[$i]="PERF_AGENT_HIP_PID=${APP_PID}"
        elif [[ "${PRODUCER_CMD[$i]}" == *"<pid>"* ]]; then
            PRODUCER_CMD[$i]="${PRODUCER_CMD[$i]//<pid>/${APP_PID}}"
        fi
    done

    : >"${RUNNER_LOG}"
    printf 'producer command: ' >>"${RUNNER_LOG}"
    quote_cmd "${PRODUCER_CMD[@]}" >>"${RUNNER_LOG}"
    printf 'profile command: ' >>"${RUNNER_LOG}"
    quote_cmd "${PROFILE_CMD[@]}" >>"${RUNNER_LOG}"
    "${PRODUCER_CMD[@]}" 2>>"${RUNNER_LOG}" | "${PROFILE_CMD[@]}" >>"${RUNNER_LOG}" 2>&1
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
    "${GPU_HTML}" \
    "${NATIVE_JSON}" 2>/dev/null || true

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
echo "  ${NATIVE_JSON}"
echo "  ${APP_LOG}"
echo "  ${RUNNER_LOG}"
