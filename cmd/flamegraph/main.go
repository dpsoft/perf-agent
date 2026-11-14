// Command flamegraph renders a pprof profile as a self-contained interactive
// HTML flame graph, with no server, no CDN and no external script.
//
// It is the offline counterpart to perf-agent's --flamegraph-output flag:
// the flag renders a profile the agent just wrote, this renders one written
// earlier, by perf-agent or by anything else that emits pprof.
//
//	flamegraph -o out.html profile.pb.gz
//	flamegraph -folded profile.pb.gz          # the a;b;c 123 text form
//
// Two collectors, one picture:
//
//	flamegraph -fuse -o both.html \
//	  -in 'cpu.pb.gz;;[cpu] perf-agent 99 Hz' \
//	  -in 'gpu.pb.gz;;[gpu] per sampled launch'
//
// Each -in hangs under its own labelled root. The order is per-file
// because foreign inputs need not agree; it defaults to leaf-first.
//
// Foreign profiles: perf-agent writes Sample.Location leaf-first, as the
// pprof proto specifies. -stack-order exists for profiles from producers
// that deviate; getting it wrong draws a plausible flame graph upside down,
// so the page always states which order it assumed.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	"github.com/google/pprof/profile"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main with its streams and exit status made explicit.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		telemetry  = fs.String("gpu-telemetry", "", "NVML JSONL sidecar with matching capture_id; aggregate only observations in the profile window")
		out        = fs.String("o", "", "HTML output path (default: <input>.html; <input>.diff.html with -diff)")
		title      = fs.String("title", "", "page title (default: the input file name)")
		folded     = fs.Bool("folded", false, "write folded stacks to stdout instead of HTML")
		sampleIdx  = fs.Int("sample-index", -1, "which sample type to fold; -1 chooses automatically")
		stackOrder = fs.String("stack-order", "leaf-first",
			"how the profile stores Sample.Location: leaf-first (the pprof proto, "+
				"and what perf-agent writes) | root-first (foreign profiles from "+
				"producers that deviate)")
		rawVendor = fs.Bool("raw-vendor-frames", false,
			"draw every vendor frame separately instead of merging runs whose names say "+
				"nothing (an address, or the CUDA symbol server's obfuscated libfoo_<hex>). "+
				"The profile always contains them either way; this decides what the PICTURE "+
				"shows")
		fuse = fs.Bool("fuse", false,
			"draw several profiles as one graph, each under its own labelled root; "+
				"repeat -in once per profile")
		execlogPath = fs.String("execlog", "", "executions sidecar for the profile (never found implicitly); with -facts, -list-nodes or -diff")
		factsNode   = fs.String("facts", "", "print the path facts of one node as JSON; NODE is a node ID or a root-first name path a;b;c")
		listNodes   = fs.Int("list-nodes", 0, "print the N widest nodes as JSON lines {id, path, cum}")
		diffBase    = fs.String("diff", "", "compare the profile argument against this BASE profile; writes a page and the diff JSON")
		execlogBase = fs.String("execlog-base", "", "executions sidecar for the -diff BASE profile")
		inputs      fuseInputs
	)
	fs.Var(&inputs, "in",
		"a profile to fuse, as path[;stack-order[;label]] (repeatable). "+
			"stack-order defaults to leaf-first, label to the file's base name")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: %s [flags] <profile.pb.gz>\n\n", os.Args[0])
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	modelMode := set["facts"] || set["list-nodes"] || set["diff"] || set["execlog"] || set["execlog-base"]
	if modelMode && *fuse {
		return usageErr(stderr, "-facts, -list-nodes, -diff, -execlog and -execlog-base cannot be combined with -fuse")
	}

	if *fuse {
		if fs.NArg() != 0 {
			return failf(stderr, "-fuse takes its profiles via -in, not as positional arguments")
		}
		return runFuse(stdout, stderr, inputs, *out, *title, *rawVendor, *telemetry)
	}
	if len(inputs) > 0 {
		return failf(stderr, "-in is only meaningful with -fuse")
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	in := fs.Arg(0)

	order := foldedstacks.LeafFirst
	switch *stackOrder {
	case "leaf-first":
	case "root-first":
		order = foldedstacks.RootFirst
	default:
		return failf(stderr, "unknown -stack-order %q: want root-first or leaf-first", *stackOrder)
	}

	if modelMode {
		return runModel(stdout, stderr, modelArgs{
			in: in, out: *out, title: *title, sampleIdx: *sampleIdx,
			execlog: *execlogPath, execlogBase: *execlogBase,
			facts: *factsNode, listNodes: *listNodes, diffBase: *diffBase,
			set: set, rootFirst: order == foldedstacks.RootFirst, folded: *folded,
		})
	}

	if *folded {
		if *telemetry != "" {
			return failf(stderr, "-gpu-telemetry requires HTML output")
		}
		return writeFolded(stdout, stderr, in, *sampleIdx, order)
	}

	dst := *out
	if dst == "" {
		dst = in + ".html"
	}
	res, err := flamegraph.FromProfileFile(in, dst, flamegraph.Options{
		Title:            *title,
		GPUTelemetryPath: *telemetry,
		RawVendorFrames:  *rawVendor,
		StackOrder:       order,
	})
	if err != nil {
		return failf(stderr, "%v", err)
	}
	report(stdout, dst, res)
	return 0
}

