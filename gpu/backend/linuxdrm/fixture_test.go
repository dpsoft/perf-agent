package linuxdrm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dpsoft/perf-agent/gpu"
)

func TestAMDGPUObservationFixtureContainsDecodedInfoEvent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "amdgpu_observation.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var snap gpu.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(snap.Executions) != 0 {
		t.Fatalf("executions=%d want 0", len(snap.Executions))
	}
	if len(snap.Events) == 0 {
		t.Fatal("expected events")
	}

	var found bool
	for _, event := range snap.Events {
		if event.Name != "amdgpu-info" {
			continue
		}
		found = true
		if event.Kind != gpu.TimelineEventIOCtl {
			t.Fatalf("kind=%q", event.Kind)
		}
		if event.Attributes["command_family"] != "amdgpu" {
			t.Fatalf("command_family=%q", event.Attributes["command_family"])
		}
		if event.Attributes["command_name"] != "info" {
			t.Fatalf("command_name=%q", event.Attributes["command_name"])
		}
		if event.Attributes["semantic"] != "query" {
			t.Fatalf("semantic=%q", event.Attributes["semantic"])
		}
		if event.Attributes["driver"] != "amdgpu" {
			t.Fatalf("driver=%q", event.Attributes["driver"])
		}
		if event.Attributes["drm_node"] != "renderD128" {
			t.Fatalf("drm_node=%q", event.Attributes["drm_node"])
		}
	}
	if !found {
		t.Fatal("expected amdgpu-info event")
	}
}
