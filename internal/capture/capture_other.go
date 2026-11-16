//go:build !linux

package capture

import (
	"io/fs"
	"os"
)

// Supported reports whether capture can run on this platform.
const Supported = false

func notLinux() *Error { return Errorf(CodeDisabled, "", "capture needs Linux") }

// VerifyBinary is disabled: capture needs Linux.
func VerifyBinary(string) (Binary, error) { return Binary{}, notLinux() }

// IdentityOf is disabled: capture needs Linux.
func IdentityOf(string) (Identity, error) { return Identity{}, notLinux() }

// Unchanged is disabled: capture needs Linux.
func (Binary) Unchanged() *Error { return notLinux() }

// VerifyShim is disabled: capture needs Linux.
func VerifyShim(string) (Shim, error) { return Shim{}, notLinux() }

// ReadSelfStatus is disabled: capture needs Linux.
func ReadSelfStatus() ([]byte, error) { return nil, notLinux() }

// StartTime is disabled: capture needs Linux.
func StartTime(Proc, int) (uint64, error) { return 0, notLinux() }

// Alive is disabled: capture needs Linux.
func Alive(Proc, int, uint64) bool { return false }

// Observe is disabled: capture needs Linux.
func Observe(Proc, int) (TargetState, *Error) { return TargetState{}, notLinux() }

// Preflight is disabled: capture needs Linux.
func Preflight(Proc, int, SelfID) (TargetState, *Error) { return TargetState{}, notLinux() }

// Compare is disabled: capture needs Linux.
func (TargetState) Compare(TargetState) *Error { return notLinux() }

// ReadBuildID is disabled: capture needs Linux.
func ReadBuildID(Proc, int) string { return "" }

// CheckShimMapped is disabled: capture needs Linux.
func CheckShimMapped(Proc, int, Shim) *Error { return notLinux() }

// CheckCommandFile is disabled: capture needs Linux.
func CheckCommandFile(string) *Error { return notLinux() }

// executableHere is the any-x-bit check: capture is disabled here, and
// ResolveCommand needs one.
func executableHere() func(fs.FileInfo) bool {
	return func(fi fs.FileInfo) bool { return fi.Mode()&0o111 != 0 }
}

// Lease is the captures lock, which needs Linux.
type Lease struct{}

// TryLock is disabled: capture needs Linux.
func TryLock(*os.Root, Holder) (*Lease, *Error) { return nil, notLinux() }

// File is disabled: capture needs Linux.
func (*Lease) File() *os.File { return nil }

// Update is disabled: capture needs Linux.
func (*Lease) Update(Holder) error { return notLinux() }

// Close is disabled: capture needs Linux.
func (*Lease) Close() error { return nil }

// Recheck is disabled: capture needs Linux.
func (Binary) Recheck() *Error { return notLinux() }

// RunInternal never handles anything here: the internal modes need Linux.
func RunInternal() (code int, handled bool) { return 0, false }

// Reaper is a capture's reaper, which needs Linux.
type Reaper struct{}

// StartReaper is disabled: capture needs Linux.
func StartReaper(string, ReaperSpec, ReaperFiles) (*Reaper, error) { return nil, notLinux() }

// Send is disabled: capture needs Linux.
func (*Reaper) Send(Command) error { return notLinux() }

// Events is closed at once: there is never a reaper here.
func (*Reaper) Events() <-chan Event {
	ch := make(chan Event)
	close(ch)
	return ch
}

// Done is closed at once: there is never a reaper here.
func (*Reaper) Done() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// PID is 0: there is never a reaper here.
func (*Reaper) PID() int { return 0 }

// Kill does nothing: there is never a reaper here.
func (*Reaper) Kill() {}

// DelegatedCgroup never finds one: capture needs Linux.
func DelegatedCgroup([]byte, string) (parent, why string) { return "", "capture needs Linux" }

// CreateCgroup is disabled: capture needs Linux.
func CreateCgroup(string, string) (string, error) { return "", notLinux() }

// KillCgroup is disabled: capture needs Linux.
func KillCgroup(string) error { return notLinux() }

// RemoveCgroup is disabled: capture needs Linux.
func RemoveCgroup(string) error { return notLinux() }

// Run is one capture, which needs Linux.
type Run struct{}

// Start is disabled: capture needs Linux.
func Start(RunSpec) (*Run, *Error) { return nil, notLinux() }

// Phase is done: there is never a capture here.
func (*Run) Phase() string { return PhaseDone }

// Cancel does nothing: there is never a capture here.
func (*Run) Cancel() {}

// Kill does nothing: there is never a capture here.
func (*Run) Kill() {}

// Reaper is no process: there is never a capture here.
func (*Run) Reaper() (int, uint64) { return 0, 0 }

// Wait returns at once: there is never a capture here.
func (*Run) Wait() Outcome { return Outcome{Err: notLinux(), PerfAgentExit: -1} }
