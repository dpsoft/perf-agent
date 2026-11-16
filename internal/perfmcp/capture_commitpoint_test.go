package perfmcp

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// landedButFailed is a link seam whose link to a manifest name is made but
// reported as failed with errno, as a retransmitted NFS request reports
// EEXIST for the link it already made.
func landedButFailed(errno syscall.Errno) func(*os.Root, string, string) error {
	return func(r *os.Root, oldname, newname string) error {
		err := r.Link(oldname, newname)
		if err == nil && strings.HasSuffix(newname, manifestSuffix) {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
		}
		return err
	}
}

// TestALinkThatLandedIsCommitted: whatever link(2) answered, a manifest
// name that now is the manifest's temporary is the commit point passed:
// the capture is committed and keeps its files.
func TestALinkThatLandedIsCommitted(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EEXIST, syscall.EIO} {
		t.Run(errno.Error(), func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			s.link = landedButFailed(errno)
			r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
			if !r.committed || r.err != nil {
				t.Fatalf("committed %v, err %v", r.committed, r.err)
			}
			if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureOK || len(m.Files) != 3 {
				t.Fatalf("manifest %+v", m)
			}
			notes := recoverNow(t, s, d)
			noOrphans(t, notes)
			invariant(t, d, notes)
		})
	}

	// The same for a failure manifest: the rollback is done, not stranded.
	t.Run("failure-manifest", func(t *testing.T) {
		s, d, _ := newCaptureServer(t)
		s.link = landedButFailed(syscall.EEXIST)
		s.captureFault = inject(shot{faultAfterLinks, 1, errInjected})
		r := drive(t, s, d, capCase{})
		s.captureFault = nil
		if r.committed || captureErr(t, r.err).Code != capture.CodeDisk {
			t.Fatalf("committed %v err %v", r.committed, r.err)
		}
		if len(s.stranded) != 0 {
			t.Fatal("the rollback was stranded although its manifest is linked")
		}
		if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed {
			t.Fatalf("manifest %+v", m)
		}
		notes := recoverNow(t, s, d)
		noOrphans(t, notes)
		invariant(t, d, notes)
	})
}

// TestAForeignFileAtTheManifestNameIsExists: a manifest name taken by a
// file this server did not write is exists, never disk, and the file is
// kept byte for byte.
func TestAForeignFileAtTheManifestNameIsExists(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	theirs := []byte("not a manifest\n")
	name := filepath.Join(d.root.Name(), "adhoc", manifestName(1, "run"))
	s.captureFault = func(p faultPoint) error {
		if p == faultAfterManifestTemp {
			mustWrite(t, name, theirs)
		}
		return nil
	}
	r := drive(t, s, d, capCase{})
	s.captureFault = nil
	if r.committed || captureErr(t, r.err).Code != capture.CodeExists {
		t.Fatalf("committed %v err %v, want exists", r.committed, r.err)
	}
	if got, err := os.ReadFile(name); err != nil || !bytes.Equal(got, theirs) {
		t.Fatalf("the foreign file is %q, %v", got, err)
	}
	if files, _ := filepath.Glob(filepath.Join(d.root.Name(), "adhoc", "001-run-*")); len(files) != 0 {
		t.Fatalf("capture files left: %v", files)
	}
}

