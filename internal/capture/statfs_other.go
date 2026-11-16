//go:build !linux

package capture

// FreeBytes is disabled: capture needs Linux.
func FreeBytes(string) (int64, error) { return 0, notLinux() }
