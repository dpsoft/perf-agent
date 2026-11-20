//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// buildServer builds the binary into a temporary directory.
func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "perf-agent-mcp")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// A signal ends the server with status 0 even when the client has stopped
// reading its output and the server is blocked writing a response.
func TestSignalEndsAServerBlockedOnOutput(t *testing.T) {
	bin := buildServer(t)
	for _, c := range []struct {
		name    string
		sig     syscall.Signal
		blocked bool
		// errBlocked also leaves stderr full and unread.
		errBlocked bool
	}{
		{"SIGTERM blocked", syscall.SIGTERM, true, false},
		{"SIGINT blocked", syscall.SIGINT, true, false},
		{"SIGTERM drained", syscall.SIGTERM, false, false},
		{"SIGTERM stdout and stderr blocked", syscall.SIGTERM, true, true},
		{"SIGINT stdout and stderr blocked", syscall.SIGINT, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			outR, outW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = outR.Close() }()
			if _, err := unix.FcntlInt(outW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
				t.Skipf("cannot shrink the pipe: %v", err)
			}
			inR, inW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = inW.Close() }()
			cmd := exec.Command(bin, "--profiles", t.TempDir())
			cmd.Stdin, cmd.Stdout = inR, outW
			// blockStderr fills stderr and stops reading it.
			blockStderr := func() {}
			if c.errBlocked {
				errR, errW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = errR.Close() }()
				if _, err := unix.FcntlInt(errW.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
					t.Skipf("cannot shrink the pipe: %v", err)
				}
				cmd.Stderr = errW
				defer func() { _ = errW.Close() }()
				// Until the server has answered initialize, stderr is read:
				// the server logs while it starts.
				drained := make(chan struct{})
				go func() {
					defer close(drained)
					_, _ = io.Copy(io.Discard, errR)
				}()
				// errW is in blocking mode once exec has taken its descriptor,
				// so the write that fills the pipe runs on its own: it returns
				// when the pipe has room for all of it, or when errR closes.
				blockStderr = func() {
					_ = errR.SetReadDeadline(time.Now())
					<-drained
					go func() { _, _ = errW.Write(make([]byte, 4096)) }()
				}
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			_ = inR.Close()
			_ = outW.Close()
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			r := bufio.NewReader(outR)
			id := 0
			send := func(method string, params any) {
				t.Helper()
				id++
				b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := inW.Write(append(b, '\n')); err != nil {
					t.Fatal(err)
				}
			}
			send("initialize", map[string]any{"protocolVersion": "2025-11-25"})
			if _, err := r.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			blockStderr()
			send("tools/list", map[string]any{})
			if !c.blocked {
				go func() { _, _ = io.Copy(io.Discard, r) }()
			}
			time.Sleep(500 * time.Millisecond)
			if err := cmd.Process.Signal(c.sig); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			const budget = 3 * time.Second
			select {
			case err := <-done:
				took := time.Since(start)
				if err != nil {
					t.Errorf("exit: %v", err)
				}
				t.Logf("exited %v after the signal", took.Round(time.Millisecond))
			case <-time.After(budget):
				t.Fatalf("the server is still running %v after the signal", budget)
			}
		})
	}
}
