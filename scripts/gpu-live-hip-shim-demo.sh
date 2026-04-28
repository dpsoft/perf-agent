#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-live-hip-shim-demo.sh [--dry-run] [--outdir <dir>] [--binary <path>] [--hip-library <path>] [--join-window <dur>] [--duration <dur>] [--sleep-before-ms <ms>] [--sleep-after-ms <ms>]

Builds a tiny local HIP host process, launches it, then attaches the existing
live HIP + linuxdrm wrapper to that PID.
EOF
}

discover_hip_library() {
    local env_path="${PERF_AGENT_HIP_LIBRARY:-}"
    if [[ -n "${env_path}" && -e "${env_path}" ]]; then
        printf '%s\n' "${env_path}"
        return 0
    fi

    local candidate
    for candidate in \
        "/usr/local/lib/ollama/rocm/libamdhip64.so.6.3.60303" \
        "/usr/local/lib/ollama/rocm/libamdhip64.so.6" \
        "/opt/rocm/lib/libamdhip64.so"
    do
        if [[ -e "${candidate}" ]]; then
            printf '%s\n' "${candidate}"
            return 0
        fi
    done

    return 1
}

quote_cmd() {
    local parts=()
    local arg
    for arg in "$@"; do
        parts+=("$(printf '%q' "${arg}")")
    done
    printf '%s\n' "${parts[*]}"
}

DRY_RUN=0
OUTDIR="/tmp/gpu-live"
BINARY_PATH="/tmp/gpu-hip-launch-shim"
HIP_LIBRARY=""
JOIN_WINDOW="5ms"
DURATION="2s"
SLEEP_BEFORE_MS="2000"
SLEEP_AFTER_MS="4000"

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
        --binary)
            BINARY_PATH="${2:-}"
            shift 2
            ;;
        --hip-library)
            HIP_LIBRARY="${2:-}"
            shift 2
            ;;
        --join-window)
            JOIN_WINDOW="${2:-}"
            shift 2
            ;;
        --duration)
            DURATION="${2:-}"
            shift 2
            ;;
        --sleep-before-ms)
            SLEEP_BEFORE_MS="${2:-}"
            shift 2
            ;;
        --sleep-after-ms)
            SLEEP_AFTER_MS="${2:-}"
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

SOURCE_PATH="${SCRIPT_DIR}/hip-launch-shim.c"
LOG_PATH="${OUTDIR}/hip_launch_shim.log"

declare -a BUILD_CMD=(
    cc
    -O2
    -g
    -Wall
    -Wextra
    "${SOURCE_PATH}"
    -ldl
    -o
    "${BINARY_PATH}"
)

declare -a WRAPPER_CMD=(
    bash
    scripts/gpu-live-hip-linuxdrm.sh
    --outdir
    "${OUTDIR}"
    --pid
    "<shim-pid>"
    --hip-library
    "${HIP_LIBRARY}"
    --join-window
    "${JOIN_WINDOW}"
    --duration
    "${DURATION}"
)

if [[ "${DRY_RUN}" == "1" ]]; then
    quote_cmd "${BUILD_CMD[@]}"
    quote_cmd \
        env \
        "HIP_LAUNCH_SHIM_LIBRARY=${HIP_LIBRARY}" \
        "HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS=${SLEEP_BEFORE_MS}" \
        "HIP_LAUNCH_SHIM_SLEEP_AFTER_MS=${SLEEP_AFTER_MS}" \
        "${BINARY_PATH}"
    quote_cmd "${WRAPPER_CMD[@]}"
    exit 0
fi

mkdir -p "${OUTDIR}"
(
    cd "${REPO_ROOT}"
    "${BUILD_CMD[@]}"
)

HIP_LAUNCH_SHIM_LIBRARY="${HIP_LIBRARY}" \
HIP_LAUNCH_SHIM_SLEEP_BEFORE_MS="${SLEEP_BEFORE_MS}" \
HIP_LAUNCH_SHIM_SLEEP_AFTER_MS="${SLEEP_AFTER_MS}" \
"${BINARY_PATH}" >"${LOG_PATH}" 2>&1 &
shim_pid=$!

cleanup() {
    kill "${shim_pid}" >/dev/null 2>&1 || true
    wait "${shim_pid}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

(
    cd "${REPO_ROOT}"
    bash scripts/gpu-live-hip-linuxdrm.sh \
        --outdir "${OUTDIR}" \
        --pid "${shim_pid}" \
        --hip-library "${HIP_LIBRARY}" \
        --join-window "${JOIN_WINDOW}" \
        --duration "${DURATION}"
)

wait "${shim_pid}" || true
trap - EXIT

echo
echo "hip shim log: ${LOG_PATH}"
