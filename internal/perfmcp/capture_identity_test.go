package perfmcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// TestRemovalQuarantinesBeforeItUnlinks: a file swapped in under the name
// after the identity check is not unlinked. removeByIdentity moves the
// name aside, checks what it moved, and puts back what is not its own.
func TestRemovalQuarantinesBeforeItUnlinks(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeFile(t, root, "f", "ours")
	want := recordedIdentity(t, root, "f")
	quarantineHook = func(r *os.Root, name string) {
		mustWrite(t, filepath.Join(dir, "swap"), []byte("user"))
		if err := os.Rename(filepath.Join(dir, "swap"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { quarantineHook = nil }()
	removed, left, err := removeByIdentity(root, "f", want)
	if removed || left == "" || err != nil {
		t.Fatalf("removed %v, left %q, err %v", removed, left, err)
	}
	if got, err := root.ReadFile("f"); err != nil || string(got) != "user" {
		t.Fatalf("the swapped-in file is %q, %v", got, err)
	}
	if names := dirNames(t, root, "."); len(names) != 1 {
		t.Fatalf("left behind %v", names)
	}
}

func recordedIdentity(t *testing.T, root *os.Root, name string) fileIdentity {
	t.Helper()
	id, err := identify(root, name)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := openNoFollow(root, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if id.SHA256, err = hashFile(f); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRemovalProvesContentsNotJustTheInode: a recorded temporary whose
// inode is the same but whose bytes are not (rewritten in place, or a
// reused inode number) is not provably the protocol's, and stays.
func TestRemovalProvesContentsNotJustTheInode(t *testing.T) {
	s, d, clk := newCaptureServer(t)
	s.captureFault = inject(shot{faultAfterManifestTemp, 1, errCrash})
	r := drive(t, s, d, capCase{})
	s.captureFault = nil
	tmp := filepath.Join(d.root.Name(), "adhoc", "."+manifestName(r.id, "run")+".tmp-"+r.txn.in.Token[:16])
	b, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	other := []byte(strings.Repeat("x", len(b)))
	if err := os.WriteFile(tmp, other, 0o600); err != nil {
		t.Fatal(err)
	}
	notes := crashRecover(t, s, d, clk)
	if got, err := os.ReadFile(tmp); err != nil || string(got) != string(other) {
		t.Fatalf("a temporary with other contents was removed: %v", err)
	}
	if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
		t.Errorf("outcome %q", m.Outcome)
	}
	invariant(t, d, notes)
}

// TestAFutureHeartbeatIsStale: a heartbeat dated further ahead than the
// staleness bound (a clock that jumped back) does not keep a dead owner's
// intent alive.
func TestAFutureHeartbeatIsStale(t *testing.T) {
	s, d, _, r := crashedIntent(t)
	s.ownerAlive = func(intent) bool { return false }
	hb := filepath.Join(d.root.Name(), "adhoc", heartbeatName(r.id, "run"))
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(hb, future, future); err != nil {
		t.Fatal(err)
	}
	notes := recoverNow(t, s, d)
	if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
		t.Fatalf("outcome %q (notes %q)", m.Outcome, notes)
	}
	invariant(t, d, notes)
}

// noOrphans fails on any note that says something was left in place.
func noOrphans(t *testing.T, notes []string) {
	t.Helper()
	for _, n := range notes {
		if strings.HasPrefix(n, "orphan_") || strings.HasPrefix(n, "left ") {
			t.Errorf("left in place: %s", n)
		}
	}
}

// TestATokenTempSharingTheIntentsInodeIsRemoved: a crash between linking
// the intent and removing its temporary's name leaves a second name of the
// intent itself; recovery proves it by the intent's identity and removes
// it.
func TestATokenTempSharingTheIntentsInodeIsRemoved(t *testing.T) {
	s, d, clk := newCaptureServer(t)
	s.writeErr = failOp("remove intent temp", errCrash)
	r := drive(t, s, d, capCase{})
	s.writeErr = nil
	temps, _ := filepath.Glob(filepath.Join(d.root.Name(), "adhoc", "."+intentName(1, "run")+".tmp-*"))
	if len(temps) != 1 || r.err == nil {
		t.Fatalf("temporaries %v, err %v", temps, r.err)
	}
	notes := crashRecover(t, s, d, clk)
	noOrphans(t, notes)
	invariant(t, d, notes)
	if _, err := os.Lstat(temps[0]); !os.IsNotExist(err) {
		t.Fatalf("the intent's second name is still there: %v", err)
	}
}

// TestAnIntentTempThatCouldNotBeRemovedIsRetried: when removing the
// intent's temporary fails, the capture goes on, records it, and removes
// it with the rest.
func TestAnIntentTempThatCouldNotBeRemovedIsRetried(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	failed := false
	s.writeErr = func(op string) error {
		if op == "remove intent temp" && !failed {
			failed = true
			return syscall.EIO
		}
		return nil
	}
	r := drive(t, s, d, capCase{})
	s.writeErr = nil
	if !r.committed || r.err != nil {
		t.Fatalf("committed %v, err %v", r.committed, r.err)
	}
	notes := recoverNow(t, s, d)
	noOrphans(t, notes)
	invariant(t, d, notes)
	if temps, _ := filepath.Glob(filepath.Join(d.root.Name(), "adhoc", ".*.tmp-*")); len(temps) != 0 {
		t.Fatalf("temporaries left: %v", temps)
	}
}

// TestAStagingDirectorySwappedAfterItsCheckIsLeft: the staging directory
// is proven again once it is open, so a directory swapped in after the
// first check keeps what it holds.
func TestAStagingDirectorySwappedAfterItsCheckIsLeft(t *testing.T) {
	s, d, clk, r := crashedIntent(t)
	series := filepath.Join(d.root.Name(), "adhoc")
	staging := filepath.Join(series, r.txn.in.Staging)
	openStagingHook = func(*os.Root, string) {
		if err := os.Rename(staging, staging+".moved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(staging, "cpu.pb.gz"), []byte("user"))
	}
	defer func() { openStagingHook = nil }()
	notes := crashRecover(t, s, d, clk)
	if got, err := os.ReadFile(filepath.Join(staging, "cpu.pb.gz")); err != nil || string(got) != "user" {
		t.Fatalf("the swapped-in directory's file is %q, %v", got, err)
	}
	invariant(t, d, notes, filepath.Base(staging)+".moved")
}

// quarantineCrash makes the n-th quarantine move the last thing the
// process does: the file is renamed aside, and nothing after happens.
func quarantineCrash(n int) func() {
	var seen int
	quarantineAfterHook = func() error {
		seen++
		if seen == n {
			return errCrash
		}
		return nil
	}
	return func() { quarantineAfterHook = nil }
}

// TestAQuarantinedFileIsProvenByItsCapture: a crash between moving a file
// aside and unlinking it leaves a quarantine name that carries the
// capture's token; recovery proves it against what the intent recorded
// (or the token it holds) and removes it, at every removal of a commit's
// cleanup and of a rollback.
func TestAQuarantinedFileIsProvenByItsCapture(t *testing.T) {
	// The removals of a clean capture, counted in a run that does not crash.
	count := 0
	s0, d0, _ := newCaptureServer(t)
	quarantineAfterHook = func() error { count++; return nil }
	if r := drive(t, s0, d0, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}}); !r.committed {
		t.Fatal(r.err)
	}
	quarantineAfterHook = nil
	if count < 6 {
		t.Fatalf("only %d removals in a clean capture", count)
	}
	for n := 1; n <= count; n++ {
		t.Run(fmt.Sprintf("commit-removal-%d", n), func(t *testing.T) {
			s, d, clk := newCaptureServer(t)
			defer quarantineCrash(n)()
			r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
			quarantineAfterHook = nil
			if !errors.Is(r.err, errCrash) {
				t.Fatalf("no crash at removal %d: %v", n, r.err)
			}
			notes := crashRecover(t, s, d, clk)
			noOrphans(t, notes)
			invariant(t, d, notes)
		})
	}
	t.Run("rollback-removal", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.captureFault = inject(shot{faultAfterLinks, 1, errInjected})
		defer quarantineCrash(1)()
		r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
		quarantineAfterHook = nil
		s.captureFault = nil
		if !errors.Is(r.err, errCrash) {
			t.Fatalf("no crash: %v", r.err)
		}
		notes := crashRecover(t, s, d, clk)
		noOrphans(t, notes)
		invariant(t, d, notes)
	})
}

// TestTokenNamedTemporariesAreProvenByWhatTheyHold: a temporary a crash
// left after it was written and before anything recorded it carries the
// intent's token in its name and holds this capture's intent or manifest;
// that proves it.
func TestTokenNamedTemporariesAreProvenByWhatTheyHold(t *testing.T) {
	s, d, clk, r := crashedIntent(t)
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	tok := r.txn.in.Token[:16]
	m := r.txn.manifestOf(manifest{}, captureFailed, nil)
	b, _ := json.Marshal(m)
	writeFile(t, ser, "."+manifestName(1, "run")+".tmp-"+tok, string(b))
	notes := crashRecover(t, s, d, clk)
	noOrphans(t, notes)
	invariant(t, d, notes)

	// An intent temporary whose intent was never linked (the id wasted).
	s2, d2, clk2 := newCaptureServer(t)
	s2.link = func(r *os.Root, oldname, newname string) error {
		if strings.HasSuffix(newname, intentSuffix) {
			return errCrash
		}
		return r.Link(oldname, newname)
	}
	drive(t, s2, d2, capCase{})
	s2.link = (*os.Root).Link
	temps, _ := filepath.Glob(filepath.Join(d2.root.Name(), "adhoc", "."+intentName(1, "run")+".tmp-*"))
	if len(temps) != 1 {
		t.Fatalf("intent temporaries %v", temps)
	}
	notes = crashRecover(t, s2, d2, clk2)
	noOrphans(t, notes)
	invariant(t, d2, notes)
}
