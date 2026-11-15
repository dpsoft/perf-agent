//go:build linux

package capture

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// testChildEnv names the helper body a re-executed test binary runs
// instead of the tests.
const testChildEnv = "PERFMCP_TEST_CHILD"

// testChildBodies are the helper children. Each prints one line once it is
// set up ("ready", or "skip <reason>" when the host cannot give it the
// state the test needs), then lives until its stdin reaches EOF. The
// parent holds the write end, so a child never outlives the test process.
var testChildBodies = map[string]func() string{
	"sleep": func() string { return "ready" },
	"nondumpable": func() string {
		if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
			return "skip prctl(PR_SET_DUMPABLE, 0): " + err.Error()
		}
		return "ready"
	},
	"setresgid": func() string {
		egid := os.Getegid()
		groups, err := os.Getgroups()
		if err != nil {
			return "skip getgroups: " + err.Error()
		}
		for _, g := range groups {
			if g == egid {
				continue
			}
			if err := syscall.Setresgid(-1, g, -1); err != nil {
				return fmt.Sprintf("skip setresgid to supplementary gid %d: %v", g, err)
			}
			return "ready"
		}
		return "skip no supplementary gid other than the effective one"
	},
}

func TestMain(m *testing.M) {
	if mode, ok := os.LookupEnv(testChildEnv); ok {
		os.Exit(runTestChild(mode))
	}
	os.Exit(runTests(m))
}

func runTestChild(mode string) int {
	body, ok := testChildBodies[mode]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown %s=%q\n", testChildEnv, mode)
		return 2
	}
	fmt.Println(body())
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// runTests runs the tests, then kills and reaps, by recorded process, any
// helper child a test left behind. The package owns the children for the
// life of the test binary, not the test that happened to start one.
func runTests(m *testing.M) int {
	code := m.Run()
	testChildren.killAll()
	return code
}

// testChildren is the package-level record of live helper children.
var testChildren = &childSet{live: map[*testChild]struct{}{}}

type childSet struct {
	mu   sync.Mutex
	live map[*testChild]struct{}
}

func (s *childSet) add(c *testChild) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.live[c] = struct{}{}
}

func (s *childSet) remove(c *testChild) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, c)
}

func (s *childSet) killAll() {
	s.mu.Lock()
	left := make([]*testChild, 0, len(s.live))
	for c := range s.live {
		left = append(left, c)
	}
	s.mu.Unlock()
	for _, c := range left {
		c.kill()
	}
}

// testChild is a running helper child.
type testChild struct {
	PID   int
	Line  string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	once  sync.Once
	err   error
}

// startChild re-executes the test binary as the helper child mode and
// waits for its first line. The child is killed and reaped when the test
// ends, if the test has not reaped it.
func startChild(t *testing.T, mode string) *testChild {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), testChildEnv+"="+mode)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &testChild{PID: cmd.Process.Pid, cmd: cmd, stdin: stdin}
	testChildren.add(c)
	t.Cleanup(c.kill)

	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(stdout).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case c.Line = <-line:
	case <-time.After(10 * time.Second):
		t.Fatalf("helper child %s (pid %d) printed nothing in 10s", mode, c.PID)
	}
	if c.Line == "" {
		t.Fatalf("helper child %s (pid %d) exited before it was ready", mode, c.PID)
	}
	return c
}

// exit ends the child by closing its stdin, without reaping it.
func (c *testChild) exit() { _ = c.stdin.Close() }

// reap closes the child's stdin and waits for it, once.
func (c *testChild) reap() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		c.err = c.cmd.Wait()
		testChildren.remove(c)
	})
	return c.err
}

// kill kills the child through its process handle and reaps it.
func (c *testChild) kill() {
	_ = c.cmd.Process.Kill()
	_ = c.reap()
}

// waitExitedNoReap blocks until pid has exited and leaves it a zombie:
// waitid with WNOWAIT reports the exit without reaping.
func waitExitedNoReap(t *testing.T, pid int) {
	t.Helper()
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			t.Fatalf("waitid(%d, WNOWAIT): %v", pid, err)
		}
		return
	}
}
