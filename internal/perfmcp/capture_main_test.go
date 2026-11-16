package perfmcp

import (
	"os"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// TestMain lets this test binary serve as a capture's reaper and launcher,
// as perf-agent-mcp's own binary does, and builds the fake perf-agent and
// the workload the capture tests run.
func TestMain(m *testing.M) {
	if code, handled := capture.RunInternal(); handled {
		os.Exit(code)
	}
	os.Exit(capturetest.Main(m))
}
