#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-offline-demo.sh [--dry-run] host-exec <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] host-driver <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] multi-exec <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] multi-driver <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] live-hip-linuxdrm <outdir> --pid <pid> --hip-library <path> [--hip-symbol <symbol>] [--join-window <dur>] [--duration <dur>]

Modes:
  host-exec         checked-in host->execution replay
  host-driver       checked-in host->driver replay
  multi-exec        checked-in multi-workload execution replay
  multi-driver      checked-in multi-workload lifecycle replay
  live-hip-linuxdrm experimental live host HIP + linuxdrm path

Outputs:
  <outdir>/<name>.raw.json
  <outdir>/<name>.attributions.json
  <outdir>/<name>.folded
  <outdir>/<name>.pb.gz
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

run_cmd() {
    if [[ "${DRY_RUN}" == "1" ]]; then
        quote_cmd "$@"
        return 0
    fi
    (
        cd "${REPO_ROOT}"
        "$@"
    )
}

DRY_RUN=0
if [[ "${1:-}" == "--dry-run" ]]; then
    DRY_RUN=1
    shift
fi

MODE="${1:-}"
OUTDIR="${2:-}"
if [[ -z "${MODE}" || -z "${OUTDIR}" ]]; then
    usage
    exit 1
fi
shift 2

PID=""
HIP_LIBRARY=""
HIP_SYMBOL="hipLaunchKernel"
DURATION="1ms"
JOIN_WINDOW="5ms"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --pid)
            PID="${2:-}"
            shift 2
            ;;
        --hip-library)
            HIP_LIBRARY="${2:-}"
            shift 2
            ;;
        --hip-symbol)
            HIP_SYMBOL="${2:-}"
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
        *)
            echo "Unknown argument: $1" >&2
            usage
            exit 1
            ;;
    esac
done

mkdir -p "${OUTDIR}"

HOST_REPLAY=""
GPU_REPLAY=""
NAME=""
declare -a EXTRA_ARGS=()

case "${MODE}" in
    host-exec)
        HOST_REPLAY="gpu/testdata/host/replay/flash_attn_launches.json"
        GPU_REPLAY="gpu/testdata/replay/host_exec_sample.json"
        NAME="host_exec_sample"
        ;;
    host-driver)
        HOST_REPLAY="gpu/testdata/host/replay/flash_attn_launches.json"
        GPU_REPLAY="gpu/testdata/replay/host_driver_submit.json"
        NAME="host_driver_submit"
        ;;
    multi-exec)
        HOST_REPLAY="gpu/testdata/host/replay/multi_workload_launches.json"
        GPU_REPLAY="gpu/testdata/replay/multi_workload_exec.json"
        NAME="multi_workload_exec"
        ;;
    multi-driver)
        HOST_REPLAY="gpu/testdata/host/replay/multi_workload_launches.json"
        GPU_REPLAY="gpu/testdata/replay/multi_workload_submit.json"
        NAME="multi_workload_submit"
        ;;
    live-hip-linuxdrm)
        if [[ -z "${PID}" ]]; then
            echo "live-hip-linuxdrm requires --pid" >&2
            exit 1
        fi
        if [[ -z "${HIP_LIBRARY}" ]]; then
            HIP_LIBRARY="$(discover_hip_library || true)"
        fi
        if [[ -z "${HIP_LIBRARY}" ]]; then
            echo "live-hip-linuxdrm requires --hip-library or PERF_AGENT_HIP_LIBRARY" >&2
            exit 1
        fi
        NAME="live_hip_linuxdrm"
        EXTRA_ARGS=(
            "--pid" "${PID}"
            "--gpu-linux-drm"
            "--gpu-host-hip-library" "${HIP_LIBRARY}"
            "--gpu-host-hip-symbol" "${HIP_SYMBOL}"
            "--gpu-hip-linuxdrm-join-window" "${JOIN_WINDOW}"
        )
        ;;
    *)
        echo "Unknown mode: ${MODE}" >&2
        usage
        exit 1
        ;;
esac

RAW_PATH="${OUTDIR}/${NAME}.raw.json"
ATTR_PATH="${OUTDIR}/${NAME}.attributions.json"
FOLDED_PATH="${OUTDIR}/${NAME}.folded"
PROFILE_PATH="${OUTDIR}/${NAME}.pb.gz"

declare -a CMD=("go" "run" ".")
if [[ -n "${HOST_REPLAY}" ]]; then
    CMD+=("--gpu-host-replay-input" "${HOST_REPLAY}")
fi
if [[ -n "${GPU_REPLAY}" ]]; then
    CMD+=("--gpu-replay-input" "${GPU_REPLAY}")
fi
CMD+=("${EXTRA_ARGS[@]}")
CMD+=(
    "--gpu-raw-output" "${RAW_PATH}"
    "--gpu-attribution-output" "${ATTR_PATH}"
    "--gpu-folded-output" "${FOLDED_PATH}"
    "--gpu-profile-output" "${PROFILE_PATH}"
    "--duration" "${DURATION}"
)

run_cmd "${CMD[@]}"

if [[ "${DRY_RUN}" == "1" ]]; then
    exit 0
fi

cat <<EOF
Wrote:
  ${RAW_PATH}
  ${ATTR_PATH}
  ${FOLDED_PATH}
  ${PROFILE_PATH}

Render folded output with:
  flamegraph.pl ${FOLDED_PATH} > ${OUTDIR}/${NAME}.svg
EOF
