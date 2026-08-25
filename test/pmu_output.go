package test

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// Parsing the PMU "Context Switch Reasons" block (issue #63)
// ---------------------------------------------------------------------
//
// The PMU integration tests used to assert on the PRESENCE of the
// context-switch labels:
//
//	strings.Contains(out, "I/O Wait (D state):") ||
//	strings.Contains(out, "Voluntary (sleep/mutex):")
//
// metrics/console.go prints all three of those lines inside a single
// `if totalSwitches > 0` guard, so they appear together or not at all:
// the `||` could not distinguish them, and what the assertion actually
// verified was "at least one context switch of any kind was recorded".
// A collector that attributed every blocked read to preemption and
// classified zero I/O wait passed it — the exact regression the test
// exists to catch.
//
// The counts are right there on the same lines:
//
//	  I/O Wait (D state):      37.2%  (1483 times)
//
// so this parser lifts them out and lets the tests assert on the
// VALUES. Presence of the label becomes a precondition, not a verdict.
//
// The parser is deliberately strict and loud. If metrics/console.go
// changes the line shape, every caller must fail with "could not parse
// the counts from this output" and the output attached — never fall
// back to a substring check that would pass by accident. That silent
// fallback is what this whole file exists to remove, so it must not be
// reintroduced in the parser's own error path.

var (
	// errNoContextSwitchBlock reports that the output carries no
	// "Context Switch Reasons" section at all.
	//
	// This is NOT a format failure: metrics/console.go omits the whole
	// block when the collection recorded zero context switches for the
	// target, and it prints "No PMU metrics collected" when no process
	// was seen. Both are data-sufficiency misses — the collection
	// caught nothing to classify — so callers retry them under a
	// deadline rather than failing immediately.
	errNoContextSwitchBlock = errors.New(`the output contains no "Context Switch Reasons" block`)

	// errContextSwitchFormat reports that a block WAS found but did not
	// have the shape metrics/console.go emits. Callers must treat this
	// as fatal: the counts this suite asserts on are unreadable, and
	// no weaker check may stand in for them.
	errContextSwitchFormat = errors.New("could not parse the counts from this output")
)

var (
	// Matches both variants printSinglePIDMetrics and
	// printAggregateMetrics emit ("Context Switch Reasons:" and
	// "Context Switch Reasons (aggregate):"). Neither is indented.
	ctxSwitchHeaderRe = regexp.MustCompile(`^Context Switch Reasons(?: \(aggregate\))?:$`)

	// Matches one classification line, e.g.
	//   "  I/O Wait (D state):      37.2%  (1483 times)"
	// Capturing the label, the percentage and the count means a change
	// to ANY part of the line shape — including the percentage this
	// parser does not aggregate — surfaces as a format error rather
	// than as a silently missing count.
	ctxSwitchLineRe = regexp.MustCompile(`^\s+(\S.*\S):\s+(\d+(?:\.\d+)?)%\s+\((\d+) times\)$`)
)

// ctxSwitchLabels are the three lines metrics/console.go prints under
// the header, in the order it prints them. The order is part of the
// contract this parser checks: a reordering is a format change.
var ctxSwitchLabels = [3]string{
	"Preempted (running)",
	"Voluntary (sleep/mutex)",
	"I/O Wait (D state)",
}

// ctxSwitchJudgeableFloor is the number of classified context switches
// below which the classification distribution is too thin to judge.
//
// Same role as degenerateSampleFloor in collect_until.go, and the same
// reasoning: a verdict on how switches were classified means nothing
// when there were two of them. A 5s window on either of the Go test
// workloads records switches in the hundreds or thousands — the Go
// runtime's sysmon thread alone parks and wakes on the order of 100
// times a second — so this floor is a "did the collection see the
// target at all" bar, not a performance expectation.
//
// It is a PRECONDITION for the collect-until loops, never the property
// under test: "at least 20 switches were classified somehow" is
// satisfied by a collector that files all 20 under the wrong reason,
// which is precisely the failure the assertions must still be able to
// report. See the loop-condition invariants in collect_until.go.
const ctxSwitchJudgeableFloor = 20