// TestTheHeartbeatGoesBeforeTheIntent: the heartbeat is created after the
// intent and removed before it, so a crash between the two never leaves a
// heartbeat no intent records.
func TestTheHeartbeatGoesBeforeTheIntent(t *testing.T) {
	for _, op := range []string{"remove heartbeat", "remove intent"} {
		t.Run(op, func(t *testing.T) {
			s, d, clk := newCaptureServer(t)
			s.writeErr = failOp(op, errCrash)
			r := drive(t, s, d, capCase{})
			s.writeErr = nil
			if !r.committed {
				t.Fatalf("not committed: %v", r.err)
			}
			notes := crashRecover(t, s, d, clk)
			noOrphans(t, notes)
			invariant(t, d, notes)
		})
	}

	// A crash after the heartbeat is written and before the intent records
	// it: the heartbeat holds the intent's token, which proves it.
	t.Run("before-the-record", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.writeErr = failOpAt("intent rewrite", 1, errCrash)
		r := drive(t, s, d, capCase{})
		s.writeErr = nil
		if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", heartbeatName(1, "run"))); err != nil || !r.intent {
			t.Fatalf("no heartbeat to prove: %v (intent %v)", err, r.intent)
		}
		notes := crashRecover(t, s, d, clk)
		// The rewrite's own temporary was created empty when the crash
		// came, and proves nothing: the one leftover of that window.
		for _, n := range notes {
			if !strings.HasPrefix(n, "orphan_temp adhoc/."+intentName(1, "run")+".tmp-") && (strings.HasPrefix(n, "orphan_") || strings.HasPrefix(n, "left ")) {
				t.Errorf("left in place: %s", n)
			}
		}
		invariant(t, d, notes)
	})

	// An intent temporary a crash left after writing it holds the intent
	// with its token, which proves it.
	t.Run("written-intent-temp", func(t *testing.T) {
		s, d, clk, r := crashedIntent(t)
		ser, _ := d.root.OpenRoot("adhoc")
		defer func() { _ = ser.Close() }()
		b, err := ser.ReadFile(intentName(1, "run"))
		if err != nil {
			t.Fatal(err)
		}
		tmp := "." + intentName(1, "run") + ".tmp-" + r.txn.in.Token[:16] + "-1"
		writeFile(t, ser, tmp, string(b))
		notes := crashRecover(t, s, d, clk)
		noOrphans(t, notes)
		invariant(t, d, notes)
	})

	// An empty file at the heartbeat's name proves nothing and stays.
	t.Run("empty-heartbeat", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.writeErr = failOp("heartbeat", errCrash)
		drive(t, s, d, capCase{})
		s.writeErr = nil
		notes := crashRecover(t, s, d, clk)
		if !noted(notes, "left", heartbeatName(1, "run")) {
			t.Fatalf("notes %q", notes)
		}
		invariant(t, d, notes, heartbeatName(1, "run"))
	})

	// A crash after the intent and before the heartbeat leaves an intent
	// judged by its owner's liveness alone.
	t.Run("before-the-heartbeat", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.captureFault = func(p faultPoint) error {
			if p == faultFsync {
				if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", intentName(1, "run"))); err == nil {
					return errCrash
				}
			}
			return nil
		}
		r := drive(t, s, d, capCase{})
		s.captureFault = nil
		if !r.intent {
			t.Fatalf("no intent: %v", r.err)
		}
		if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", heartbeatName(1, "run"))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a heartbeat exists before the intent was synced: %v", err)
		}
		notes := crashRecover(t, s, d, clk)
		noOrphans(t, notes)
		invariant(t, d, notes)
		if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureInterrupted {
			t.Fatalf("outcome %q", m.Outcome)
		}
	})
}

// TestBeginFailsClosedWithoutAStartTime: an intent records its owner's
// start time so recovery can tell the owner is alive; without one no
// intent is written.
func TestBeginFailsClosedWithoutAStartTime(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.selfStartTime = func() (uint64, error) { return 0, errors.New("no /proc") }
	r := drive(t, s, d, capCase{})
	if r.intent || r.err == nil {
		t.Fatalf("intent %v, err %v", r.intent, r.err)
	}
	if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureFailed {
		t.Fatalf("manifest %+v", m)
	}
	invariant(t, d, recoverNow(t, s, d))
}

// TestAStagingDirectoryThatWouldNotGoKeepsItsIntent: when removing the
// emptied staging directory fails, the cleanup stops with the intent in
// place, and the next recovery removes the directory.
func TestAStagingDirectoryThatWouldNotGoKeepsItsIntent(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EIO, syscall.ENOSPC} {
		t.Run(errno.Error(), func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			s.writeErr = failOpAt("remove staging", 1, errno)
			r := drive(t, s, d, capCase{})
			s.writeErr = nil
			if !r.committed || r.err == nil {
				t.Fatalf("committed %v, err %v", r.committed, r.err)
			}
			if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", intentName(1, "run"))); err != nil {
				t.Fatalf("the intent went although staging stayed: %v", err)
			}
			notes := recoverNow(t, s, d)
			noOrphans(t, notes)
			invariant(t, d, notes)
		})
	}
}
