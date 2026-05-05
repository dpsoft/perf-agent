package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dpsoft/perf-agent/gpu/host"
)

type Source struct {
	path string
}

func New(path string) (*Source, error) {
	if path == "" {
		return nil, fmt.Errorf("replay path is required")
	}
	return &Source{path: path}, nil
}

func (s *Source) ID() string { return "host-replay" }

func (s *Source) Start(_ context.Context, sink host.HostSink) error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read host replay fixture: %w", err)
	}
	var records []host.LaunchRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return fmt.Errorf("decode host replay fixture: %w", err)
	}
	for _, record := range records {
		if err := sink.EmitLaunchRecord(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *Source) Stop(context.Context) error { return nil }

func (s *Source) Close() error { return nil }
