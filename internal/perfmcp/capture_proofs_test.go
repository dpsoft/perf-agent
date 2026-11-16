package perfmcp

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// crashAtFault runs a cpu+gpu capture that dies at the first time p is
// reached.
func crashAtFault(t *testing.T, p faultPoint) (*Server, *captureDir, *testClock, capRun) {
	t.Helper()
	s, d, clk := newCaptureServer(t)
	s.captureFault = inject(shot{p, 1, errCrash})
	r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
	s.captureFault = nil
	if !errors.Is(r.err, errCrash) {
		t.Fatalf("no crash at %v: %v", p, r.err)
	}
	return s, d, clk, r
}

// serPath is name's path in the adhoc series.
func serPath(d *captureDir, name string) string {
	return filepath.Join(d.root.Name(), "adhoc", name)
}

// landsWithEEXIST is a link seam whose links to names that pass pick are
// made but answered EEXIST, as a retransmitted NFS request answers.
func landsWithEEXIST(pick func(newname string) bool) func(*os.Root, string, string) error {
	return func(r *os.Root, oldname, newname string) error {
		err := r.Link(oldname, newname)
		if err == nil && pick(newname) {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EEXIST}
		}
		return err
	}
}

// TestAnUnknownManifestLinkIsNeverRolledBack: when the manifest link
// answered an error and the check of the name fails too, nobody knows
// whether the capture committed; it is not rolled back, and the next
// capture's retry settles it from what is on disk.
func TestAnUnknownManifestLinkIsNeverRolledBack(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.link = landsWithEEXIST(func(n string) bool { return strings.HasSuffix(n, manifestSuffix) })
	failed := false
	landedLstat = func(root *os.Root, name string) (fs.FileInfo, error) {
		if strings.HasSuffix(name, manifestSuffix) && !failed {
			failed = true
			return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.EIO}
		}
		return root.Lstat(name)
	}
	defer func() { landedLstat = (*os.Root).Lstat }()
	r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
	s.link = (*os.Root).Link
	if r.committed || r.err == nil {
		t.Fatalf("committed %v, err %v", r.committed, r.err)
	}
	m := manifestFor(t, d, "adhoc", 1, "run")
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	if err := verifyFiles(ser, m); m.Outcome != captureOK || err != nil {
		t.Fatalf("the committed capture lost its files: outcome %q, %v", m.Outcome, err)
	}
	if err := s.retryStranded(); err != nil {
		t.Fatalf("retryStranded: %v", err)
	}
	notes := recoverNow(t, s, d)
	noOrphans(t, notes)
	invariant(t, d, notes)
}

// adoptedAfterLinks is a capture that crashed after its links, adopted as
// recovery adopts it.
func adoptedAfterLinks(t *testing.T) (*Server, *captureDir, *os.Root, *captureTxn) {
	t.Helper()
	s, d, _, _ := crashAtFault(t, faultAfterLinks)
	ser, err := d.root.OpenRoot("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ser.Close() })
	in, self, err := readIntent(ser, intentName(1, "run"))
	if err != nil {
		t.Fatal(err)
	}
	return s, d, ser, s.adopt(d, ser, "adhoc", in, self)
}

// TestUnwindRemovesFinalsOnlyWhereNoManifestIs: a rollback removes the
// published files only when nothing is at the manifest name. This
// capture's own ok manifest there finishes the commit instead; anything
// else (another file, a name that cannot be read) strands the rollback
// with the files kept.
func TestUnwindRemovesFinalsOnlyWhereNoManifestIs(t *testing.T) {
	final := finalName(1, "run", "cpu.pb.gz")
	t.Run("own-ok-manifest", func(t *testing.T) {
		s, d, ser, txn := adoptedAfterLinks(t)
		ok, _ := json.Marshal(txn.manifestOf(manifest{}, captureOK, nil))
		writeFile(t, ser, manifestName(1, "run"), string(append(ok, '\n')))
		err := txn.unwind(txn.manifestOf(manifest{}, captureInterrupted, nil), faultInRecovery)
		if !errors.Is(err, errAlreadyCommitted) {
			t.Fatalf("unwind: %v", err)
		}
		if _, err := ser.Lstat(final); err != nil {
			t.Fatalf("a committed file was removed: %v", err)
		}
		notes := recoverNow(t, s, d)
		noOrphans(t, notes)
		invariant(t, d, notes)
	})
	t.Run("foreign-file", func(t *testing.T) {
		_, _, ser, txn := adoptedAfterLinks(t)
		writeFile(t, ser, manifestName(1, "run"), "not ours")
		if err := txn.unwind(txn.manifestOf(manifest{}, captureInterrupted, nil), faultInRecovery); err == nil {
			t.Fatal("unwind went on beside a foreign file")
		}
		if _, err := ser.Lstat(final); err != nil {
			t.Fatalf("a published file was removed beside an unknown manifest: %v", err)
		}
	})
	t.Run("unreadable-name", func(t *testing.T) {
		_, _, ser, txn := adoptedAfterLinks(t)
		landedLstat = func(root *os.Root, name string) (fs.FileInfo, error) {
			if strings.HasSuffix(name, manifestSuffix) {
				return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.EIO}
			}
			return root.Lstat(name)
		}
		defer func() { landedLstat = (*os.Root).Lstat }()
		if err := txn.unwind(txn.manifestOf(manifest{}, captureInterrupted, nil), faultInRecovery); err == nil {
			t.Fatal("unwind went on although the manifest name could not be read")
		}
		if _, err := ser.Lstat(final); err != nil {
			t.Fatalf("a published file was removed: %v", err)
		}
	})
}

