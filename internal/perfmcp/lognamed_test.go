package perfmcp

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// A capture.Error logged as it is names the binary, as the agent's
// answers do, wherever the mark is: the message, a string, an error, an
// attribute bound with With, and one in a group.
func TestLogLinesNameTheBinary(t *testing.T) {
	const hub = "perf-agent-hub"
	var out lockedBuffer
	cfg := testConfig(t)
	cfg.Command = hub
	cfg.Log = slog.New(slog.NewTextHandler(&out, nil))
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()

	e := capture.Errorf(capture.CodeBinaryChanged, "restart "+capture.CommandMark, "perf-agent changed since start-up")
	s.log.With("bound", "start "+capture.CommandMark).WithGroup("g").Warn("the reaper of "+capture.CommandMark,
		"err", e, "why", e.Message+"; "+e.Next, slog.Group("inner", "err", error(e)), "n", 3)
	var got string
	for line := range strings.SplitSeq(out.String(), "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "the reaper of") {
			got = line
		}
	}
	if strings.Contains(got, capture.CommandMark) || strings.Count(got, hub) != 5 {
		t.Errorf("the log line does not name %s in each of its five places: %s", hub, got)
	}
	if !strings.Contains(got, "n=3") {
		t.Errorf("an attribute without the mark changed: %s", got)
	}
}
