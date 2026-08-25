package test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/metrics"
)

// These tests cover the "Context Switch Reasons" parser (issue #63).
// They load no BPF and spawn no workload, so they run without root or
// capabilities.
//
// The fixtures for the happy paths are not hand-written strings: they
// are produced by driving metrics.ConsoleExporter — the very code whose
// output the integration tests parse. That makes this a contract test
// between the two. If console.go's format drifts, these fail here, on
// an unprivileged machine, instead of surfacing as an unexplained
// integration failure on a runner.

// renderPMUReport runs the real console exporter over a snapshot and
// returns exactly what perf-agent --pmu would print.
func renderPMUReport(t *testing.T, systemWide, perPID bool, procs ...*metrics.ProcessMetrics) string {
	t.Helper()
	snap := metrics.NewMetricsSnapshot(systemWide)
	for _, p := range procs {
		snap.Processes[p.PID] = p
	}
	var buf bytes.Buffer
	exp := metrics.NewConsoleExporter(perPID)
	exp.Writer = &buf
	if err := exp.Export(context.Background(), snap); err != nil {
		t.Fatalf("console exporter failed: %v", err)
	}
	return buf.String()
}

func procWithSwitches(pid uint32, preempted, voluntary, ioWait uint64) *metrics.ProcessMetrics {
	return &metrics.ProcessMetrics{
		PID:         pid,
		SampleCount: 1000,
		ContextSwitches: metrics.ContextSwitchStats{
			PreemptedCount: preempted,
			VoluntaryCount: voluntary,
			IOWaitCount:    ioWait,
		},
	}
}

func TestParseContextSwitchReasonsSinglePID(t *testing.T) {
	out := renderPMUReport(t, false, false, procWithSwitches(4242, 812, 1904, 1483))
	if !strings.Contains(out, "I/O Wait (D state):") {
		t.Fatalf("fixture does not look like a PMU report:\n%s", out)
	}

	c, err := parseContextSwitchReasons(out)
	if err != nil {
		t.Fatalf("parse failed on real console output: %v\n%s", err, out)
	}
	if c.Blocks != 1 {
		t.Errorf("Blocks = %d, want 1", c.Blocks)
	}
	if c.Preempted != 812 || c.Voluntary != 1904 || c.IOWait != 1483 {
		t.Errorf("got %+v, want preempted=812 voluntary=1904 io_wait=1483", c)
	}
	if got, want := c.Total(), uint64(812+1904+1483); got != want {
		t.Errorf("Total() = %d, want %d", got, want)
	}
	if got, want := c.Blocking(), uint64(1904+1483); got != want {
		t.Errorf("Blocking() = %d, want %d", got, want)
	}
}

func TestParseContextSwitchReasonsAggregate(t *testing.T) {
	// System-wide without --per-pid: one "(aggregate)" block summing
	// every process.
	out := renderPMUReport(t, true, false,
		procWithSwitches(1, 10, 20, 30),
		procWithSwitches(2, 1, 2, 3),
	)
	if !strings.Contains(out, "Context Switch Reasons (aggregate):") {
		t.Fatalf("fixture is not the aggregate variant:\n%s", out)
	}

	c, err := parseContextSwitchReasons(out)
	if err != nil {
		t.Fatalf("parse failed on aggregate output: %v\n%s", err, out)
	}
	if c.Blocks != 1 {
		t.Errorf("Blocks = %d, want 1 (the aggregate prints a single block)", c.Blocks)
	}
	if c.Preempted != 11 || c.Voluntary != 22 || c.IOWait != 33 {
		t.Errorf("got %+v, want preempted=11 voluntary=22 io_wait=33", c)
	}
}

func TestParseContextSwitchReasonsSumsPerPIDBlocks(t *testing.T) {
	// System-wide --per-pid prints one block per process; the counts
	// are summed so "did this run classify any blocking" stays a
	// question about the run.
	out := renderPMUReport(t, true, true,
		procWithSwitches(1, 10, 20, 30),
		procWithSwitches(2, 1, 2, 3),
	)

	c, err := parseContextSwitchReasons(out)
	if err != nil {
		t.Fatalf("parse failed on per-pid output: %v\n%s", err, out)
	}
	if c.Blocks != 2 {
		t.Errorf("Blocks = %d, want 2", c.Blocks)
	}
	if c.Preempted != 11 || c.Voluntary != 22 || c.IOWait != 33 {
		t.Errorf("got %+v, want preempted=11 voluntary=22 io_wait=33", c)
	}
}

// TestParseContextSwitchReasonsSeesTheRegression is the point of the
// whole exercise: output in which every switch was filed as
// "Preempted" — a collector that lost D-state classification entirely —
// parses fine and reports Blocking() == 0. The old substring assertion
// passed on this output.
func TestParseContextSwitchReasonsSeesTheRegression(t *testing.T) {
	out := renderPMUReport(t, false, false, procWithSwitches(7, 5000, 0, 0))

	if !strings.Contains(out, "I/O Wait (D state):") || !strings.Contains(out, "Voluntary (sleep/mutex):") {
		t.Fatalf("both labels should still be printed - that is the defect:\n%s", out)
	}

	c, err := parseContextSwitchReasons(out)
	if err != nil {
		t.Fatalf("parse failed: %v\n%s", err, out)
	}
	if c.Blocking() != 0 {
		t.Errorf("Blocking() = %d, want 0 for an all-preempted report", c.Blocking())
	}
	if c.Total() != 5000 {
		t.Errorf("Total() = %d, want 5000", c.Total())
	}
}

