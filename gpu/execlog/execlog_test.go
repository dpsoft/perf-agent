package execlog

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

var hdr = Header{CaptureID: "cap-1", Backend: "cupti", SampleType: "gpu/nanoseconds"}

func write(t *testing.T, dir string, rows []Row, aggregated uint64, profile []byte) string {
	t.Helper()
	path := filepath.Join(dir, "gpu.exec")
	w, err := Create(path, hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		w.Add(r)
	}
	if err := w.Finish(aggregated, ProfileSHA256(profile)); err != nil {
		t.Fatal(err)
	}
	return path
}

func readAll(t *testing.T, b []byte) ([]Row, Header, Trailer, error) {
	t.Helper()
	var got []Row
	h, tr, err := Read(bytes.NewReader(b), func(r Row) error { got = append(got, r); return nil })
	return got, h, tr, err
}

// Every row round-trips exactly, including the correlation edges the modular
// delta must survive and rows that carry no weight.
func TestRoundTripIsExact(t *testing.T) {
	rows := []Row{
		{Sample: 0, Execution: 0, HasCorrelation: true, Correlation: 0, Value: 100},
		{Sample: 2, Execution: 0, HasCorrelation: true, Correlation: 0, Value: 0}, // same exec (PC sample), zero weight
		{Sample: 1, Execution: 1, HasCorrelation: true, Correlation: math.MaxUint64, Value: 7},
		{Sample: 1, Execution: 2, HasCorrelation: true, Correlation: 5, Value: 9}, // wraps forward past MaxUint64
		{Sample: 0, Execution: 3, HasCorrelation: true, Correlation: 3, Value: 1}, // goes backwards
		{Sample: 2, Execution: 7, HasCorrelation: false, Value: 42},               // gap in ordinals, no correlation
		{Sample: 2, Execution: 8, HasCorrelation: true, Correlation: 1 << 63, Value: math.MaxUint64},
	}
	profile := []byte("profile bytes")
	path := write(t, t.TempDir(), rows, 3, profile)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, h, tr, err := readAll(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if h != hdr {
		t.Errorf("header %+v, want %+v", h, hdr)
	}
	if len(got) != len(rows) {
		t.Fatalf("%d rows, want %d", len(got), len(rows))
	}
	for i := range rows {
		if got[i] != rows[i] {
			t.Errorf("row %d: %+v, want %+v", i, got[i], rows[i])
		}
	}
	if tr.Rows != 7 || tr.Executions != 6 || tr.AggregatedSamples != 3 {
		t.Errorf("trailer %+v: want 7 rows, 6 executions, 3 aggregated", tr)
	}
	if err := tr.Verify(profile, 3); err != nil {
		t.Errorf("verify against its own profile: %v", err)
	}
}

// A log is bound to one profile: another profile's bytes, or a different
// sample count, is rejected.
func TestALogDoesNotVerifyAgainstAnotherProfile(t *testing.T) {
	path := write(t, t.TempDir(), []Row{{Execution: 0, Value: 1}}, 1, []byte("A"))
	b, _ := os.ReadFile(path)
	_, _, tr, err := readAll(t, b)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Verify([]byte("B"), 1) == nil {
		t.Error("verified against another profile's bytes")
	}
	if tr.Verify([]byte("A"), 2) == nil {
		t.Error("verified against a different aggregated sample count")
	}
}

// Truncation at any point, and any flipped byte, is rejected.
func TestTruncatedOrCorruptLogsAreRejected(t *testing.T) {
	path := write(t, t.TempDir(), []Row{
		{Sample: 0, Execution: 0, HasCorrelation: true, Correlation: 10, Value: 5},
		{Sample: 1, Execution: 1, HasCorrelation: true, Correlation: 11, Value: 6},
	}, 2, []byte("p"))
	good, _ := os.ReadFile(path)
	for n := range len(good) {
		if _, _, _, err := readAll(t, good[:n]); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("truncated to %d of %d bytes: err = %v, want ErrCorrupt", n, len(good), err)
		}
	}
	for i := range good {
		bad := bytes.Clone(good)
		bad[i] ^= 0x41
		if _, _, _, err := readAll(t, bad); err == nil {
			t.Fatalf("byte %d flipped: accepted", i)
		}
	}
	if _, _, _, err := readAll(t, append(bytes.Clone(good), 0)); !errors.Is(err, ErrCorrupt) {
		t.Errorf("trailing byte: err = %v", err)
	}
}

// A row naming a sample the profile does not have is rejected.
func TestSampleIndexOutOfRangeIsRejected(t *testing.T) {
	path := write(t, t.TempDir(), []Row{{Sample: 5, Execution: 0, Value: 1}}, 5, []byte("p"))
	b, _ := os.ReadFile(path)
	if _, _, _, err := readAll(t, b); !errors.Is(err, ErrCorrupt) {
		t.Errorf("index 5 of 5 samples: err = %v, want ErrCorrupt", err)
	}
}

// Ordinals are non-decreasing; a decrease is a bug, and the log is not
// published.
func TestADecreasingOrdinalInvalidatesTheLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpu.exec")
	w, err := Create(path, hdr)
	if err != nil {
		t.Fatal(err)
	}
	w.Add(Row{Execution: 3, Value: 1})
	w.Add(Row{Execution: 2, Value: 1})
	if w.Err() == nil {
		t.Fatal("decreasing ordinal accepted")
	}
	if err := w.Finish(1, [32]byte{}); err == nil {
		t.Fatal("Finish published an invalid log")
	}
	assertOnlyFiles(t, dir)
}

// A failed or aborted log leaves no temporary file and never removes a file
// that was already at the path.
func TestAbortKeepsAPreexistingFileAndLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gpu.exec")
	if err := os.WriteFile(path, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := Create(path, hdr)
	if err != nil {
		t.Fatal(err)
	}
	w.Add(Row{Execution: 0, Value: 1})
	w.Abort()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "someone else's" {
		t.Fatalf("pre-existing file changed: %q, %v", b, err)
	}
	assertOnlyFiles(t, dir, "gpu.exec")
}

func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	if !slices.Equal(got, want) {
		t.Fatalf("files in dir: %v, want %v", got, want)
	}
}

// The decoder never panics or loops on arbitrary input.
func FuzzRead(f *testing.F) {
	dir := f.TempDir()
	path := filepath.Join(dir, "seed.exec")
	w, _ := Create(path, hdr)
	w.Add(Row{Sample: 0, Execution: 0, HasCorrelation: true, Correlation: 1, Value: 2})
	_ = w.Finish(1, [32]byte{})
	seed, _ := os.ReadFile(path)
	f.Add(seed)
	f.Add([]byte(magic))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _, _ = Read(bytes.NewReader(b), nil)
	})
}