// TestEveryLinkThatLandedCounts: a link answered EEXIST although it was
// made counts as made for each link the protocol makes: the intent, the
// heartbeat, the probe and each capture file.
func TestEveryLinkThatLandedCounts(t *testing.T) {
	for _, c := range []struct {
		name string
		pick func(string) bool
	}{
		{"intent", func(n string) bool { return strings.HasSuffix(n, intentSuffix) }},
		{"heartbeat", func(n string) bool { return strings.HasSuffix(n, heartbeatSuffix) }},
		{"probe", func(n string) bool { return strings.HasSuffix(n, probeLinkName) }},
		{"capture-file", func(n string) bool { return strings.HasSuffix(n, "-cpu.pb.gz") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			s.link = landsWithEEXIST(c.pick)
			r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
			s.link = (*os.Root).Link
			if !r.committed || r.err != nil {
				t.Fatalf("committed %v, err %v", r.committed, r.err)
			}
			notes := recoverNow(t, s, d)
			noOrphans(t, notes)
			invariant(t, d, notes)
		})
	}
}

// canonicalIntent is in as its writer encodes it.
func canonicalIntent(in intent) []byte {
	b, _ := json.Marshal(in)
	return b
}

// withField is the JSON object b with one more field.
func withField(t *testing.T, b []byte, key, value string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m[key] = value
	out, _ := json.Marshal(m)
	return out
}

