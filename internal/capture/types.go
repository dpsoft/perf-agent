package capture

import (
	"log/slog"
	"os"
	"strings"
	"time"
)

// Identity pins a file's contents and attributes, so a change after
// verification is seen before the next exec. Mtime and Ctime are in
// nanoseconds; Xattr is the SHA-256 of the raw security.capability value,
// zero when the file has none.
type Identity struct {
	Dev, Ino     uint64
	Size         int64
	Mtime, Ctime int64
	Xattr        [32]byte
}

// Binary is a verified perf-agent: its resolved path, its identity at
// verification, its permitted capabilities, its Go module version and VCS
// revision, and warnings to log at start-up.
type Binary struct {
	Path     string
	ID       Identity
	Caps     CapSet
	Version  string
	Revision string
	Warnings []string
}

// Shim is a verified CUPTI adapter. perf-agent attaches to it by inode, so
// a target must map this (Dev, Ino).
type Shim struct {
	Path     string
	Dev, Ino uint64
}

// Proc is where target state is read from. Root is the proc filesystem;
// tests point it at a fake tree. StartTimeHook, when set, rewrites every
// start time read, so a test can simulate PID reuse. owner returns a
// file's owning uid; nil means stat.
type Proc struct {
	Root          string
	StartTimeHook func(pid int, start uint64) uint64
	owner         func(path string) (int, error)
}

// DefaultProc reads the host's /proc.
func DefaultProc() Proc { return Proc{Root: "/proc"} }

// SelfID is the server as the target rule sees it: its uid, gid and pid,
// and Exclude, which names its own children (perf-agent, reapers) that
// may never be a target.
type SelfID struct {
	UID, GID, PID int
	Exclude       func(pid int) bool
}

// TargetState is what the preflight saw of a target. StartTime is
// /proc/<pid>/stat field 22; UIDs and GIDs are the real, effective, saved
// and filesystem ids; StatusOwner owns /proc/<pid>/status, which is root
// for a non-dumpable task; ExeReadable says /proc/<pid>/exe could be read,
// which the kernel allows only for a dumpable task the server may
// ptrace-read; CapPrm is the permitted capability set. BuildID is filled
// by Preflight only.
type TargetState struct {
	PID         int
	StartTime   uint64
	UIDs, GIDs  [4]int
	StatusOwner int
	CapPrm      uint64
	Comm, Exe   string
	BuildID     string
	ExeReadable bool
}

// Holder is what the captures lock file records about the capture that
// holds it: the server and the reaper, each by PID and start time, the
// capture and its series, and the latest time it can end. A reader judges
// a process gone only when both its PID and start time no longer match.
type Holder struct {
	ServerPID   int       `json:"server_pid"`
	ServerStart uint64    `json:"server_start"`
	ReaperPID   int       `json:"reaper_pid"`
	ReaperStart uint64    `json:"reaper_start"`
	CaptureID   string    `json:"capture_id"`
	Series      string    `json:"series"`
	Deadline    time.Time `json:"deadline"`
}

// InternalEnv selects an internal mode of the server's own binary: the
// reaper or the launcher. Each reads its spec as JSON from fd 3.
const InternalEnv = "PERF_AGENT_MCP_INTERNAL"

// ReaperSpec is what a reaper is started with. PerfAgent and PerfAgentID
// are the verified binary, re-checked before it is started. Launch, when
// set, is the command the reaper starts through the launcher. Deadline is
// counted from the reaper's start: at it, everything is killed whatever
// the server says. StopGrace bounds each wait for a signalled process.
// Cgroup is a cgroup created for this capture (its name starts with
// CgroupPrefix; any other is not used), which the workload starts in when
// the kernel allows it and which the reaper removes. FaultPause is
// a test hook (see the FaultPause constants).
type ReaperSpec struct {
	PerfAgent           string
	PerfAgentID         Identity
	Launch              *LaunchSpec
	Deadline, StopGrace time.Duration
	Cgroup              string
	FaultPause          string
}

// LaunchSpec is the workload the launcher executes under no_new_privs:
// Path with the argv Args (Args[0] included) and exactly the environment
// Env, in Dir.
type LaunchSpec struct {
	Path      string
	Args, Env []string
	Dir       string
}

