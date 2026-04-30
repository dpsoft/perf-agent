#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)
BLAZESYM_ROOT="/home/diego/github/blazesym"
BLAZESYM_LIB_DIR="${BLAZESYM_ROOT}/target/release"
BLAZESYM_INCLUDE_DIR="${BLAZESYM_ROOT}/capi/include"

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-offline-demo.sh [--dry-run] host-exec <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] hip-amd-sample <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] hip-amd-sample-rich <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] hip-rocprofv2-rich <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] hip-rocprofv2-command-rich <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] hip-rocprofv3-command-rich <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] hip-rocprofiler-sdk-command-rich <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] host-driver <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] multi-exec <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] multi-driver <outdir>
  scripts/gpu-offline-demo.sh [--dry-run] live-hip-amdsample <outdir> --pid <pid> --hip-library <path> [--hip-symbol <symbol>] [--duration <dur>]
  scripts/gpu-offline-demo.sh [--dry-run] live-hip-linuxdrm <outdir> --pid <pid> --hip-library <path> [--hip-symbol <symbol>] [--join-window <dur>] [--duration <dur>]
  scripts/gpu-offline-demo.sh [--dry-run] live-hip-linuxkfd <outdir> --pid <pid> --hip-library <path> [--hip-symbol <symbol>] [--join-window <dur>] [--duration <dur>]

Modes:
  host-exec         checked-in host->execution replay
  hip-amd-sample    checked-in host->AMD execution/sample stdin path
  hip-amd-sample-rich checked-in host->AMD execution/sample stdin path with richer function/source/pc frames
  hip-rocprofv2-rich checked-in host->rocprofv2->collector->AMD sample path with richer function/source/pc frames
  hip-rocprofv2-command-rich checked-in host->rocprofv2-command->collector->AMD sample path with richer function/source/pc frames
  hip-rocprofv3-command-rich checked-in host->rocprofv3-command->collector->AMD sample path with richer function/source/pc frames
  hip-rocprofiler-sdk-command-rich checked-in host->rocprofiler-sdk-command->collector->AMD sample path with richer function/source/pc frames
  host-driver       checked-in host->driver replay
  multi-exec        checked-in multi-workload execution replay
  multi-driver      checked-in multi-workload lifecycle replay
  live-hip-amdsample experimental live host HIP + AMD execution/sample stdin path
  live-hip-linuxdrm experimental live host HIP + linuxdrm path
  live-hip-linuxkfd experimental live host HIP + linuxkfd path

Outputs:
  <outdir>/<name>.raw.json
  <outdir>/<name>.attributions.json
  <outdir>/<name>.folded
  <outdir>/<name>.svg
  <outdir>/<name>.html
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
STDIN_PATH=""
AMD_SAMPLE_SOURCE_PATH=""
AMD_SAMPLE_SOURCE_COMMAND=""
AMD_SAMPLE_SOURCE_REAL_SOURCE=""
AMD_SAMPLE_SOURCE_COMMAND_ENV=""
AMD_SAMPLE_SOURCE_OUTPUT_ENV=""
AMD_SAMPLE_SOURCE_OUTPUT_FILE=""
NAME=""
DEBUG_GPU_LIVE=0
declare -a EXTRA_ARGS=()