// TestTokenProofsNeverTakeAFileWithDataOfItsOwn: a file at one of the
// capture's private names that is not exactly bytes this server writes
// (an intent or manifest with a field added, another token, another
// capture, a token followed by more) stays; an exact copy of the server's
// own bytes goes.
func TestTokenProofsNeverTakeAFileWithDataOfItsOwn(t *testing.T) {
	type plant struct {
		what string
		name func(in intent) string
		body func(t *testing.T, txn *captureTxn) []byte
		gone bool
	}
	intentTemp := func(in intent) string { return "." + intentName(1, "run") + ".tmp-" + in.Token[:16] + "-7" }
	manifestTemp := func(in intent) string { return "." + manifestName(1, "run") + ".tmp-" + in.Token[:16] + "-7" }
	hbTemp := func(in intent) string { return "." + heartbeatName(1, "run") + ".tmp-" + in.Token[:16] + "-7" }
	quarantined := func(in intent) string { return quarantinePrefix + in.Token[:16] + "-0123456789abcdef" }
	otherToken := func(in intent) intent { in.Token = strings.Repeat("e", 32); return in }
	failure := func(txn *captureTxn) manifest { return txn.manifestOf(manifest{}, captureFailed, nil) }
	cases := []plant{
		{"intent-temp-extra-field", intentTemp, func(t *testing.T, x *captureTxn) []byte {
			return withField(t, canonicalIntent(x.in), "user_notes", "PRECIOUS")
		}, false},
		{"intent-temp-other-token", intentTemp, func(t *testing.T, x *captureTxn) []byte { return canonicalIntent(otherToken(x.in)) }, false},
		{"intent-temp-reindented", intentTemp, func(t *testing.T, x *captureTxn) []byte {
			b, _ := json.MarshalIndent(x.in, "", " ")
			return b
		}, false},
		{"intent-temp-exact", intentTemp, func(t *testing.T, x *captureTxn) []byte { return canonicalIntent(x.in) }, true},
		{"manifest-temp-extra-field", manifestTemp, func(t *testing.T, x *captureTxn) []byte {
			b, _ := json.Marshal(failure(x))
			return append(withField(t, b, "user_notes", "PRECIOUS"), '\n')
		}, false},
		{"manifest-temp-no-token", manifestTemp, func(t *testing.T, x *captureTxn) []byte {
			m := failure(x)
			m.Token = ""
			b, _ := json.Marshal(m)
			return append(b, '\n')
		}, false},
		{"manifest-temp-not-written-by-us", manifestTemp, func(t *testing.T, x *captureTxn) []byte {
			m := failure(x)
			m.WrittenBy = "someone"
			b, _ := json.Marshal(m)
			return append(b, '\n')
		}, false},
		{"manifest-temp-public-fields-only", manifestTemp, func(t *testing.T, x *captureTxn) []byte {
			return []byte(`{"written_by":"perf-agent-mcp","id":"001","label":"run","series":"adhoc","user_notes":"PRECIOUS"}`)
		}, false},
		{"manifest-temp-exact", manifestTemp, func(t *testing.T, x *captureTxn) []byte {
			b, _ := json.Marshal(failure(x))
			return append(b, '\n')
		}, true},
		{"heartbeat-temp-extra-line", hbTemp, func(t *testing.T, x *captureTxn) []byte { return []byte(x.in.Token + "\nPRECIOUS\n") }, false},
		{"heartbeat-temp-exact", hbTemp, func(t *testing.T, x *captureTxn) []byte { return []byte(x.in.Token + "\n") }, true},
		{"quarantine-intent-extra-field", quarantined, func(t *testing.T, x *captureTxn) []byte {
			return withField(t, canonicalIntent(x.in), "user_notes", "PRECIOUS")
		}, false},
		{"quarantine-intent-of-another-capture", quarantined, func(t *testing.T, x *captureTxn) []byte {
			// A valid intent of capture 002 with this token.
			in := x.in
			in.ID, in.Temps = "002", nil
			in.Finals = nil
			for _, o := range in.Outputs {
				in.Finals = append(in.Finals, finalName(2, "run", o))
			}
			in.Files = nil
			hb := *in.Heartbeat
			hb.Name = heartbeatName(2, "run")
			in.Heartbeat = &hb
			if err := in.check(intentName(2, "run")); err != nil {
				t.Fatal(err)
			}
			return canonicalIntent(in)
		}, false},
		{"quarantine-intent-invalid", quarantined, func(t *testing.T, x *captureTxn) []byte {
			in := x.in
			in.Staging = "../elsewhere"
			return canonicalIntent(in)
		}, false},
		{"quarantine-heartbeat-exact", quarantined, func(t *testing.T, x *captureTxn) []byte { return []byte(x.in.Token + "\n") }, true},
	}
	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			s, d, ser, txn := adoptedAfterLinks(t)
			name := c.name(txn.in)
			body := c.body(t, txn)
			writeFile(t, ser, name, string(body))
			clk := &testClock{}
			s.now = clk.now
			notes := crashRecover(t, s, d, clk)
			_, err := ser.Lstat(name)
			switch gone := errors.Is(err, fs.ErrNotExist); {
			case c.gone && !gone:
				t.Fatalf("an exact copy of the server's bytes stayed (notes %q)", notes)
			case !c.gone && gone:
				t.Fatal("a file with data of its own was removed")
			case !c.gone:
				if got, _ := ser.ReadFile(name); string(got) != string(body) {
					t.Fatal("the file changed")
				}
			}
			invariant(t, d, notes)
		})
	}
}