// The mirror case, for TestPMUCPUWorkloadMostlyRunning: everything
// filed as voluntary, nothing preempted.
func TestParseContextSwitchReasonsSeesZeroPreempted(t *testing.T) {
	out := renderPMUReport(t, false, false, procWithSwitches(7, 0, 3000, 12))

	c, err := parseContextSwitchReasons(out)
	if err != nil {
		t.Fatalf("parse failed: %v\n%s", err, out)
	}
	if c.Preempted != 0 {
		t.Errorf("Preempted = %d, want 0", c.Preempted)
	}
	if c.Total() != 3012 {
		t.Errorf("Total() = %d, want 3012", c.Total())
	}
}

func TestParseContextSwitchReasonsNoBlock(t *testing.T) {
	// console.go omits the whole block when nothing was classified.
	out := renderPMUReport(t, false, false, procWithSwitches(7, 0, 0, 0))
	if strings.Contains(out, "Context Switch Reasons") {
		t.Fatalf("expected console.go to omit the block for zero switches:\n%s", out)
	}

	c, err := parseContextSwitchReasons(out)
	if !errors.Is(err, errNoContextSwitchBlock) {
		t.Fatalf("err = %v, want errNoContextSwitchBlock", err)
	}
	if errors.Is(err, errContextSwitchFormat) {
		t.Error("a missing block must NOT be reported as a format error: the two are retried differently")
	}
	if c.Total() != 0 {
		t.Errorf("Total() = %d, want 0", c.Total())
	}
}

func TestParseContextSwitchReasonsNoMetricsAtAll(t *testing.T) {
	out := renderPMUReport(t, false, false)
	if !strings.Contains(out, "No PMU metrics collected") {
		t.Fatalf("expected the empty-snapshot message:\n%s", out)
	}
	if _, err := parseContextSwitchReasons(out); !errors.Is(err, errNoContextSwitchBlock) {
		t.Fatalf("err = %v, want errNoContextSwitchBlock", err)
	}
}

// Every mutation below is a plausible edit to metrics/console.go's
// format. Each must produce a format error naming the line, never a
// silent zero and never a bare "no block".
func TestParseContextSwitchReasonsFormatChangesFailLoudly(t *testing.T) {
	const (
		preempted = uint64(812)
		voluntary = uint64(1904)
		ioWait    = uint64(1483)
		total     = preempted + voluntary + ioWait
	)
	good := renderPMUReport(t, false, false, procWithSwitches(9, preempted, voluntary, ioWait))

	preemptedLine := ctxLine(0, preempted, total)
	voluntaryLine := ctxLine(1, voluntary, total)
	ioWaitLine := ctxLine(2, ioWait, total)

	mutations := []struct {
		name string
		out  string
	}{
		{
			name: "count field renamed",
			out:  strings.ReplaceAll(good, " times)", " switches)"),
		},
		{
			name: "count dropped",
			out:  strings.Replace(good, ioWaitLine, "  I/O Wait (D state):      35.3%", 1),
		},
		{
			name: "percentage dropped",
			out:  strings.Replace(good, ioWaitLine, "  I/O Wait (D state):      (1483 times)", 1),
		},
		{
			name: "label reworded",
			out:  strings.Replace(good, "I/O Wait (D state):", "IO Wait (D-state):", 1),
		},
		{
			name: "reasons reordered",
			out: strings.Replace(good,
				preemptedLine+"\n"+voluntaryLine,
				voluntaryLine+"\n"+preemptedLine, 1),
		},
		{
			name: "report truncated after the header",
			out:  good[:strings.Index(good, "Context Switch Reasons:")+len("Context Switch Reasons:\n")],
		},
		{
			name: "block truncated mid-way",
			out:  good[:strings.Index(good, voluntaryLine)] + "(output truncated)\n",
		},
	}

	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			if m.out == good {
				t.Fatalf("mutation %q did not change the fixture; the format it targets moved:\n%s", m.name, good)
			}
			c, err := parseContextSwitchReasons(m.out)
			if err == nil {
				t.Fatalf("parse SUCCEEDED on mutated output (%+v) - a format change must fail loudly:\n%s", c, m.out)
			}
			if !errors.Is(err, errContextSwitchFormat) {
				t.Fatalf("err = %v, want it to wrap errContextSwitchFormat (got a retryable "+
					"no-block error instead, which would be waited on rather than reported)", err)
			}
			if !strings.Contains(err.Error(), "could not parse the counts from this output") {
				t.Errorf("error text does not name the failure: %v", err)
			}
		})
	}
}

// ctxLineFormats mirror the three Fprintf formats in
// metrics/console.go, so the mutation fixtures above can target a real
// line without hard-coding a percentage. If console.go's spacing
// changes these no longer match, the Replace becomes a no-op, and the
// `m.out == good` guard in the test above turns that into a failure
// rather than a vacuous pass.
var ctxLineFormats = [3]string{
	"  Preempted (running):     %.1f%%  (%d times)",
	"  Voluntary (sleep/mutex): %.1f%%  (%d times)",
	"  I/O Wait (D state):      %.1f%%  (%d times)",
}

func ctxLine(k int, n, total uint64) string {
	return fmt.Sprintf(ctxLineFormats[k], float64(n)/float64(total)*100, n)
}

func TestContextSwitchCountsString(t *testing.T) {
	c := contextSwitchCounts{Preempted: 1, Voluntary: 2, IOWait: 1, Blocks: 1}
	s := c.String()
	for _, want := range []string{"total=4", "preempted=1", "voluntary=2", "io_wait=1", "25.0%", "50.0%"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
	// An empty total must not divide by zero.
	if got := (contextSwitchCounts{}).Percent(0); got != 0 {
		t.Errorf("Percent on empty counts = %v, want 0", got)
	}
}
