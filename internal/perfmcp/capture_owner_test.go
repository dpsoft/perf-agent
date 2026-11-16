package perfmcp

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// ownerTemps are the temporaries of .owner left in the capture directory
// under profiles.
func ownerTemps(t *testing.T, profiles string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(profiles, defaultStdioCaptures))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "."+ownerFile+".tmp-") || strings.HasPrefix(e.Name(), ownerFile+".tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// Opening a capture directory whose .owner already names this server's
// kind reads it and writes nothing, so no exit or crash can leave a
// temporary of it behind.
func TestAnOwnedCaptureDirIsOpenedWithoutAWrite(t *testing.T) {
	var writes atomic.Int32
	e := newJobEnv(t, jobOpts{setup: func(s *Server) {
		s.beforePublish = func(name string) {
			if name == ownerFile {
				writes.Add(1)
			}
		}
	}})
	// The start-up pass must not race the first open for the new .owner.
	<-e.s.startPassDone
	for range 3 {
		d, ce := e.s.openCaptureDir()
		if ce != nil {
			t.Fatal(ce)
		}
		d.close()
	}
	if n := writes.Load(); n != 1 {
		t.Errorf(".owner was written %d times, want once (when the directory was new)", n)
	}
	if left := ownerTemps(t, e.profiles); len(left) > 0 {
		t.Errorf("temporaries left: %q", left)
	}
}

// Close waits for the maintenance goroutine: a pass that is writing when
// the server closes has finished, its temporary removed, by the time Close
// returns, so a process that exits after Close leaves none behind.
func TestCloseWaitsForTheMaintenancePass(t *testing.T) {
	profiles := t.TempDir()
	if err := os.Mkdir(filepath.Join(profiles, defaultStdioCaptures), 0o700); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	e := newJobEnv(t, jobOpts{profiles: profiles, setup: func(s *Server) {
		s.beforePublish = func(name string) {
			if name == ownerFile {
				close(entered)
				time.Sleep(300 * time.Millisecond)
			}
		}
	}})
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the start-up pass never wrote .owner")
	}
	if err := e.s.Close(); err != nil {
		t.Fatal(err)
	}
	if left := ownerTemps(t, profiles); len(left) > 0 {
		t.Errorf("Close returned while maintenance had written %q", left)
	}
}

// A start-up pass that finds the captures lock held (a dead server's
// reaper, say) is not given up: it is run again while the lock is held,
// and runs once it is free, with no capture to trigger it.
func TestABusyStartPassIsRunAgain(t *testing.T) {
	profiles := t.TempDir()
	dir := filepath.Join(profiles, defaultStdioCaptures)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	lease, ce := capture.TryLock(root, capture.Holder{ServerPID: os.Getpid(), CaptureID: "adhoc/001", Deadline: time.Now().Add(time.Hour)})
	if ce != nil {
		t.Fatal(ce)
	}
	e := newJobEnv(t, jobOpts{profiles: profiles})
	select {
	case <-e.s.startPassDone:
		t.Fatal("the start-up pass ran while the lock was held")
	case <-time.After(1500 * time.Millisecond):
	}
	_ = lease.Close()
	select {
	case <-e.s.startPassDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the start-up pass never ran once the lock was free")
	}
}
