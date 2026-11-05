package gpuprofile

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/google/pprof/profile"
)

func collect(t *testing.T, p *Profiler) ([]byte, *profile.Profile) {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Collect(&buf); err != nil {
		t.Fatal(err)
	}
	pr, err := profile.Parse(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), pr
}

func sumValues(pr *profile.Profile) (sum int64) {
	for _, s := range pr.Sample {
		sum += s.Value[0]
	}
	return sum
}

// By default executions with the same stack and labels merge into one sample:
// memory grows with distinct stacks, not with kernels. The total is unchanged
// and the per-execution correlation label is gone.
func TestExecutionsAggregateByDefault(t *testing.T) {
	p, tl := drainOnly(t, 1024)
	emitExecs(t, tl, 100)
	p.Stop()
	_, pr := collect(t, p)
	c := p.Counts()
	if c.Executions != 100 || c.ProjectedRows != 100 || c.AggregatedSamples != 1 {
		t.Fatalf("counts %+v, want 100 executions, 100 rows, 1 aggregated sample", c)
	}
	if len(pr.Sample) != 1 || sumValues(pr) != 100*5 {
		t.Fatalf("profile: %d samples summing to %d ns, want 1 sample, 500 ns", len(pr.Sample), sumValues(pr))
	}
	if _, ok := pr.Sample[0].Label["gpu_correlation"]; ok {
		t.Error("aggregated sample still carries gpu_correlation")
	}
}

// PerExecutionSamples restores one sample per execution with its correlation.
func TestPerExecutionSamplesKeepTheLegacyShape(t *testing.T) {
	p, tl := drainOnlyWith(t, 1024, Config{PerExecutionSamples: true})
	emitExecs(t, tl, 100)
	p.Stop()
	_, pr := collect(t, p)
	if len(pr.Sample) != 100 || sumValues(pr) != 100*5 {
		t.Fatalf("profile: %d samples summing to %d, want 100 samples, 500 ns", len(pr.Sample), sumValues(pr))
	}
	for _, s := range pr.Sample {
		if len(s.Label["gpu_correlation"]) != 1 {
			t.Fatal("a per-execution sample lost its gpu_correlation")
		}
	}
}

// The executions log holds exactly what aggregation removed: every
// execution's correlation and value, bound to the profile bytes written, with
// ordinals continuing across drains.
func TestTheExecutionsLogRecoversEveryExecution(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpu.exec")
	p, tl := drainOnlyWith(t, 64, Config{ExecutionsPath: path, CaptureID: "cap-x"})
	first := execSeq + 1
	emitExecs(t, tl, 32) // half the ring: a pressure drain
	waitSamples(t, p, 32)
	emitExecs(t, tl, 40)
	p.Stop()
	written, pr := collect(t, p)

	f, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []execlog.Row
	h, tr, err := execlog.Read(bytes.NewReader(f), func(r execlog.Row) error { rows = append(rows, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Verify(written, len(pr.Sample)); err != nil {
		t.Fatalf("log does not bind to the written profile: %v", err)
	}
	if h.CaptureID != "cap-x" || h.Backend != "cupti" {
		t.Errorf("header %+v", h)
	}
	if tr.Rows != 72 || tr.Executions != 72 || len(rows) != 72 {
		t.Fatalf("trailer %+v, %d rows; want 72 rows and executions", tr, len(rows))
	}
	var sum uint64
	for i, r := range rows {
		if r.Execution != uint64(i) {
			t.Fatalf("row %d has execution ordinal %d; ordinals must continue across drains", i, r.Execution)
		}
		if !r.HasCorrelation || strconv.FormatUint(r.Correlation, 10) != strconv.Itoa(first+i) {
			t.Fatalf("row %d correlation %d (present %v), want %d", i, r.Correlation, r.HasCorrelation, first+i)
		}
		if int(r.Sample) >= len(pr.Sample) {
			t.Fatalf("row %d names sample %d of %d", i, r.Sample, len(pr.Sample))
		}
		sum += r.Value
	}
	if int64(sum) != sumValues(pr) {
		t.Errorf("log values sum to %d, profile to %d", sum, sumValues(pr))
	}
}

// A capture with no samples publishes no log and leaves no temporary file.
func TestAnEmptyCaptureLeavesNoExecutionsLog(t *testing.T) {
	dir := t.TempDir()
	p, _ := drainOnlyWith(t, 64, Config{ExecutionsPath: filepath.Join(dir, "gpu.exec")})
	p.Stop()
	if err := p.Collect(&bytes.Buffer{}); err == nil {
		t.Fatal("Collect of an empty capture succeeded")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("files left: %v", ents)
	}
}
