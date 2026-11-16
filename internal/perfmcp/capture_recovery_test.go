package perfmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// crashedIntent runs a capture that dies just after its staging step and
// returns it: an intent whose heartbeat no longer runs.
func crashedIntent(t *testing.T) (*Server, *captureDir, *testClock, capRun) {
	t.Helper()
	s, d, clk := newCaptureServer(t)
	s.captureFault = inject(shot{faultAfterStaging, 1, errCrash})
	r := drive(t, s, d, capCase{})
	s.captureFault = nil
	if !r.intent {
		t.Fatalf("no intent: %v", r.err)
	}
	return s, d, clk, r
}

func TestRecoveryJudgesTheOwner(t *testing.T) {
	for _, c := range []struct {
		name    string
		alive   bool
		advance time.Duration
		left    bool
	}{
		{"alive-fresh", true, 0, true},
		{"gone-stale", false, staleAfter + time.Second, false},
		{"gone-fresh", false, staleAfter - 2*time.Second, true},
		{"alive-stale", true, staleAfter + time.Second, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, d, clk, r := crashedIntent(t)
			s.ownerAlive = func(intent) bool { return c.alive }
			clk.advance(c.advance)
			notes := recoverNow(t, s, d)
			if c.left {
				if hasManifest(t, d, "adhoc", r.id, "run") {
					t.Fatal("recovery wrote a manifest for an owner it should leave")
				}
				if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", intentName(r.id, "run"))); err != nil {
					t.Fatalf("intent removed: %v", err)
				}
				if !noted(notes, "left", intentName(r.id, "run")) {
					t.Errorf("notes %q", notes)
				}
				return
			}
			if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted || len(m.Files) != 0 {
				t.Fatalf("manifest %+v", m)
			}
			if !noted(notes, "interrupted", "adhoc/"+formatID(r.id)) {
				t.Errorf("notes %q", notes)
			}
			invariant(t, d, notes)
		})
	}

	// Without the seam the owner is judged by its recorded PID and start
	// time: this process is alive, a PID that does not exist is not.
	t.Run("recorded-identity", func(t *testing.T) {
		s, d, clk, r := crashedIntent(t)
		clk.advance(staleAfter + time.Second)
		if notes := recoverNow(t, s, d); !noted(notes, "left", intentName(r.id, "run")) {
			t.Fatalf("this live process's intent was not left: %q", notes)
		}
		ser, _ := d.root.OpenRoot("adhoc")
		defer func() { _ = ser.Close() }()
		in, _, err := readIntent(ser, intentName(r.id, "run"))
		if err != nil {
			t.Fatal(err)
		}
		in.ServerPID = 1 << 30
		b, _ := json.Marshal(in)
		writeFile(t, ser, intentName(r.id, "run")+".new", string(b))
		if err := ser.Rename(intentName(r.id, "run")+".new", intentName(r.id, "run")); err != nil {
			t.Fatal(err)
		}
		notes := recoverNow(t, s, d)
		if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
			t.Fatalf("outcome %q (notes %q)", m.Outcome, notes)
		}
		invariant(t, d, notes)
	})

	// A live reaper keeps the capture its own after its server is gone.
	t.Run("reaper-alive", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		txn, err := s.beginTxn(d, "adhoc", 1, "run", toWire(capture.Settings{Modes: []capture.Mode{capture.ModeCPU}}), []string{"cpu.pb.gz"})
		if err != nil {
			t.Fatal(err)
		}
		txn.in.ServerPID = 1 << 30
		if err := txn.setReaper(os.Getpid(), selfStart()); err != nil {
			t.Fatal(err)
		}
		txn.die()
		clk.advance(staleAfter + time.Second)
		if notes := recoverNow(t, s, d); !noted(notes, "left", intentName(1, "run")) {
			t.Fatalf("a live reaper's capture was not left: %q", notes)
		}
	})
}

