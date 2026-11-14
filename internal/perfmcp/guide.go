package perfmcp

import (
	_ "embed"

	"github.com/dpsoft/perf-agent/internal/mcpstdio"
)

// guideText is the agent guide: how to investigate a profile with the tools
// and how to write explanations the engine accepts.
//
//go:embed guide.md
var guideText string

// instructions are sent to the client at initialize.
const instructions = "perf-agent-mcp answers questions about perf-agent profiles. " +
	"Read the perf-agent-guide prompt (or resource perf-agent://guide) before the first call."

// guidePrompts and guideResources serve the guide both ways, since clients
// differ in which of the two they surface.
func guidePrompts() []mcpstdio.Prompt {
	return []mcpstdio.Prompt{{
		Name:        "perf-agent-guide",
		Title:       "perf-agent guide",
		Description: "How to investigate a perf-agent profile and write explanations the engine accepts.",
		Text:        guideText,
	}}
}

func guideResources() []mcpstdio.Resource {
	return []mcpstdio.Resource{{
		URI:         "perf-agent://guide",
		Name:        "perf-agent-guide",
		Title:       "perf-agent guide",
		Description: "How to investigate a perf-agent profile and write explanations the engine accepts.",
		MIMEType:    "text/markdown",
		Text:        guideText,
	}}
}
