#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)
WRAPPER_SCRIPT="${PERF_AGENT_GPU_LIVE_WRAPPER_SCRIPT:-}"

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-live-hip-shim-demo.sh [--dry-run] [--outdir <dir>] [--binary <path>] [--hip-library <path>] [--linux-surface <drm|kfd|amdsample>] [--kernel-name <name>] [--device-id <id>] [--device-name <name>] [--queue-id <id>] [--sample-command <cmd>] [--sample-collector-path <path>] [--sample-collector-command <cmd>] [--join-window <dur>] [--duration <dur>] [--sleep-before-ms <ms>] [--sleep-after-ms <ms>]

Builds a tiny local HIP host process, launches it, then attaches the existing
live HIP + linux wrapper to that PID.
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
LINUX_SURFACE="drm"
KERNEL_NAME="hip_launch_shim_kernel"
DEVICE_ID="gfx1103:0"
DEVICE_NAME="AMD Radeon 780M Graphics"
QUEUE_ID="compute:0"
SAMPLE_COMMAND=""
SAMPLE_COLLECTOR_PATH=""
SAMPLE_COLLECTOR_COMMAND=""
JOIN_WINDOW="5ms"
DURATION="2s"
SLEEP_BEFORE_MS="5000"
SLEEP_AFTER_MS="10000"

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
        --linux-surface)
            LINUX_SURFACE="${2:-}"
            shift 2
            ;;
        --kernel-name)
            KERNEL_NAME="${2:-}"
            shift 2
            ;;
        --device-id)
            DEVICE_ID="${2:-}"
            shift 2
            ;;
        --device-name)
            DEVICE_NAME="${2:-}"
            shift 2
            ;;
        --queue-id)
            QUEUE_ID="${2:-}"
            shift 2
            ;;
        --sample-command)
            SAMPLE_COMMAND="${2:-}"
            shift 2
            ;;
        --sample-collector-path)
            SAMPLE_COLLECTOR_PATH="${2:-}"
            shift 2
            ;;
        --sample-collector-command)
            SAMPLE_COLLECTOR_COMMAND="${2:-}"
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

if [[ -z "${WRAPPER_SCRIPT}" ]]; then
    case "${LINUX_SURFACE}" in
        drm)
            WRAPPER_SCRIPT="scripts/gpu-live-hip-linuxdrm.sh"
            ;;
        kfd)
            WRAPPER_SCRIPT="scripts/gpu-live-hip-linuxkfd.sh"
            ;;
        amdsample)
            WRAPPER_SCRIPT="scripts/gpu-live-hip-amdsample.sh"
            ;;
        *)
            echo "unsupported --linux-surface: ${LINUX_SURFACE}" >&2
            exit 1
            ;;
    esac
fi

if [[ -z "${HIP_LIBRARY}" ]]; then
    HIP_LIBRARY="$(discover_hip_library || true)"
fi
if [[ -z "${HIP_LIBRARY}" ]]; then
    echo "could not discover HIP library; pass --hip-library or set PERF_AGENT_HIP_LIBRARY" >&2
    exit 1
fi
if [[ -n "${SAMPLE_COMMAND}" && -n "${SAMPLE_COLLECTOR_PATH}" ]]; then
    echo "cannot combine --sample-command with --sample-collector-path" >&2
    exit 1
fi
if [[ -n "${SAMPLE_COMMAND}" && -n "${SAMPLE_COLLECTOR_COMMAND}" ]]; then
    echo "cannot combine --sample-command with --sample-collector-command" >&2
    exit 1
fi
if [[ -n "${SAMPLE_COLLECTOR_PATH}" && -n "${SAMPLE_COLLECTOR_COMMAND}" ]]; then
    echo "cannot combine --sample-collector-path with --sample-collector-command" >&2
    exit 1
fi
if [[ "${DRY_RUN}" != "1" && "${LINUX_SURFACE}" == "amdsample" && -n "${SAMPLE_COLLECTOR_PATH}" && ! -x "${SAMPLE_COLLECTOR_PATH}" ]]; then
    echo "sample collector path is not executable: ${SAMPLE_COLLECTOR_PATH}" >&2
    exit 1
fi
SOURCE_PATH="${SCRIPT_DIR}/hip-launch-shim.c"
LOG_PATH="${OUTDIR}/hip_launch_shim.log"
WRAPPER_LOG_PATH="${OUTDIR}/gpu_live_wrapper.log"

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
    "${WRAPPER_SCRIPT}"
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
if [[ "${LINUX_SURFACE}" == "amdsample" ]]; then
    if [[ -n "${KERNEL_NAME}" ]]; then
        WRAPPER_CMD+=(
            --kernel-name
            "${KERNEL_NAME}"
        )
    fi
    if [[ -n "${DEVICE_ID}" ]]; then
        WRAPPER_CMD+=(
            --device-id
            "${DEVICE_ID}"
        )
    fi
    if [[ -n "${DEVICE_NAME}" ]]; then
        WRAPPER_CMD+=(
            --device-name
            "${DEVICE_NAME}"
        )
    fi
    if [[ -n "${QUEUE_ID}" ]]; then
        WRAPPER_CMD+=(
            --queue-id
            "${QUEUE_ID}"
        )
    fi
    if [[ -n "${SAMPLE_COLLECTOR_PATH}" ]]; then
        WRAPPER_CMD+=(
            --sample-collector-path
            "${SAMPLE_COLLECTOR_PATH}"
        )
    fi
    if [[ -n "${SAMPLE_COLLECTOR_COMMAND}" ]]; then
        WRAPPER_CMD+=(
            --sample-collector-command
            "${SAMPLE_COLLECTOR_COMMAND}"
        )
    fi
    if [[ -n "${SAMPLE_COMMAND}" ]]; then
        WRAPPER_CMD+=(
            --sample-command
            "${SAMPLE_COMMAND}"
        )
    fi
fi

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

# Acquire sudo credentials before starting the short-lived shim process so the
# password prompt cannot consume the attach window.
sudo -v

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

set +e
(
    cd "${REPO_ROOT}"
    : >"${WRAPPER_LOG_PATH}"
    printf 'wrapper command: ' >>"${WRAPPER_LOG_PATH}"
    quote_cmd "${WRAPPER_CMD[@]/<shim-pid>/${shim_pid}}" >>"${WRAPPER_LOG_PATH}"
    "${WRAPPER_CMD[@]/<shim-pid>/${shim_pid}}" >>"${WRAPPER_LOG_PATH}" 2>&1
)
wrapper_status=$?
set -e
printf 'wrapper exit status: %d\n' "${wrapper_status}" >>"${WRAPPER_LOG_PATH}"
if [[ "${wrapper_status}" -ne 0 ]]; then
    exit "${wrapper_status}"
fi

wait "${shim_pid}" || true
trap - EXIT

echo
echo "hip shim log: ${LOG_PATH}"
echo "wrapper log: ${WRAPPER_LOG_PATH}"
