package schema

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"
)

// SchemaVersion is bumped when the JSON layout changes incompatibly.
const SchemaVersion = 1

// Document is the top-level JSON object written per scenario run.
type Document struct {
	SchemaVersion int       `json:"schema_version"`
	Scenario      string    `json:"scenario"`
	Config        Config    `json:"config"`
	System        System    `json:"system"`
	StartedAt     time.Time `json:"started_at"`
	Runs          []Run     `json:"runs"`
}

type Config struct {
	Processes   int            `json:"processes"`
	Runs        int            `json:"runs"`
	DropCache   bool           `json:"drop_cache"`
	UnwindMode  string         `json:"unwind_mode,omitzero"`
	WorkloadMix map[string]int `json:"workload_mix,omitempty"`
}

type System struct {
	Kernel          string `json:"kernel"`
	CPUModel        string `json:"cpu_model"`
	NCPU            int    `json:"ncpu"`
	GoVersion       string `json:"go_version"`
	PerfAgentCommit string `json:"perf_agent_commit"`
}

type Run struct {
	RunN                int      `json:"run_n"`
	TotalMs             float64  `json:"total_ms"`
	PIDCount            int      `json:"pid_count"`
	DistinctBinaryCount int      `json:"distinct_binary_count"`
	PerBinary           []Binary `json:"per_binary"`
}

type Binary struct {
	Path         string  `json:"path"`
	BuildID      string  `json:"build_id"`
	EhFrameBytes int     `json:"eh_frame_bytes"`
	CompileMs    float64 `json:"compile_ms"`
}

// SortPerBinary sorts each Run's PerBinary by CompileMs descending so a
// human reader sees hot binaries at the top.
func (d *Document) SortPerBinary() {
	for i := range d.Runs {
		sort.Slice(d.Runs[i].PerBinary, func(a, b int) bool {
			return d.Runs[i].PerBinary[a].CompileMs > d.Runs[i].PerBinary[b].CompileMs
		})
	}
}

// Write encodes d to w as indented JSON, stamping SchemaVersion and
// sorting per_binary descending by compile_ms.
func Write(w io.Writer, d *Document) error {
	d.SchemaVersion = SchemaVersion
	d.SortPerBinary()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// ErrSchemaMismatch is returned by Read when the input's schema_version
// does not match the build's SchemaVersion.
var ErrSchemaMismatch = errors.New("schema version mismatch")

// Read decodes a Document from r. Returns ErrSchemaMismatch if the
// schema_version field doesn't match this package's SchemaVersion.
func Read(r io.Reader) (*Document, error) {
	var d Document
	if err := json.NewDecoder(r).Decode(&d); err != nil {
		return nil, err
	}
	if d.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrSchemaMismatch, d.SchemaVersion, SchemaVersion)
	}
	return &d, nil
}
