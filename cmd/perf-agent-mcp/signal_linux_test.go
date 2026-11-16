//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
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
// reading its output and the server is blocked writing a response. With
// the live page listening, a browser holding a stream open and another
// not reading a model delay it by no more than the page's bounded close.
func TestSignalEndsAServerBlockedOnOutput(t *testing.T) {
	bin := buildServer(t)
	for _, c := range []struct {
		name    string
		sig     syscall.Signal
		blocked bool
		// errBlocked also leaves stderr full and unread.
		errBlocked bool
		// live starts the page first, with a stream and a stuck download.
		live bool
	}{
		{"SIGTERM blocked", syscall.SIGTERM, true, false, false},
		{"SIGINT blocked", syscall.SIGINT, true, false, false},
		{"SIGTERM drained", syscall.SIGTERM, false, false, false},
		{"SIGTERM stdout and stderr blocked", syscall.SIGTERM, true, true, false},
		{"SIGINT stdout and stderr blocked", syscall.SIGINT, true, true, false},
		{"live SIGTERM drained", syscall.SIGTERM, false, false, true},
		{"live SIGTERM stdout and stderr blocked", syscall.SIGTERM, true, true, true},
		{"live SIGINT stdout and stderr blocked", syscall.SIGINT, true, true, true},
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
			profiles := t.TempDir()
			args := []string{"--profiles", profiles}
			if c.live {
				if err := os.WriteFile(filepath.Join(profiles, "long.pb.gz"), longNames(8000, 1000), 0o644); err != nil {
					t.Fatal(err)
				}
				// Pages large enough for the 8 MiB model of long.pb.gz.
				args = append(args, "--limit", "MaxPageBytes=67108864", "--limit", "MaxPageNodes=1048576")
			}
			cmd := exec.Command(bin, args...)
			if c.live {
				cmd = liveCommand(t, args...)
			}
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
				// Until the page is set up, stderr is read: the server logs
				// while it opens the profile and starts the page.
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
			// tool calls a tool and returns its text.
			tool := func(name string, args any) string {
				t.Helper()
				send("tools/call", map[string]any{"name": name, "arguments": args})
				line, err := r.ReadBytes('\n')
				if err != nil {
					t.Fatal(err)
				}
				var res struct {
					Result struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
						IsError bool `json:"isError"`
					} `json:"result"`
				}
				if err := json.Unmarshal(line, &res); err != nil || res.Result.IsError || len(res.Result.Content) != 1 {
					t.Fatalf("%s: %s (%v)", name, line, err)
				}
				return res.Result.Content[0].Text
			}
			send("initialize", map[string]any{"protocolVersion": "2025-11-25"})
			if _, err := r.ReadBytes('\n'); err != nil {
				t.Fatal(err)
			}
			if c.live {
				startPage(t, tool)
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
			budget := 3 * time.Second
			if c.live {
				budget = 2 * time.Second
			}
			select {
			case err := <-done:
				took := time.Since(start)
				if err != nil {
					t.Errorf("exit: %v", err)
				}
				// Drained, the server closes the page in full, and its
				// bounded wait for the stuck download shows the download
				// really was stuck.
				if c.live && !c.blocked && took < 400*time.Millisecond {
					t.Errorf("exited %v after the signal: the download was not stuck", took)
				}
				t.Logf("exited %v after the signal", took.Round(time.Millisecond))
			case <-time.After(budget):
				t.Fatalf("the server is still running %v after the signal", budget)
			}
		})
	}
}

// longNames is cpu/nanoseconds with one stack of depth frames, each a
// function with its own name of about nameLen bytes: every frame is as
// wide as the view, so a page draws them all.
func longNames(depth, nameLen int) []byte {
	frames := make([]pmfixture.Frame, depth)
	for i := range frames {
		frames[i] = pmfixture.Fn(fmt.Sprintf("f%05d_", i) + strings.Repeat("x", nameLen))
	}
	return pmfixture.Build("cpu", "nanoseconds", nil, pmfixture.Sample{Frames: frames, Value: 1})
}

// startPage opens long.pb.gz through tool, publishes its default view's
// facts on the live page, and leaves two browser connections open: a
// stream read up to its first state, and a download of the model, far
// larger than the socket buffers, that is never read.
func startPage(t *testing.T, tool func(name string, args any) string) {
	t.Helper()
	var o struct {
		Handle      string `json:"handle"`
		DefaultView string `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(tool("open", map[string]any{"path": "long.pb.gz"})), &o); err != nil {
		t.Fatal(err)
	}
	var pub struct {
		Publication string `json:"publication"`
		URL         string `json:"url"`
	}
	text := tool("page_explain", map[string]any{"handle": o.Handle, "view": o.DefaultView, "explanations": []any{}})
	if err := json.Unmarshal([]byte(text), &pub); err != nil || pub.URL == "" {
		t.Fatalf("page_explain: %s (%v)", text, err)
	}
	u, err := url.Parse(pub.URL)
	if err != nil {
		t.Fatal(err)
	}
	// A small receive buffer keeps the unread download from ending inside
	// the kernel.
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
		}); err != nil {
			return err
		}
		return serr
	}}
	request := func(route string) net.Conn {
		t.Helper()
		conn, err := d.Dial("tcp", u.Host)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Path, route, u.Host); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	stream := request("events")
	_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(stream)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		if line == "event: state\n" {
			break
		}
	}
	request("pub/" + pub.Publication + "/model")
}
