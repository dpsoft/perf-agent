#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-live-hip-amdsample.sh [--dry-run] [--outdir <dir>] [--pid <pid>] [--hip-library <path>] [--hip-symbol <symbol>] [--kernel-name <name>] [--device-id <id>] [--device-name <name>] [--queue-id <id>] [--sample-mode <synthetic|real>] [--real-source <rocm-smi>] [--rocm-smi-path <path>] [--real-poll-interval <dur>] [--sample-command <cmd>] [--sample-collector-path <path>] [--sample-collector-command <cmd>] [--duration <dur>]

Real runs require:
  - --pid to point at an existing HIP process
  - AMD sample NDJSON on stdout, either from:
      - --sample-collector-path
      - --sample-collector-command
      - --sample-command
      - PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH
      - PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND
      - the default checked-in adapter / producer path
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

pid_exists() {
    [[ -d "/proc/$1" ]]
}

pid_maps_hip() {
    local pid="$1"
    if grep -q 'libamdhip64' "/proc/${pid}/maps" 2>/dev/null; then
        return 0
    fi
    sudo grep -q 'libamdhip64' "/proc/${pid}/maps" 2>/dev/null
}

DRY_RUN=0
OUTDIR="/tmp/gpu-live"
PID=""
HIP_LIBRARY=""
HIP_SYMBOL="hipLaunchKernel"
KERNEL_NAME="${PERF_AGENT_GPU_KERNEL_NAME:-hip_launch_shim_kernel}"
DEVICE_ID="${PERF_AGENT_GPU_DEVICE_ID:-gfx1103:0}"
DEVICE_NAME="${PERF_AGENT_GPU_DEVICE_NAME:-AMD Radeon 780M Graphics}"
QUEUE_ID="${PERF_AGENT_GPU_QUEUE_ID:-compute:0}"
SAMPLE_MODE="${PERF_AGENT_AMD_SAMPLE_MODE:-synthetic}"
REAL_SOURCE="${PERF_AGENT_AMD_SAMPLE_REAL_SOURCE:-rocm-smi}"
ROCM_SMI_PATH="${PERF_AGENT_ROCM_SMI_PATH:-}"
REAL_POLL_INTERVAL="${PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL:-}"
SAMPLE_COMMAND=""
SAMPLE_COLLECTOR_PATH="${PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH:-}"
SAMPLE_COLLECTOR_COMMAND="${PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND:-}"
DURATION="2s"

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
        --sample-mode)
            SAMPLE_MODE="${2:-}"
            shift 2
            ;;
        --real-source)
            REAL_SOURCE="${2:-}"
            shift 2
            ;;
        --rocm-smi-path)
            ROCM_SMI_PATH="${2:-}"
            shift 2
            ;;
        --real-poll-interval)
            REAL_POLL_INTERVAL="${2:-}"
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
        --duration)
            DURATION="${2:-}"
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
if [[ -n "${PERF_AGENT_AMD_SAMPLE_COMMAND:-}" ]]; then
    echo "PERF_AGENT_AMD_SAMPLE_COMMAND is no longer supported; use --sample-command or PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND" >&2
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
if [[ "${DRY_RUN}" != "1" && -n "${SAMPLE_COLLECTOR_PATH}" && ! -x "${SAMPLE_COLLECTOR_PATH}" ]]; then
    echo "sample collector path is not executable: ${SAMPLE_COLLECTOR_PATH}" >&2
    exit 1
fi
if [[ "${DRY_RUN}" != "1" && "${REAL_SOURCE}" == "rocm-smi" && -n "${ROCM_SMI_PATH}" && ! -x "${ROCM_SMI_PATH}" ]]; then
    echo "rocm-smi path is not executable: ${ROCM_SMI_PATH}" >&2
    exit 1
fi
if [[ -z "${SAMPLE_COMMAND}" ]]; then
    SAMPLE_COMMAND="bash scripts/amd-sample-adapter.sh"
fi

RAW_PATH="${OUTDIR}/live_hip_amdsample.raw.json"
ATTR_PATH="${OUTDIR}/live_hip_amdsample.attributions.json"
FOLDED_PATH="${OUTDIR}/live_hip_amdsample.folded"
PROFILE_PATH="${OUTDIR}/live_hip_amdsample.pb.gz"
WRAPPER_LOG_PATH="${OUTDIR}/live_hip_amdsample_wrapper.log"

