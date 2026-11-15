package capture

import (
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Internal mode names, the values of InternalEnv.
const (
	modeReaper   = "reaper"
	modeLauncher = "launcher"
)

// child is a process this package started. The *exec.Cmd stays here: the
// reaper reaps with its own wait4 loop, never Cmd.Wait, and release drops
// the handle and the pipe ends the child was given once it is reaped.
type child struct {
	cmd   *exec.Cmd
	who   string
	pid   int
	start uint64
	// files are this process's copies of the child's stdout and stderr,
	// closed by release.
	files []*os.File
	// reported says an event named the child; reaped says its exit was
	// handled.
	reported, reaped bool
}

// selfCommand runs the server's own binary in an internal mode, with that
// mode as its whole environment.
func selfCommand(self, mode string) *exec.Cmd {
	cmd := exec.Command(self)
	cmd.Env = []string{InternalEnv + "=" + mode}
	return cmd
}

// startReaperProcess starts self as a reaper in its own session, with
// extra from fd 3 on. Its stdio is the null device: a reaper outliving the
// server must never take SIGPIPE on a closed stderr.
func startReaperProcess(self string, extra []*os.File) (*child, error) {
	cmd := selfCommand(self, modeReaper)
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &child{cmd: cmd, who: "reaper", pid: cmd.Process.Pid}, nil
}

// startLauncher starts self as the launcher in its own session, reading
// its spec from spec (fd 3), in dir, writing to stdout and stderr. A
// cgroup directory, when given, is the cgroup it starts in.
func startLauncher(self string, spec *os.File, dir string, stdout, stderr, cgroup *os.File) (*child, error) {
	cmd := selfCommand(self, modeLauncher)
	cmd.ExtraFiles = []*os.File{spec}
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = nilIfNone(stdout), nilIfNone(stderr)
	attr := &syscall.SysProcAttr{Setsid: true}
	if cgroup != nil {
		attr.UseCgroupFD = true
		attr.CgroupFD = int(cgroup.Fd())
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &child{cmd: cmd, who: WhoWorkload, pid: cmd.Process.Pid, files: []*os.File{stdout, stderr}}, nil
}

// startPerfAgent starts the verified perf-agent with argv (argv[0]
// included) in its own process group, with exactly PerfAgentEnv.
func startPerfAgent(path string, argv []string, dir string, stdout, stderr *os.File) (*child, error) {
	cmd := exec.Command(path)
	if len(argv) > 0 {
		cmd.Args = argv
	}
	cmd.Env = PerfAgentEnv
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = nilIfNone(stdout), nilIfNone(stderr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &child{cmd: cmd, who: WhoPerfAgent, pid: cmd.Process.Pid, files: []*os.File{stdout, stderr}}, nil
}

// nilIfNone keeps a missing file a nil interface, which os/exec maps to
// the null device; a nil *os.File in an io.Writer would not be.
func nilIfNone(f *os.File) io.Writer {
	if f == nil {
		return nil
	}
	return f
}

// release drops the process handle and closes this process's copies of
// the child's output pipes. It runs once the child is reaped.
func (c *child) release() {
	_ = c.cmd.Process.Release()
	for _, f := range c.files {
		if f != nil {
			_ = f.Close()
		}
	}
	c.files = nil
}

// wait waits for a process this process does not reap itself: the
// server's wait for its reaper.
func (c *child) wait() error { return c.cmd.Wait() }