// contextSwitchCounts holds the counts parsed out of every "Context
// Switch Reasons" block in one PMU report.
//
// Counts are summed across blocks: --per-pid system-wide output prints
// one block per process, and "did this run classify any I/O wait" is a
// question about the run, not about a particular block. Percentages are
// deliberately NOT carried over — averaging or summing per-block
// percentages is meaningless, and Percent() recomputes them from the
// totals when a diagnostic needs them.
type contextSwitchCounts struct {
	Preempted uint64
	Voluntary uint64
	IOWait    uint64

	// Blocks is how many "Context Switch Reasons" blocks were summed.
	Blocks int
}

// Total is every classified switch.
func (c contextSwitchCounts) Total() uint64 { return c.Preempted + c.Voluntary + c.IOWait }

// Blocking is the switches where the task gave up the CPU because it
// could not proceed: a voluntary sleep (S) or an uninterruptible I/O
// wait (D).
//
// The tests assert on the SUM rather than on IOWait alone because which
// of the two a file operation lands in depends on the filesystem and
// the page-cache state: a write that hits page cache parks the task in
// S, the fsync behind it blocks in D, and a re-read served from cache
// blocks in neither. Requiring IOWait specifically would make the test
// a property of the runner's storage stack. Requiring the sum keeps it
// a property of the collector: an I/O-bound workload must show that it
// blocked, somewhere.
func (c contextSwitchCounts) Blocking() uint64 { return c.Voluntary + c.IOWait }

// Percent renders n as a share of Total, matching console.go's own
// one-decimal formatting. Returns 0 for an empty total.
func (c contextSwitchCounts) Percent(n uint64) float64 {
	total := c.Total()
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

// String is the one-line diagnostic quoted by the collect-until loops
// and by the assertion failures.
func (c contextSwitchCounts) String() string {
	return fmt.Sprintf(
		"context switches: total=%d preempted=%d (%.1f%%) voluntary=%d (%.1f%%) io_wait=%d (%.1f%%) [%d block(s)]",
		c.Total(),
		c.Preempted, c.Percent(c.Preempted),
		c.Voluntary, c.Percent(c.Voluntary),
		c.IOWait, c.Percent(c.IOWait),
		c.Blocks)
}

// parseContextSwitchReasons extracts the context-switch counts from a
// perf-agent --pmu report.
//
// Every block in the output is parsed and the counts summed. The three
// classification lines must follow the header in console.go's order
// with console.go's exact labels; anything else is a format error
// wrapping errContextSwitchFormat. An output with no block at all
// returns errNoContextSwitchBlock, which is a different condition
// entirely — see the sentinel's doc comment.
func parseContextSwitchReasons(out string) (contextSwitchCounts, error) {
	var c contextSwitchCounts
	dst := [3]*uint64{&c.Preempted, &c.Voluntary, &c.IOWait}

	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		if !ctxSwitchHeaderRe.MatchString(strings.TrimRight(lines[i], "\r")) {
			continue
		}
		header := i
		for k, label := range ctxSwitchLabels {
			i++
			if i >= len(lines) {
				return c, fmt.Errorf(
					"%w: the %q block at line %d ends after %d of its %d classification lines "+
						"(expected %q); the report is truncated or console.go's format changed",
					errContextSwitchFormat, lines[header], header+1, k, len(ctxSwitchLabels), label)
			}
			line := strings.TrimRight(lines[i], "\r")
			m := ctxSwitchLineRe.FindStringSubmatch(line)
			if m == nil {
				return c, fmt.Errorf(
					"%w: line %d should be the %q line of a %q block, formatted like "+
						`"  %s:      37.2%%  (1483 times)", but reads %q`,
					errContextSwitchFormat, i+1, label, lines[header], label, line)
			}
			if m[1] != label {
				return c, fmt.Errorf(
					"%w: line %d of the %q block is labelled %q, expected %q "+
						"(console.go prints the three reasons in a fixed order)",
					errContextSwitchFormat, i+1, lines[header], m[1], label)
			}
			// The percentage is not aggregated, but it must still be a
			// number: parsing it is how a change to that half of the
			// line becomes a loud failure instead of a silent one.
			if _, err := strconv.ParseFloat(m[2], 64); err != nil {
				return c, fmt.Errorf("%w: line %d of the %q block has an unreadable percentage %q: %w",
					errContextSwitchFormat, i+1, lines[header], m[2], err)
			}
			n, err := strconv.ParseUint(m[3], 10, 64)
			if err != nil {
				return c, fmt.Errorf("%w: line %d of the %q block has an unreadable count %q: %w",
					errContextSwitchFormat, i+1, lines[header], m[3], err)
			}
			*dst[k] += n
		}
		c.Blocks++
	}

	if c.Blocks == 0 {
		return c, errNoContextSwitchBlock
	}
	return c, nil
}