declare -a SUDO_CMD=(
    sudo
    /usr/bin/env
    "LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release"
    "GOCACHE=/tmp/perf-agent-gocache"
    "GOMODCACHE=/tmp/perf-agent-gomodcache"
    "GOTOOLCHAIN=auto"
    "CGO_CFLAGS=-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
    "CGO_LDFLAGS=-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic"
    bash
    "scripts/gpu-offline-demo.sh"
    "live-hip-amdsample"
    "${OUTDIR}"
    "--pid"
    ""
    "--hip-library"
    "${HIP_LIBRARY}"
    "--hip-symbol"
    "${HIP_SYMBOL}"
    "--duration"
    "${DURATION}"
)

if [[ -n "${PID}" ]]; then
    if [[ ! "${PID}" =~ ^[0-9]+$ ]]; then
        echo "pid must be numeric: ${PID}" >&2
        exit 1
    fi
    if [[ "${DRY_RUN}" != "1" ]]; then
        if ! pid_exists "${PID}"; then
            echo "pid does not exist: ${PID}" >&2
            exit 1
        fi
        if ! pid_maps_hip "${PID}"; then
            echo "pid does not map libamdhip64: ${PID}" >&2
            exit 1
        fi
    fi
    SUDO_CMD[13]="${PID}"
elif [[ "${DRY_RUN}" == "1" ]]; then
    echo "dry-run placeholder: pass --pid <live-hip-process-pid> for a real run"
    SUDO_CMD[13]="<pid>"
else
    echo "live runs require --pid for an existing HIP process" >&2
    exit 1
fi

declare -a PRODUCER_CMD=(
    env
    "PERF_AGENT_HIP_PID=${SUDO_CMD[13]}"
    "PERF_AGENT_HIP_LIBRARY=${HIP_LIBRARY}"
    "PERF_AGENT_HIP_SYMBOL=${HIP_SYMBOL}"
    "PERF_AGENT_GPU_DURATION=${DURATION}"
    "PERF_AGENT_GPU_KERNEL_NAME=${KERNEL_NAME}"
    "PERF_AGENT_GPU_DEVICE_ID=${DEVICE_ID}"
    "PERF_AGENT_GPU_DEVICE_NAME=${DEVICE_NAME}"
    "PERF_AGENT_GPU_QUEUE_ID=${QUEUE_ID}"
    "PERF_AGENT_AMD_SAMPLE_MODE=${SAMPLE_MODE}"
    "PERF_AGENT_AMD_SAMPLE_REAL_SOURCE=${REAL_SOURCE}"
    "PERF_AGENT_ROCM_SMI_PATH=${ROCM_SMI_PATH}"
    "PERF_AGENT_AMD_SAMPLE_REAL_POLL_INTERVAL=${REAL_POLL_INTERVAL}"
    "PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH=${SAMPLE_COLLECTOR_PATH}"
    "PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND=${SAMPLE_COLLECTOR_COMMAND}"
    bash
    -lc
    "${SAMPLE_COMMAND}"
)

if [[ "${DRY_RUN}" == "1" ]]; then
    printf '%s | %s\n' "$(quote_cmd "${PRODUCER_CMD[@]}")" "$(quote_cmd "${SUDO_CMD[@]}")"
    exit 0
fi

mkdir -p "${OUTDIR}"

sudo -v

set +e
(
    cd "${REPO_ROOT}"
    : >"${WRAPPER_LOG_PATH}"
    printf 'wrapper command: ' >>"${WRAPPER_LOG_PATH}"
    printf '%s | %s\n' "$(quote_cmd "${PRODUCER_CMD[@]}")" "$(quote_cmd "${SUDO_CMD[@]}")" >>"${WRAPPER_LOG_PATH}"
    "${PRODUCER_CMD[@]}" 2>>"${WRAPPER_LOG_PATH}" | "${SUDO_CMD[@]}" >>"${WRAPPER_LOG_PATH}" 2>&1
)
wrapper_status=$?
set -e
printf 'wrapper exit status: %d\n' "${wrapper_status}" >>"${WRAPPER_LOG_PATH}"
if [[ "${wrapper_status}" -ne 0 ]]; then
    exit "${wrapper_status}"
fi

echo
echo "wrapper summary:"
echo "  raw snapshot: ${RAW_PATH}"
echo "  attribution output: ${ATTR_PATH}"
echo "  folded output: ${FOLDED_PATH}"
echo "  profile output: ${PROFILE_PATH}"
echo "  wrapper log: ${WRAPPER_LOG_PATH}"

if command -v jq >/dev/null 2>&1 && [[ -f "${RAW_PATH}" ]]; then
    echo
    echo "wrapper join_stats:"
    jq '.join_stats' "${RAW_PATH}"
fi
