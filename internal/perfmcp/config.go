// Package perfmcp is the perf-agent MCP server: tools that answer questions
// about perf-agent profiles from the profile model, over internal/mcpstdio.
// Every file it reads is opened through an os.Root, every integer it answers
// with is a decimal string, and every handle it hands out names evidence that
// stays open in this server only.
package perfmcp

import (
	"log/slog"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// Config is what a Server is built from. New copies it; changing a Config
// after New has no effect on the server.
type Config struct {
	// ProfilesDir is the root profiles and executions logs are read from.
	// It is required and must exist.
	ProfilesDir string
	// SourceDirs are the source roots; SourceManifests are source manifest
	// files. SourceGit allows read-only git probes in the source roots.
	SourceDirs, SourceManifests []string
	SourceGit                   bool
	// OutputDir is the root rendered pages are written to; "" disables
	// rendering.
	OutputDir string
	// Live enables the live page: the page_* tools, and a page the browser
	// reads from a loopback port the server opens at the first of them.
	// The stdio command never sets it, so it registers no page_* tool and
	// never listens; it is for a long-lived server and for tests.
	Live bool
	// Limits bound every input and response; they are fixed at New, which
	// fills in defaults for a zero Limits or a zero Model.
	Limits Limits
	// Version is reported to clients in serverInfo.
	Version string
	// Log receives diagnostics; nil discards them. It must not write to the
	// stream Serve answers on.
	Log *slog.Logger
	// Capture configures the capture tools.
	Capture CaptureConfig
}

// CaptureConfig configures capturing. With PerfAgent empty capture is
// disabled; otherwise New verifies the binary, the GPU shim and every
// command, and a failure is New's error.
type CaptureConfig struct {
	// PerfAgent is the absolute path of the setcap'd perf-agent binary.
	PerfAgent string
	// Dir is the captures directory; GPUShim the absolute path of the
	// CUPTI adapter a gpu capture injects, "" when gpu captures are off.
	Dir, GPUShim string
	// Owner is the kind of server that writes in Dir ("stdio" or
	// "serve"), so two kinds never share one captures directory.
	Owner string
	// Commands, when set, are the only programs a capture may launch, each
	// an absolute path.
	Commands []string
	// Verify checks PerfAgent; nil means capture.VerifyBinary. It is a seam
	// for tests, whose binaries cannot be setcap'd.
	Verify func(path string) (capture.Binary, error)
}
