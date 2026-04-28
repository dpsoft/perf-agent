#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/gpu-live-hip-linuxdrm.sh [--dry-run] [--outdir <dir>] [--pid <pid>] [--hip-library <path>] [--hip-symbol <symbol>] [--join-window <dur>] [--duration <dur>]

If --pid is omitted, the script starts a short delayed local workload
(rocminfo first, then amdgpu-arch) and targets that PID automatically.
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

discover_workload_tool() {
    local tool
    for tool in rocminfo amdgpu-arch; do
        if command -v "${tool}" >/dev/null 2>&1; then
            printf '%s\n' "${tool}"
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

WORKLOAD_PID=""
WORKLOAD_TOOL=""
if [[ -z "${PID}" ]]; then
    WORKLOAD_TOOL="$(discover_workload_tool || true)"
    if [[ -z "${WORKLOAD_TOOL}" ]]; then
        echo "could not discover local AMDGPU workload tool; pass --pid explicitly" >&2
        exit 1
    fi
fi

RAW_PATH="${OUTDIR}/live_hip_linuxdrm.raw.json"
ATTR_PATH="${OUTDIR}/live_hip_linuxdrm.attributions.json"
FOLDED_PATH="${OUTDIR}/live_hip_linuxdrm.folded"
PROFILE_PATH="${OUTDIR}/live_hip_linuxdrm.pb.gz"

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
    "live-hip-linuxdrm"
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
    SUDO_CMD[13]="${PID}"
else
    if [[ "${DRY_RUN}" == "1" ]]; then
        echo "would start local workload tool: ${WORKLOAD_TOOL}"
        SUDO_CMD[13]="<auto-pid>"
        quote_cmd "${SUDO_CMD[@]}"
        exit 0
    fi

    (
        sleep 1
        exec "${WORKLOAD_TOOL}" >/tmp/${WORKLOAD_TOOL}.out 2>/tmp/${WORKLOAD_TOOL}.err
    ) &
    WORKLOAD_PID=$!
    SUDO_CMD[13]="${WORKLOAD_PID}"
fi

cleanup() {
    if [[ -n "${WORKLOAD_PID}" ]]; then
        kill "${WORKLOAD_PID}" >/dev/null 2>&1 || true
        wait "${WORKLOAD_PID}" >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

if [[ "${DRY_RUN}" == "1" ]]; then
    quote_cmd "${SUDO_CMD[@]}"
    exit 0
fi

(
    cd "${REPO_ROOT}"
    "${SUDO_CMD[@]}"
)

echo
echo "wrapper summary:"
echo "  raw snapshot: ${RAW_PATH}"
echo "  attribution output: ${ATTR_PATH}"
echo "  folded output: ${FOLDED_PATH}"
echo "  profile output: ${PROFILE_PATH}"

if command -v jq >/dev/null 2>&1 && [[ -f "${RAW_PATH}" ]]; then
    echo
    echo "wrapper join_stats:"
    jq '.join_stats' "${RAW_PATH}"
fi
