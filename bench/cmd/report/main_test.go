package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/bench/internal/schema"
)

func TestWriteSummary_Markdown(t *testing.T) {
	doc := &schema.Document{
		SchemaVersion: schema.SchemaVersion,
		Scenario:      "system-wide-mixed",
		Config:        schema.Config{Processes: 30, Runs: 3, WorkloadMix: map[string]int{"go": 10}},
		System: schema.System{
			Kernel: "6.19", CPUModel: "Test CPU", NCPU: 4,
			GoVersion: "go1.26.0", PerfAgentCommit: "deadbeef",
		},
		StartedAt: time.Date(2026, 4, 25, 19, 30, 0, 0, time.UTC),
		Runs: []schema.Run{
			{RunN: 1, TotalMs: 1000, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []schema.Binary{
					{Path: "/lib/libc.so", BuildID: "111111111111aaaaaaa", EhFrameBytes: 30000, CompileMs: 10},
					{Path: "/bin/foo", BuildID: "222222222222bbbbbbb", EhFrameBytes: 9000, CompileMs: 50},
				}},
			{RunN: 2, TotalMs: 1100, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []schema.Binary{
					{Path: "/lib/libc.so", BuildID: "111111111111aaaaaaa", EhFrameBytes: 30000, CompileMs: 11},
					{Path: "/bin/foo", BuildID: "222222222222bbbbbbb", EhFrameBytes: 9000, CompileMs: 55},
				}},
			{RunN: 3, TotalMs: 950, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []schema.Binary{
					{Path: "/lib/libc.so", BuildID: "111111111111aaaaaaa", EhFrameBytes: 30000, CompileMs: 9},
					{Path: "/bin/foo", BuildID: "222222222222bbbbbbb", EhFrameBytes: 9000, CompileMs: 48},
				}},
		},
	}

	var got bytes.Buffer
	writeSummary(&got, doc, "markdown")

	wantPath := filepath.Join("testdata", "summary.md")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(wantPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(wantPath, got.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("missing golden file %s; regenerate with UPDATE_GOLDEN=1: %v", wantPath, err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Errorf("output diverges from golden\n--- got ---\n%s\n--- want ---\n%s", got.String(), string(want))
	}
	// Sanity: median ordering — /bin/foo's median (50) > /lib/libc.so's (10), so /bin/foo first.
	if !strings.Contains(got.String(), "| `/bin/foo`") {
		t.Errorf("missing /bin/foo row")
	}
}

func TestRunDiff_Markdown(t *testing.T) {
	mkDoc := func(scenario string, runs []float64) *schema.Document {
		d := &schema.Document{
			SchemaVersion: schema.SchemaVersion,
			Scenario:      scenario,
			Config:        schema.Config{Runs: len(runs)},
			System:        schema.System{Kernel: "x"},
			StartedAt:     time.Now(),
		}
		for i, ms := range runs {
			d.Runs = append(d.Runs, schema.Run{RunN: i + 1, TotalMs: ms})
		}
		return d
	}

	beforeF := writeTempJSON(t, mkDoc("system-wide-mixed", []float64{1000, 1100, 950}))
	afterF := writeTempJSON(t, mkDoc("system-wide-mixed", []float64{800, 850, 870}))

	var got bytes.Buffer
	runDiff(&got, beforeF, afterF, "markdown")

	out := got.String()
	for _, want := range []string{"# Diff:", "## Wall time", "| p50 |", "Δ%"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n%s", want, out)
		}
	}
	// p50: before=1000, after=850, Δ = -15%.
	if !strings.Contains(out, "-15.0%") {
		t.Errorf("expected -15.0%% somewhere in p50 row; got:\n%s", out)
	}
}

func writeTempJSON(t *testing.T, d *schema.Document) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.json")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := schema.Write(f, d); err != nil {
		t.Fatal(err)
	}
	return p
}