// ReaperFiles are the descriptors the reaper inherits: the locked captures
// lock, which it holds until both children are reaped, and the write ends
// of perf-agent's and the workload's stdout and stderr. A nil file is
// passed as the null device, and the reaper treats it as absent.
// StartReaper does not close them; the caller closes its write ends once
// the reaper has started, so its readers see EOF.
type ReaperFiles struct {
	Lock, PerfOut, PerfErr, WorkOut, WorkErr *os.File
}

// The commands the server sends a reaper.
const (
	OpAttach = "attach" // start perf-agent with Argv in Dir
	OpStop   = "stop"   // SIGINT to perf-agent's group
	OpCancel = "cancel" // SIGINT, then SIGKILL after StopGrace
)

// Command is one line on the reaper's command pipe.
type Command struct {
	Op   string
	Argv []string `json:",omitzero"`
	Dir  string   `json:",omitzero"`
}

// The events a reaper publishes, and who they are about.
const (
	EventLaunched = "launched"
	EventAttached = "attached"
	EventExited   = "exited"
	EventReaped   = "reaped"
	// EventDeadline says the reaper's absolute deadline passed and it is
	// killing the capture, before the exits that causes.
	EventDeadline = "deadline"
	EventError    = "error"

	WhoPerfAgent = "perf-agent"
	WhoWorkload  = "workload"
)

// CleanupComplete starts the Cleanup of a reaped event that left nothing
// behind; any other value starts with "incomplete: " and gives the reasons.
// Either may end with notes in parentheses, such as a cgroup fallback.
const CleanupComplete = "complete"

// CleanupIsComplete reports whether a reaped event's Cleanup says nothing
// was left behind.
func CleanupIsComplete(cleanup string) bool {
	return cleanup == CleanupComplete || strings.HasPrefix(cleanup, CleanupComplete+" (")
}

// Event is one line on the reaper's event pipe. launched and attached
// carry a child's PID, PGID and StartTime; Cgroup says the workload
// started in the capture's cgroup. exited carries the exit Status (-1
// when killed) or the Signal's name. reaped is the last event: Survivors
// are the processes left after the stop grace, and Cleanup says whether
// containment was complete. error carries Err.
type Event struct {
	Op        string
	Who       string     `json:",omitzero"`
	PID       int        `json:",omitzero"`
	PGID      int        `json:",omitzero"`
	StartTime uint64     `json:",omitzero"`
	Status    int        `json:",omitzero"`
	Signal    string     `json:",omitzero"`
	Cgroup    bool       `json:",omitzero"`
	Survivors []Survivor `json:",omitzero"`
	Cleanup   string     `json:",omitzero"`
	Err       string     `json:",omitzero"`
}

// The FaultPause values. Each but the launcher's suspends only the
// publication of one event, for the life of the reaper; the reaper still
// owns the child and kills it on EOF or at the deadline.
const (
	// PauseAfterPerfAgentFork holds back the attached event, and with it
	// perf-agent's exited event.
	PauseAfterPerfAgentFork = "after-perf-agent-fork"
	// PauseAfterLauncherFork holds back the launched event, and with it
	// the workload's exited event.
	PauseAfterLauncherFork = "after-launcher-fork"
	// PauseNoSubreaper skips PR_SET_CHILD_SUBREAPER.
	PauseNoSubreaper = "no-subreaper"
	// PauseBeforeExit, followed by a path, keeps the reaper and its lock
	// after cleanup until that path disappears.
	PauseBeforeExit = "before-exit:"
	// PauseLauncherDiesAfterMarker makes the launcher SIGKILL itself after
	// it writes the exec marker and before it executes the command, so the
	// reaper publishes launched for a process that never ran it.
	PauseLauncherDiesAfterMarker = "launcher-dies-after-marker"
)

