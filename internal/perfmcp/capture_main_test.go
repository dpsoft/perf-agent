package perfmcp

import (
	"os"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// testServerEnv, set to 1, makes this test binary a stdio server driven by
// the JSON config in testConfigEnv, which the multi-process tests start.
const (
	testServerEnv = "PERFMCP_TEST_SERVER"
	testConfigEnv = "PERFMCP_TEST_CONFIG"
)

// serverMain runs that server and returns its exit status; nil where the
// process tests do not build.
var serverMain func() int

// TestMain lets this test binary serve as a capture's reaper and launcher,
// as perf-agent-mcp's own binary does, and as a whole server for the
// multi-process tests; otherwise it builds the fake perf-agent and the
// workload the capture tests run.
func TestMain(m *testing.M) {
	if code, handled := capture.RunInternal(); handled {
		os.Exit(code)
	}
	if os.Getenv(testServerEnv) == "1" && serverMain != nil {
		os.Exit(serverMain())
	}
	os.Exit(capturetest.Main(m))
}
