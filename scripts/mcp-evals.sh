#!/usr/bin/env bash
# Local model-in-the-loop evals for perf-agent-mcp.
#
# This needs a model: it runs the `claude` CLI once per question in
# internal/perfmcp/evals/questions.json, so it costs tokens and is not run in
# CI. Only synthetic fixtures are used, so no capture leaves the machine.
#
# The script builds perf-agent-mcp, writes the fixtures, asks each question
# through `claude -p` with a generated MCP config (your own Claude MCP
# configuration is neither loaded nor modified), then checks the pages the
# agent rendered with TestEvalPages. It exits non-zero if any question fails.
#
# Usage: scripts/mcp-evals.sh
set -euo pipefail

for tool in claude go jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "mcp-evals: $tool is required and was not found in PATH" >&2
        exit 127
    fi
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"

work=$(mktemp -d)
failed=0
# The scratch directory holds the rendered pages and the agent's answers, so
# it is kept when anything failed.
cleanup() {
    if [ "$failed" -eq 0 ]; then
        rm -rf -- "$work"
    else
        echo "mcp-evals: kept $work (pages in out/, agent answers in <id>.answer)" >&2
    fi
}
trap cleanup EXIT
mkdir -p -- "$work/bin" "$work/profiles" "$work/out"

# perf-agent-mcp links no blazesym, so no cgo environment is needed.
go build -o "$work/bin/perf-agent-mcp" ./cmd/perf-agent-mcp

export PERF_AGENT_MCP_EVAL_DIR="$work"
go test ./internal/perfmcp/ -run '^TestWriteEvalFixtures$' -count=1

jq -n --arg bin "$work/bin/perf-agent-mcp" --arg profiles "$work/profiles" --arg out "$work/out" \
    '{mcpServers: {"perf-agent": {command: $bin, args: ["--profiles", $profiles, "--output", $out]}}}' \
    >"$work/mcp.json"

# The agent gets the perf-agent-mcp skill, as an installed skill would give it.
skill=$(cat "$root/skills/perf-agent-mcp/SKILL.md")

questions="$root/internal/perfmcp/evals/questions.json"
while IFS=$'\t' read -r id prompt; do
    echo "== $id" >&2
    # stdin is closed so claude cannot swallow the rest of the question list.
    if ! claude -p "$prompt Render the explanation to $id.html." \
        --mcp-config "$work/mcp.json" --strict-mcp-config --append-system-prompt "$skill" \
        --allowedTools 'mcp__perf-agent__*' </dev/null >"$work/$id.answer"; then
        echo "mcp-evals: claude failed on $id; answer in $work/$id.answer" >&2
        failed=1
    fi
done < <(jq -r '.[] | [.id, .prompt] | @tsv' "$questions")

go test ./internal/perfmcp/ -run '^TestEvalPages$' -count=1 -v || failed=1
if [ "$failed" -ne 0 ]; then
    echo "mcp-evals: FAILED; answers are $work/*.answer" >&2
fi
exit "$failed"
