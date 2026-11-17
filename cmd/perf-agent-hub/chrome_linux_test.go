//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// killWithTest makes the kernel kill the test's Chrome when the test
// process dies, so a crashed or killed test leaves no browser behind.
func killWithTest(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
