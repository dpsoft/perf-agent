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