// pmuContextSwitchWindow is how long a single --pmu collection runs for
// in the context-switch tests. Unchanged from the fixed 5s window these
// tests used before; what changed is that a window catching too little
// is now retried rather than asserted on.
const pmuContextSwitchWindow = 5 * time.Second

// collectPMUContextSwitches runs `perf-agent --pmu ...` against a live
// workload until the report carries enough classified context switches
// to judge how they were classified, and returns the parsed counts
// together with the output they came from.
//
// The loop condition is `Total() >= ctxSwitchJudgeableFloor` — a
// DATA-SUFFICIENCY precondition, not the property any caller asserts.
// Both callers go on to make a claim about the SHAPE of the
// distribution (an I/O workload must show blocking switches; a
// CPU-bound one must show preemption), and a run in which every switch
// landed in the wrong bucket satisfies this condition on the first
// attempt and then fails the caller's assertion. That is the invariant
// collect_until.go's header sets out, and getting it backwards here
// would retry a real regression into a pass.
//
// The three outcomes are kept distinct on purpose:
//
//   - the agent itself failed            -> fatal, immediately
//   - console.go's format is unreadable  -> fatal, immediately, quoting
//     the output (never a fallback to a weaker check)
//   - no block / too few switches        -> retried, and fatal at the
//     deadline with the last output attached
func collectPMUContextSwitches(
	t *testing.T,
	agentPath string,
	workload *exec.Cmd,
	name string,
	agentArgs ...string,
) (contextSwitchCounts, string) {
	t.Helper()

	args := append(append([]string{}, agentArgs...), "--duration", pmuContextSwitchWindow.String())
	var (
		counts contextSwitchCounts
		last   string
	)

	ok, report := collectUntil(t,
		fmt.Sprintf("a PMU report classifying at least %d context switches", ctxSwitchJudgeableFloor),
		pmuContextSwitchWindow, collectBudgetFor(pmuContextSwitchWindow),
		func(int) (bool, string) {
			requireWorkloadAlive(t, workload, name)

			out, err := exec.Command(agentPath, args...).CombinedOutput()
			last = string(out)
			if err != nil {
				t.Fatalf("perf-agent %v failed: %v\nOutput: %s", args, err, last)
			}

			c, perr := parseContextSwitchReasons(last)
			if perr != nil {
				if errors.Is(perr, errNoContextSwitchBlock) {
					// console.go omits the block when it classified
					// nothing at all: the window caught the target
					// doing nothing, or the target was never seen.
					counts = contextSwitchCounts{}
					return false, perr.Error()
				}
				t.Fatalf("%v\n\nThis test asserts on the counts inside metrics/console.go's "+
					"\"Context Switch Reasons\" lines. The format no longer matches, so the "+
					"counts cannot be read — update the parser in test/pmu_output.go. There is "+
					"deliberately no weaker fallback: substring checks on these labels are what "+
					"issue #63 removed.\nFull output:\n%s", perr, last)
			}

			counts = c
			return c.Total() >= ctxSwitchJudgeableFloor, c.String()
		})
	if !ok {
		t.Fatalf("%s\nLast output:\n%s", report, last)
	}

	return counts, last
}
