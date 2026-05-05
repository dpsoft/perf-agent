#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)

usage() {
    cat <<'EOF'
Usage:
  scripts/amd-v1-smoke.sh [--dry-run] [--outdir <dir>] [--hip-library <path>] [--rocprofiler-sdk-library <path>] [--rocprofiler-sdk-include-dir <path>] [--duration <dur>] [--iterations <n>] [--sleep-before-ms <ms>] [--sleep-between-ms <ms>] [--sleep-after-ms <ms>] [--cpu-spin <n>]

Runs the canonical AMD-first v1 path:
  real Rust HIP workload -> real rocprofiler-sdk bridge -> perf-agent -> HTML/SVG

Then validates that the expected CPU+GPU artifacts exist and contain both CPU-
and GPU-side structure.
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

print_failure_summary() {
    local runner_status="${1:-1}"
    echo "AMD v1 smoke failed (runner exit ${runner_status})." >&2
    if [[ -s "${RUNNER_LOG}" ]]; then
        echo >&2
        echo "runner log:" >&2
        sed -n '1,200p' "${RUNNER_LOG}" >&2
    fi
    if [[ -s "${APP_LOG}" ]]; then
        echo >&2
        echo "app log:" >&2
        sed -n '1,120p' "${APP_LOG}" >&2
    fi
    if [[ -f "${RUNNER_LOG}" ]] && grep -q 'no new privileges' "${RUNNER_LOG}"; then
        echo >&2
        echo "hint: this environment blocks sudo escalation with no_new_privileges; run the smoke on the target AMD host or adjust the container/sandbox policy." >&2
    elif [[ -f "${RUNNER_LOG}" ]] && grep -q 'a terminal is required\|password is required\|sudo:' "${RUNNER_LOG}"; then
        echo >&2
        echo "hint: run 'sudo -v' in your terminal first, then rerun scripts/amd-v1-smoke.sh." >&2
    fi
    if [[ -f "${APP_LOG}" ]] && grep -q 'no ROCm-capable device is detected\|hipGetDeviceCount -> err=100 count=0\|hipInit -> err=100' "${APP_LOG}"; then
        echo >&2
        echo "hint: no ROCm-capable AMD GPU was visible to the workload. Check the target host/device assignment and HIP library path." >&2
    fi
}

DRY_RUN=0
OUTDIR="/tmp/perf-agent-amd-v1"
declare -a RUNNER_ARGS=()

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
        --hip-library|--rocprofiler-sdk-library|--rocprofiler-sdk-include-dir|--duration|--iterations|--sleep-before-ms|--sleep-between-ms|--sleep-after-ms|--cpu-spin)
            RUNNER_ARGS+=("$1" "${2:-}")
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

declare -a RUNNER_CMD=(
    bash
    scripts/run-real-rust-rocprofiler-sdk-flamegraph.sh
    --outdir
    "${OUTDIR}"
    "${RUNNER_ARGS[@]}"
)

declare -a CHECKS=(
    "test -s ${CPU_PROFILE}"
    "test -s ${GPU_RAW}"
    "test -s ${GPU_ATTR}"
    "test -s ${GPU_FOLDED}"
    "test -s ${GPU_PPROF}"
    "test -s ${GPU_SVG}"
    "test -s ${GPU_HTML}"
    "test -s ${NATIVE_JSON}"
    "test -s ${APP_LOG}"
    "test -s ${RUNNER_LOG}"
    "grep -q '\"join_stats\"' ${GPU_RAW}"
    "grep -q 'CPU + GPU Flame Graph:' ${GPU_HTML}"
    "grep -q '\\[gpu:kernel:' ${GPU_FOLDED}"
    "grep -q '\\[gpu:function:' ${GPU_FOLDED}"
    "grep -Eq 'hipModuleLaunchKernel|real_hip_attention_workload::' ${GPU_FOLDED}"
)

if [[ "${DRY_RUN}" == "1" ]]; then
    echo "runner:"
    quote_cmd "${RUNNER_CMD[@]}" --dry-run
    echo
    echo "checks:"
    for check_cmd in "${CHECKS[@]}"; do
        printf '%s\n' "${check_cmd}"
    done
    exit 0
fi

mkdir -p "${OUTDIR}"

if ! sudo -n true >/dev/null 2>&1; then
    echo "amd-v1-smoke requires cached sudo credentials. Run 'sudo -v' first, then rerun this script." >&2
    exit 1
fi

set +e
(
    cd "${REPO_ROOT}"
    "${RUNNER_CMD[@]}"
)
RUNNER_STATUS=$?
set -e

if [[ "${RUNNER_STATUS}" -ne 0 ]]; then
    print_failure_summary "${RUNNER_STATUS}"
    exit "${RUNNER_STATUS}"
fi

for check_cmd in "${CHECKS[@]}"; do
    if ! bash -lc "${check_cmd}"; then
        echo "artifact check failed: ${check_cmd}" >&2
        print_failure_summary 1
        exit 1
    fi
done

echo "AMD v1 smoke passed:"
echo "  ${GPU_HTML}"
echo "  ${GPU_SVG}"