// Bounds are the limits one capture runs under. StartTimeout runs from
// Start to perf-agent's readiness line (for a command, its launch
// included); FinishTimeout from the stop to perf-agent's exit; StopGrace
// bounds each wait for a signalled process. MaxOutputTail is the most of
// each output an Outcome carries. MaxProfileBytes and MaxExeclogBytes
// bound the files in the staging directory while they are written.
// Recheck is the interval of the target recheck and Poll that of the size
// and liveness checks.
type Bounds struct {
	StartTimeout, FinishTimeout, StopGrace time.Duration
	MaxOutputTail                          int
	MaxProfileBytes, MaxExeclogBytes       int64
	Recheck, Poll                          time.Duration
}

// DefaultBounds are the default limits of §6.1.
func DefaultBounds() Bounds {
	return Bounds{
		StartTimeout:    15 * time.Second,
		FinishTimeout:   30 * time.Second,
		StopGrace:       5 * time.Second,
		MaxOutputTail:   2 << 10,
		MaxProfileBytes: 256 << 20,
		MaxExeclogBytes: 256 << 20,
		Recheck:         time.Second,
		Poll:            250 * time.Millisecond,
	}
}

// RunSpec is one capture. Self is the server's own binary, which runs the
// reaper and the launcher. The target is PID, or the command Launch, which
// the reaper starts (exactly one is set); Launch.Path is the resolved
// command. Staging is the open staging directory, at StagingPath, which
// perf-agent writes into. GPU is the verified shim for a gpu capture.
// Lock is the locked captures lock, handed to the reaper, which holds it
// until both children are reaped; the caller still closes its own copy.
// Cgroup is the delegated parent cgroup (from DelegatedCgroup), under
// which a command gets a cgroup of its own; empty means none. Log takes
// the children's output at debug level. FaultPause is passed to the
// reaper. Events, a test seam, may rewrite the reaper's event stream.
type RunSpec struct {
	Self        string
	Binary      Binary
	Proc        Proc
	SelfID      SelfID
	PID         int
	Launch      *LaunchSpec
	Settings    Settings
	Staging     *os.Root
	StagingPath string
	CaptureID   string
	GPU         *Shim
	Bounds      Bounds
	Lock        *os.File
	Cgroup      string
	Log         *slog.Logger
	FaultPause  string
	Events      func(<-chan Event) <-chan Event
}

// What stopped a capture that profiled.
const (
	StoppedByDuration    = "duration"
	StoppedByTargetExit  = "target_exit"
	StoppedByCommandExit = "command_exit"
	StoppedByCancel      = "cancel"
)

// The phases of a Run, as Phase reports them.
const (
	PhaseLaunching = "launching"
	PhaseAttaching = "attaching"
	PhaseProfiling = "profiling"
	PhaseFinishing = "finishing"
	PhaseDone      = "done"
)

// Tail is the end of a launched command's stdout and stderr: at most
// Bounds.MaxOutputTail bytes each, valid UTF-8, cut on a rune boundary.
// It is the program's own output, and untrusted.
type Tail struct {
	Stdout, Stderr string
}

// Outcome is how one capture ended. Err is nil only for a capture whose
// files may be kept. StoppedBy is one of the StoppedBy values once
// profiling began. Target is what the preflight saw; Launched says the
// command was proven to run (its PID alive and running the command, not
// the launcher). CommandPath and ArgCount describe the command (ArgCount
// is its argv length, argv[0] included). Started is when the capture
// began. AttachLatency runs to the readiness line from the command's
// launch, or for a PID from the attach; Profiled from the readiness line
// to the stop. PerfAgentExit is perf-agent's exit status, -1 when it was
// killed or never ran; StderrTail is its stderr's end, on failure only.
// Survivors and Cleanup are the reaper's report. Argv is perf-agent's
// argv, once it was built. WorkloadExited says the launched command was
// reaped, with its exit status, or WorkloadSignal naming the signal that
// killed it (the status is then -1).
type Outcome struct {
	Err                     *Error
	StoppedBy               string
	Target                  TargetState
	Launched                bool
	CommandPath             string
	ArgCount                int
	Started                 time.Time
	AttachLatency, Profiled time.Duration
	PerfAgentExit           int
	StderrTail              string
	Workload                Tail
	Survivors               []Survivor
	Cleanup                 string
	Notes                   []string
	Argv                    []string
	WorkloadExited          bool
	WorkloadStatus          int
	WorkloadSignal          string
}
