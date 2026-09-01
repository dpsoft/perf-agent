// Command bpfload loads a compiled BPF object and reports, per program,
// whether the verifier accepted it and what it said.
//
// It exists because the agent embeds its objects with go:embed, so the only
// way to ask "does THIS object load on THIS kernel" was to rebuild the agent
// -- and rebuilding clears the file capabilities that let it load anything at
// all. That made every verifier experiment cost a privileged setcap round.
// This binary is capped once and then answers the question for any object.
//
//	bpfload profile/perf_dwarf_x86_bpfel.o
//	bpfload -v profile/perf_dwarf_x86_bpfel.o   # full verifier log on failure
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"

	"github.com/dpsoft/perf-agent/internal/kernelver"
)

func main() {
	verbose := flag.Bool("v", false, "print the full verifier log for failures")
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: bpfload [-v] <object.o> [...]")
		os.Exit(2)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		fmt.Fprintf(os.Stderr, "remove memlock: %v\n", err)
		os.Exit(1)
	}
	bad := 0
	for _, path := range flag.Args() {
		spec, err := ebpf.LoadCollectionSpec(path)
		if err != nil {
			fmt.Printf("%s: SPEC ERROR: %v\n", path, err)
			bad++
			continue
		}
		// A kprobe/uprobe-typed program makes cilium/ebpf probe the running
		// kernel version, and the probe is refused for a process running with
		// file capabilities -- which this binary is. The failure surfaces as
		// "REJECTED ... detecting kernel version", which reads exactly like a
		// verifier verdict and is not one. Supplying the version from uname
		// skips the probe, so a rejection here always means the verifier.
		// Shared with the agent's own loaders; see internal/kernelver.
		kernelver.Apply(spec)

		fmt.Printf("=== %s ===\n", path)
		for name, ps := range spec.Programs {
			// Load each program on its own so one rejection does not mask
			// the rest: the question is which programs the verifier takes.
			sub := &ebpf.CollectionSpec{
				Maps:      spec.Maps,
				Programs:  map[string]*ebpf.ProgramSpec{name: ps},
				Types:     spec.Types,
				ByteOrder: spec.ByteOrder,
			}
			opts := ebpf.CollectionOptions{}
			if *verbose {
				opts.Programs = ebpf.ProgramOptions{
					LogLevel:     ebpf.LogLevelBranch,
					LogSizeStart: 64 << 20,
				}
			}
			coll, err := ebpf.NewCollectionWithOptions(sub, opts)
			if err == nil {
				fmt.Printf("  %-34s OK (%d insns)\n", name, len(ps.Instructions))
				// The verifier's own summary is the number that matters:
				// "processed N insns (limit 1000000) ... peak_states P".
				// A program that loads at 950k is one kernel bump from the
				// failure we are here to fix, so headroom is the metric, not
				// pass/fail -- and it is only available when a log was asked for.
				if *verbose {
					if p := coll.Programs[name]; p != nil {
						for _, line := range strings.Split(p.VerifierLog, "\n") {
							if strings.HasPrefix(line, "processed ") {
								fmt.Printf("    %s\n", line)
							}
						}
					}
				}
				coll.Close()
				continue
			}
			bad++
			fmt.Printf("  %-34s REJECTED (%d insns)\n", name, len(ps.Instructions))
			var ve *ebpf.VerifierError
			if errors.As(err, &ve) {
				if *verbose {
					fmt.Printf("%+v\n", ve)
				} else {
					fmt.Printf("    %v\n", ve)
				}
			} else {
				fmt.Printf("    %v\n", err)
			}
		}
	}
	if bad > 0 {
		os.Exit(1)
	}
}
