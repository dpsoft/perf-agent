//go:build !linux

// Command capworkload is the scripted workload the capture tests profile.
// It needs Linux.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "capworkload needs Linux")
	os.Exit(2)
}
