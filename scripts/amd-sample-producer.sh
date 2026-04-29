#!/bin/bash

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  scripts/amd-sample-producer.sh [--kernel-name <name>] [--device-id <id>] [--device-name <name>] [--queue-id <id>] [--sleep-before-ms <ms>]

Emits a tiny AMD execution/sample NDJSON stream with producer-native correlation
IDs and boot-relative timestamps suitable for the live HIP + amdsample wrapper.
EOF
}

duration_to_ns() {
    local raw="$1"
    local value unit
    if [[ "${raw}" =~ ^([0-9]+)(ms|s)$ ]]; then
        value="${BASH_REMATCH[1]}"
        unit="${BASH_REMATCH[2]}"
    else
        echo "Unsupported PERF_AGENT_GPU_DURATION: ${raw}" >&2
        exit 1
    fi
    case "${unit}" in
        ms)
            echo "$((value * 1000000))"
            ;;
        s)
            echo "$((value * 1000000000))"
            ;;
    esac
}

boot_time_ns() {
    awk '{ printf "%.0f\n", $1 * 1000000000 }' /proc/uptime
}

sleep_ms() {
    local ms="$1"
    if [[ "${ms}" == "0" ]]; then
        return 0
    fi
    sleep "$(awk -v ms="${ms}" 'BEGIN { printf "%.3f", ms / 1000 }')"
}

KERNEL_NAME="${PERF_AGENT_GPU_KERNEL_NAME:-hip_launch_shim_kernel}"
DEVICE_ID="gfx1103:0"
DEVICE_NAME="AMD Radeon 780M Graphics"
QUEUE_ID="compute:0"
SLEEP_BEFORE_MS="250"
HIP_PID="${PERF_AGENT_HIP_PID:-}"
GPU_DURATION="${PERF_AGENT_GPU_DURATION:-140ms}"

while [[ $# -gt 0 ]]; do
    case "$1" in
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
        --sleep-before-ms)
            SLEEP_BEFORE_MS="${2:-}"
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

sleep_ms "${SLEEP_BEFORE_MS}"

start_ns="$(boot_time_ns)"
duration_ns="$(duration_to_ns "${GPU_DURATION}")"
sample1_offset_ns="$((duration_ns / 4))"
sample2_offset_ns="$(((duration_ns * 3) / 4))"
if (( sample1_offset_ns <= 0 )); then
    sample1_offset_ns=1
fi
if (( sample2_offset_ns <= sample1_offset_ns )); then
    sample2_offset_ns=$((sample1_offset_ns + 1))
fi
if (( sample2_offset_ns >= duration_ns )); then
    sample2_offset_ns=$((duration_ns - 1))
fi
if (( sample2_offset_ns <= sample1_offset_ns )); then
    sample1_offset_ns=1
    sample2_offset_ns=2
    duration_ns=3
fi
sample1_ns="$((start_ns + sample1_offset_ns))"
sample2_ns="$((start_ns + sample2_offset_ns))"
end_ns="$((start_ns + duration_ns))"

context_id="ctx0"
exec_corr="dispatch:${start_ns}"
if [[ -n "${HIP_PID}" ]]; then
    context_id="pid-${HIP_PID}"
    exec_corr="dispatch:${HIP_PID}:${start_ns}"
fi
sample1_corr="sample:${sample1_ns}"
sample2_corr="sample:${sample2_ns}"

printf '%s\n' "{\"kind\":\"exec\",\"execution\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"queue_id\":\"${QUEUE_ID}\",\"context_id\":\"${context_id}\",\"exec_id\":\"${exec_corr}\"},\"correlation\":{\"backend\":\"amdsample\",\"value\":\"${exec_corr}\"},\"queue\":{\"backend\":\"amdsample\",\"device\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"name\":\"${DEVICE_NAME}\"},\"queue_id\":\"${QUEUE_ID}\"},\"kernel_name\":\"${KERNEL_NAME}\",\"start_ns\":${start_ns},\"end_ns\":${end_ns}}"
printf '%s\n' "{\"kind\":\"sample\",\"correlation\":{\"backend\":\"amdsample\",\"value\":\"${sample1_corr}\"},\"device\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"name\":\"${DEVICE_NAME}\"},\"time_ns\":${sample1_ns},\"kernel_name\":\"${KERNEL_NAME}\",\"stall_reason\":\"memory_wait\",\"weight\":11}"
printf '%s\n' "{\"kind\":\"sample\",\"correlation\":{\"backend\":\"amdsample\",\"value\":\"${sample2_corr}\"},\"device\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"name\":\"${DEVICE_NAME}\"},\"time_ns\":${sample2_ns},\"kernel_name\":\"${KERNEL_NAME}\",\"stall_reason\":\"wave_barrier\",\"weight\":5}"
