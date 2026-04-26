// Command report aggregates one or more bench/cmd/scenario JSON outputs
// into a markdown summary.
package main

import (
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
)

func main() {
	var (
		inFlag     stringSlice
		diffArgs   stringSlice
		formatFlag = flag.String("format", "markdown", "output format: markdown | csv")
	)
	flag.Var(&inFlag, "in", "input JSON file (repeatable)")
	flag.Var(&diffArgs, "diff", "two JSON files to diff (use --diff a.json --diff b.json)")
	flag.Parse()

	if *formatFlag != "markdown" && *formatFlag != "csv" {
		fmt.Fprintln(os.Stderr, "format must be markdown or csv")
		os.Exit(2)
	}

	switch {
	case len(diffArgs) == 2:
		runDiff(os.Stdout, string(diffArgs[0]), string(diffArgs[1]), *formatFlag)
	case len(diffArgs) != 0:
		fmt.Fprintln(os.Stderr, "--diff requires exactly two arguments")
		os.Exit(2)
	case len(inFlag) > 0:
		runSummary(os.Stdout, inFlag, *formatFlag)
	default:
		fmt.Fprintln(os.Stderr, "usage: report --in PATH... | --diff A.json --diff B.json")
		os.Exit(2)
	}
}

type stringSlice []string

func (s *stringSlice) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// runSummary writes a markdown report covering each input doc.
func runSummary(w io.Writer, paths []string, format string) {
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", p, err)
			os.Exit(3)
		}
		doc, err := schema.Read(f)
		_ = f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", p, err)
			os.Exit(3)
		}
		writeSummary(w, doc, format)
		fmt.Fprintln(w)
	}
}

func writeSummary(w io.Writer, d *schema.Document, format string) {
	if format != "markdown" {
		fmt.Fprintf(os.Stderr, "csv output is not implemented in v1\n")
		os.Exit(4)
	}

	fmt.Fprintf(w, "# Scenario: `%s`\n\n", d.Scenario)
	fmt.Fprintf(w, "- **Started:** %s\n", d.StartedAt.UTC().Format("2006-01-02 15:04:05 UTC"))
	fmt.Fprintf(w, "- **Kernel:** %s · **CPU:** %s · **NCPU:** %d · **Go:** %s · **Commit:** %s\n",
		d.System.Kernel, d.System.CPUModel, d.System.NCPU, d.System.GoVersion, d.System.PerfAgentCommit)
	fmt.Fprintf(w, "- **Config:** processes=%d runs=%d drop_cache=%v\n\n",
		d.Config.Processes, d.Config.Runs, d.Config.DropCache)

	// Wall-time stats.
	totals := make([]float64, 0, len(d.Runs))
	for _, r := range d.Runs {
		totals = append(totals, r.TotalMs)
	}
	p50, p95, max := stats(totals)
	fmt.Fprintln(w, "## Wall time (newSession startup)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| metric | value (ms) |")
	fmt.Fprintln(w, "|--------|-----------|")
	fmt.Fprintf(w, "| p50 | %.1f |\n", p50)
	fmt.Fprintf(w, "| p95 | %.1f |\n", p95)
	fmt.Fprintf(w, "| max | %.1f |\n", max)
	fmt.Fprintln(w)

	// Top binaries by median compile_ms.
	type agg struct {
		path    string
		buildID string
		bytes   int
		samples []float64
	}
	byKey := map[string]*agg{}
	for _, r := range d.Runs {
		for _, b := range r.PerBinary {
			key := b.BuildID + "|" + b.Path
			a, ok := byKey[key]
			if !ok {
				a = &agg{path: b.Path, buildID: b.BuildID, bytes: b.EhFrameBytes}
				byKey[key] = a
			}
			a.samples = append(a.samples, b.CompileMs)
		}
	}
	rows := make([]*agg, 0, len(byKey))
	for _, a := range byKey {
		rows = append(rows, a)
	}
	sort.Slice(rows, func(i, j int) bool {
		return median(rows[i].samples) > median(rows[j].samples)
	})
	if n := 10; len(rows) > n {
		rows = rows[:n]
	}

	fmt.Fprintln(w, "## Top binaries by median compile_ms")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| binary | build_id | eh_frame_bytes | median compile_ms |")
	fmt.Fprintln(w, "|--------|----------|----------------|-------------------|")
	for _, a := range rows {
		fmt.Fprintf(w, "| `%s` | `%s` | %d | %.2f |\n", a.path, shortID(a.buildID), a.bytes, median(a.samples))
	}
}

// stats returns p50, p95, max of xs (sorts in place).
func stats(xs []float64) (p50, p95, max float64) {
	if len(xs) == 0 {
		return 0, 0, 0
	}
	sort.Float64s(xs)
	max = xs[len(xs)-1]
	p50 = xs[len(xs)/2]
	idx := int(math.Ceil(0.95*float64(len(xs)))) - 1
	if idx < 0 {
		idx = 0
	}
	p95 = xs[idx]
	return
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	return cp[len(cp)/2]
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// runDiff is implemented in Task 10.
func runDiff(w io.Writer, beforePath, afterPath, format string) {
	fmt.Fprintln(os.Stderr, "diff mode not yet implemented (see Task 10)")
	os.Exit(5)
}