case "${MODE}" in
    host-exec)
        HOST_REPLAY="gpu/testdata/host/replay/flash_attn_launches.json"
        GPU_REPLAY="gpu/testdata/replay/host_exec_sample.json"
        NAME="host_exec_sample"
        ;;
    hip-amd-sample)
        HOST_REPLAY="gpu/testdata/host/replay/hip_kfd_launches.json"
        STDIN_PATH="gpu/testdata/replay/amd_sample_exec.ndjson"
        NAME="amd_sample_exec"
        EXTRA_ARGS=("--gpu-amd-sample-stdin")
        ;;
    hip-amd-sample-rich)
        HOST_REPLAY="gpu/testdata/host/replay/hip_kfd_launches.json"
        STDIN_PATH="gpu/testdata/replay/amd_sample_exec_rich.ndjson"
        NAME="amd_sample_exec_rich"
        EXTRA_ARGS=("--gpu-amd-sample-stdin")
        ;;
    hip-rocprofv2-rich)
        HOST_REPLAY="gpu/testdata/host/replay/hip_kfd_launches.json"
        AMD_SAMPLE_SOURCE_PATH="scripts/emit-rocprofv2-rich-fixture.sh"
        AMD_SAMPLE_SOURCE_REAL_SOURCE="rocprofv2"
        AMD_SAMPLE_SOURCE_OUTPUT_ENV="PERF_AGENT_ROCPROFV2_OUTPUT_PATH"
        AMD_SAMPLE_SOURCE_OUTPUT_FILE="${OUTDIR}/rocprofv2_native_rich.ndjson"
        NAME="rocprofv2_sample_exec_rich"
        EXTRA_ARGS=("--gpu-amd-sample-stdin")
        ;;
    hip-rocprofv2-command-rich)
        HOST_REPLAY="gpu/testdata/host/replay/hip_kfd_launches.json"
        AMD_SAMPLE_SOURCE_COMMAND='scripts/emit-rocprofv2-rich-fixture.sh > "$PERF_AGENT_ROCPROFV2_OUTPUT_PATH"'
        AMD_SAMPLE_SOURCE_REAL_SOURCE="rocprofv2"
        AMD_SAMPLE_SOURCE_COMMAND_ENV="PERF_AGENT_ROCPROFV2_COMMAND"
        AMD_SAMPLE_SOURCE_OUTPUT_ENV="PERF_AGENT_ROCPROFV2_OUTPUT_PATH"
        AMD_SAMPLE_SOURCE_OUTPUT_FILE="${OUTDIR}/rocprofv2_native_rich.ndjson"
        NAME="rocprofv2_command_sample_exec_rich"
        EXTRA_ARGS=("--gpu-amd-sample-stdin")
        ;;
    hip-rocprofv3-command-rich)
        HOST_REPLAY="gpu/testdata/host/replay/hip_kfd_launches.json"
        AMD_SAMPLE_SOURCE_COMMAND='scripts/emit-rocprofv2-rich-fixture.sh > "$PERF_AGENT_ROCPROFV3_OUTPUT_PATH"'
        AMD_SAMPLE_SOURCE_REAL_SOURCE="rocprofv3"
        AMD_SAMPLE_SOURCE_COMMAND_ENV="PERF_AGENT_ROCPROFV3_COMMAND"
        AMD_SAMPLE_SOURCE_OUTPUT_ENV="PERF_AGENT_ROCPROFV3_OUTPUT_PATH"
        AMD_SAMPLE_SOURCE_OUTPUT_FILE="${OUTDIR}/rocprofv3_native_rich.ndjson"
        NAME="rocprofv3_command_sample_exec_rich"
        EXTRA_ARGS=("--gpu-amd-sample-stdin")
        ;;
    hip-rocprofiler-sdk-command-rich)
        HOST_REPLAY="gpu/testdata/host/replay/hip_kfd_launches.json"
        AMD_SAMPLE_SOURCE_COMMAND='cat gpu/testdata/replay/rocprofiler_sdk_native_rich.ndjson'
        AMD_SAMPLE_SOURCE_REAL_SOURCE="rocprofiler-sdk"
        AMD_SAMPLE_SOURCE_COMMAND_ENV="PERF_AGENT_ROCPROFILER_SDK_COMMAND"
        NAME="rocprofiler_sdk_command_sample_exec_rich"
        EXTRA_ARGS=("--gpu-amd-sample-stdin")
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
    live-hip-amdsample)
        if [[ -z "${PID}" ]]; then
            echo "live-hip-amdsample requires --pid" >&2
            exit 1
        fi
        if [[ -z "${HIP_LIBRARY}" ]]; then
            HIP_LIBRARY="$(discover_hip_library || true)"
        fi
        if [[ -z "${HIP_LIBRARY}" ]]; then
            echo "live-hip-amdsample requires --hip-library or PERF_AGENT_HIP_LIBRARY" >&2
            exit 1
        fi
        NAME="live_hip_amdsample"
        EXTRA_ARGS=(
            "--pid" "${PID}"
            "--gpu-amd-sample-stdin"
            "--gpu-host-hip-library" "${HIP_LIBRARY}"
            "--gpu-host-hip-symbol" "${HIP_SYMBOL}"
        )
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
    live-hip-linuxkfd)
        if [[ -z "${PID}" ]]; then
            echo "live-hip-linuxkfd requires --pid" >&2
            exit 1
        fi
        if [[ -z "${HIP_LIBRARY}" ]]; then
            HIP_LIBRARY="$(discover_hip_library || true)"
        fi
        if [[ -z "${HIP_LIBRARY}" ]]; then
            echo "live-hip-linuxkfd requires --hip-library or PERF_AGENT_HIP_LIBRARY" >&2
            exit 1
        fi
        NAME="live_hip_linuxkfd"
        DEBUG_GPU_LIVE=1
        EXTRA_ARGS=(
            "--pid" "${PID}"
            "--gpu-linux-kfd"
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
SVG_PATH="${OUTDIR}/${NAME}.svg"
HTML_PATH="${OUTDIR}/${NAME}.html"
PROFILE_PATH="${OUTDIR}/${NAME}.pb.gz"
RUNNER_LOG_PATH="${OUTDIR}/${NAME}.runner.log"

DEFAULT_LD_LIBRARY_PATH="${LD_LIBRARY_PATH:-${BLAZESYM_LIB_DIR}}"
DEFAULT_CGO_CFLAGS="${CGO_CFLAGS:-"-I /usr/include/bpf -I /usr/include/pcap -I ${BLAZESYM_INCLUDE_DIR}"}"
DEFAULT_CGO_LDFLAGS="${CGO_LDFLAGS:-"-L${BLAZESYM_LIB_DIR} -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"}"

declare -a CMD=(
    "env"
    "LD_LIBRARY_PATH=${DEFAULT_LD_LIBRARY_PATH}"
    "GOCACHE=${GOCACHE:-/tmp/perf-agent-gocache}"
    "GOMODCACHE=${GOMODCACHE:-/tmp/perf-agent-gomodcache}"
    "GOTOOLCHAIN=${GOTOOLCHAIN:-auto}"
    "CGO_CFLAGS=${DEFAULT_CGO_CFLAGS}"
    "CGO_LDFLAGS=${DEFAULT_CGO_LDFLAGS}"
    "go"
    "run"
    "."
)

declare -a AMD_SAMPLE_COLLECTOR_CMD=()
if [[ -n "${AMD_SAMPLE_SOURCE_PATH}" ]]; then
    AMD_SAMPLE_COLLECTOR_CMD=(
        "env"
        "GOCACHE=${GOCACHE:-/tmp/perf-agent-gocache}"
        "GOMODCACHE=${GOMODCACHE:-/tmp/perf-agent-gomodcache}"
        "GOTOOLCHAIN=${GOTOOLCHAIN:-auto}"
        "PERF_AGENT_ROCPROFV2_PATH=${REPO_ROOT}/${AMD_SAMPLE_SOURCE_PATH}"
        "PERF_AGENT_ROCPROFV2_OUTPUT_PATH=${AMD_SAMPLE_SOURCE_OUTPUT_FILE}"
        "go"
        "run"
        "./cmd/amd-sample-collector"
        "--mode"
        "real"
        "--real-source"
        "${AMD_SAMPLE_SOURCE_REAL_SOURCE}"
    )
elif [[ -n "${AMD_SAMPLE_SOURCE_COMMAND}" ]]; then
    AMD_SAMPLE_COLLECTOR_CMD=(
        "env"
        "GOCACHE=${GOCACHE:-/tmp/perf-agent-gocache}"
        "GOMODCACHE=${GOMODCACHE:-/tmp/perf-agent-gomodcache}"
        "GOTOOLCHAIN=${GOTOOLCHAIN:-auto}"
        "${AMD_SAMPLE_SOURCE_COMMAND_ENV}=${AMD_SAMPLE_SOURCE_COMMAND}"
        "go"
        "run"
        "./cmd/amd-sample-collector"
        "--mode"
        "real"
        "--real-source"
        "${AMD_SAMPLE_SOURCE_REAL_SOURCE}"
    )
    if [[ -n "${AMD_SAMPLE_SOURCE_OUTPUT_ENV}" ]]; then
        AMD_SAMPLE_COLLECTOR_CMD=(
            "${AMD_SAMPLE_COLLECTOR_CMD[@]:0:4}"
            "${AMD_SAMPLE_COLLECTOR_CMD[4]}"
            "${AMD_SAMPLE_SOURCE_OUTPUT_ENV}=${AMD_SAMPLE_SOURCE_OUTPUT_FILE}"
            "${AMD_SAMPLE_COLLECTOR_CMD[@]:5}"
        )
    fi
fi

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
    if [[ ${#AMD_SAMPLE_COLLECTOR_CMD[@]} -gt 0 ]]; then
        printf '%s | %s\n' "$(quote_cmd "${AMD_SAMPLE_COLLECTOR_CMD[@]}")" "$(quote_cmd "${CMD[@]}")"
    elif [[ -n "${STDIN_PATH}" ]]; then
        printf '%s < %s\n' "$(quote_cmd "${CMD[@]}")" "${STDIN_PATH}"
    else
        run_cmd "${CMD[@]}"
    fi
    exit 0
fi

set +e
(
    cd "${REPO_ROOT}"
    : >"${RUNNER_LOG_PATH}"
    printf 'runner command: ' >>"${RUNNER_LOG_PATH}"
    if [[ ${#AMD_SAMPLE_COLLECTOR_CMD[@]} -gt 0 ]]; then
        printf '%s | %s\n' "$(quote_cmd "${AMD_SAMPLE_COLLECTOR_CMD[@]}")" "$(quote_cmd "${CMD[@]}")" >>"${RUNNER_LOG_PATH}"
    elif [[ -n "${STDIN_PATH}" ]]; then
        printf '%s < %s\n' "$(quote_cmd "${CMD[@]}")" "${STDIN_PATH}" >>"${RUNNER_LOG_PATH}"
    else
        quote_cmd "${CMD[@]}" >>"${RUNNER_LOG_PATH}"
    fi
    if [[ "${DEBUG_GPU_LIVE}" == "1" ]]; then
        if [[ -n "${STDIN_PATH}" ]]; then
            PERF_AGENT_DEBUG_GPU_LIVE=1 "${CMD[@]}" <"${STDIN_PATH}" >>"${RUNNER_LOG_PATH}" 2>&1
        else
            PERF_AGENT_DEBUG_GPU_LIVE=1 "${CMD[@]}" >>"${RUNNER_LOG_PATH}" 2>&1
        fi
    else
        if [[ ${#AMD_SAMPLE_COLLECTOR_CMD[@]} -gt 0 ]]; then
            "${AMD_SAMPLE_COLLECTOR_CMD[@]}" | "${CMD[@]}" >>"${RUNNER_LOG_PATH}" 2>&1
        elif [[ -n "${STDIN_PATH}" ]]; then
            "${CMD[@]}" <"${STDIN_PATH}" >>"${RUNNER_LOG_PATH}" 2>&1
        else
            "${CMD[@]}" >>"${RUNNER_LOG_PATH}" 2>&1
        fi
    fi
)
runner_status=$?
set -e
printf 'runner exit status: %d\n' "${runner_status}" >>"${RUNNER_LOG_PATH}"
if [[ "${runner_status}" -ne 0 ]]; then
    exit "${runner_status}"
fi

if [[ -s "${FOLDED_PATH}" ]]; then
    (
        cd "${REPO_ROOT}"
        env \
            "LD_LIBRARY_PATH=${DEFAULT_LD_LIBRARY_PATH}" \
            "GOTOOLCHAIN=${GOTOOLCHAIN:-auto}" \
            "GOCACHE=${GOCACHE:-/tmp/perf-agent-gocache}" \
            "GOMODCACHE=${GOMODCACHE:-/tmp/perf-agent-gomodcache}" \
            "CGO_CFLAGS=${DEFAULT_CGO_CFLAGS}" \
            "CGO_LDFLAGS=${DEFAULT_CGO_LDFLAGS}" \
            go run ./cmd/flamegraph-svg \
            --title "GPU Flame Graph: ${NAME}" \
            --input "${FOLDED_PATH}" \
            --output "${SVG_PATH}" \
            --html-output "${HTML_PATH}" \
            >>"${RUNNER_LOG_PATH}" 2>&1
    )
else
    : >"${SVG_PATH}"
    : >"${HTML_PATH}"
fi

cat <<EOF
Wrote:
  ${RAW_PATH}
  ${ATTR_PATH}
  ${FOLDED_PATH}
  ${SVG_PATH}
  ${HTML_PATH}
  ${PROFILE_PATH}
  ${RUNNER_LOG_PATH}

Inspect join diagnostics with:
  jq '.join_stats' ${RAW_PATH}

Inspect workload attribution with:
  jq '.' ${ATTR_PATH}

Open flamegraph artifacts:
  ${SVG_PATH}
  ${HTML_PATH}
EOF

if command -v jq >/dev/null 2>&1; then
    echo
    echo "join_stats:"
    jq '.join_stats' "${RAW_PATH}"

    launch_count="$(jq -r '.join_stats.launch_count // 0' "${RAW_PATH}")"
    matched_launch_count="$(jq -r '.join_stats.matched_launch_count // 0' "${RAW_PATH}")"
    unmatched_launch_count="$(jq -r '.join_stats.unmatched_launch_count // 0' "${RAW_PATH}")"
    exact_execution_join_count="$(jq -r '.join_stats.exact_execution_join_count // 0' "${RAW_PATH}")"
    heuristic_execution_join_count="$(jq -r '.join_stats.heuristic_execution_join_count // 0' "${RAW_PATH}")"
    heuristic_event_join_count="$(jq -r '.join_stats.heuristic_event_join_count // 0' "${RAW_PATH}")"
    unmatched_candidate_event_count="$(jq -r '.join_stats.unmatched_candidate_event_count // 0' "${RAW_PATH}")"

    echo
    echo "join summary:"
    echo "  launches matched: ${matched_launch_count}/${launch_count}"
    echo "  exact execution joins: ${exact_execution_join_count}"
    echo "  heuristic execution joins: ${heuristic_execution_join_count}"
    echo "  heuristic event joins: ${heuristic_event_join_count}"
    echo "  unmatched launches: ${unmatched_launch_count}"
    echo "  unmatched candidate events: ${unmatched_candidate_event_count}"

    echo
    echo "tuning hint:"
    if (( unmatched_candidate_event_count > heuristic_event_join_count )); then
        echo "  many submit/wait events are unmatched; try a wider --join-window"
    elif (( unmatched_launch_count > matched_launch_count )); then
        echo "  many launches are unmatched; verify the target PID and HIP library path first"
    elif (( exact_execution_join_count > 0 || heuristic_execution_join_count > 0 || heuristic_event_join_count > 0 )); then
        echo "  join activity looks healthy; only widen --join-window if you still see missing lifecycle matches"
    else
        echo "  no joins were produced; verify the workload, PID, and data sources before tuning the window"
    fi
fi
