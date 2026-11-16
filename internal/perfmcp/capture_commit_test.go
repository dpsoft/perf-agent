package perfmcp

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// testClock is a clock a test moves forward; the server reads it through
// s.now, from any goroutine.
type testClock struct{ off atomic.Int64 }

func (c *testClock) now() time.Time          { return time.Now().Add(time.Duration(c.off.Load())) }
func (c *testClock) advance(d time.Duration) { c.off.Add(int64(d)) }

// newCaptureServer is a server over a fresh profiles root, with its capture
// directory open and a clock the test moves.
func newCaptureServer(t *testing.T) (*Server, *captureDir, *testClock) {
	t.Helper()
	s, err := New(Config{ProfilesDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	clk := &testClock{}
	s.now = clk.now
	d, e := s.openCaptureDir()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(d.close)
	return s, d, clk
}

// capCase is one synthetic capture: no process runs; the files perf-agent
// would write are written into staging by the test.
type capCase struct {
	series, label string
	modes         []capture.Mode
	dec           *decisionWord
	// content overrides the bytes written for a staging file; nil content
	// means the file is not written at all.
	content map[string][]byte
}

// capRun is what drive saw.
type capRun struct {
	id        int  // the id, once nextID handed it out
	intent    bool // an intent was written
	committed bool
	err       error
	txn       *captureTxn
}

func fileContent(name, label string) []byte {
	return bytes.Repeat([]byte(name+" of "+label+"\n"), 64)
}

// drive runs one capture through §7.4's steps as the job does, with the
// file protocol only: the lease and recovery, the id, the intent, the
// space check, staging, the (synthetic) capture and the commit. A failure
// after the id ends in a failure manifest; errCrash stops everything where
// it is, as a process that died would.
func drive(t *testing.T, s *Server, d *captureDir, c capCase) (r capRun) {
	t.Helper()
	c.series = cmp.Or(c.series, "adhoc")
	c.label = cmp.Or(c.label, "run")
	if c.modes == nil {
		c.modes = []capture.Mode{capture.ModeCPU}
	}
	if c.dec == nil {
		c.dec = &decisionWord{}
	}
	if r.err = s.fault(faultAfterLease); r.err != nil {
		return r
	}
	if r.err = s.retryStranded(); r.err != nil {
		return r
	}
	if _, r.err = s.recoverCaptures(d); r.err != nil {
		return r
	}
	for _, p := range []faultPoint{faultAfterRecovery, faultInEngine, faultAfterEngine} {
		if r.err = s.fault(p); r.err != nil {
			return r
		}
	}
	ser, err := d.series(c.series)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	if r.id, r.err = s.nextID(ser); r.err != nil {
		r.id = 0
		return r
	}
	settings := toWire(capture.Settings{Modes: c.modes, Duration: 10 * time.Second, SampleRate: 99, Unwind: "auto"})
	outputs := capture.ExpectedFiles(c.modes)
	failEarly := func(err error) {
		r.err = err
		if !errors.Is(err, errCrash) {
			r.err = d.cause(err)
			if werr := s.writeFailure(d, c.series, r.id, c.label, settings, d.cause(err), manifest{}); werr != nil {
				t.Logf("failure manifest: %v", werr)
			}
		}
	}
	if err := s.fault(faultAfterSeq); err != nil {
		failEarly(err)
		return r
	}
	txn, err := s.beginTxn(d, c.series, r.id, c.label, settings, outputs)
	r.txn = txn
	if txn == nil {
		if err == nil {
			t.Fatal("beginTxn returned neither a transaction nor an error")
		}
		failEarly(err)
		return r
	}
	r.intent = true
	fail := func(err error) {
		r.err = err
		if !errors.Is(err, errCrash) {
			r.err = txn.rollback(txn.cause(err), manifest{})
			if r.err == nil {
				r.err = txn.cause(err)
			}
		}
	}
	if err != nil {
		fail(err)
		return r
	}
	for _, p := range []faultPoint{faultAfterIntent, faultAfterSpace} {
		if err := txn.fault(p); err != nil {
			fail(err)
			return r
		}
	}
	if err := txn.makeStaging(); err != nil {
		fail(err)
		return r
	}
	if err := txn.fault(faultAfterStaging); err != nil {
		fail(err)
		return r
	}
	for _, out := range outputs {
		b := fileContent(out, c.label)
		if c.content != nil {
			var ok bool
			if b, ok = c.content[out]; !ok || b == nil {
				continue
			}
		}
		if err := os.WriteFile(filepath.Join(ser.Name(), txn.in.Staging, out), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r.committed, r.err = txn.finish(manifest{StoppedBy: "duration", ProfiledDuration: "10s"}, c.dec)
	return r
}

// shot fails the n-th time p is reached with err.
type shot struct {
	p   faultPoint
	n   int
	err error
}

// inject is a captureFault that fires each shot once, at its occurrence.
func inject(shots ...shot) func(faultPoint) error {
	var mu sync.Mutex
	counts := map[faultPoint]int{}
	return func(p faultPoint) error {
		mu.Lock()
		defer mu.Unlock()
		counts[p]++
		for _, s := range shots {
			if s.p == p && s.n == counts[p] {
				return s.err
			}
		}
		return nil
	}
}

// failOp is a writeErr that fails op with err every time.
func failOp(op string, err error) func(string) error {
	return func(o string) error {
		if o == op {
			return err
		}
		return nil
	}
}

var errInjected = errors.New("injected failure")

// crashRecover lets every owner be gone and every heartbeat go stale, then
// recovers, as the next server would.
func crashRecover(t *testing.T, s *Server, d *captureDir, clk *testClock) []string {
	t.Helper()
	s.ownerAlive = func(intent) bool { return false }
	clk.advance(staleAfter + time.Second)
	notes, err := s.recoverCaptures(d)
	if err != nil {
		t.Fatalf("recovery: %v (notes %q)", err, notes)
	}
	return notes
}

func recoverNow(t *testing.T, s *Server, d *captureDir) []string {
	t.Helper()
	notes, err := s.recoverCaptures(d)
	if err != nil {
		t.Fatalf("recovery: %v (notes %q)", err, notes)
	}
	return notes
}

// noted reports whether a note of kind names name.
func noted(notes []string, kind, name string) bool {
	return slices.ContainsFunc(notes, func(n string) bool {
		return strings.HasPrefix(n, kind+" ") && strings.Contains(n, name)
	})
}

// invariant is the oracle every fault case ends with (§7.4): each series
// holds only committed manifests whose files verify, failure or
// interrupted manifests with no files, .seq, and orphans that recovery
// noted (a staging directory or a temporary it cannot prove it made).
// allow names other entries the test planted itself.
func invariant(t *testing.T, d *captureDir, notes []string, allow ...string) {
	t.Helper()
	for _, series := range dirNames(t, d.root, ".") {
		if !validSeries(series) {
			continue
		}
		ser, err := d.root.OpenRoot(series)
		if err != nil {
			t.Fatal(err)
		}
		names := dirNames(t, ser, ".")
		owned := map[string]bool{}
		var failedPrefixes []string
		for _, n := range names {
			if _, _, kind, ok := parseName(n); !ok || kind != kindManifest {
				continue
			}
			m, err := readManifest(ser, n)
			if err != nil {
				t.Errorf("%s/%s: %v", series, n, err)
				continue
			}
			switch m.Outcome {
			case captureOK:
				if err := verifyFiles(ser, m); err != nil {
					t.Errorf("%s/%s: committed files do not verify: %v", series, n, err)
				}
				for _, f := range m.Files {
					owned[f.Name] = true
				}
			case captureFailed, captureInterrupted:
				if len(m.Files) != 0 {
					t.Errorf("%s/%s: a %s manifest lists files %v", series, n, m.Outcome, m.Files)
				}
				failedPrefixes = append(failedPrefixes, m.ID+"-"+m.Label+"-")
			default:
				t.Errorf("%s/%s: outcome %q", series, n, m.Outcome)
			}
		}
		for _, n := range names {
			_, _, kind, ok := parseName(n)
			switch {
			case n == seqFile, slices.Contains(allow, n):
			case strings.HasPrefix(n, ".staging-"):
				if !noted(notes, "orphan_staging", n) {
					t.Errorf("%s/%s: a staging directory recovery did not note (notes %q)", series, n, notes)
				}
			case strings.HasPrefix(n, "."):
				if !noted(notes, "orphan_temp", n) {
					t.Errorf("%s/%s: a leftover recovery did not note (notes %q)", series, n, notes)
				}
			case ok && kind == kindManifest:
			case ok && (kind == kindIntent || kind == kindHeartbeat):
				t.Errorf("%s/%s: an unresolved intent", series, n)
			case owned[n]:
			default:
				t.Errorf("%s/%s: no committed manifest owns it", series, n)
			}
			for _, p := range failedPrefixes {
				if strings.HasPrefix(n, p) && !slices.Contains(allow, n) {
					t.Errorf("%s/%s: a capture file of a failed capture", series, n)
				}
			}
		}
		_ = ser.Close()
	}
}

func dirNames(t *testing.T, root *os.Root, dir string) []string {
	t.Helper()
	f, err := root.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

// manifestFor reads the manifest of id in series, failing the test when
// there is none.
func manifestFor(t *testing.T, d *captureDir, series string, id int, label string) manifest {
	t.Helper()
	ser, err := d.root.OpenRoot(series)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	m, err := readManifest(ser, manifestName(id, label))
	if err != nil {
		t.Fatalf("manifest of %s/%s: %v", series, formatID(id), err)
	}
	return m
}

func hasManifest(t *testing.T, d *captureDir, series string, id int, label string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(d.root.Name(), series, manifestName(id, label)))
	return err == nil
}

func captureErr(t *testing.T, err error) *capture.Error {
	t.Helper()
	e, ok := errors.AsType[*capture.Error](err)
	if !ok {
		t.Fatalf("error %v (%T) is not a capture error", err, err)
	}
	return e
}

func TestCommitHappyPath(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	modes := []capture.Mode{capture.ModeCPU, capture.ModeGPU}
	r := drive(t, s, d, capCase{series: "parse-opt", label: "base", modes: modes})
	if !r.committed || r.err != nil {
		t.Fatalf("committed %v, err %v", r.committed, r.err)
	}
	m := manifestFor(t, d, "parse-opt", 1, "base")
	if m.Format != 1 || m.ID != "001" || m.Series != "parse-opt" || m.Label != "base" || m.Outcome != captureOK || m.WrittenBy != writtenBy {
		t.Fatalf("manifest %+v", m)
	}
	if m.Settings.SampleRate != "99" || m.Settings.Duration != "10s" || !slices.Equal(m.Settings.Modes, []string{"cpu", "gpu"}) {
		t.Errorf("settings %+v", m.Settings)
	}
	if !strings.HasPrefix(m.SettingsDigest, "sha256:") || m.SettingsDigest != settingsDigest(m.Settings) {
		t.Errorf("digest %q", m.SettingsDigest)
	}
	if m.StoppedBy != "duration" || m.Error != nil {
		t.Errorf("stopped_by %q, error %+v", m.StoppedBy, m.Error)
	}
	want := []string{"001-base-cpu.pb.gz", "001-base-gpu.pb.gz", "001-base-gpu.execlog"}
	var got []string
	for _, f := range m.Files {
		got = append(got, f.Name)
		fi, err := os.Lstat(filepath.Join(d.root.Name(), "parse-opt", f.Name))
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if uint64(st.Ino) != f.Ino || uint64(st.Nlink) != 1 || fi.Mode().Perm() != 0o600 || fi.Size() != f.Size {
			t.Errorf("%s: ino %d nlink %d mode %v size %d, manifest %+v", f.Name, st.Ino, st.Nlink, fi.Mode(), fi.Size(), f)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("files %v, want %v", got, want)
	}
	if r.txn.cancelNote != "" {
		t.Errorf("cancel note %q without a cancel", r.txn.cancelNote)
	}
	ser, _ := d.root.OpenRoot("parse-opt")
	defer func() { _ = ser.Close() }()
	if got := seqNext(t, ser); got != "2" {
		t.Errorf(".seq = %q", got)
	}
	ms, bad, err := seriesManifests(ser)
	if err != nil || len(bad) != 0 || len(ms) != 1 || ms[0].ID != "001" {
		t.Errorf("seriesManifests = %v, %v, %v", ms, bad, err)
	}
	invariant(t, d, nil)
	if names := dirNames(t, ser, "."); len(names) != 5 {
		t.Errorf("series holds %v, want .seq, the manifest and three files", names)
	}
	// A second capture takes the next id.
	if r := drive(t, s, d, capCase{series: "parse-opt", label: "cand1"}); !r.committed || r.id != 2 {
		t.Fatalf("second capture: id %d committed %v err %v", r.id, r.committed, r.err)
	}
}

// occurrence is the n-th time a fault point is reached in a clean run,
// and whether the manifest link had succeeded by then.
type occurrence struct {
	p        faultPoint
	n        int
	postLink bool
}

// cleanRun records, in a capture that succeeds, every fault point reached
// and where the manifest link succeeded.
func cleanRun(t *testing.T, modes []capture.Mode) []occurrence {
	t.Helper()
	s, d, _ := newCaptureServer(t)
	var (
		mu     sync.Mutex
		occs   []occurrence
		counts = map[faultPoint]int{}
		linked bool
	)
	s.captureFault = func(p faultPoint) error {
		mu.Lock()
		defer mu.Unlock()
		counts[p]++
		occs = append(occs, occurrence{p, counts[p], linked})
		return nil
	}
	s.link = func(r *os.Root, oldname, newname string) error {
		err := r.Link(oldname, newname)
		if err == nil && strings.HasSuffix(newname, ".capture.json") {
			mu.Lock()
			linked = true
			mu.Unlock()
		}
		return err
	}
	if r := drive(t, s, d, capCase{modes: modes}); !r.committed {
		t.Fatalf("clean run: %v", r.err)
	}
	return occs
}

// laterFaultPoints are reached by retention and the post-commit
// derivations, which other layers run.
var laterFaultPoints = []faultPoint{faultRetentionHash, faultBeforeOpen, faultBeforeDiff}

// TestEveryFaultPointLeavesAValidState runs a capture with each fault point
// failing, at each time it is reached, once with an error and once with a
// simulated crash. The outcome is judged by whether the manifest link had
// succeeded: before it, an error rolls back live to a failure manifest
// and a crash is recovered to an interrupted one, with no capture files;
// after it, the capture is committed, its files stay, and the next
// recovery removes the leftovers by identity.
func TestEveryFaultPointLeavesAValidState(t *testing.T) {
	modes := []capture.Mode{capture.ModeCPU, capture.ModeGPU}
	occs := cleanRun(t, modes)
	reached := map[faultPoint]bool{}
	for _, o := range occs {
		reached[o.p] = true
	}
	for _, fp := range faultPoints {
		switch {
		case slices.Contains(laterFaultPoints, fp.p), fp.p == faultInRollback, fp.p == faultInRecovery:
		case !reached[fp.p]:
			t.Errorf("%v is never reached", fp.p)
		}
	}
	for _, o := range occs {
		if fp := faultPoints[slices.IndexFunc(faultPoints, func(f faultInfo) bool { return f.p == o.p })]; fp.postLink && !o.postLink {
			t.Errorf("%v is listed as after the link but is reached before it", o.p)
		}
	}

	for _, o := range occs {
		for _, crash := range []bool{false, true} {
			err := errInjected
			if crash {
				err = errCrash
			}
			t.Run(fmt.Sprintf("%v#%d/crash=%v", o.p, o.n, crash), func(t *testing.T) {
				s, d, clk := newCaptureServer(t)
				s.captureFault = inject(shot{o.p, o.n, err})
				r := drive(t, s, d, capCase{modes: modes})
				s.captureFault = nil
				checkFaultRun(t, s, d, clk, r, o.postLink, crash)
			})
		}
	}

	// Inside live rollback: an error strands the rollback until the next
	// capture retries it; a crash leaves it to recovery.
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("%v/crash=%v", faultInRollback, crash), func(t *testing.T) {
			err := errInjected
			if crash {
				err = errCrash
			}
			s, d, clk := newCaptureServer(t)
			s.captureFault = inject(shot{faultAfterLinks, 1, errInjected}, shot{faultInRollback, 1, err})
			r := drive(t, s, d, capCase{modes: modes})
			s.captureFault = nil
			checkFaultRun(t, s, d, clk, r, false, crash)
		})
	}

	// Inside recovery: an error stops it and leaves the intent; a crash
	// does too. The next recovery finishes either.
	for _, crash := range []bool{false, true} {
		t.Run(fmt.Sprintf("%v/crash=%v", faultInRecovery, crash), func(t *testing.T) {
			err := errInjected
			if crash {
				err = errCrash
			}
			s, d, clk := newCaptureServer(t)
			s.captureFault = inject(shot{faultAfterLinks, 1, errCrash}, shot{faultInRecovery, 1, err})
			r := drive(t, s, d, capCase{modes: modes})
			s.ownerAlive = func(intent) bool { return false }
			clk.advance(staleAfter + time.Second)
			if _, rerr := s.recoverCaptures(d); !errors.Is(rerr, err) {
				t.Fatalf("recovery with a fault inside: %v, want %v", rerr, err)
			}
			s.captureFault = nil
			checkFaultRun(t, s, d, clk, r, false, true)
		})
	}
}

// checkFaultRun judges a run that met a fault, after the recovery or the
// retry the next capture would do.
func checkFaultRun(t *testing.T, s *Server, d *captureDir, clk *testClock, r capRun, postLink, crash bool) {
	t.Helper()
	if r.committed != postLink {
		t.Fatalf("committed %v, want %v (err %v)", r.committed, postLink, r.err)
	}
	var notes []string
	if crash {
		notes = crashRecover(t, s, d, clk)
	} else {
		if err := s.retryStranded(); err != nil {
			t.Fatalf("retryStranded: %v", err)
		}
		notes = recoverNow(t, s, d)
	}
	invariant(t, d, notes)
	if r.id == 0 || (crash && !postLink && !r.intent) {
		// No id, or a crash before the intent: there is nothing to record.
		return
	}
	switch m := manifestFor(t, d, "adhoc", r.id, "run"); {
	case postLink:
		if m.Outcome != captureOK || len(m.Files) != 3 {
			t.Errorf("after the link: outcome %q with %d files", m.Outcome, len(m.Files))
		}
	case crash && r.intent:
		if m.Outcome != captureInterrupted && m.Outcome != captureFailed {
			t.Errorf("after a crash: outcome %q", m.Outcome)
		}
	case !crash:
		if m.Outcome != captureFailed || m.Error == nil {
			t.Errorf("after an error: outcome %q, error %+v", m.Outcome, m.Error)
		}
	}
}

// TestENOSPCAtEveryWrite fails each write the protocol makes with ENOSPC.
// Before the manifest link the capture ends failed, live or once the
// stranded rollback is retried with space back; at the directory fsync
// after a successful link it is committed and keeps its files.
func TestENOSPCAtEveryWrite(t *testing.T) {
	for _, op := range []string{"seq", "seq rename", "intent", "heartbeat", "staging", "intent rewrite", "manifest temp", "manifest link"} {
		t.Run(op, func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			s.writeErr = failOp(op, syscall.ENOSPC)
			r := drive(t, s, d, capCase{})
			if r.committed || r.err == nil {
				t.Fatalf("committed %v, err %v", r.committed, r.err)
			}
			s.writeErr = nil
			if err := s.retryStranded(); err != nil {
				t.Fatalf("retryStranded with space back: %v", err)
			}
			invariant(t, d, recoverNow(t, s, d))
			if r.id > 0 {
				if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureFailed || m.Error == nil || m.Error.Code != string(capture.CodeDisk) {
					t.Errorf("manifest outcome %q error %+v", m.Outcome, m.Error)
				}
			}
		})
	}
	t.Run("fsync manifest", func(t *testing.T) {
		s, d, _ := newCaptureServer(t)
		s.writeErr = failOp("fsync manifest", syscall.ENOSPC)
		r := drive(t, s, d, capCase{})
		s.writeErr = nil
		if !r.committed {
			t.Fatalf("not committed: %v", r.err)
		}
		invariant(t, d, recoverNow(t, s, d))
		if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureOK || len(m.Files) != 1 {
			t.Errorf("manifest %+v", m)
		}
	})
}

// TestALostLinkRaceSparesTheUserFile: a file the user creates at a final
// name between the identity record and the links survives byte for byte,
// and the capture rolls back the files it did link.
func TestALostLinkRaceSparesTheUserFile(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	user := []byte("the user's own file\n")
	userName := finalName(1, "run", "gpu.pb.gz")
	s.captureFault = func(p faultPoint) error {
		if p == faultAfterRecord {
			if err := os.WriteFile(filepath.Join(d.root.Name(), "adhoc", userName), user, 0o644); err != nil {
				t.Error(err)
			}
		}
		return nil
	}
	r := drive(t, s, d, capCase{modes: []capture.Mode{capture.ModeCPU, capture.ModeGPU}})
	if r.committed || captureErr(t, r.err).Code != capture.CodeExists {
		t.Fatalf("committed %v, err %v; want exists", r.committed, r.err)
	}
	if got, err := os.ReadFile(filepath.Join(d.root.Name(), "adhoc", userName)); err != nil || !bytes.Equal(got, user) {
		t.Fatalf("user file %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", finalName(1, "run", "cpu.pb.gz"))); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the linked cpu profile was not rolled back: %v", err)
	}
	if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed || m.Error.Code != string(capture.CodeExists) {
		t.Errorf("manifest %+v", m)
	}
	invariant(t, d, recoverNow(t, s, d), userName)
}

// TestLiveOwnerENOSPCIsFinishedByTheNextCapture: when the failure manifest
// cannot be written, the intent stays with its live owner, recovery leaves
// it alone, and the owner's next capture completes the rollback.
func TestLiveOwnerENOSPCIsFinishedByTheNextCapture(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.writeErr = failOp("manifest link", syscall.ENOSPC)
	r := drive(t, s, d, capCase{})
	if r.committed || r.err == nil {
		t.Fatalf("committed %v err %v", r.committed, r.err)
	}
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	in, _, err := readIntent(ser, intentName(1, "run"))
	if err != nil || in.State != stateRollingBack {
		t.Fatalf("intent %+v, %v; want rolling_back", in, err)
	}
	notes := recoverNow(t, s, d)
	if !noted(notes, "left", intentName(1, "run")) {
		t.Errorf("recovery did not leave the live owner's intent: %q", notes)
	}
	if _, err := ser.Lstat(intentName(1, "run")); err != nil {
		t.Fatalf("recovery removed a live owner's intent: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(ser.Name(), "001-run-*")); len(left) != 0 {
		t.Errorf("capture files while rolling back: %v", left)
	}

	s.writeErr = nil
	if r := drive(t, s, d, capCase{}); !r.committed || r.id != 2 {
		t.Fatalf("next capture: id %d committed %v err %v", r.id, r.committed, r.err)
	}
	if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed {
		t.Errorf("the stranded capture's manifest: %+v", m)
	}
	invariant(t, d, recoverNow(t, s, d))
}

// TestPostLinkErrorsFollowTheCommittedPath: a failure after the manifest
// link (the directory fsync, staging removal, the intent's removal) is
// logged and the capture stays committed with its files.
func TestPostLinkErrorsFollowTheCommittedPath(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(s *Server)
	}{
		{"after-manifest-link", func(s *Server) { s.captureFault = inject(shot{faultAfterManifestLink, 1, errInjected}) }},
		{"after-staging-removal", func(s *Server) { s.captureFault = inject(shot{faultAfterStagingRemoval, 1, errInjected}) }},
		{"fsync", func(s *Server) { s.writeErr = failOp("fsync manifest", syscall.EIO) }},
		{"cleanup", func(s *Server) { s.writeErr = failOp("fsync cleanup", syscall.EIO) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			c.setup(s)
			r := drive(t, s, d, capCase{})
			s.captureFault, s.writeErr = nil, nil
			if !r.committed {
				t.Fatalf("not committed: %v", r.err)
			}
			if r.err == nil {
				t.Errorf("the cleanup error was not reported")
			}
			if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureOK || len(m.Files) != 1 {
				t.Fatalf("manifest %+v", m)
			}
			invariant(t, d, recoverNow(t, s, d))
		})
	}
}

// TestTheDecisionRule: a cancel before the commit claim wins and leaves a
// cancelled failure manifest; a cancel during the manifest link answers
// "cancelling" and is "too late: committed" once the link succeeded, or
// "cancelled" when the link failed.
func TestTheDecisionRule(t *testing.T) {
	t.Run("word", func(t *testing.T) {
		var d decisionWord
		if got := d.requestCancel(); got != cancelAnswerCancelling || !d.cancelled() {
			t.Fatalf("open → %q, cancelled %v", got, d.cancelled())
		}
		if got := d.requestCancel(); got != cancelAnswerCancelling {
			t.Fatalf("twice → %q", got)
		}
		if d.beginManifest() {
			t.Fatal("beginManifest won over a cancel")
		}
		var e decisionWord
		if !e.beginManifest() || e.cancelled() {
			t.Fatal("beginManifest on an open word")
		}
		if got := e.requestCancel(); got != cancelAnswerCancelling || e.cancelled() {
			t.Fatalf("in flight → %q, cancelled %v", got, e.cancelled())
		}
		if got := e.settle(true); got != cancelAnswerTooLate {
			t.Fatalf("settle after an in-flight cancel = %q", got)
		}
		if got := e.requestCancel(); got != cancelAnswerTooLate {
			t.Fatalf("after commit → %q", got)
		}
		var f decisionWord
		f.beginManifest()
		f.requestCancel()
		if got := f.settle(false); got != cancelAnswerCancelled || !f.cancelled() {
			t.Fatalf("failed link with a pending cancel = %q", got)
		}
	})

	t.Run("cancel-before-the-manifest", func(t *testing.T) {
		s, d, _ := newCaptureServer(t)
		dec := &decisionWord{}
		dec.requestCancel()
		r := drive(t, s, d, capCase{dec: dec})
		if r.committed || captureErr(t, r.err).Code != capture.CodeCancelled {
			t.Fatalf("committed %v err %v", r.committed, r.err)
		}
		m := manifestFor(t, d, "adhoc", 1, "run")
		if m.Outcome != captureFailed || m.Error.Code != string(capture.CodeCancelled) || len(m.Files) != 0 {
			t.Fatalf("manifest %+v", m)
		}
		invariant(t, d, recoverNow(t, s, d))
	})

	blocked := func(t *testing.T, linkErr error) (*Server, *captureDir, *decisionWord, capRun) {
		s, d, _ := newCaptureServer(t)
		dec := &decisionWord{}
		in, out := make(chan struct{}), make(chan struct{})
		s.captureFault = func(p faultPoint) error {
			if p != faultInManifestLink {
				return nil
			}
			close(in)
			<-out
			return linkErr
		}
		done := make(chan capRun)
		go func() { done <- drive(t, s, d, capCase{dec: dec}) }()
		<-in
		if got := dec.requestCancel(); got != cancelAnswerCancelling {
			t.Errorf("cancel during the manifest link = %q, want cancelling", got)
		}
		close(out)
		return s, d, dec, <-done
	}

	t.Run("blocked-link-then-success", func(t *testing.T) {
		s, d, dec, r := blocked(t, nil)
		if !r.committed || r.err != nil {
			t.Fatalf("committed %v err %v", r.committed, r.err)
		}
		if r.txn.cancelNote != cancelAnswerTooLate {
			t.Errorf("cancel note %q, want %q", r.txn.cancelNote, cancelAnswerTooLate)
		}
		if got := dec.requestCancel(); got != cancelAnswerTooLate {
			t.Errorf("cancel after commit = %q", got)
		}
		invariant(t, d, recoverNow(t, s, d))
	})

	t.Run("blocked-link-then-error", func(t *testing.T) {
		s, d, _, r := blocked(t, errInjected)
		if r.committed || captureErr(t, r.err).Code != capture.CodeCancelled {
			t.Fatalf("committed %v err %v", r.committed, r.err)
		}
		if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed || m.Error.Code != string(capture.CodeCancelled) {
			t.Fatalf("manifest %+v", m)
		}
		invariant(t, d, recoverNow(t, s, d))
	})

	t.Run("no-cancel", func(t *testing.T) {
		s, d, _ := newCaptureServer(t)
		dec := &decisionWord{}
		r := drive(t, s, d, capCase{dec: dec})
		if !r.committed || r.txn.cancelNote != "" {
			t.Fatalf("committed %v note %q", r.committed, r.txn.cancelNote)
		}
		if got := dec.requestCancel(); got != cancelAnswerTooLate {
			t.Errorf("cancel after commit = %q", got)
		}
		invariant(t, d, recoverNow(t, s, d))
	})
}

// TestRemoveByIdentityNeverRemovesWhatItCannotProve: a name is removed
// only when it is the recorded regular file (and, for a final name, still
// has its size and digest); anything else stays.
func TestRemoveByIdentityNeverRemovesWhatItCannotProve(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	record := func(name, content string) fileIdentity {
		writeFile(t, root, name, content)
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
	gone := func(name string) bool {
		_, err := root.Lstat(name)
		return errors.Is(err, fs.ErrNotExist)
	}

	id := record("ours", "ours")
	if removed, left, err := removeByIdentity(root, "ours", id); !removed || left != "" || err != nil || !gone("ours") {
		t.Fatalf("our own file: %v %q %v", removed, left, err)
	}
	if removed, left, err := removeByIdentity(root, "ours", id); removed || left != "" || err != nil {
		t.Fatalf("a missing name: %v %q %v", removed, left, err)
	}

	cases := []struct {
		name string
		make func(t *testing.T) fileIdentity
		full bool
	}{
		{"replaced", func(t *testing.T) fileIdentity {
			id := record("f", "ours")
			_ = root.Remove("f")
			writeFile(t, root, "f", "ours")
			return id
		}, false},
		{"rewritten-in-place", func(t *testing.T) fileIdentity {
			id := record("f", "ours")
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte("user"), 0o600); err != nil {
				t.Fatal(err)
			}
			return id
		}, true},
		{"grown", func(t *testing.T) fileIdentity {
			id := record("f", "ours")
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte("ours and more"), 0o600); err != nil {
				t.Fatal(err)
			}
			return id
		}, true},
		{"symlink", func(t *testing.T) fileIdentity {
			id := record("target", "ours")
			if err := root.Symlink("target", "f"); err != nil {
				t.Fatal(err)
			}
			return id
		}, false},
		{"directory", func(t *testing.T) fileIdentity {
			if err := root.Mkdir("f", 0o700); err != nil {
				t.Fatal(err)
			}
			id, _ := identify(root, "f")
			return id
		}, false},
		{"three-links", func(t *testing.T) fileIdentity {
			id := record("f", "ours")
			_ = root.Link("f", "f2")
			_ = root.Link("f", "f3")
			return id
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, n := range []string{"f", "f2", "f3", "target"} {
				_ = root.Remove(n)
			}
			want := c.make(t)
			removed, left, err := removeByIdentity(root, "f", want)
			if removed || left == "" || err != nil {
				t.Fatalf("removed %v, left %q, err %v", removed, left, err)
			}
			if gone("f") {
				t.Fatal("removed a file it could not prove")
			}
		})
	}

	// Two links are the staging and final names of one capture file; the
	// name goes and the other link keeps the bytes.
	pair := record("staged", "ours")
	_ = root.Link("staged", "final")
	if removed, _, err := removeByIdentity(root, "staged", pair); !removed || err != nil {
		t.Fatalf("a staging+final pair: %v %v", removed, err)
	}
	if got, _ := root.ReadFile("final"); string(got) != "ours" {
		t.Fatalf("final %q", got)
	}

	// A staging directory replaced after its identity was recorded is left
	// where it is, and so is everything in it.
	s, d, clk := newCaptureServer(t)
	s.captureFault = inject(shot{faultAfterStaging, 1, errCrash})
	r := drive(t, s, d, capCase{})
	staging := filepath.Join(d.root.Name(), "adhoc", r.txn.in.Staging)
	if err := os.Rename(staging, staging+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "cpu.pb.gz"), []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	notes := crashRecover(t, s, d, clk)
	if got, err := os.ReadFile(filepath.Join(staging, "cpu.pb.gz")); err != nil || string(got) != "user" {
		t.Fatalf("a replaced staging directory was emptied: %q %v", got, err)
	}
	if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
		t.Errorf("outcome %q", m.Outcome)
	}
	invariant(t, d, notes, filepath.Base(staging)+".moved")
}

