//go:build !linux

package main

import "os/exec"

// killWithTest has no portable equivalent outside Linux; the test's
// cleanup still closes Chrome.
func killWithTest(*exec.Cmd) {}
