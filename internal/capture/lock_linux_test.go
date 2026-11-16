//go:build linux

package capture

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openDir(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return dir, root
}

func TestTryLockIsExclusiveAcrossProcesses(t *testing.T) {
	dir, root := openDir(t)
	deadline := time.Now().Add(90 * time.Second).UTC().Truncate(time.Second)
	c := startChildWith(t, "hold-lock", []string{testLockDirEnv + "=" + dir,
		testLockDeadlineEnv + "=" + deadline.Format(time.RFC3339)}, nil)
	if c.Line != "held" {
		t.Fatalf("child said %q", c.Line)
	}
	l, e := TryLock(root, Holder{ServerPID: os.Getpid()})
	if e == nil {
		_ = l.Close()
		t.Fatal("TryLock succeeded while another process holds it")
	}
	if e.Code != CodeBusy || !e.Retryable {
		t.Fatalf("got %v, want retryable busy", e)
	}
	for _, want := range []string{"pid " + strconv.Itoa(c.PID), "cap/001", deadline.Format(time.RFC3339)} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q lacks %q", e.Message, want)
		}
	}
	if e.RetryAfter < time.Second {
		t.Errorf("RetryAfter = %v, want >= 1s", e.RetryAfter)
	}
	c.kill()
	l, e = TryLock(root, Holder{ServerPID: os.Getpid()})
	if e != nil {
		t.Fatalf("after the holder died: %v", e)
	}
	_ = l.Close()
}

func TestTheLockSurvivesInAnInheritedDescriptor(t *testing.T) {
	_, root := openDir(t)
	l, e := TryLock(root, Holder{ServerPID: os.Getpid(), CaptureID: "cap/002"})
	if e != nil {
		t.Fatal(e)
	}
	c := startChildWith(t, "sleep", nil, []*os.File{l.File()})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if l.File() != nil {
		t.Error("File is not nil after Close")
	}
	if l2, e := TryLock(root, Holder{}); e == nil {
		_ = l2.Close()
		t.Fatal("the lock was released while the child holds a duplicate")
	} else if e.Code != CodeBusy || !strings.Contains(e.Message, "cap/002") {
		t.Fatalf("got %v", e)
	}
	c.kill()
	l2, e := TryLock(root, Holder{})
	if e != nil {
		t.Fatalf("after the inheriting child died: %v", e)
	}
	_ = l2.Close()
}

func TestTryLockRewritesTheHolder(t *testing.T) {
	dir, root := openDir(t)
	l, e := TryLock(root, Holder{ServerPID: os.Getpid(), CaptureID: "a/001", Series: strings.Repeat("s", 200)})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = l.Close() }()
	h := Holder{ServerPID: os.Getpid(), ReaperPID: 424242, CaptureID: "a/001", Deadline: time.Now().Add(time.Minute)}
	if err := l.Update(h); err != nil {
		t.Fatal(err)
	}
	// A second description in this process conflicts like another process.
	_, e = TryLock(root, Holder{})
	if e == nil || e.Code != CodeBusy || !strings.Contains(e.Message, "reaper pid 424242") {
		t.Fatalf("got %v", e)
	}
	if b, _ := os.ReadFile(dir + "/.lock"); strings.Contains(string(b), "sss") {
		t.Errorf("old record left behind: %q", b)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Update(h); err == nil {
		t.Error("Update after Close succeeded")
	}
}

func TestBusyWithAGarbledHolderStillSaysBusy(t *testing.T) {
	dir, root := openDir(t)
	l, e := TryLock(root, Holder{ServerPID: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = l.Close() }()
	for _, junk := range []string{"", "{not json", "null", "\x00\x01\x02"} {
		if err := os.WriteFile(dir+"/.lock", []byte(junk), 0o600); err != nil {
			t.Fatal(err)
		}
		_, e := TryLock(root, Holder{})
		if e == nil || e.Code != CodeBusy || e.RetryAfter < time.Second {
			t.Errorf("record %q: got %v", junk, e)
		}
	}
}

func TestAStaleRecordDoesNotBlock(t *testing.T) {
	dir, root := openDir(t)
	rec := `{"server_pid":999999,"server_start":1,"capture_id":"old/001"}`
	if err := os.WriteFile(dir+"/.lock", []byte(rec+strings.Repeat(" ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	l, e := TryLock(root, Holder{ServerPID: os.Getpid(), CaptureID: "new/001"})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = l.Close() }()
	b, _ := os.ReadFile(dir + "/.lock")
	if strings.Contains(string(b), "old/001") {
		t.Errorf("stale record not replaced: %q", b)
	}
}

func TestTryLockRefusesASymlinkedLockFile(t *testing.T) {
	dir, root := openDir(t)
	victim := dir + "/victim"
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("victim", dir+"/.lock"); err != nil {
		t.Fatal(err)
	}
	l, e := TryLock(root, Holder{ServerPID: os.Getpid()})
	if e == nil {
		_ = l.Close()
		t.Fatal("TryLock followed a symlink")
	}
	if e.Code != CodeDisk {
		t.Errorf("got %v, want a disk error", e)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim modified: %q", b)
	}
}

func TestBusyReadsAHolderWithATornTail(t *testing.T) {
	dir, root := openDir(t)
	l, e := TryLock(root, Holder{ServerPID: os.Getpid()})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = l.Close() }()
	long := `{"server_pid":7,"capture_id":"x/001","series":"` + strings.Repeat("s", 300) + `"}`
	short := `{"server_pid":7,"reaper_pid":8,"capture_id":"x/002"}`
	// A crash between the write and the truncate leaves the new record
	// over the old one's tail.
	if err := os.WriteFile(dir+"/.lock", []byte(short+long[len(short):]), 0o600); err != nil {
		t.Fatal(err)
	}
	_, e = TryLock(root, Holder{})
	if e == nil || e.Code != CodeBusy || !strings.Contains(e.Message, "x/002") || !strings.Contains(e.Message, "reaper pid 8") {
		t.Fatalf("got %v", e)
	}
}

func TestBusyWithoutADeadlineSaysUnknown(t *testing.T) {
	_, root := openDir(t)
	l, e := TryLock(root, Holder{ServerPID: os.Getpid(), CaptureID: "d/001"})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = l.Close() }()
	_, e = TryLock(root, Holder{})
	if e == nil || !strings.Contains(e.Message, "ends by unknown") || strings.Contains(e.Message, "0001") {
		t.Fatalf("got %v", e)
	}
}

// A holder with no capture yet (a maintenance pass) and no reaper is named
// by what it is, without empty fields.
func TestBusyNamesAHolderWithoutACapture(t *testing.T) {
	_, root := openDir(t)
	l, e := TryLock(root, Holder{ServerPID: os.Getpid(), Deadline: time.Now().Add(5 * time.Second)})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { _ = l.Close() }()
	_, e = TryLock(root, Holder{})
	if e == nil || strings.Contains(e.Message, "capture  by") || strings.Contains(e.Message, "reaper pid 0") ||
		!strings.Contains(e.Message, "perf-agent-mcp pid "+strconv.Itoa(os.Getpid())) || e.RetryAfter > 6*time.Second {
		t.Fatalf("got %v (retry after %v)", e, e.RetryAfter)
	}
}
