package perfmcp

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// plantAndRecover writes body at name in a capture that crashed after its
// links, recovers, and reports whether name is gone, with the notes.
func plantAndRecover(t *testing.T, name func(in intent) string, body func(in intent) []byte) (gone bool, notes []string, n string) {
	t.Helper()
	s, d, ser, txn := adoptedAfterLinks(t)
	n = name(txn.in)
	writeFile(t, ser, n, string(body(txn.in)))
	clk := &testClock{}
	s.now = clk.now
	notes = crashRecover(t, s, d, clk)
	_, err := ser.Lstat(n)
	invariant(t, d, notes)
	return errors.Is(err, fs.ErrNotExist), notes, n
}

// TestEditedValuesAreNeverProven: an intent copy carrying the capture's
// token is proven only when every field that never changes is the adopted
// intent's own: a copy with an edited value (settings, outputs, finals,
// staging, the owner) stays and is reported.
func TestEditedValuesAreNeverProven(t *testing.T) {
	sites := map[string]func(in intent) string{
		"intent-temp": func(in intent) string { return "." + intentName(1, "run") + ".tmp-" + in.Token[:16] + "-3" },
		"quarantine":  func(in intent) string { return quarantinePrefix + in.Token[:16] + "-0123456789abcdef" },
	}
	edits := map[string]func(*intent){
		"exact":         func(*intent) {},
		"settings":      func(in *intent) { in.Settings.Unwind, in.Settings.Duration = "PRECIOUS USER NOTES", "999h" },
		"outputs":       func(in *intent) { in.Outputs = in.Outputs[:1]; in.Finals = in.Finals[:1]; in.Files = nil },
		"staging":       func(in *intent) { in.Staging = stagingPrefix + strings.Repeat("0", 16) },
		"staging-inode": func(in *intent) { in.StagingIno++ },
		"staging-dev":   func(in *intent) { in.StagingDev++ },
		"state":         func(in *intent) { in.State = "PRECIOUS USER NOTES" },
		// An earlier version, before staging, the heartbeat and the files
		// were recorded, is still this capture's.
		"earlier-version": func(in *intent) {
			in.State, in.StagingDev, in.StagingIno, in.Heartbeat, in.Files, in.Temps = statePrepared, 0, 0, nil, nil, nil
		},
		"server-pid":     func(in *intent) { in.ServerPID++ },
		"server-start":   func(in *intent) { in.ServerStart++ },
		"recorded-files": func(in *intent) { in.Files[0].Size++ },
	}
	for site, name := range sites {
		for edit, f := range edits {
			t.Run(site+"/"+edit, func(t *testing.T) {
				gone, notes, n := plantAndRecover(t, name, func(in intent) []byte {
					f(&in)
					return canonicalIntent(in)
				})
				if gone != (edit == "exact" || edit == "earlier-version") {
					t.Fatalf("gone %v (notes %q)", gone, notes)
				}
				if !gone && !noted(notes, "orphan_temp", n) {
					t.Fatalf("kept but not reported: %q", notes)
				}
			})
		}
	}
}

// TestManifestTempsAreNeverProvenByContent: a manifest temporary nothing
// recorded holds free text (an error message, notes) no intent pins, so
// even exact server bytes stay, reported.
func TestManifestTempsAreNeverProvenByContent(t *testing.T) {
	gone, notes, n := plantAndRecover(t, func(in intent) string {
		return "." + manifestName(1, "run") + ".tmp-" + in.Token[:16] + "-3"
	}, func(in intent) []byte {
		m := manifest{Format: manifestFormat, ID: in.ID, Series: in.Series, Label: in.Label, Outcome: captureFailed,
			Error: &wireError{Code: "disk", Message: "PRECIOUS USER NOTES"}, Settings: in.Settings,
			SettingsDigest: settingsDigest(in.Settings), Token: in.Token, WrittenBy: writtenBy}
		b, _ := json.Marshal(m)
		return append(b, '\n')
	})
	if gone || !noted(notes, "orphan_temp", n) {
		t.Fatalf("gone %v, notes %q", gone, notes)
	}
}

// TestStrayFilesAreOnlyNoted: a file no adopted intent accounts for is
// never removed by what it holds, since there is nothing to check it
// against but itself; it is reported.
func TestStrayFilesAreOnlyNoted(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	r := drive(t, s, d, capCase{})
	if !r.committed {
		t.Fatal(r.err)
	}
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	in := r.txn.in
	names := []string{
		quarantinePrefix + in.Token[:16] + "-0123456789abcdef",
		"." + intentName(2, "run") + ".tmp-" + in.Token[:16],
	}
	for _, n := range names {
		writeFile(t, ser, n, string(canonicalIntent(in)))
	}
	notes := recoverNow(t, s, d)
	for _, n := range names {
		if _, err := ser.Lstat(n); err != nil {
			t.Errorf("%s: removed: %v", n, err)
		}
		if !noted(notes, "orphan_temp", n) {
			t.Errorf("%s not reported: %q", n, notes)
		}
	}
}

