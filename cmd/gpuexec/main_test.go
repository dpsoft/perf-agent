package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/google/pprof/profile"
)

// writeFixture writes a one-sample profile whose leaf is name, and a log for it.
func writeFixture(t *testing.T, name string) (profilePath, logPath string) {
	t.Helper()
	dir := t.TempDir()
	fn := &profile.Function{ID: 1, Name: name}
	loc := &profile.Location{ID: 1, Line: []profile.Line{{Function: fn}}}
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "gpu", Unit: "nanoseconds"}},
		Sample:   []*profile.Sample{{Location: []*profile.Location{loc}, Value: []int64{7}}},
		Location: []*profile.Location{loc}, Function: []*profile.Function{fn}}
	var raw bytes.Buffer
	if err := p.Write(&raw); err != nil {
		t.Fatal(err)
	}
	profilePath = filepath.Join(dir, "gpu.pb.gz")
	if err := os.WriteFile(profilePath, raw.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(dir, "gpu.exec")
	w, err := execlog.Create(logPath, execlog.Header{Backend: "cupti"})
	if err != nil {
		t.Fatal(err)
	}
	w.Add(execlog.Row{HasCorrelation: true, Correlation: 42, Value: 7})
	if err := w.Finish(1, execlog.ProfileSHA256(raw.Bytes())); err != nil {
		t.Fatal(err)
	}
	return profilePath, logPath
}

func withStdout(t *testing.T, f *os.File) {
	t.Helper()
	old := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = old })
}

// The CSV output is real CSV: a kernel name with quotes and commas survives a
// round trip through encoding/csv.
func TestCSVOutputIsValidCSV(t *testing.T) {
	name := `kernel"quoted, with comma`
	profilePath, logPath := writeFixture(t, name)
	out, err := os.Create(filepath.Join(t.TempDir(), "out.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	withStdout(t, out)
	if err := run(profilePath, logPath, true); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(out).ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV: %v", err)
	}
	want := [][]string{{"sample", "leaf", "execution", "correlation", "value"}, {"0", name, "0", "cupti:42", "7"}}
	if len(rows) != 2 || rows[1][1] != want[1][1] || rows[1][3] != want[1][3] || rows[1][4] != want[1][4] {
		t.Fatalf("rows %q, want %q", rows, want)
	}
}

// A failed write of the output is an error, not a silent success.
func TestCSVOutputWriteFailureIsAnError(t *testing.T) {
	profilePath, logPath := writeFixture(t, "k")
	closed, err := os.Create(filepath.Join(t.TempDir(), "closed.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	withStdout(t, closed)
	if err := run(profilePath, logPath, true); err == nil {
		t.Fatal("writing to a closed output reported success")
	}
}
