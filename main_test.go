package main

import (
	"testing"

	"github.com/dpsoft/perf-agent/perfagent"
)

func TestBuildOptionsGPUStreamMode(t *testing.T) {
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUStreamStdin = true
	*flagGPUHostReplayInput = ""
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 0
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestBuildOptionsGPUHostReplayPlusStreamMode(t *testing.T) {
	prevStream := *flagGPUStreamStdin
	prevHostReplay := *flagGPUHostReplayInput
	prevReplay := *flagGPUReplayInput
	prevProfile := *flagProfile
	prevOffCPU := *flagOffCpu
	prevPMU := *flagPMU
	prevPID := *flagPID
	prevAll := *flagAll

	t.Cleanup(func() {
		*flagGPUStreamStdin = prevStream
		*flagGPUHostReplayInput = prevHostReplay
		*flagGPUReplayInput = prevReplay
		*flagProfile = prevProfile
		*flagOffCpu = prevOffCPU
		*flagPMU = prevPMU
		*flagPID = prevPID
		*flagAll = prevAll
	})

	*flagGPUStreamStdin = true
	*flagGPUHostReplayInput = "gpu/testdata/host/replay/flash_attn_launches.json"
	*flagGPUReplayInput = ""
	*flagProfile = false
	*flagOffCpu = false
	*flagPMU = false
	*flagPID = 0
	*flagAll = false

	opts := buildOptions()
	if _, err := perfagent.New(opts...); err != nil {
		t.Fatalf("New: %v", err)
	}
}
