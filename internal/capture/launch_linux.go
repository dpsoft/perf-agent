package capture

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
)

// prctl options the syscall package does not define.
const (
	prSetChildSubreaper = 36
	prSetNoNewPrivs     = 38
)

// noNewPrivs sets no_new_privs on the calling thread: no later execve on
// it, or in anything it forks, can gain privileges through setuid, setgid
// or file capabilities.
func noNewPrivs() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); e != 0 {
		return e
	}
	return nil
}

// setChildSubreaper makes this process the reaper of its orphaned
// descendants, so a double-forked or setsid'd one reparents here rather
// than to init.
func setChildSubreaper() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); e != 0 {
		return e
	}
	return nil
}

// launch is the launcher: it sets no_new_privs and executes the workload
// in place. no_new_privs is a per-thread attribute that survives execve,
// so the thread that sets it must be the one that executes: the OS thread
// is locked first. The reaper set the session, the cgroup, the working
// directory and the output descriptors; the environment is exactly
// spec.Env. It returns only on failure, with 126 (or 127 when the command
// is missing) and the reason on stderr, which is the workload's stderr.
func launch(spec LaunchSpec) int {
	runtime.LockOSThread()
	if err := noNewPrivs(); err != nil {
		fmt.Fprintf(os.Stderr, "perf-agent-mcp launcher: prctl(PR_SET_NO_NEW_PRIVS): %v\n", err)
		return 126
	}
	argv := spec.Args
	if len(argv) == 0 {
		argv = []string{spec.Path}
	}
	err := syscall.Exec(spec.Path, argv, spec.Env)
	fmt.Fprintf(os.Stderr, "perf-agent-mcp launcher: exec %s: %v\n", spec.Path, err)
	if errors.Is(err, syscall.ENOENT) {
		return 127
	}
	return 126
}
