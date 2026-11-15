package capture

import "time"

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
