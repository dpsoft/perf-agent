package capture

import (
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// Mode is one kind of profile a capture can produce.
type Mode string

const (
	ModeCPU    Mode = "cpu"
	ModeOffCPU Mode = "offcpu"
	ModeGPU    Mode = "gpu"
)

// canonicalModes is the order modes are emitted and reported in.
var canonicalModes = []Mode{ModeCPU, ModeOffCPU, ModeGPU}

// Settings are the capture parameters that shape the perf-agent argv.
type Settings struct {
	Modes        []Mode
	Duration     time.Duration
	SampleRate   int
	Unwind       string
	KernelStacks bool
}

// Canonical returns the settings with the modes ordered cpu, offcpu, gpu.
func (s Settings) Canonical() Settings {
	s.Modes = slices.SortedFunc(slices.Values(s.Modes), func(a, b Mode) int {
		return slices.Index(canonicalModes, a) - slices.Index(canonicalModes, b)
	})
	return s
}

// ExeclogFile is the GPU execution log written next to the GPU profile.
const ExeclogFile = "gpu.execlog"

// OutputFile is the profile file name a mode writes inside the staging
// directory.
func OutputFile(m Mode) string { return string(m) + ".pb.gz" }

// ExpectedFiles lists the files a successful capture of modes leaves in
// the staging directory, in canonical mode order.
func ExpectedFiles(modes []Mode) []string {
	var files []string
	for _, m := range (Settings{Modes: modes}).Canonical().Modes {
		files = append(files, OutputFile(m))
	}
	if slices.Contains(modes, ModeGPU) {
		files = append(files, ExeclogFile)
	}
	return files
}

// PerfAgentEnv is the whole environment perf-agent runs with.
var PerfAgentEnv = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}

// ReadinessFormat is the line perf-agent prints on stdout once profiling
// has started; ReadinessPrefix is the part the reader matches.
const (
	ReadinessFormat = "Collecting data for %v (Ctrl+C to stop early)..."
	ReadinessPrefix = "Collecting data for "
)

// Flag names a perf-agent flag the builder may emit and whether it takes
// no value.
type Flag struct {
	Name string
	Bool bool
}

// Flags is the complete list of flags BuildArgv can emit.
var Flags = []Flag{
	{"pid", false}, {"duration", false}, {"sample-rate", false}, {"unwind", false},
	{"no-debuginfod", true}, {"profile", true}, {"profile-output", false},
	{"offcpu", true}, {"offcpu-output", false},
	{"gpu", true}, {"gpu-shim", false}, {"gpu-output", false}, {"gpu-executions-output", false},
	{"kernel-stacks", true}, {"tag", false},
}

// ArgvSpec is everything BuildArgv needs.
type ArgvSpec struct {
	Binary    string
	PID       int
	Settings  Settings
	Staging   string
	GPUShim   string
	CaptureID string
}

// BuildArgv is the only producer of the perf-agent argv.
func BuildArgv(a ArgvSpec) []string {
	s := a.Settings.Canonical()
	out := func(name string) string { return filepath.Join(a.Staging, name) }
	argv := []string{
		a.Binary,
		"--pid", strconv.Itoa(a.PID),
		"--duration", s.Duration.String(),
		"--sample-rate", strconv.Itoa(s.SampleRate),
		"--unwind", s.Unwind,
		"--no-debuginfod",
	}
	for _, m := range s.Modes {
		switch m {
		case ModeCPU:
			argv = append(argv, "--profile", "--profile-output", out(OutputFile(m)))
		case ModeOffCPU:
			argv = append(argv, "--offcpu", "--offcpu-output", out(OutputFile(m)))
		case ModeGPU:
			argv = append(argv, "--gpu", "--gpu-shim", a.GPUShim,
				"--gpu-output", out(OutputFile(m)),
				"--gpu-executions-output", out(ExeclogFile))
		}
	}
	if s.KernelStacks {
		argv = append(argv, "--kernel-stacks")
	}
	return append(argv, "--tag", "perf_agent_mcp_capture="+a.CaptureID)
}
