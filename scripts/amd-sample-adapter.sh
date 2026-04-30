#!/bin/bash

set -euo pipefail

usage() {
    cat <<'EOF'
Usage:
  scripts/amd-sample-adapter.sh

Adapts the live HIP + amdsample wrapper contract into an NDJSON producer.

Behavior:
  - if PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH is set, execs that program directly
  - if PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND is set, runs that command
  - otherwise falls back to the checked-in amd-sample-producer.sh

Relevant env:
  - PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH
  - PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND
  - PERF_AGENT_HIP_PID
  - PERF_AGENT_HIP_LIBRARY
  - PERF_AGENT_HIP_SYMBOL
  - PERF_AGENT_GPU_DURATION
  - PERF_AGENT_GPU_KERNEL_NAME
  - PERF_AGENT_GPU_DEVICE_ID
  - PERF_AGENT_GPU_DEVICE_NAME
  - PERF_AGENT_GPU_QUEUE_ID
EOF
}

if [[ $# -gt 0 ]]; then
    case "$1" in
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
fi

if [[ -n "${PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH:-}" ]]; then
    exec "${PERF_AGENT_AMD_SAMPLE_COLLECTOR_PATH}"
fi

if [[ -n "${PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND:-}" ]]; then
    exec bash -lc "${PERF_AGENT_AMD_SAMPLE_COLLECTOR_COMMAND}"
fi

exec bash scripts/amd-sample-producer.sh
