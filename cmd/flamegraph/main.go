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
// Foreign profiles: perf-agent writes Sample.Location root-first, which is
// the reverse of what the pprof proto specifies. -stack-order exists for
// profiles from other producers; getting it wrong draws a plausible flame
// graph upside down, so the page always states which order it assumed.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	"github.com/google/pprof/profile"
)

func main() {
	var (
		out        = flag.String("o", "", "HTML output path (default: <input>.html)")
		title      = flag.String("title", "", "page title (default: the input file name)")
		folded     = flag.Bool("folded", false, "write folded stacks to stdout instead of HTML")
		sampleIdx  = flag.Int("sample-index", -1, "which sample type to fold; -1 chooses automatically")
		stackOrder = flag.String("stack-order", "root-first", "how the profile stores Sample.Location: root-first (perf-agent) | leaf-first (pprof proto)")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <profile.pb.gz>\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	in := flag.Arg(0)

	order := foldedstacks.RootFirst
	switch *stackOrder {
	case "root-first":
	case "leaf-first":
		order = foldedstacks.LeafFirst
	default:
		fatalf("unknown -stack-order %q: want root-first or leaf-first", *stackOrder)
	}

	if *folded {
		writeFolded(in, *sampleIdx, order)
		return
	}

	dst := *out
	if dst == "" {
		dst = in + ".html"
	}
	res, err := flamegraph.FromProfileFile(in, dst, flamegraph.Options{
		Title: *title,
	})
	if err != nil {
		fatalf("%v", err)
	}
	report(dst, res)
}

func writeFolded(in string, sampleIdx int, order foldedstacks.StackOrder) {
	res := fold(in, sampleIdx, order)
	n, err := res.WriteFolded(os.Stdout)
	if err != nil {
		fatalf("write folded: %v", err)
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d frame names contained ';' or a newline and were substituted; the folded text form has no escape for them\n", n)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "note: %s\n", w)
	}
}

func fold(in string, sampleIdx int, order foldedstacks.StackOrder) *foldedstacks.Result {
	f, err := os.Open(in)
	if err != nil {
		fatalf("open %s: %v", in, err)
	}
	defer func() { _ = f.Close() }()
	p, err := profile.Parse(f)
	if err != nil {
		fatalf("parse %s: %v", in, err)
	}
	res, err := foldedstacks.Fold(p, foldedstacks.Options{SampleIndex: sampleIdx, StackOrder: order})
	if err != nil {
		fatalf("fold %s: %v", in, err)
	}
	return res
}

// report prints the same honest numbers the page carries, so a user running
// this in a pipeline sees a degenerate profile without opening a browser.
func report(dst string, res *foldedstacks.Result) {
	fmt.Printf("wrote %s\n", dst)
	fmt.Printf("  axis     %s/%s\n", res.SampleTypeName, res.Unit)
	fmt.Printf("  total    %s across %d samples in %d distinct stacks\n",
		flamegraph.FormatValue(res.Total, res.Unit), res.Samples, len(res.Stacks))
	for _, w := range res.Warnings {
		fmt.Printf("  note     %s\n", w)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