// refuseProbeLinks is a link seam for a filesystem without hard links: the
// staging probe is refused, as link(2) refuses it there.
func refuseProbeLinks(r *os.Root, oldname, newname string) error {
	if strings.Contains(oldname, ".probe") {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	return r.Link(oldname, newname)
}

// TestStagingProbeRefusesAFilesystemWithoutHardLinks: the job fails while
// preparing, before perf-agent would start, with the advice to put the
// captures on a filesystem with hard links.
func TestStagingProbeRefusesAFilesystemWithoutHardLinks(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.link = refuseProbeLinks
	reachedStaging := false
	s.captureFault = func(p faultPoint) error {
		if p == faultAfterStaging {
			reachedStaging = true
		}
		return nil
	}
	r := drive(t, s, d, capCase{})
	e := captureErr(t, r.err)
	if e.Code != capture.CodeDisabled || !strings.Contains(e.Message, "hard link") || !strings.Contains(e.Next+e.Message, "--captures") {
		t.Fatalf("error %+v", e)
	}
	if reachedStaging {
		t.Error("the capture went on past the staging step")
	}
	if left, _ := filepath.Glob(filepath.Join(d.root.Name(), "adhoc", ".staging-*")); len(left) != 0 {
		t.Errorf("staging left: %v", left)
	}
	if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed || m.Error.Code != string(capture.CodeDisabled) {
		t.Errorf("manifest %+v", m)
	}
	invariant(t, d, recoverNow(t, s, d))
}

// TestCommitRefusesOutputsThatAreNotPlainFiles: validation (step 8) takes
// only a regular, non-empty file with one link and within its limit; the
// capture fails with a code that says why and keeps no file.
func TestCommitRefusesOutputsThatAreNotPlainFiles(t *testing.T) {
	for _, c := range []struct {
		name string
		code capture.Code
		// plant prepares cpu.pb.gz in staging instead of the usual write.
		plant func(t *testing.T, staging string)
	}{
		{"missing", capture.CodeNoOutput, func(*testing.T, string) {}},
		{"empty", capture.CodeNoOutput, func(t *testing.T, staging string) {
			mustWrite(t, filepath.Join(staging, "cpu.pb.gz"), nil)
		}},
		{"symlink", capture.CodePerfAgentFailed, func(t *testing.T, staging string) {
			mustWrite(t, filepath.Join(staging, "..", "elsewhere"), []byte("x"))
			if err := os.Symlink("../elsewhere", filepath.Join(staging, "cpu.pb.gz")); err != nil {
				t.Fatal(err)
			}
		}},
		{"two-links", capture.CodePerfAgentFailed, func(t *testing.T, staging string) {
			mustWrite(t, filepath.Join(staging, "cpu.pb.gz"), []byte("x"))
			if err := os.Link(filepath.Join(staging, "cpu.pb.gz"), filepath.Join(staging, "..", "extra-link")); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", capture.CodePerfAgentFailed, func(t *testing.T, staging string) {
			if err := os.Mkdir(filepath.Join(staging, "cpu.pb.gz"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"too-large", capture.CodeTooLarge, func(t *testing.T, staging string) {
			mustWrite(t, filepath.Join(staging, "cpu.pb.gz"), make([]byte, 2048))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			s.lim.Capture.MaxProfileBytes = 1024
			s.captureFault = func(p faultPoint) error {
				if p == faultAfterStaging {
					ser := filepath.Join(d.root.Name(), "adhoc")
					stagings, _ := filepath.Glob(filepath.Join(ser, ".staging-*"))
					if len(stagings) == 1 {
						c.plant(t, stagings[0])
					}
				}
				return nil
			}
			r := drive(t, s, d, capCase{content: map[string][]byte{}})
			if r.committed || captureErr(t, r.err).Code != c.code {
				t.Fatalf("committed %v err %v, want %s", r.committed, r.err, c.code)
			}
			if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed || m.Error.Code != string(c.code) {
				t.Errorf("manifest %+v", m)
			}
			notes := recoverNow(t, s, d)
			// A directory perf-agent should not have made keeps its staging
			// directory, which recovery then reports.
			invariant(t, d, notes, "elsewhere", "extra-link")
		})
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAReplacedIntentIsNeverOverwritten: an intent replaced by another
// file under its name is not this capture's any more; the capture fails
// and leaves that file as it is.
func TestAReplacedIntentIsNeverOverwritten(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	theirs := []byte("someone else's file\n")
	s.captureFault = func(p faultPoint) error {
		if p == faultAfterStaging {
			name := filepath.Join(d.root.Name(), "adhoc", intentName(1, "run"))
			mustWrite(t, name+".new", theirs)
			if err := os.Rename(name+".new", name); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	r := drive(t, s, d, capCase{})
	if r.committed {
		t.Fatal("committed over a replaced intent")
	}
	if got, err := os.ReadFile(filepath.Join(d.root.Name(), "adhoc", intentName(1, "run"))); err != nil || !bytes.Equal(got, theirs) {
		t.Fatalf("the replacing file is %q, %v", got, err)
	}
	if m := manifestFor(t, d, "adhoc", 1, "run"); m.Outcome != captureFailed {
		t.Errorf("manifest %+v", m)
	}
	notes := recoverNow(t, s, d)
	if !noted(notes, "left", intentName(1, "run")) {
		t.Errorf("recovery did not report the file it cannot read: %q", notes)
	}
	invariant(t, d, notes, intentName(1, "run"))
}
