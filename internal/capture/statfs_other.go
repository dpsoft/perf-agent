//go:build !linux

package capture

import "os"

// FreeBytes is disabled: capture needs Linux.
func FreeBytes(*os.File) (int64, error) { return 0, notLinux() }
