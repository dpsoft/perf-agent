// Package fakemode is the contract between the capturetest helper and the
// fake perf-agent binary: the mode names, the mode file and the fixture
// names. It imports nothing, so the fake binary stays small.
package fakemode

// Mode selects what the fake perf-agent does.
type Mode string

const (
	Normal          Mode = "normal"
	NeverReady      Mode = "never-ready"
	IgnoreSIGINT    Mode = "ignore-sigint"
	Exit1Stderr     Mode = "exit1-stderr"
	Exit2           Mode = "exit2"
	Exit127Loader   Mode = "exit127-loader"
	NoFile          Mode = "no-file"
	HugeFile        Mode = "huge-file"
	SymlinkOutput   Mode = "symlink-output"
	HardlinkOutput  Mode = "hardlink-output"
	SpamStdout      Mode = "spam-stdout"
	ForkGrandchild  Mode = "fork-grandchild"
	HangAfterSIGINT Mode = "hang-after-sigint"
	NoGPUSamples    Mode = "no-gpu-samples"
	// HugeFileThenHang gives its first output a 256 MiB size right after
	// readiness, then ignores SIGINT and never exits until killed.
	HugeFileThenHang Mode = "huge-file-then-hang"
)

// All lists every mode; the fake refuses a mode that is not here.
var All = []Mode{Normal, NeverReady, IgnoreSIGINT, Exit1Stderr, Exit2, Exit127Loader, NoFile,
	HugeFile, SymlinkOutput, HardlinkOutput, SpamStdout, ForkGrandchild, HangAfterSIGINT, NoGPUSamples, HugeFileThenHang}

// File is fake-mode.json, which sits beside the fake executable because the
// fake runs with a scrubbed environment.
type File struct {
	Mode       Mode   `json:"mode"`
	ReadyAfter int64  `json:"ready_after_ns"`
	Record     string `json:"record"`
}

// Record is what the fake writes to File.Record.
type Record struct {
	Argv       []string `json:"argv"`
	Env        []string `json:"env"`
	PID        int      `json:"pid"`
	PGID       int      `json:"pgid"`
	Cwd        string   `json:"cwd"`
	Grandchild int      `json:"grandchild,omitzero"`
}

// Names inside fixtures/, beside the fake.
const (
	FixtureCPU          = "cpu.pb.gz"
	FixtureOffCPU       = "offcpu.pb.gz"
	FixtureGPU          = "gpu.pb.gz"
	FixtureExeclog      = "gpu.execlog"
	FixtureGPUEmpty     = "gpu-empty.pb.gz"
	FixtureExeclogEmpty = "gpu-empty.execlog"
)

// GrandchildArg makes the fake binary act as the long-lived grandchild.
const GrandchildArg = "--fake-grandchild"
