package gpuprofile

import "log"

// logf is the package's one log call, so a caller embedding this in a
// library can see at a glance what it prints.
func logf(format string, args ...any) { log.Printf(format, args...) }
