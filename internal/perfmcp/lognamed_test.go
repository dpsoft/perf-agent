package perfmcp

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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

// markStringer is a Stringer whose text carries the mark.
type markStringer struct{}

func (markStringer) String() string { return "restart " + capture.CommandMark }

// namingLog is a logger through namingHandler, naming hub, that writes
// text lines to out.
func namingLog(out *lockedBuffer) *slog.Logger {
	return slog.New(namingHandler{next: slog.NewTextHandler(out, nil), command: func() string { return "perf-agent-hub" }})
}

// A nil *capture.Error, whose Error dereferences it, logs as the stock
// handler logs it, and does not panic.
func TestLogLinesTakeATypedNilError(t *testing.T) {
	var plain, named lockedBuffer
	var e *capture.Error
	slog.New(slog.NewTextHandler(&plain, nil)).Info("m", "err", e, "why", error(e))
	namingLog(&named).Info("m", "err", e, "why", error(e))
	strip := func(s string) string { _, rest, _ := strings.Cut(s, " level="); return rest }
	if strip(named.String()) != strip(plain.String()) {
		t.Errorf("a nil error logs %q, the stock handler %q", named.String(), plain.String())
	}
}

// A Stringer and a []error carrying the mark name the binary too.
func TestLogLinesNameTheBinaryInStringersAndErrorLists(t *testing.T) {
	var out lockedBuffer
	e := capture.Errorf(capture.CodeBinaryChanged, "restart "+capture.CommandMark, "changed")
	namingLog(&out).Info("m", "s", markStringer{}, "errs", []error{e, nil})
	got := out.String()
	if strings.Contains(got, capture.CommandMark) || strings.Count(got, "perf-agent-hub") != 2 {
		t.Errorf("the line does not name the binary in the Stringer and the error list: %s", got)
	}
}

// nopHandler handles every record and does nothing with it.
type nopHandler struct{}

func (nopHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (nopHandler) Handle(context.Context, slog.Record) error { return nil }
func (h nopHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h nopHandler) WithGroup(string) slog.Handler           { return h }

// A record with no mark goes to the next handler as it is: logging it
// allocates nothing more than without the naming handler.
func TestAMarkFreeRecordAllocatesNothingMore(t *testing.T) {
	plainErr := errors.New("plain")
	logWith := func(l *slog.Logger) float64 {
		return testing.AllocsPerRun(200, func() {
			// More attributes than a record holds inline, so a copy of
			// the record would allocate.
			l.LogAttrs(context.Background(), slog.LevelInfo, "opened",
				slog.String("path", "cpu.pb.gz"), slog.Int("n", 3), slog.Any("err", plainErr),
				slog.Group("g", slog.String("x", "y"), slog.Duration("d", time.Second)),
				slog.Bool("b", true), slog.String("view", "cpu"), slog.Int64("bytes", 1<<20))
		})
	}
	bare := logWith(slog.New(nopHandler{}))
	named := logWith(slog.New(namingHandler{next: nopHandler{}, command: func() string { return "perf-agent-hub" }}))
	if named != bare {
		t.Errorf("a mark-free record allocates %v times through the naming handler, %v without it", named, bare)
	}
}

func BenchmarkAMarkFreeRecord(b *testing.B) {
	l := slog.New(namingHandler{next: nopHandler{}, command: func() string { return "perf-agent-hub" }})
	plainErr := errors.New("plain")
	b.ReportAllocs()
	for b.Loop() {
		l.LogAttrs(context.Background(), slog.LevelInfo, "opened",
			slog.String("path", "cpu.pb.gz"), slog.Int("n", 3), slog.Any("err", plainErr),
			slog.Group("g", slog.String("x", "y")))
	}
}
