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
func launch(w launcherWire) int {
	spec := w.Spec
	runtime.LockOSThread()
	// The sync descriptor closes on a successful exec, and also when the
	// launcher dies, so EOF alone proves nothing; a failure is written to
	// it first, and the marker says the exec was reached.
	syscall.CloseOnExec(launchSyncFD)
	fail := func(code int, format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		_, _ = syscall.Write(launchSyncFD, []byte(msg))
		fmt.Fprintln(os.Stderr, "perf-agent-mcp launcher: "+msg)
		return code
	}
	if err := noNewPrivs(); err != nil {
		return fail(126, "prctl(PR_SET_NO_NEW_PRIVS): %v", err)
	}
	argv := spec.Args
	if len(argv) == 0 {
		argv = []string{spec.Path}
	}
	// The marker comes last before the exec: EOF after it, and nothing
	// more, is the exec closing the pipe or the launcher dying before the
	// exec took effect; the two cannot be told apart from here.
	if _, err := syscall.Write(launchSyncFD, []byte{execMarker}); err != nil {
		fmt.Fprintf(os.Stderr, "perf-agent-mcp launcher: write the exec marker: %v\n", err)
		return 126
	}
	if w.DieAfterMarker {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	err := syscall.Exec(spec.Path, argv, spec.Env)
	code := 126
	if errors.Is(err, syscall.ENOENT) {
		code = 127
	}
	return fail(code, "exec %s: %v", spec.Path, err)
}

// launchSyncFD is the launcher's end of the exec handshake.
const launchSyncFD = 4

// execMarker is what the launcher writes on the sync pipe just before it
// executes the command: EOF right after it is an exec attempted (it
// succeeded, or the launcher died before it took effect), the marker
// followed by text is a failed one, and nothing at all is a launcher that
// exited before it got that far.
const execMarker byte = '!'
