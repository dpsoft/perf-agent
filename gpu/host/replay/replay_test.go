package replay

import (
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/host"
)

type captureSink struct {
	records []host.LaunchRecord
}

func (s *captureSink) EmitLaunchRecord(record host.LaunchRecord) error {
	s.records = append(s.records, record)
	return nil
}

func TestReplaySourceEmitsLaunchRecords(t *testing.T) {
	src, err := New(filepath.Join("..", "..", "testdata", "host", "replay", "flash_attn_launches.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var sink captureSink
	if err := src.Start(t.Context(), &sink); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(sink.records); got == 0 {
		t.Fatal("expected records")
	}
}

func TestReplaySourceRejectsMalformedFixture(t *testing.T) {
	src, err := New(filepath.Join("testdata", "missing.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := src.Start(t.Context(), &captureSink{}); err == nil {
		t.Fatal("expected error")
	}
}
