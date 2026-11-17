//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// killWithTest makes the kernel kill a child of the test, a hub or
// Chrome, when the test process dies, so a crashed, killed or timed-out
// test leaves no hub or browser behind.
func killWithTest(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