// TestACommitFoundByTheRollbackMovesTheWord: a manifest link that was
// made, answered EEXIST, and first looked absent (a stale cache) is found
// by the rollback; the capture is reported committed and the decision
// word says so: a later cancel is too late.
func TestACommitFoundByTheRollbackMovesTheWord(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.link = landsWithEEXIST(func(n string) bool { return strings.HasSuffix(n, manifestSuffix) })
	stale := false
	landedLstat = func(r *os.Root, n string) (fs.FileInfo, error) {
		if strings.HasSuffix(n, manifestSuffix) && !stale {
			if _, err := r.Lstat(n); err == nil {
				stale = true
				return nil, &fs.PathError{Op: "lstat", Path: n, Err: syscall.ENOENT}
			}
		}
		return r.Lstat(n)
	}
	defer func() { landedLstat = (*os.Root).Lstat }()
	dec := &decisionWord{}
	r := drive(t, s, d, capCase{dec: dec})
	if !r.committed || r.err != nil {
		t.Fatalf("committed %v, err %v", r.committed, r.err)
	}
	if got := dec.requestCancel(); got != cancelAnswerTooLate {
		t.Fatalf("a cancel after the commit answers %q", got)
	}
	notes := recoverNow(t, s, d)
	noOrphans(t, notes)
	invariant(t, d, notes)
}

// TestEveryLinkThatLandedUnreadablyIsSettled: each link answers EEXIST
// although it was made, and the check of its outcome fails k times. No
// committed file is ever removed, and the retry or a restart settles the
// capture leaving nothing behind, heartbeat included.
func TestEveryLinkThatLandedUnreadablyIsSettled(t *testing.T) {
	modes := []capture.Mode{capture.ModeCPU, capture.ModeGPU}
	for _, c := range []struct {
		what, suffix string
		failing      bool
	}{
		{"intent", intentName(1, "run"), false},
		{"heartbeat", heartbeatName(1, "run"), false},
		{"probe", probeLinkName, false},
		{"cpu-file", finalName(1, "run", "cpu.pb.gz"), false},
		{"manifest", manifestName(1, "run"), false},
		{"failure-manifest", manifestName(1, "run"), true},
	} {
		for _, k := range []int{1, 3, 1 << 30} {
			for _, restart := range []bool{false, true} {
				t.Run(c.what+"/"+map[bool]string{false: "retry", true: "restart"}[restart]+"/"+map[bool]string{false: "few", true: "persistent"}[k > 3], func(t *testing.T) {
					s, d, clk := newCaptureServer(t)
					var mu sync.Mutex
					made, fails := false, 0
					s.link = func(r *os.Root, o, n string) error {
						err := r.Link(o, n)
						mu.Lock()
						defer mu.Unlock()
						if err == nil && !made && strings.HasSuffix(n, c.suffix) {
							made = true
							return &os.LinkError{Op: "link", Old: o, New: n, Err: syscall.EEXIST}
						}
						return err
					}
					landedLstat = func(r *os.Root, n string) (fs.FileInfo, error) {
						mu.Lock()
						fail := made && fails < k && strings.HasSuffix(n, c.suffix)
						if fail {
							fails++
						}
						mu.Unlock()
						if fail {
							return nil, &fs.PathError{Op: "lstat", Path: n, Err: syscall.EIO}
						}
						return r.Lstat(n)
					}
					defer func() { landedLstat = (*os.Root).Lstat }()
					cc := capCase{modes: modes}
					if c.failing {
						cc.content = map[string][]byte{"cpu.pb.gz": []byte("x")}
					}
					r := drive(t, s, d, cc)
					ser, _ := d.root.OpenRoot("adhoc")
					defer func() { _ = ser.Close() }()
					if hasManifest(t, d, "adhoc", 1, "run") {
						m := manifestFor(t, d, "adhoc", 1, "run")
						if m.Outcome == captureOK {
							if err := verifyFiles(ser, m); err != nil {
								t.Fatalf("an ok manifest whose files are gone: %v", err)
							}
						} else if r.committed {
							t.Fatalf("committed, but the manifest is %s", m.Outcome)
						}
						if c.failing && m.Outcome == captureOK {
							t.Fatal("a failing capture has an ok manifest")
						}
					} else if r.committed {
						t.Fatal("committed without a manifest")
					}
					landedLstat = (*os.Root).Lstat
					s.link = (*os.Root).Link
					var notes []string
					if restart {
						notes = crashRecover(t, s, d, clk)
					} else {
						if err := s.retryStranded(); err != nil {
							t.Fatalf("retryStranded with the name readable again: %v", err)
						}
						notes = recoverNow(t, s, d)
					}
					noOrphans(t, notes)
					invariant(t, d, notes)
					if c.what == "manifest" {
						if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureOK {
							t.Fatalf("the made manifest link was undone: %s", m.Outcome)
						}
					}
				})
			}
		}
	}
}

