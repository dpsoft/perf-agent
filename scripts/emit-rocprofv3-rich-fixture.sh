#!/bin/bash

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/.." && pwd)
FIXTURE_PATH="${REPO_ROOT}/gpu/testdata/replay/rocprofv3_native_rich.ndjson"

if [[ -n "${PERF_AGENT_ROCPROFV3_OUTPUT_PATH:-}" ]]; then
    cp "${FIXTURE_PATH}" "${PERF_AGENT_ROCPROFV3_OUTPUT_PATH}"
    exit 0
fi

cat "${FIXTURE_PATH}"