func writeFolded(stdout, stderr io.Writer, in string, sampleIdx int, order foldedstacks.StackOrder) int {
	res, err := fold(in, sampleIdx, order)
	if err != nil {
		return failf(stderr, "%v", err)
	}
	n, err := res.WriteFolded(stdout)
	if err != nil {
		return failf(stderr, "write folded: %v", err)
	}
	if n > 0 {
		_, _ = fmt.Fprintf(stderr, "warning: %d frame names contained ';' or a newline and were substituted; the folded text form has no escape for them\n", n)
	}
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintf(stderr, "note: %s\n", w)
	}
	return 0
}

func fold(in string, sampleIdx int, order foldedstacks.StackOrder) (*foldedstacks.Result, error) {
	f, err := os.Open(in)
	if err != nil {
		return nil, fmt.Errorf("open %s: %v", in, err)
	}
	defer func() { _ = f.Close() }()
	p, err := profile.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %v", in, err)
	}
	res, err := foldedstacks.Fold(p, foldedstacks.Options{SampleIndex: sampleIdx, StackOrder: order})
	if err != nil {
		return nil, fmt.Errorf("fold %s: %v", in, err)
	}
	return res, nil
}

// report prints the same honest numbers the page carries, so a user running
// this in a pipeline sees a degenerate profile without opening a browser.
func report(w io.Writer, dst string, res *foldedstacks.Result) {
	_, _ = fmt.Fprintf(w, "wrote %s\n", dst)
	_, _ = fmt.Fprintf(w, "  axis     %s/%s\n", res.SampleTypeName, res.Unit)
	_, _ = fmt.Fprintf(w, "  total    %s across %d samples in %d distinct stacks\n",
		flamegraph.FormatValue(res.Total, res.Unit), res.Samples, len(res.Stacks))
	for _, w2 := range res.Warnings {
		_, _ = fmt.Fprintf(w, "  note     %s\n", w2)
	}
}

// failf prints a runtime failure and returns exit status 1.
func failf(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, format+"\n", args...)
	return 1
}

// usageErr prints a flag-combination error and returns exit status 2.
func usageErr(stderr io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(stderr, format+"\n", args...)
	return 2
}

// fuseInputs collects repeated -in flags.
//
// The separator is ';' rather than ',' or ':' because a frame name may not
// contain ';' -- the folded format has no escape for it, which is exactly
// why foldedstacks substitutes it -- so a label can never be ambiguous.
type fuseInputs []flamegraph.FuseFile

func (f *fuseInputs) String() string { return fmt.Sprintf("%d profile(s)", len(*f)) }

func (f *fuseInputs) Set(v string) error {
	path, rest, _ := strings.Cut(v, ";")
	if path == "" {
		return fmt.Errorf("empty path in %q", v)
	}
	in := flamegraph.FuseFile{Path: path}

	ord, label, _ := strings.Cut(rest, ";")
	switch ord {
	case "", "leaf-first":
	case "root-first":
		in.StackOrder = foldedstacks.RootFirst
	default:
		return fmt.Errorf("unknown stack order %q: want root-first or leaf-first", ord)
	}
	in.Label = label

	*f = append(*f, in)
	return nil
}

func runFuse(stdout, stderr io.Writer, inputs fuseInputs, out, title string, rawVendor bool, telemetry string) int {
	if len(inputs) < 2 {
		return failf(stderr, "-fuse needs at least 2 profiles; pass -in once per profile (got %d)", len(inputs))
	}
	dst := out
	if dst == "" {
		dst = "fused.html"
	}
	res, err := flamegraph.FuseProfileFiles(inputs, dst, flamegraph.Options{
		Title:            title,
		GPUTelemetryPath: telemetry,
		RawVendorFrames:  rawVendor,
	})
	if err != nil {
		return failf(stderr, "%v", err)
	}
	report(stdout, dst, res)
	return 0
}
