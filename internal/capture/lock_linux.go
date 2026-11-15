//go:build linux

package capture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// lockName is the captures directory's lock file. It also carries the
// holder record, so a refused capture can say who is running.
const lockName = ".lock"

// minRetryAfter is the shortest retry_after a busy answer gives.
const minRetryAfter = time.Second

// Lease is the captures lock: an exclusive flock(2) on <captures>/.lock.
// flock locks belong to the open file description, so any process holding
// a duplicate of File's descriptor keeps the directory locked after this
// process closes its own. The lock is released when the last descriptor on
// the description is closed, or the last holder exits.
type Lease struct {
	mu sync.Mutex
	f  *os.File
}

// TryLock takes the captures lock without blocking and records h in the
// lock file. When another description holds it, TryLock returns a busy
// Error naming the recorded holder; a record that is missing, torn or
// garbled still yields busy. The kernel lock, not the record, decides who
// holds the directory, so a record left by a holder that is gone is
// overwritten by the next successful TryLock and never blocks it.
func TryLock(dir *os.Root, h Holder) (*Lease, *Error) {
	f, err := dir.OpenFile(lockName, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, Errorf(CodeDisk, "check the captures directory", "open %s: %v", lockName, err)
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		defer func() { _ = f.Close() }()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, busyError(f)
		}
		return nil, Errorf(CodeDisk, "check the captures directory", "lock %s: %v", lockName, err)
	}
	l := &Lease{f: f}
	if err := l.write(h); err != nil {
		_ = f.Close()
		return nil, Errorf(CodeDisk, "check the captures directory", "record the lock holder: %v", err)
	}
	return l, nil
}

// busyError reads the holder record from f and describes it.
func busyError(f *os.File) *Error {
	e := &Error{Code: CodeBusy, Retryable: true, RetryAfter: minRetryAfter,
		Next: "retry after the running capture ends"}
	buf := make([]byte, 64<<10)
	n, _ := f.ReadAt(buf, 0)
	var h Holder
	if n == 0 || json.Unmarshal(buf[:n], &h) != nil || h.ServerPID == 0 && h.ReaperPID == 0 {
		e.Message = "another capture holds the captures directory (its holder record is unreadable)"
		return e
	}
	e.Message = fmt.Sprintf("capture %s by perf-agent-mcp pid %d (reaper pid %d), ends by %s",
		h.CaptureID, h.ServerPID, h.ReaperPID, h.Deadline.UTC().Format(time.RFC3339))
	if !h.Deadline.IsZero() {
		e.RetryAfter = max(time.Until(h.Deadline), minRetryAfter)
	}
	return e
}

// write replaces the record in one write and then trims, so a concurrent
// reader never sees an empty file in between.
func (l *Lease) write(h Holder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if _, err := l.f.WriteAt(b, 0); err != nil {
		return err
	}
	return l.f.Truncate(int64(len(b)))
}

// File is the locked descriptor. To hand the lock to a child, pass it as
// an exec.Cmd.ExtraFiles entry: the child receives a duplicate of the same
// open file description, without close-on-exec, and keeps the lock until it
// closes the descriptor or exits, whatever this process does. The Lease
// still owns the original, which is close-on-exec, so no other child
// inherits it. File is nil after Close.
func (l *Lease) File() *os.File {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f
}

// Update rewrites the holder record, for instance once the reaper's PID is
// known. The lock is unchanged.
func (l *Lease) Update(h Holder) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return os.ErrClosed
	}
	return l.write(h)
}

// Close closes this process's descriptor. The directory stays locked while
// any inherited duplicate is open.
func (l *Lease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