// TestASameBytesCopySwappedInBeforeTheMoveStays: a byte-identical copy put
// under a final name after its stat was checked is told apart by the stat
// at the quarantine name, and is not unlinked.
func TestASameBytesCopySwappedInBeforeTheMoveStays(t *testing.T) {
	s, d, clk, r := crashAtFault(t, faultAfterLinks)
	name := finalName(r.id, "run", "cpu.pb.gz")
	fired := false
	quarantineHook = func(_ *os.Root, n string) {
		if fired || n != name {
			return
		}
		fired = true
		b, _ := os.ReadFile(serPath(d, name))
		mustWrite(t, serPath(d, name+".u"), b)
		if err := os.Rename(serPath(d, name+".u"), serPath(d, name)); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { quarantineHook = nil }()
	notes := crashRecover(t, s, d, clk)
	if !fired {
		t.Fatal("the swap never happened")
	}
	if _, err := os.Stat(serPath(d, name)); err != nil {
		t.Fatalf("the same-bytes copy was unlinked: %v (notes %q)", err, notes)
	}
}

// TestAMismatchedFileIsNeverMovedAside: a name whose stat is not the
// recorded file's is left where it is, without even a move aside.
func TestAMismatchedFileIsNeverMovedAside(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	writeFile(t, root, "f", "ours")
	want := recordedIdentity(t, root, "f")
	want.Size++
	moved := false
	quarantineHook = func(*os.Root, string) { moved = true }
	defer func() { quarantineHook = nil }()
	if removed, left, _ := removeByIdentity(root, "f", want); removed || left == "" || moved {
		t.Fatalf("removed %v, left %q, moved aside %v", removed, left, moved)
	}
}

// TestUnwindTakesOnlyItsOwnManifest: an ok manifest at the name counts as
// this capture's commit only with its token and its recorded files.
func TestUnwindTakesOnlyItsOwnManifest(t *testing.T) {
	final := finalName(1, "run", "cpu.pb.gz")
	for name, edit := range map[string]func(*manifest){
		"other-token": func(m *manifest) { m.Token = strings.Repeat("f", 32) },
		"other-files": func(m *manifest) { m.Files[0].Size++ },
	} {
		t.Run(name, func(t *testing.T) {
			_, _, ser, txn := adoptedAfterLinks(t)
			m := txn.manifestOf(manifest{}, captureOK, nil)
			edit(&m)
			b, _ := json.Marshal(m)
			writeFile(t, ser, manifestName(1, "run"), string(append(b, '\n')))
			err := txn.unwind(txn.manifestOf(manifest{}, captureInterrupted, nil), faultInRecovery)
			if err == nil || errors.Is(err, errAlreadyCommitted) {
				t.Fatalf("unwind: %v", err)
			}
			if _, err := ser.Lstat(final); err != nil {
				t.Fatalf("a published file was removed: %v", err)
			}
		})
	}
}

// TestACancelAfterAFailedLinkSettledIsHonoured: a cancel that lands after
// the failed manifest link was settled, but before the rollback starts,
// still makes the failure a cancellation.
func TestACancelAfterAFailedLinkSettledIsHonoured(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.writeErr = failOp("manifest link", syscall.EIO)
	dec := &decisionWord{}
	afterSettle = func() { dec.requestCancel() }
	defer func() { afterSettle = nil }()
	r := drive(t, s, d, capCase{dec: dec})
	s.writeErr = nil
	if r.committed || captureErr(t, r.err).Code != capture.CodeCancelled {
		t.Fatalf("committed %v, err %v", r.committed, r.err)
	}
}

// TestALaterVersionOfTheIntentIsProven: the temporary of a rewrite a crash
// cut short is a newer version of the adopted intent, recording what the
// adopted one does not yet (the staging inode, the files); it is the
// capture's own and goes.
func TestALaterVersionOfTheIntentIsProven(t *testing.T) {
	_, d, clk, r := crashAtFault(t, faultAfterIntent)
	s := r.txn.s
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	in, _, err := readIntent(ser, intentName(1, "run"))
	if err != nil {
		t.Fatal(err)
	}
	later := in
	later.State, later.StagingDev, later.StagingIno = stateLinking, 11, 12
	later.Files = []fileIdentity{{Name: in.Finals[0], Staged: in.Outputs[0], Dev: 1, Ino: 2, Size: 3, SHA256: strings.Repeat("a", 64)}}
	name := "." + intentName(1, "run") + ".tmp-" + in.Token[:16]
	writeFile(t, ser, name, string(canonicalIntent(later)))
	notes := crashRecover(t, s, d, clk)
	if _, err := ser.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a later version of the intent stayed (notes %q)", notes)
	}
	invariant(t, d, notes)
}
