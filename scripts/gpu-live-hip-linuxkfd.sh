#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-live-hip-linuxkfd.sh [--dry-run] [--outdir <dir>] [--pid <pid>] [--hip-library <path>] [--hip-symbol <symbol>] [--join-window <dur>] [--duration <dur>]

Real runs require --pid to point at an existing HIP process. Dry-run mode
may omit --pid to preview the wrapped command with a placeholder PID.
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
JOIN_WINDOW="5ms"
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
        --join-window)
            JOIN_WINDOW="${2:-}"
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

RAW_PATH="${OUTDIR}/live_hip_linuxkfd.raw.json"
ATTR_PATH="${OUTDIR}/live_hip_linuxkfd.attributions.json"
FOLDED_PATH="${OUTDIR}/live_hip_linuxkfd.folded"
PROFILE_PATH="${OUTDIR}/live_hip_linuxkfd.pb.gz"
WRAPPER_LOG_PATH="${OUTDIR}/live_hip_linuxkfd_wrapper.log"

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
    "live-hip-linuxkfd"
    "${OUTDIR}"
    "--pid"
    ""
    "--hip-library"
    "${HIP_LIBRARY}"
    "--hip-symbol"
    "${HIP_SYMBOL}"
    "--join-window"
    "${JOIN_WINDOW}"
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
    quote_cmd "${SUDO_CMD[@]}"
    exit 0
else
    echo "live runs require --pid for an existing HIP process; auto-target is not supported yet" >&2
    exit 1
fi

if [[ "${DRY_RUN}" == "1" ]]; then
    quote_cmd "${SUDO_CMD[@]}"
    exit 0
fi

mkdir -p "${OUTDIR}"

set +e
(
    cd "${REPO_ROOT}"
    : >"${WRAPPER_LOG_PATH}"
    printf 'wrapper command: ' >>"${WRAPPER_LOG_PATH}"
    quote_cmd "${SUDO_CMD[@]}" >>"${WRAPPER_LOG_PATH}"
    "${SUDO_CMD[@]}" >>"${WRAPPER_LOG_PATH}" 2>&1
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
