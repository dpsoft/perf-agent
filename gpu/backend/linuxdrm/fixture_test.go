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

func TestHIPKFDObservationFixtureContainsDecodedMemoryEvents(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "hip_kfd_observation.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var snap gpu.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(snap.Launches) != 1 {
		t.Fatalf("launches=%d want 1", len(snap.Launches))
	}
	if len(snap.Attributions) != 1 {
		t.Fatalf("attributions=%d want 1", len(snap.Attributions))
	}
	if snap.JoinStats.LaunchCount != 1 || snap.JoinStats.UnmatchedLaunchCount != 1 {
		t.Fatalf("join_stats=%+v", snap.JoinStats)
	}

	var foundUnmap bool
	var foundFree bool
	for _, event := range snap.Events {
		switch event.Name {
		case "kfd-unmap-memory-from-gpu":
			foundUnmap = true
			if event.Kind != gpu.TimelineEventMemory {
				t.Fatalf("unmap kind=%q", event.Kind)
			}
			if event.Attributes["command_family"] != "kfd" {
				t.Fatalf("unmap command_family=%q", event.Attributes["command_family"])
			}
			if event.Attributes["command_name"] != "unmap_memory_from_gpu" {
				t.Fatalf("unmap command_name=%q", event.Attributes["command_name"])
			}
			if event.Attributes["semantic"] != "memory-unmap" {
				t.Fatalf("unmap semantic=%q", event.Attributes["semantic"])
			}
		case "kfd-free-memory-of-gpu":
			foundFree = true
			if event.Kind != gpu.TimelineEventMemory {
				t.Fatalf("free kind=%q", event.Kind)
			}
			if event.Attributes["command_family"] != "kfd" {
				t.Fatalf("free command_family=%q", event.Attributes["command_family"])
			}
			if event.Attributes["command_name"] != "free_memory_of_gpu" {
				t.Fatalf("free command_name=%q", event.Attributes["command_name"])
			}
			if event.Attributes["semantic"] != "memory-release" {
				t.Fatalf("free semantic=%q", event.Attributes["semantic"])
			}
		}
	}
	if !foundUnmap {
		t.Fatal("expected kfd-unmap-memory-from-gpu event")
	}
	if !foundFree {
		t.Fatal("expected kfd-free-memory-of-gpu event")
	}
}
