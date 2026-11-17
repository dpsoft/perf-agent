//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// smallPipe is a pipe whose buffer holds 4 KiB.
func smallPipe(t *testing.T) (r, w *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	if _, err := unix.FcntlInt(w.Fd(), unix.F_SETPIPE_SZ, 4096); err != nil {
		t.Skipf("cannot shrink the pipe: %v", err)
	}
	return r, w
}

// blockedStderr is a stderr for the hub that the test reads until block,
// which stops reading and fills the pipe, so the hub's next write blocks.
type blockedStderr struct {
	t      *testing.T
	r, w   *os.File
	buf    lockedBuffer
	copied chan struct{}
}

func newBlockedStderr(t *testing.T) *blockedStderr {
	r, w := smallPipe(t)
	b := &blockedStderr{t: t, r: r, w: w, copied: make(chan struct{})}
	go func() {
		_, _ = io.Copy(&b.buf, r)
		close(b.copied)
	}()
	return b
}

func (b *blockedStderr) block() {
	b.t.Helper()
	_ = b.r.SetReadDeadline(time.Now())
	<-b.copied
	go func() {
		for {
			if _, err := b.w.Write([]byte{'x'}); err != nil {
				return
			}
		}
	}()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if n, err := unix.IoctlGetInt(int(b.r.Fd()), unix.TIOCINQ); err == nil && n >= 4096 {
			return
		}
	}
	b.t.Fatal("stderr never filled")
}

// fullStdout is a stdout already full, which the test never reads: the
// hub's write of the page URL blocks for good.
func fullStdout(t *testing.T) *os.File {
	t.Helper()
	_, w := smallPipe(t)
	if _, err := w.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	return w
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

// smallReceiveBuffer keeps a download that is never read from ending
// inside the kernel's buffers.
func smallReceiveBuffer(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_RCVBUF, 4096)
	}); err != nil {
		return err
	}
	return serr
}

// TestShutdownIsBoundedByStuckTabs signals a hub with a tab that never
// reads its stream past the first state and a download, far larger than
// the socket buffers, that is never read: the hub waits its 1 s grace
// for the download, then drops both connections and exits 0, whether or
// not its stdout and stderr are read.
func TestShutdownIsBoundedByStuckTabs(t *testing.T) {
	for _, c := range []struct {
		name    string
		sig     syscall.Signal
		blocked bool
	}{
		{"SIGTERM drained", syscall.SIGTERM, false},
		{"SIGTERM stdout and stderr blocked", syscall.SIGTERM, true},
		{"SIGINT stdout and stderr blocked", syscall.SIGINT, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			profiles := t.TempDir()
			if err := os.WriteFile(filepath.Join(profiles, "long.pb.gz"), longNames(8000, 1000), 0o644); err != nil {
				t.Fatal(err)
			}
			o := hubOptions{profiles: profiles, args: []string{"--limit", "MaxPageBytes=67108864", "--limit", "MaxPageNodes=1048576"}}
			var stderr *blockedStderr
			if c.blocked {
				stderr = newBlockedStderr(t)
				o.stdout, o.stderr = fullStdout(t), stderr.w
			}
			p := startHub(t, o)
			cl := p.client("stuck")
			pub := cl.publish(cl.open("long.pb.gz"))
			page := must(url.Parse(p.pageURL()))(t)
			code, model := httpGet(t, page.String()+"pub/"+pub+"/model")
			if code != 200 || len(model) < 8<<20 {
				t.Fatalf("the model: %d, %d bytes; the test needs at least 8 MiB", code, len(model))
			}
			d := net.Dialer{Control: smallReceiveBuffer}
			stuck := func(route string) net.Conn {
				conn := must(d.Dial("tcp", page.Host))(t)
				t.Cleanup(func() { _ = conn.Close() })
				if _, err := fmt.Fprintf(conn, "GET %s%s HTTP/1.1\r\nHost: %s\r\n\r\n", page.Path, route, page.Host); err != nil {
					t.Fatal(err)
				}
				return conn
			}
			stream := stuck("events")
			_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
			sr := bufio.NewReader(stream)
			for {
				line, err := sr.ReadString('\n')
				if err != nil {
					t.Fatalf("stream: %v", err)
				}
				if line == "event: state\n" {
					break
				}
			}
			download := stuck("pub/" + pub + "/model")
			if c.blocked {
				stderr.block()
			}
			time.Sleep(200 * time.Millisecond)

			took := p.stop(c.sig, 2*time.Second)
			// The HTTP server waits its whole grace only for a response still
			// being written: the download was stuck, as the test needs.
			if took < 800*time.Millisecond {
				t.Errorf("exited %v after the signal: the download was not stuck", took)
			}
			t.Logf("exited %v after %v", took.Round(time.Millisecond), c.sig)
			_ = stream.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.Copy(io.Discard, sr); isTimeout(err) {
				t.Error("the stream's connection is still open after the hub exited")
			}
			_ = download.SetReadDeadline(time.Now().Add(5 * time.Second))
			res, err := http.ReadResponse(bufio.NewReader(download), nil)
			if err != nil {
				t.Fatal(err)
			}
			n, err := io.Copy(io.Discard, res.Body)
			if isTimeout(err) {
				t.Error("the download's connection is still open after the hub exited")
			}
			if err == nil || n >= int64(len(model)) {
				t.Errorf("the stuck download got %d of %d bytes (%v)", n, len(model), err)
			}
			if got := p.runtimeFiles(); len(got) != 1 || got[0] != "hub.lock" {
				t.Errorf("runtime files after the signal: %v", got)
			}
		})
	}
}

