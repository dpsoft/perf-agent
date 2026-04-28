// Package fleet spawns and manages a set of child processes used as a
// fixture for the perf-agent scenario benchmark. It is not safe for
// production use — error handling assumes a controlled test environment.
package fleet

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Opts configures Spawn. Mix maps language name to the count of
// processes to launch (e.g. {"go": 10, "python": 10, "rust": 5, "node": 5}).
// WorkloadDir points to the test/workloads root.
type Opts struct {
	Mix         map[string]int
	WorkloadDir string

	// StartupTimeout bounds the per-process wait for visibility in
	// /proc/<pid>/stat. 10s is a sensible default.
	StartupTimeout time.Duration
}

// Fleet is a running set of child processes. Stop is idempotent.
type Fleet struct {
	procs []*exec.Cmd

	startupTimeout time.Duration

	mu      sync.Mutex
	stopped bool
}

// Spawn launches Mix processes and returns a Fleet. On any spawn failure,
// already-launched processes are killed and the error is returned.
//
// Per-language launch convention (matches Makefile `test-workloads`):
//   - go:     {dir}/go/cpu_bound (build artifact, executable)
//   - rust:   {dir}/rust/target/release/rust-workload (cargo build artifact)
//   - python: python3 {dir}/python/cpu_bound.py (interpreter required)
//   - node:   node {dir}/node/cpu_bound.js (interpreter required)
//
// Falls back to io_bound for go/python where cpu_bound is unavailable.
// Rust and Node only ship cpu_bound.
func Spawn(opts Opts) (*Fleet, error) {
	if opts.StartupTimeout == 0 {
		opts.StartupTimeout = 10 * time.Second
	}
	if opts.WorkloadDir == "" {
		return nil, errors.New("fleet: WorkloadDir must be set")
	}

	f := &Fleet{startupTimeout: opts.StartupTimeout}
	for lang, count := range opts.Mix {
		argv, err := commandFor(opts.WorkloadDir, lang)
		if err != nil {
			_ = f.Stop()
			return nil, fmt.Errorf("fleet: resolve %s workload: %w", lang, err)
		}
		for i := 0; i < count; i++ {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Stdin = nil
			cmd.Stdout = nil
			cmd.Stderr = nil
			// New process group so SIGKILL doesn't leak grandchildren.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := cmd.Start(); err != nil {
				_ = f.Stop()
				return nil, fmt.Errorf("fleet: start %s[%d] (%v): %w", lang, i, argv, err)
			}
			f.procs = append(f.procs, cmd)
		}
	}
	return f, nil
}

// commandFor returns the argv to spawn one workload of the given lang,
// using the build artifacts produced by `make test-workloads`.
func commandFor(dir, lang string) ([]string, error) {
	switch lang {
	case "go":
		for _, variant := range []string{"cpu_bound", "io_bound"} {
			p := filepath.Join(dir, "go", variant)
			if isExecFile(p) {
				return []string{p}, nil
			}
		}
		return nil, fmt.Errorf("no go binary at %s/go/cpu_bound or io_bound (run `make test-workloads`?)", dir)
	case "rust":
		// Cargo.toml's [package].name is "rust-workload".
		p := filepath.Join(dir, "rust", "target", "release", "rust-workload")
		if isExecFile(p) {
			return []string{p}, nil
		}
		return nil, fmt.Errorf("no rust binary at %s (run `make test-workloads`?)", p)
	case "python":
		for _, variant := range []string{"cpu_bound.py", "io_bound.py"} {
			p := filepath.Join(dir, "python", variant)
			if isFile(p) {
				return []string{"python3", p}, nil
			}
		}
		return nil, fmt.Errorf("no python script at %s/python/cpu_bound.py or io_bound.py", dir)
	case "node":
		p := filepath.Join(dir, "node", "cpu_bound.js")
		if isFile(p) {
			return []string{"node", p}, nil
		}
		return nil, fmt.Errorf("no node script at %s", p)
	default:
		return nil, fmt.Errorf("unknown language %q (want go, rust, python, node)", lang)
	}
}

func isExecFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// Wait blocks until every spawned process is visible in /proc/<pid>/stat
// (i.e. the kernel has it as a task), or timeout elapses, or any process
// has exited (which is treated as a fatal startup failure).
//
// If timeout is 0, falls back to opts.StartupTimeout from Spawn.
func (f *Fleet) Wait(timeout time.Duration) error {
	if timeout == 0 {
		timeout = f.startupTimeout
	}
	deadline := time.Now().Add(timeout)
	for _, cmd := range f.procs {
		pid := cmd.Process.Pid
		for {
			// Check liveness: signal 0 returns ESRCH if process is gone.
			if err := syscall.Kill(pid, 0); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return fmt.Errorf("fleet: pid=%d exited before reaching ready state", pid)
				}
				// EPERM or other — process exists but we can't signal it; fall through to /proc check.
			}
			if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("fleet: pid=%d not ready within %s", pid, timeout)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return nil
}

// PIDs returns a snapshot of currently-tracked process IDs.
func (f *Fleet) PIDs() []int {
	out := make([]int, 0, len(f.procs))
	for _, cmd := range f.procs {
		out = append(out, cmd.Process.Pid)
	}
	return out
}

// Stop sends SIGTERM to every process group, waits 1s, then SIGKILLs
// any still alive. Idempotent — second call returns nil. Returns nil
// even if individual signal/wait operations error; this is a
// best-effort teardown for a benchmark fixture, not a strict
// supervisor.
func (f *Fleet) Stop() error {
	f.mu.Lock()
	if f.stopped {
		f.mu.Unlock()
		return nil
	}
	f.stopped = true
	f.mu.Unlock()

	// SIGTERM the whole group of each process.
	for _, cmd := range f.procs {
		if cmd.Process == nil {
			continue
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}

	// Wait up to graceful timeout for them to exit; SIGKILL stragglers.
	done := make(chan struct{})
	go func() {
		for _, cmd := range f.procs {
			_ = cmd.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(1 * time.Second):
	}

	// Grace expired — escalate. SIGKILL the groups; the goroutine
	// above will unblock as each Wait returns. We do NOT call Wait
	// again here (would race with the goroutine and return "Wait
	// was already called" without doing anything useful).
	for _, cmd := range f.procs {
		if cmd.Process == nil {
			continue
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Block until the goroutine drains. SIGKILL is uninterruptible,
	// so this returns within milliseconds.
	<-done
	return nil
}
