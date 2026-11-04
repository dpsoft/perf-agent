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
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

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

func run(profilePath, execPath string, asCSV bool) (err error) {
	raw, err := os.ReadFile(profilePath)
	if err != nil {
		return err
	}
	prof, err := profile.Parse(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", profilePath, err)
	}
	// One descriptor for both passes: the file verified is the file printed,
	// even if something replaces the path in between.
	f, err := os.Open(execPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// First pass: verify the whole log without keeping rows, so a corrupt
	// file prints nothing but the error and memory stays O(1).
	h, tr, err := execlog.Read(f, nil)
	if err != nil {
		return fmt.Errorf("%s: %w", execPath, err)
	}
	if err := tr.Verify(raw, len(prof.Sample)); err != nil {
		return err
	}
	if !asCSV {
		_, err := fmt.Printf("capture %s, backend %s, %s\n%d rows from %d executions over %d aggregated samples; verified against %s\n",
			cmp.Or(h.CaptureID, "(none)"), h.Backend, h.SampleType,
			tr.Rows, tr.Executions, tr.AggregatedSamples, profilePath)
		return err
	}

	// Second pass: stream the verified rows.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	bw := bufio.NewWriter(os.Stdout)
	out := csv.NewWriter(bw)
	if err := out.Write([]string{"sample", "leaf", "execution", "correlation", "value"}); err != nil {
		return err
	}
	_, _, err = execlog.Read(f, func(r execlog.Row) error {
		if int(r.Sample) >= len(prof.Sample) {
			return fmt.Errorf("row names sample %d of %d", r.Sample, len(prof.Sample))
		}
		corr := ""
		if r.HasCorrelation {
			corr = h.Backend + ":" + strconv.FormatUint(r.Correlation, 10)
		}
		return out.Write([]string{
			strconv.FormatUint(uint64(r.Sample), 10), leaf(prof.Sample[r.Sample]),
			strconv.FormatUint(r.Execution, 10), corr, strconv.FormatUint(r.Value, 10),
		})
	})
	out.Flush()
	return errors.Join(err, out.Error(), bw.Flush())
}

// leaf names a sample's innermost frame.
func leaf(s *profile.Sample) string {
	if len(s.Location) == 0 || len(s.Location[0].Line) == 0 || s.Location[0].Line[0].Function == nil {
		return ""
	}
	return s.Location[0].Line[0].Function.Name
}
