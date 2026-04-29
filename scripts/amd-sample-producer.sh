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

KERNEL_NAME="hip_launch_shim_kernel"
DEVICE_ID="gfx1103:0"
DEVICE_NAME="AMD Radeon 780M Graphics"
QUEUE_ID="compute:0"
SLEEP_BEFORE_MS="250"

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
sample1_ns="$((start_ns + 30000000))"
sample2_ns="$((start_ns + 90000000))"
end_ns="$((start_ns + 140000000))"

exec_corr="dispatch:${start_ns}"
sample1_corr="sample:${sample1_ns}"
sample2_corr="sample:${sample2_ns}"

printf '%s\n' "{\"kind\":\"exec\",\"execution\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"queue_id\":\"${QUEUE_ID}\",\"context_id\":\"ctx0\",\"exec_id\":\"${exec_corr}\"},\"correlation\":{\"backend\":\"amdsample\",\"value\":\"${exec_corr}\"},\"queue\":{\"backend\":\"amdsample\",\"device\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"name\":\"${DEVICE_NAME}\"},\"queue_id\":\"${QUEUE_ID}\"},\"kernel_name\":\"${KERNEL_NAME}\",\"start_ns\":${start_ns},\"end_ns\":${end_ns}}"
printf '%s\n' "{\"kind\":\"sample\",\"correlation\":{\"backend\":\"amdsample\",\"value\":\"${sample1_corr}\"},\"device\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"name\":\"${DEVICE_NAME}\"},\"time_ns\":${sample1_ns},\"kernel_name\":\"${KERNEL_NAME}\",\"stall_reason\":\"memory_wait\",\"weight\":11}"
printf '%s\n' "{\"kind\":\"sample\",\"correlation\":{\"backend\":\"amdsample\",\"value\":\"${sample2_corr}\"},\"device\":{\"backend\":\"amdsample\",\"device_id\":\"${DEVICE_ID}\",\"name\":\"${DEVICE_NAME}\"},\"time_ns\":${sample2_ns},\"kernel_name\":\"${KERNEL_NAME}\",\"stall_reason\":\"wave_barrier\",\"weight\":5}"
