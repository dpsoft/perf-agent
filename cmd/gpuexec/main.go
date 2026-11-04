// Command gpuexec reads the per-execution log that perf-agent writes beside an
// aggregated GPU profile (--gpu-executions-output) and checks it belongs to
// that profile. It prints a summary, or every row as CSV.
//
//	gpuexec -profile gpu.pb.gz -executions gpu.exec [-csv]
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"os"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/google/pprof/profile"
)

func main() {
	profilePath := flag.String("profile", "", "aggregated GPU profile (pprof) the log was written for")
	execPath := flag.String("executions", "", "executions log (--gpu-executions-output)")
	asCSV := flag.Bool("csv", false, "print every row: sample,leaf,execution,correlation,value")
	flag.Parse()
	if *profilePath == "" || *execPath == "" {
		fmt.Fprintln(os.Stderr, "usage: gpuexec -profile gpu.pb.gz -executions gpu.exec [-csv]")
		os.Exit(2)
	}
	if err := run(*profilePath, *execPath, *asCSV); err != nil {
		fmt.Fprintln(os.Stderr, "gpuexec:", err)
		os.Exit(1)
	}
}

func run(profilePath, execPath string, asCSV bool) error {
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return err
	}
	prof, err := profile.Parse(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", profilePath, err)
	}
	// First pass: verify the whole log without keeping rows, so a corrupt
	// file prints nothing but the error and memory stays O(1).
	h, tr, err := readLog(execPath, nil)
	if err != nil {
		return err
	}
	if err := tr.Verify(raw, len(prof.Sample)); err != nil {
		return err
	}
	if !asCSV {
		fmt.Printf("capture %s, backend %s, %s\n", cmp.Or(h.CaptureID, "(none)"), h.Backend, h.SampleType)
		fmt.Printf("%d rows from %d executions over %d aggregated samples; verified against %s\n",
			tr.Rows, tr.Executions, tr.AggregatedSamples, profilePath)
		return nil
	}
	// Second pass: stream the verified rows.
	w := bufio.NewWriter(os.Stdout)
	defer func() { _ = w.Flush() }()
	if _, err := fmt.Fprintln(w, "sample,leaf,execution,correlation,value"); err != nil {
		return err
	}
	_, _, err = readLog(execPath, func(r execlog.Row) error {
		corr := ""
		if r.HasCorrelation {
			corr = fmt.Sprintf("%s:%d", h.Backend, r.Correlation)
		}
		_, err := fmt.Fprintf(w, "%d,%q,%d,%s,%d\n", r.Sample, leaf(prof.Sample[r.Sample]), r.Execution, corr, r.Value)
		return err
	})
	return err
}

func readLog(path string, fn func(execlog.Row) error) (execlog.Header, execlog.Trailer, error) {
	f, err := os.Open(path)
	if err != nil {
		return execlog.Header{}, execlog.Trailer{}, err
	}
	defer func() { _ = f.Close() }()
	h, tr, err := execlog.Read(f, fn)
	if err != nil {
		err = fmt.Errorf("%s: %w", path, err)
	}
	return h, tr, err
}

// leaf names a sample's innermost frame.
func leaf(s *profile.Sample) string {
	if len(s.Location) == 0 || len(s.Location[0].Line) == 0 || s.Location[0].Line[0].Function == nil {
		return ""
	}
	return s.Location[0].Line[0].Function.Name
}
