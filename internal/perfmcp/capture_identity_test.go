package perfmcp

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
