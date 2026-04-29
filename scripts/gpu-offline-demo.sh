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
    set +e
    (
        cd "${REPO_ROOT}"
        "$@"
    )
    local status=$?
    set -e
    return "${status}"
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
DEBUG_GPU_LIVE=0
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
        DEBUG_GPU_LIVE=1
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
RUNNER_LOG_PATH="${OUTDIR}/${NAME}.runner.log"

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

if [[ "${DRY_RUN}" == "1" ]]; then
    run_cmd "${CMD[@]}"
    exit 0
fi

set +e
(
    cd "${REPO_ROOT}"
    : >"${RUNNER_LOG_PATH}"
    printf 'runner command: ' >>"${RUNNER_LOG_PATH}"
    quote_cmd "${CMD[@]}" >>"${RUNNER_LOG_PATH}"
    if [[ "${DEBUG_GPU_LIVE}" == "1" ]]; then
        PERF_AGENT_DEBUG_GPU_LIVE=1 "${CMD[@]}" >>"${RUNNER_LOG_PATH}" 2>&1
    else
        "${CMD[@]}" >>"${RUNNER_LOG_PATH}" 2>&1
    fi
)
runner_status=$?
set -e
printf 'runner exit status: %d\n' "${runner_status}" >>"${RUNNER_LOG_PATH}"
if [[ "${runner_status}" -ne 0 ]]; then
    exit "${runner_status}"
fi

cat <<EOF
Wrote:
  ${RAW_PATH}
  ${ATTR_PATH}
  ${FOLDED_PATH}
  ${PROFILE_PATH}
  ${RUNNER_LOG_PATH}

Inspect join diagnostics with:
  jq '.join_stats' ${RAW_PATH}

Inspect workload attribution with:
  jq '.' ${ATTR_PATH}

Render folded output with:
  flamegraph.pl ${FOLDED_PATH} > ${OUTDIR}/${NAME}.svg
EOF

if command -v jq >/dev/null 2>&1; then
    echo
    echo "join_stats:"
    jq '.join_stats' "${RAW_PATH}"

    launch_count="$(jq -r '.join_stats.launch_count // 0' "${RAW_PATH}")"
    matched_launch_count="$(jq -r '.join_stats.matched_launch_count // 0' "${RAW_PATH}")"
    unmatched_launch_count="$(jq -r '.join_stats.unmatched_launch_count // 0' "${RAW_PATH}")"
    exact_execution_join_count="$(jq -r '.join_stats.exact_execution_join_count // 0' "${RAW_PATH}")"
    heuristic_event_join_count="$(jq -r '.join_stats.heuristic_event_join_count // 0' "${RAW_PATH}")"
    unmatched_candidate_event_count="$(jq -r '.join_stats.unmatched_candidate_event_count // 0' "${RAW_PATH}")"

    echo
    echo "join summary:"
    echo "  launches matched: ${matched_launch_count}/${launch_count}"
    echo "  exact execution joins: ${exact_execution_join_count}"
    echo "  heuristic event joins: ${heuristic_event_join_count}"
    echo "  unmatched launches: ${unmatched_launch_count}"
    echo "  unmatched candidate events: ${unmatched_candidate_event_count}"

    echo
    echo "tuning hint:"
    if (( unmatched_candidate_event_count > heuristic_event_join_count )); then
        echo "  many submit/wait events are unmatched; try a wider --join-window"
    elif (( unmatched_launch_count > matched_launch_count )); then
        echo "  many launches are unmatched; verify the target PID and HIP library path first"
    elif (( exact_execution_join_count > 0 || heuristic_event_join_count > 0 )); then
        echo "  join activity looks healthy; only widen --join-window if you still see missing lifecycle matches"
    else
        echo "  no joins were produced; verify the workload, PID, and data sources before tuning the window"
    fi
fi
