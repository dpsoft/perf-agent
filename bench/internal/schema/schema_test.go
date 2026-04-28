package schema

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRoundtrip(t *testing.T) {
	in := &Document{
		Scenario:  "system-wide-mixed",
		Config:    Config{Processes: 30, Runs: 5, WorkloadMix: map[string]int{"go": 10}},
		System:    System{Kernel: "6.19", NCPU: 16, GoVersion: "go1.26.0"},
		StartedAt: time.Date(2026, 4, 25, 19, 30, 0, 0, time.UTC),
		Runs: []Run{
			{RunN: 1, TotalMs: 3214.7, PIDCount: 30, DistinctBinaryCount: 24,
				PerBinary: []Binary{
					{Path: "/lib/libc.so", BuildID: "abc", EhFrameBytes: 31420, CompileMs: 12.3},
					{Path: "/bin/foo", BuildID: "def", EhFrameBytes: 9000, CompileMs: 50.0},
				}},
		},
	}

	var buf bytes.Buffer
	if err := Write(&buf, in); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if out.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", out.SchemaVersion, SchemaVersion)
	}
	if out.Scenario != in.Scenario {
		t.Errorf("Scenario = %q, want %q", out.Scenario, in.Scenario)
	}
	if len(out.Runs) != 1 || len(out.Runs[0].PerBinary) != 2 {
		t.Fatalf("Runs/PerBinary shape mismatch: %#v", out.Runs)
	}
	// Sort: highest compile_ms first.
	if out.Runs[0].PerBinary[0].Path != "/bin/foo" {
		t.Errorf("PerBinary[0].Path = %q, want /bin/foo (sorted desc by compile_ms)",
			out.Runs[0].PerBinary[0].Path)
	}
}

func TestSchemaMismatch(t *testing.T) {
	const wrong = `{"schema_version": 999, "scenario": "x"}`
	_, err := Read(strings.NewReader(wrong))
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
	}
}