// TestARemovalChecksTheFileItUnlinks: a file renamed onto the quarantine
// name after the contents were checked is not unlinked.
func TestARemovalChecksTheFileItUnlinks(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeFile(t, root, "f", "ours")
	want := recordedIdentity(t, root, "f")
	writeFile(t, root, "victim", "USER FILE")
	quarantineBeforeUnlink = func(r *os.Root, q string) {
		if err := r.Rename("victim", q); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { quarantineBeforeUnlink = nil }()
	if removed, _, _ := removeByIdentity(root, "f", want); removed {
		t.Fatal("reported removed")
	}
	found := false
	for _, n := range dirNames(t, root, ".") {
		if b, _ := root.ReadFile(n); string(b) == "USER FILE" {
			found = true
		}
	}
	if !found {
		t.Fatal("the file renamed onto the quarantine name was unlinked")
	}
}

// TestStrayQuarantinedIntentsNeedEveryProof: an intent left under a
// quarantine name with no intent beside it is removed only when its token
// matches the name, it is of this series, and its manifest exists; an
// intent temporary only when its intent is absent.
func TestStrayQuarantinedIntentsNeedEveryProof(t *testing.T) {
	committed := func(t *testing.T) (*Server, *captureDir, *os.Root, intent) {
		t.Helper()
		s, d, _ := newCaptureServer(t)
		r := drive(t, s, d, capCase{})
		if !r.committed {
			t.Fatal(r.err)
		}
		ser, _ := d.root.OpenRoot("adhoc")
		t.Cleanup(func() { _ = ser.Close() })
		return s, d, ser, r.txn.in
	}
	for _, c := range []struct {
		name string
		make func(t *testing.T, ser *os.Root, in intent) string
		gone bool
	}{
		{"proven", func(t *testing.T, ser *os.Root, in intent) string {
			n := quarantinePrefix + in.Token[:16] + "-0123456789abcdef"
			writeFile(t, ser, n, string(canonicalIntent(in)))
			return n
		}, true},
		{"token-not-the-name", func(t *testing.T, ser *os.Root, in intent) string {
			n := quarantinePrefix + strings.Repeat("d", 16) + "-0123456789abcdef"
			writeFile(t, ser, n, string(canonicalIntent(in)))
			return n
		}, false},
		{"another-series", func(t *testing.T, ser *os.Root, in intent) string {
			in.Series = "other"
			n := quarantinePrefix + in.Token[:16] + "-0123456789abcdef"
			writeFile(t, ser, n, string(canonicalIntent(in)))
			return n
		}, false},
		{"no-manifest", func(t *testing.T, ser *os.Root, in intent) string {
			if err := ser.Rename(manifestName(1, "run"), "kept-manifest"); err != nil {
				t.Fatal(err)
			}
			n := quarantinePrefix + in.Token[:16] + "-0123456789abcdef"
			writeFile(t, ser, n, string(canonicalIntent(in)))
			return n
		}, false},
		{"intent-temp-beside-its-intent", func(t *testing.T, ser *os.Root, in intent) string {
			// Something recovery leaves in place holds the intent's name.
			writeFile(t, ser, intentName(1, "run"), "not an intent")
			n := "." + intentName(1, "run") + ".tmp-" + in.Token[:16]
			writeFile(t, ser, n, string(canonicalIntent(in)))
			return n
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, d, ser, in := committed(t)
			n := c.make(t, ser, in)
			recoverNow(t, s, d)
			_, err := ser.Lstat(n)
			if gone := errors.Is(err, fs.ErrNotExist); gone != c.gone {
				t.Fatalf("%s: gone %v, want %v", n, gone, c.gone)
			}
		})
	}
}

// TestAQuarantinedDirectoryIsProvenByItsInode: a directory under one of
// the capture's quarantine names is the capture's staging only when its
// device and inode are the recorded ones; another stays untouched.
func TestAQuarantinedDirectoryIsProvenByItsInode(t *testing.T) {
	s, d, clk, r := crashAtFault(t, faultAfterLinks)
	name := quarantinePrefix + r.txn.in.Token[:16] + "-0123456789abcdef"
	if err := os.Mkdir(serPath(d, name), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(serPath(d, name), "cpu.pb.gz"), []byte("user"))
	notes := crashRecover(t, s, d, clk)
	if got, err := os.ReadFile(filepath.Join(serPath(d, name), "cpu.pb.gz")); err != nil || string(got) != "user" {
		t.Fatalf("a directory that is not the staging one was emptied: %q %v", got, err)
	}
	for _, n := range notes {
		if strings.HasPrefix(n, "left ") && strings.Contains(n, name) {
			t.Errorf("recovery acted on it: %s", n)
		}
	}
	if !noted(notes, "orphan_temp", name) {
		t.Errorf("not reported: %q", notes)
	}
}

// TestATokenProofNeedsAtMostTwoLinks: bytes this server writes, under a
// name with two more links elsewhere, are not provably only its own.
func TestATokenProofNeedsAtMostTwoLinks(t *testing.T) {
	s, d, ser, txn := adoptedAfterLinks(t)
	name := "." + heartbeatName(1, "run") + ".tmp-" + txn.in.Token[:16] + "-7"
	writeFile(t, ser, name, txn.in.Token+"\n")
	for _, l := range []string{"link-a", "link-b"} {
		if err := ser.Link(name, l); err != nil {
			t.Fatal(err)
		}
	}
	clk := &testClock{}
	s.now = clk.now
	notes := crashRecover(t, s, d, clk)
	if _, err := ser.Lstat(name); err != nil {
		t.Fatalf("a name of a file with three links was removed: %v", err)
	}
	invariant(t, d, notes, "link-a", "link-b")
}

// TestAnOrphanQuarantineNameSaysWhoseItIs: a quarantine name recovery
// cannot prove is reported as moved aside from one of its capture's names,
// for the user to check before removing it.
func TestAnOrphanQuarantineNameSaysWhoseItIs(t *testing.T) {
	s, d, clk, r := crashAtFault(t, faultAfterLinks)
	name := quarantinePrefix + r.txn.in.Token[:16] + "-0123456789abcdef"
	mustWrite(t, serPath(d, name), []byte("USER FILE"))
	notes := crashRecover(t, s, d, clk)
	found := false
	for _, n := range notes {
		if strings.HasPrefix(n, "orphan_temp ") && strings.Contains(n, name) {
			found = strings.Contains(n, "capture 001-run") && strings.Contains(n, "check it")
		}
	}
	if !found {
		t.Fatalf("notes %q", notes)
	}
}

// TestAHeartbeatWithMoreLinksProvesNothing: a file at the heartbeat's name
// holding the token but with three links is not proven, so its fresh
// mtime does not keep a dead owner's intent alive.
func TestAHeartbeatWithMoreLinksProvesNothing(t *testing.T) {
	s, d, _, r := crashedIntent(t)
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	hb := heartbeatName(r.id, "run")
	if err := ser.Remove(hb); err != nil {
		t.Fatal(err)
	}
	writeFile(t, ser, hb, r.txn.in.Token+"\n")
	for _, l := range []string{"link-a", "link-b"} {
		if err := ser.Link(hb, l); err != nil {
			t.Fatal(err)
		}
	}
	now := s.clock()
	if err := ser.Chtimes(hb, now, now); err != nil {
		t.Fatal(err)
	}
	s.ownerAlive = func(intent) bool { return false }
	notes := recoverNow(t, s, d)
	if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
		t.Fatalf("outcome %q (notes %q)", m.Outcome, notes)
	}
}