// TestRecoveryRemovesItsOwnTemporaries: a temporary the protocol recorded
// is removed by its identity; one it could not record (a crash between
// creating .seq's temporary and renaming it), one replaced since, and a
// foreign one with the same prefix stay, and recovery notes them.
func TestRecoveryRemovesItsOwnTemporaries(t *testing.T) {
	t.Run("manifest-temp", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.captureFault = inject(shot{faultAfterManifestTemp, 1, errCrash})
		r := drive(t, s, d, capCase{})
		s.captureFault = nil
		temps, _ := filepath.Glob(filepath.Join(d.root.Name(), "adhoc", "."+manifestName(r.id, "run")+".tmp-"+r.txn.in.Token[:16]))
		if len(temps) != 1 {
			t.Fatalf("the manifest temporary: %v", temps)
		}
		notes := crashRecover(t, s, d, clk)
		if _, err := os.Lstat(temps[0]); !os.IsNotExist(err) {
			t.Fatalf("the recorded temporary is still there: %v", err)
		}
		if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
			t.Errorf("outcome %q", m.Outcome)
		}
		invariant(t, d, notes)
	})

	t.Run("replaced-manifest-temp", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.captureFault = inject(shot{faultAfterManifestTemp, 1, errCrash})
		r := drive(t, s, d, capCase{})
		s.captureFault = nil
		tmp := filepath.Join(d.root.Name(), "adhoc", "."+manifestName(r.id, "run")+".tmp-"+r.txn.in.Token[:16])
		if err := os.Remove(tmp); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tmp, []byte("someone else's"), 0o600); err != nil {
			t.Fatal(err)
		}
		notes := crashRecover(t, s, d, clk)
		if got, err := os.ReadFile(tmp); err != nil || string(got) != "someone else's" {
			t.Fatalf("a replaced temporary was removed: %q %v", got, err)
		}
		if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
			t.Errorf("outcome %q", m.Outcome)
		}
		invariant(t, d, notes)
	})

	t.Run("seq-temp", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		s.writeErr = failOp("seq rename", errCrash)
		r := drive(t, s, d, capCase{})
		s.writeErr = nil
		if r.id != 0 {
			t.Fatalf("an id was handed out: %d", r.id)
		}
		temps, _ := filepath.Glob(filepath.Join(d.root.Name(), "adhoc", ".seq.tmp-*"))
		if len(temps) != 1 {
			t.Fatalf(".seq temporaries: %v", temps)
		}
		notes := crashRecover(t, s, d, clk)
		if _, err := os.Lstat(temps[0]); err != nil {
			t.Fatalf("an unrecorded .seq temporary was removed: %v", err)
		}
		invariant(t, d, notes)
		// The id it would have taken was never handed out, so it is used now.
		if r := drive(t, s, d, capCase{}); !r.committed || r.id != 1 {
			t.Fatalf("next capture: id %d committed %v err %v", r.id, r.committed, r.err)
		}
	})

	t.Run("foreign", func(t *testing.T) {
		s, d, _ := newCaptureServer(t)
		if r := drive(t, s, d, capCase{}); !r.committed {
			t.Fatal(r.err)
		}
		ser, _ := d.root.OpenRoot("adhoc")
		defer func() { _ = ser.Close() }()
		foreign := []string{".x.tmp-0123456789abcdef", ".seq.tmp-0123456789abcdef", "." + manifestName(2, "run") + ".tmp-0123456789abcdef"}
		for _, n := range foreign {
			writeFile(t, ser, n, "foreign")
		}
		notes := recoverNow(t, s, d)
		for _, n := range foreign {
			if got, err := ser.ReadFile(n); err != nil || string(got) != "foreign" {
				t.Errorf("%s: %q %v", n, got, err)
			}
			if !noted(notes, "orphan_temp", n) {
				t.Errorf("%s not noted: %q", n, notes)
			}
		}
		invariant(t, d, notes)
	})
}

// TestAnUnrecordedStagingDirIsALoggedOrphan: a staging directory no intent
// identifies (a crash between creating it and recording it, or one nobody
// names) is never removed; recovery notes it as orphan_staging.
func TestAnUnrecordedStagingDirIsALoggedOrphan(t *testing.T) {
	t.Run("named-by-no-intent", func(t *testing.T) {
		s, d, _ := newCaptureServer(t)
		ser, err := d.series("adhoc")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ser.Close() }()
		if err := ser.Mkdir(".staging-0123456789abcdef", 0o700); err != nil {
			t.Fatal(err)
		}
		notes := recoverNow(t, s, d)
		if !noted(notes, "orphan_staging", ".staging-0123456789abcdef") {
			t.Fatalf("notes %q", notes)
		}
		if _, err := ser.Lstat(".staging-0123456789abcdef"); err != nil {
			t.Fatalf("the orphan was removed: %v", err)
		}
		invariant(t, d, notes)
	})

	t.Run("crash-before-the-record", func(t *testing.T) {
		s, d, clk := newCaptureServer(t)
		// The first intent rewrite is the one that records staging.
		s.writeErr = failOp("intent rewrite", errCrash)
		r := drive(t, s, d, capCase{})
		s.writeErr = nil
		staging := filepath.Join(d.root.Name(), "adhoc", r.txn.in.Staging)
		notes := crashRecover(t, s, d, clk)
		entries, err := os.ReadDir(staging)
		if err != nil || len(entries) != 0 {
			t.Fatalf("the orphan staging directory: %v %v", entries, err)
		}
		if !noted(notes, "orphan_staging", r.txn.in.Staging) {
			t.Fatalf("notes %q", notes)
		}
		if m := manifestFor(t, d, "adhoc", r.id, "run"); m.Outcome != captureInterrupted {
			t.Errorf("outcome %q", m.Outcome)
		}
		invariant(t, d, notes)
		if !strings.Contains(strings.Join(notes, "\n"), "remove it by hand") {
			t.Errorf("the note does not tell the user what to do: %q", notes)
		}
	})
}
