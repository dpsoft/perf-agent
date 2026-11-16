package perfmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// okManifest is a valid committed manifest of 001-run in adhoc.
func okManifest() manifest {
	return manifest{
		Format: 1, ID: "001", Series: "adhoc", Label: "run", Outcome: captureOK, WrittenBy: writtenBy,
		Files: []fileIdentity{{Name: "001-run-cpu.pb.gz", Dev: 1, Ino: 2, Size: 3, SHA256: strings.Repeat("a", 64)}},
	}
}

// TestReadManifestRefusesWhatItDidNotWrite pins each check readManifest
// makes before anything trusts a manifest.
func TestReadManifestRefusesWhatItDidNotWrite(t *testing.T) {
	_, d, _ := newCaptureServer(t)
	ser, err := d.series("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	name := manifestName(1, "run")
	put := func(m manifest) {
		b, _ := json.Marshal(m)
		_ = ser.Remove(name)
		writeFile(t, ser, name, string(b))
	}
	put(okManifest())
	if _, err := readManifest(ser, name); err != nil {
		t.Fatalf("a valid manifest: %v", err)
	}
	failed := okManifest()
	failed.Outcome, failed.Files = captureFailed, nil
	put(failed)
	if _, err := readManifest(ser, name); err != nil {
		t.Fatalf("a valid failure manifest: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*manifest)
	}{
		{"written-by", func(m *manifest) { m.WrittenBy = "someone" }},
		{"format", func(m *manifest) { m.Format = 2 }},
		{"id", func(m *manifest) { m.ID = "002" }},
		{"label", func(m *manifest) { m.Label = "other" }},
		{"outcome", func(m *manifest) { m.Outcome = "maybe" }},
		{"outcome-without-files", func(m *manifest) { m.Outcome, m.Files = "maybe", nil }},
		{"ok-without-files", func(m *manifest) { m.Files = nil }},
		{"failed-with-files", func(m *manifest) { m.Outcome = captureFailed }},
		{"interrupted-with-files", func(m *manifest) { m.Outcome = captureInterrupted }},
		{"file-of-another-capture", func(m *manifest) { m.Files[0].Name = "002-run-cpu.pb.gz" }},
		{"file-outside", func(m *manifest) { m.Files[0].Name = "001-run-../../x" }},
		{"file-without-digest", func(m *manifest) { m.Files[0].SHA256 = "" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := okManifest()
			c.edit(&m)
			put(m)
			if _, err := readManifest(ser, name); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	t.Run("over-1-MiB", func(t *testing.T) {
		m := okManifest()
		m.Notes = []string{strings.Repeat("n", maxManifestBytes)}
		put(m)
		if _, err := readManifest(ser, name); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		put(okManifest())
		_ = ser.Rename(name, "elsewhere.json")
		if err := ser.Symlink("elsewhere.json", name); err != nil {
			t.Fatal(err)
		}
		if _, err := readManifest(ser, name); err == nil {
			t.Fatal("accepted")
		}
	})
}

// validIntent is the intent of a fresh capture, as beginTxn writes it.
func validIntent(t *testing.T) (intent, string) {
	t.Helper()
	s, d, _ := newCaptureServer(t)
	txn, err := s.beginTxn(d, "adhoc", 1, "run", toWire(capture.Settings{Modes: []capture.Mode{capture.ModeCPU}}), []string{"cpu.pb.gz"})
	if err != nil {
		t.Fatal(err)
	}
	in := txn.in
	in.Files = []fileIdentity{{Name: "001-run-cpu.pb.gz", Staged: "cpu.pb.gz", Dev: 1, Ino: 2, Size: 3, SHA256: strings.Repeat("a", 64)}}
	in.Temps = []fileIdentity{{Name: "." + manifestName(1, "run") + ".tmp-" + in.Token[:16], Dev: 1, Ino: 3, SHA256: strings.Repeat("b", 64)}}
	txn.die()
	return in, intentName(1, "run")
}

// TestIntentCheckRefusesWhatTheProtocolNeverCreates: an intent that names
// anything the protocol never makes is not acted on, so recovery reading
// it touches nothing else.
func TestIntentCheckRefusesWhatTheProtocolNeverCreates(t *testing.T) {
	good, name := validIntent(t)
	if err := good.check(name); err != nil {
		t.Fatalf("a valid intent: %v", err)
	}
	for _, c := range []struct {
		name string
		edit func(*intent)
	}{
		{"format", func(in *intent) { in.Format = 2 }},
		{"written-by", func(in *intent) { in.WrittenBy = "x" }},
		{"id", func(in *intent) { in.ID = "002" }},
		{"label", func(in *intent) { in.Label = "../run" }},
		{"series", func(in *intent) { in.Series = "a/b" }},
		{"token", func(in *intent) { in.Token = "short" }},
		{"token-case", func(in *intent) { in.Token = strings.ToUpper(in.Token) }},
		{"staging", func(in *intent) { in.Staging = "../elsewhere" }},
		{"outputs", func(in *intent) { in.Outputs = append(in.Outputs, "gpu.pb.gz") }},
		{"output-path", func(in *intent) { in.Outputs[0], in.Finals[0] = "a/b", finalName(1, "run", "a/b") }},
		{"final", func(in *intent) { in.Finals[0] = "001-run-other" }},
		{"file", func(in *intent) { in.Files[0].Name = "elsewhere" }},
		{"staged", func(in *intent) { in.Files[0].Staged = "other" }},
		{"heartbeat", func(in *intent) { in.Heartbeat.Name = "elsewhere" }},
		{"temp-prefix", func(in *intent) { in.Temps[0].Name = ".x.tmp-" + in.Token[:16] }},
		{"temp-token", func(in *intent) { in.Temps[0].Name = "." + manifestName(1, "run") + ".tmp-0123456789abcdef" }},
		{"temp-path", func(in *intent) { in.Temps[0].Name = "." + manifestName(1, "run") + ".tmp-" + in.Token[:16] + "/x" }},
		{"temp-suffix", func(in *intent) { in.Temps[0].Name = "." + manifestName(1, "run") + ".tmp-" + in.Token[:16] + "extra" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, _ := json.Marshal(good)
			var in intent
			if err := json.Unmarshal(b, &in); err != nil {
				t.Fatal(err)
			}
			c.edit(&in)
			if err := in.check(name); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// TestAnIntentOfAnotherSeriesIsLeft: an intent whose series is not the
// directory it is in is not this series' to act on.
func TestAnIntentOfAnotherSeriesIsLeft(t *testing.T) {
	s, d, clk, r := crashedIntent(t)
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	in, _, err := readIntent(ser, intentName(r.id, "run"))
	if err != nil {
		t.Fatal(err)
	}
	in.Series = "other"
	b, _ := json.Marshal(in)
	writeFile(t, ser, "new", string(b))
	if err := ser.Rename("new", intentName(r.id, "run")); err != nil {
		t.Fatal(err)
	}
	notes := crashRecover(t, s, d, clk)
	if !noted(notes, "left", intentName(r.id, "run")) || hasManifest(t, d, "adhoc", r.id, "run") {
		t.Fatalf("notes %q", notes)
	}
	if _, err := ser.Lstat(r.txn.in.Staging); err != nil {
		t.Fatalf("staging was removed for an intent recovery may not act on: %v", err)
	}
}

// TestAnUnreadableManifestBesideAnIntentIsNotCommitted: a file at the
// manifest name that is not this server's manifest does not make the
// capture committed: nothing is removed and the intent is left.
func TestAnUnreadableManifestBesideAnIntentIsNotCommitted(t *testing.T) {
	s, d, clk := newCaptureServer(t)
	s.captureFault = inject(shot{faultAfterLinks, 1, errCrash})
	r := drive(t, s, d, capCase{})
	s.captureFault = nil
	ser, _ := d.root.OpenRoot("adhoc")
	defer func() { _ = ser.Close() }()
	writeFile(t, ser, manifestName(r.id, "run"), "not a manifest")
	notes := crashRecover(t, s, d, clk)
	if !noted(notes, "left", intentName(r.id, "run")) {
		t.Fatalf("notes %q", notes)
	}
	for _, n := range []string{intentName(r.id, "run"), r.txn.in.Staging, finalName(r.id, "run", "cpu.pb.gz")} {
		if _, err := ser.Lstat(n); err != nil {
			t.Errorf("%s was removed: %v", n, err)
		}
	}
}

// TestRollbackNeverDeletesAFinalRewrittenInPlace: a published file whose
// bytes changed since they were recorded (same inode, same size) is not
// the capture's any more and survives its rollback.
func TestRollbackNeverDeletesAFinalRewrittenInPlace(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	final := filepath.Join(d.root.Name(), "adhoc", finalName(1, "run", "cpu.pb.gz"))
	var theirs []byte
	s.captureFault = func(p faultPoint) error {
		if p != faultAfterLinks {
			return nil
		}
		b, err := os.ReadFile(final)
		if err != nil {
			t.Fatal(err)
		}
		theirs = []byte(strings.Repeat("u", len(b)))
		if err := os.WriteFile(final, theirs, 0o600); err != nil {
			t.Fatal(err)
		}
		return errInjected
	}
	r := drive(t, s, d, capCase{})
	s.captureFault = nil
	if r.committed {
		t.Fatal("committed")
	}
	if got, err := os.ReadFile(final); err != nil || string(got) != string(theirs) {
		t.Fatalf("the rewritten final is %d bytes, %v", len(got), err)
	}
	invariant(t, d, recoverNow(t, s, d), filepath.Base(final))
}

// TestValidateRefusesAnOutputOfAnotherUser: an output not owned by the
// server's own user is perf_agent_failed.
func TestValidateRefusesAnOutputOfAnotherUser(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	captureEUID = func() int { return os.Geteuid() + 1 }
	defer func() { captureEUID = os.Geteuid }()
	r := drive(t, s, d, capCase{})
	if e := captureErr(t, r.err); e.Code != capture.CodePerfAgentFailed || !strings.Contains(e.Message, "owned") {
		t.Fatalf("error %+v", e)
	}
	invariant(t, d, recoverNow(t, s, d))
}

// TestTheProbeSaysWhyALinkFailed: a filesystem without hard links gets
// the hard-link advice; a full one is disk; too many links says so; each
// names the errno.
func TestTheProbeSaysWhyALinkFailed(t *testing.T) {
	for _, c := range []struct {
		errno syscall.Errno
		name  string
		code  capture.Code
		says  string
	}{
		{syscall.EPERM, "EPERM", capture.CodeDisabled, "supports hard links"},
		{syscall.EOPNOTSUPP, "EOPNOTSUPP", capture.CodeDisabled, "supports hard links"},
		{syscall.EXDEV, "EXDEV", capture.CodeDisabled, "supports hard links"},
		{syscall.ENOSPC, "ENOSPC", capture.CodeDisk, "free space"},
		{syscall.EDQUOT, "EDQUOT", capture.CodeDisk, "free space"},
		{syscall.EMLINK, "EMLINK", capture.CodeDisk, "too many links"},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			s.link = func(r *os.Root, oldname, newname string) error {
				if strings.Contains(oldname, probeName) {
					return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: c.errno}
				}
				return r.Link(oldname, newname)
			}
			e := captureErr(t, drive(t, s, d, capCase{}).err)
			text := e.Message + "; " + e.Next
			if e.Code != c.code || !strings.Contains(text, c.says) || !strings.Contains(text, "("+c.name+")") {
				t.Fatalf("error %+v", e)
			}
		})
	}
}

// TestRecoveryReportsTopLevelTemporaries: a temporary .owner's write left
// in the capture directory itself is reported, and stays.
func TestRecoveryReportsTopLevelTemporaries(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	writeFile(t, d.root, "..owner.tmp-0123456789abcdef", "{}")
	notes := recoverNow(t, s, d)
	if !noted(notes, "orphan_temp", "..owner.tmp-0123456789abcdef") {
		t.Fatalf("notes %q", notes)
	}
	if _, err := d.root.Lstat("..owner.tmp-0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
}

// TestNextIDCountsEveryName: without .seq, the next id follows the highest
// id any name in the series carries: a heartbeat, a capture file, a
// temporary.
func TestNextIDCountsEveryName(t *testing.T) {
	for _, n := range []string{"." + intentName(5, "run") + ".tmp-0123456789abcdef", heartbeatName(7, "run"), finalName(9, "run", "cpu.pb.gz")} {
		t.Run(n, func(t *testing.T) {
			s, d, _ := newCaptureServer(t)
			ser, err := d.series("adhoc")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ser.Close() }()
			writeFile(t, ser, n, "x")
			want, _, _, _ := parseName(strings.TrimPrefix(n, "."))
			if id, err := s.nextID(ser); err != nil || id != want+1 {
				t.Fatalf("nextID = %d, %v; want %d", id, err, want+1)
			}
		})
	}
}