// TestAnUnknownManifestLinkThatWasNotMadeRollsBack: when the retry finds
// nothing at the manifest name, the capture is rolled back to a failure.
func TestAnUnknownManifestLinkThatWasNotMadeRollsBack(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.link = func(r *os.Root, oldname, newname string) error {
		if strings.HasSuffix(newname, manifestSuffix) && strings.Contains(oldname, manifestSuffix+".tmp-") && !failedOnce {
			failedOnce = true
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EIO}
		}
		return r.Link(oldname, newname)
	}
	defer func() { failedOnce = false }()
	landedLstat = func(root *os.Root, name string) (fs.FileInfo, error) {
		if strings.HasSuffix(name, manifestSuffix) && !lstatFailed {
			lstatFailed = true
			return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.EIO}
		}
		return root.Lstat(name)
	}
	defer func() { landedLstat, lstatFailed = (*os.Root).Lstat, false }()
	r := drive(t, s, d, capCase{})
	if r.committed || r.err == nil {
		t.Fatalf("committed %v, err %v", r.committed, r.err)
	}
	if files, _ := filepath.Glob(serPath(d, "001-run-*")); len(files) != 1 {
		t.Fatalf("an undecided capture lost its files: %v", files)
	}
	if err := s.retryStranded(); err != nil {
		t.Fatalf("retryStranded: %v", err)
	}
	if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed {
		t.Fatalf("manifest %+v", m)
	}
	notes := recoverNow(t, s, d)
	noOrphans(t, notes)
	invariant(t, d, notes)
}

var failedOnce, lstatFailed bool