func isTimeout(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// TestABlockedStderrNeverHoldsARequest: a request whose handling logs
// (every tools/list under a tiny MaxResponseBytes logs a warning) is
// answered while nobody reads stderr.
func TestABlockedStderrNeverHoldsARequest(t *testing.T) {
	stderr := newBlockedStderr(t)
	p := startHub(t, hubOptions{stderr: stderr.w, args: []string{"--limit", "MaxResponseBytes=1024"}})
	c := p.client("blocked")
	stderr.block()
	start := time.Now()
	for range 50 {
		if r := c.rpc("tools/list", map[string]any{}); r.Error == nil {
			t.Fatalf("tools/list fit under MaxResponseBytes=1024: %s", r.Result)
		}
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("50 logging requests took %v with stderr blocked", took)
	}
	p.stop(syscall.SIGTERM, 2*time.Second)
}

// groupGone reports that no process of the group pgid is left.
func groupGone(pgid int) bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) }

// TestShutdownDoesNotWaitForLongCalls: at the signal, a capture_wait of
// 45 s and a call waiting for the engine behind another are in flight.
// Both are answered at once, and the hub exits within the HTTP grace,
// which only the call holding the engine, ignoring its context, uses.
func TestShutdownDoesNotWaitForLongCalls(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	record := filepath.Join(t.TempDir(), "record.json")
	fake := capturetest.FakePerfAgent(t, capturetest.FakeOptions{Mode: capturetest.FakeNormal, Record: record})
	hold := filepath.Join(t.TempDir(), "hold")
	t.Cleanup(func() { _ = os.WriteFile(hold+".release", nil, 0o600) })
	p := startHub(t, hubOptions{
		env: []string{fakeVerifyEnv + "=1", holdEnv + "=" + hold},
		args: []string{"--perf-agent", fake,
			"--limit", "Capture.StopGrace=1s", "--limit", "Capture.StartTimeout=30s",
			"--limit", "Capture.MaxDuration=2m", "--limit", "Capture.MinFreeBytes=1048576"},
	})
	c := p.client("long")
	text := c.tool("capture", map[string]any{"command": []any{capturetest.Workload(t), "sleep", "2m"}, "duration": "1m"})
	var job struct{ Job, Status string }
	if json.Unmarshal([]byte(text), &job) != nil || job.Status != "accepted" {
		t.Fatalf("capture: %s", text)
	}
	for end := time.Now().Add(60 * time.Second); ; {
		var v struct{ Status, Phase string }
		text := c.tool("capture_wait", map[string]any{"id": job.Job, "timeout": "500ms"})
		if json.Unmarshal([]byte(text), &v) != nil {
			t.Fatalf("capture_wait: %s", text)
		}
		if v.Status == "running" && v.Phase == "profiling" {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the capture never profiled: %s", text)
		}
	}
	var rec fakemode.Record
	if b, err := os.ReadFile(record); err != nil || json.Unmarshal(b, &rec) != nil || rec.PGID <= 0 {
		t.Fatalf("the fake's record: %s (%v)", b, err)
	}

	// One call holds the engine and ignores its context; another waits
	// for the engine behind it; a capture_wait waits for the capture.
	if err := os.WriteFile(hold+".tool", []byte("open"), 0o600); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		name string
		a    answer
		err  error
	}
	results := make(chan outcome, 3)
	var wg sync.WaitGroup
	call := func(name, id, tool string, args map[string]any) {
		wg.Go(func() {
			a, err := c.send(id, "tools/call", map[string]any{"name": tool, "arguments": args})
			results <- outcome{name, a, err}
		})
	}
	call("holder", `"holder"`, "open", map[string]any{"path": "cpu.pb.gz"})
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(hold + ".held"); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the holder never held the engine")
		}
	}
	call("waiter", `"waiter"`, "top", map[string]any{"handle": "h1", "view": "cpu"})
	call("capture_wait", `"capture_wait"`, "capture_wait", map[string]any{"id": job.Job, "timeout": "45s"})
	time.Sleep(300 * time.Millisecond)

	took := p.stop(syscall.SIGTERM, 1300*time.Millisecond)
	t.Logf("exited %v after SIGTERM", took.Round(time.Millisecond))
	wg.Wait()
	close(results)
	answered := map[string]bool{}
	for r := range results {
		if r.err != nil {
			if r.name != "holder" {
				t.Errorf("%s: %v", r.name, r.err)
			}
			continue
		}
		if _, err := decode(r.a, `"`+r.name+`"`); err != nil {
			t.Errorf("%s: %v", r.name, err)
			continue
		}
		answered[r.name] = true
	}
	if !answered["waiter"] || !answered["capture_wait"] {
		t.Errorf("answered %v: the waits in flight were not answered", answered)
	}
	for end := time.Now().Add(1500 * time.Millisecond); !groupGone(rec.PGID); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Errorf("the fake perf-agent's group %d is still alive", rec.PGID)
			_ = syscall.Kill(-rec.PGID, syscall.SIGKILL)
			break
		}
	}
}

// TestRunningOutOfDescriptorsIsNotTheEnd: a hub with 48 descriptors meets
// 80 connections at once, so its Accept fails with EMFILE; once they
// close, it takes connections again and a client initializes.
func TestRunningOutOfDescriptorsIsNotTheEnd(t *testing.T) {
	p := startHub(t, hubOptions{env: []string{nofileEnv + "=48"}})
	conns := make([]net.Conn, 0, 80)
	for range 80 {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p.port), 2*time.Second)
		if err != nil {
			break
		}
		conns = append(conns, c)
	}
	time.Sleep(300 * time.Millisecond)
	for _, c := range conns {
		_ = c.Close()
	}
	start := time.Now()
	p.client("after-emfile")
	t.Logf("initialize %v after %d connections closed", time.Since(start).Round(time.Millisecond), len(conns))
	p.stop(syscall.SIGTERM, 3*time.Second)
}
