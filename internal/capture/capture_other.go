//go:build !linux

package capture

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
