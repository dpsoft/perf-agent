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

func TestNamesAndIDs(t *testing.T) {
	for _, c := range []struct {
		name string
		max  int
		ok   bool
	}{
		{"parse-opt", 64, true},
		{"a.b_c-D9", 64, true},
		{"", 64, false},
		{".hidden", 64, false},
		{"a/b", 64, false},
		{"a b", 64, false},
		{"..", 64, false},
		{"é", 64, false},
		{strings.Repeat("a", 64), 64, true},
		{strings.Repeat("a", 65), 64, false},
		{strings.Repeat("a", 33), 32, false},
	} {
		if got := validName(c.name, c.max); got != c.ok {
			t.Errorf("validName(%q, %d) = %v, want %v", c.name, c.max, got, c.ok)
		}
	}
	if !validSeries(strings.Repeat("s", 64)) || validSeries(strings.Repeat("s", 65)) {
		t.Error("a series is 1-64 bytes")
	}
	if !validLabel(strings.Repeat("l", 32)) || validLabel(strings.Repeat("l", 33)) {
		t.Error("a label is 1-32 bytes")
	}

	for id, want := range map[int]string{1: "001", 42: "042", 999: "999", 1000: "1000", 12345: "12345"} {
		if got := formatID(id); got != want {
			t.Errorf("formatID(%d) = %q, want %q", id, got, want)
		}
	}
	if got := finalName(2, "cand1", "cpu.pb.gz"); got != "002-cand1-cpu.pb.gz" {
		t.Errorf("finalName = %q", got)
	}
	if got := finalName(4, "cand3", "gpu.execlog"); got != "004-cand3-gpu.execlog" {
		t.Errorf("finalName = %q", got)
	}
	if got := manifestName(3, "cand2"); got != "003-cand2.capture.json" {
		t.Errorf("manifestName = %q", got)
	}
	if got := intentName(3, "cand2"); got != "003-cand2.intent.json" {
		t.Errorf("intentName = %q", got)
	}
	if got := heartbeatName(3, "cand2"); got != "003-cand2.intent.hb" {
		t.Errorf("heartbeatName = %q", got)
	}

	for _, c := range []struct {
		name string
		id   int
		rest string
		kind nameKind
		ok   bool
	}{
		{"001-base.capture.json", 1, "base", kindManifest, true},
		{"001-a.capture.capture.json", 1, "a.capture", kindManifest, true},
		{"1000-x.intent.json", 1000, "x", kindIntent, true},
		{"007-x.intent.hb", 7, "x", kindHeartbeat, true},
		{"002-cand1-cpu.pb.gz", 2, "cand1-cpu.pb.gz", kindFile, true},
		{"01-base.capture.json", 0, "", 0, false},   // fewer than three digits
		{"0001-base.capture.json", 0, "", 0, false}, // not the canonical width
		{"000-base.capture.json", 0, "", 0, false},  // ids start at 1
		{"001.capture.json", 0, "", 0, false},
		{"001-.capture.json", 0, "", 0, false},
		{".seq", 0, "", 0, false},
		{"abc-x.capture.json", 0, "", 0, false},
	} {
		id, rest, kind, ok := parseName(c.name)
		if ok != c.ok || id != c.id || rest != c.rest || kind != c.kind {
			t.Errorf("parseName(%q) = %d, %q, %v, %v; want %d, %q, %v, %v", c.name, id, rest, kind, ok, c.id, c.rest, c.kind, c.ok)
		}
	}
}

// seqNext is the next id .seq holds, read straight from the file.
func seqNext(t *testing.T, ser *os.Root) string {
	t.Helper()
	b, err := ser.ReadFile(seqFile)
	if err != nil {
		t.Fatalf("read .seq: %v", err)
	}
	var r seqRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf(".seq %q: %v", b, err)
	}
	return r.Next
}

func TestNextIDIsPersistedFirstAndNeverReused(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	ser, err := d.series("loop")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()

	id, err := s.nextID(ser)
	if err != nil || id != 1 {
		t.Fatalf("first nextID = %d, %v; want 1", id, err)
	}
	// The id is persisted before it is handed out.
	if got := seqNext(t, ser); got != "2" {
		t.Fatalf(".seq after id 1 = %q, want 2", got)
	}
	if id, err = s.nextID(ser); err != nil || id != 2 {
		t.Fatalf("second nextID = %d, %v; want 2", id, err)
	}

	// Retention removing every manifest never brings an id back.
	writeFile(t, ser, "002-run.capture.json", "x")
	_ = ser.Remove("002-run.capture.json")
	if id, err = s.nextID(ser); err != nil || id != 3 {
		t.Fatalf("nextID after removals = %d, %v; want 3", id, err)
	}

	// Without .seq the next id follows the highest manifest or intent.
	if err := ser.Remove(seqFile); err != nil {
		t.Fatal(err)
	}
	writeFile(t, ser, "005-a.capture.json", "x")
	writeFile(t, ser, "007-b.intent.json", "x")
	if id, err = s.nextID(ser); err != nil || id != 8 {
		t.Fatalf("nextID without .seq = %d, %v; want 8", id, err)
	}
	// A .seq behind the files on disk never reuses one of their ids.
	writeFile(t, ser, "012-c.capture.json", "x")
	if id, err = s.nextID(ser); err != nil || id != 13 {
		t.Fatalf("nextID behind the files = %d, %v; want 13", id, err)
	}

	// A failed rename hands out nothing, leaves .seq as it was and removes
	// its own temporary.
	s.writeErr = func(op string) error {
		if op == "seq rename" {
			return syscall.ENOSPC
		}
		return nil
	}
	if id, err := s.nextID(ser); err == nil {
		t.Fatalf("nextID with a failing rename = %d, want an error", id)
	}
	s.writeErr = nil
	if got := seqNext(t, ser); got != "14" {
		t.Fatalf(".seq after a failed write = %q, want 14", got)
	}
	if left, _ := filepath.Glob(filepath.Join(ser.Name(), ".seq.tmp-*")); len(left) != 0 {
		t.Fatalf("temporaries left: %v", left)
	}
	if id, err = s.nextID(ser); err != nil || id != 14 {
		t.Fatalf("nextID after a failed write = %d, %v; want 14", id, err)
	}

	// A .seq that is not ours is refused rather than guessed past.
	writeFile(t, ser, seqFile+".new", "garbage")
	if err := ser.Rename(seqFile+".new", seqFile); err != nil {
		t.Fatal(err)
	}
	if id, err := s.nextID(ser); err == nil {
		t.Fatalf("nextID with a corrupt .seq = %d, want an error", id)
	}
}

func writeFile(t *testing.T, root *os.Root, name, content string) {
	t.Helper()
	if err := root.WriteFile(name, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnerFileRefusesTheOtherKind(t *testing.T) {
	for _, order := range [][2]string{{ownerStdio, ownerServe}, {ownerServe, ownerStdio}} {
		t.Run(order[0]+"-first", func(t *testing.T) {
			profiles := t.TempDir()
			first := newKindServer(t, profiles, order[0], "shared")
			second := newKindServer(t, profiles, order[1], "shared")
			if e := second.ownerMismatch(); e != nil {
				t.Fatalf("before reading .owner the check is not synchronous, got %v", e)
			}

			d, e := first.openCaptureDir()
			if e != nil {
				t.Fatalf("first open: %v", e)
			}
			d.close()
			b, err := os.ReadFile(filepath.Join(profiles, "shared", ownerFile))
			if err != nil {
				t.Fatal(err)
			}
			var rec ownerRecord
			if err := json.Unmarshal(b, &rec); err != nil || rec.Kind != order[0] || rec.Format != 1 {
				t.Fatalf(".owner = %s (%v)", b, err)
			}
			// The same kind opens the directory again.
			if d, e := first.openCaptureDir(); e != nil {
				t.Fatalf("second open by the owner: %v", e)
			} else {
				d.close()
			}

			_, e = second.openCaptureDir()
			if e == nil || e.Code != capture.CodeOwnerMismatch {
				t.Fatalf("other kind: %v, want owner_mismatch", e)
			}
			for _, want := range []string{"shared inside the profiles root", order[0]} {
				if !strings.Contains(e.Message, want) {
					t.Errorf("message %q does not name %q", e.Message, want)
				}
			}
			if strings.Contains(e.Message, profiles) {
				t.Errorf("message %q names the profiles root's path", e.Message)
			}
			// The kind is cached: the next refusal needs no I/O.
			if e := second.ownerMismatch(); e == nil || e.Code != capture.CodeOwnerMismatch {
				t.Fatalf("cached check: %v", e)
			}
			if e := first.ownerMismatch(); e != nil {
				t.Fatalf("the owner's cached check: %v", e)
			}
		})
	}

	t.Run("default-dirs-differ", func(t *testing.T) {
		profiles := t.TempDir()
		for _, kind := range []string{ownerStdio, ownerServe} {
			d, e := newKindServer(t, profiles, kind, "").openCaptureDir()
			if e != nil {
				t.Fatalf("%s: %v", kind, e)
			}
			d.close()
		}
		for _, dir := range []string{"captures", "captures-serve"} {
			if _, err := os.Stat(filepath.Join(profiles, dir, ownerFile)); err != nil {
				t.Errorf("%s: %v", dir, err)
			}
		}
	})

	t.Run("foreign-owner-file", func(t *testing.T) {
		profiles := t.TempDir()
		if err := os.MkdirAll(filepath.Join(profiles, "shared"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profiles, "shared", ownerFile), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, e := newKindServer(t, profiles, ownerStdio, "shared").openCaptureDir()
		if e == nil || e.Code != capture.CodeOwnerMismatch {
			t.Fatalf("foreign .owner: %v, want owner_mismatch", e)
		}
	})

	t.Run("outside-the-profiles-root", func(t *testing.T) {
		_, e := newKindServer(t, t.TempDir(), ownerStdio, t.TempDir()).openCaptureDir()
		if e == nil || e.Code != capture.CodeDisabled || !strings.Contains(e.Next, "--captures") {
			t.Fatalf("outside the root: %v", e)
		}
	})
}

func newKindServer(t *testing.T, profiles, owner, dir string) *Server {
	t.Helper()
	s, err := New(Config{ProfilesDir: profiles, Capture: CaptureConfig{Owner: owner, Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestCaptureMessagesNameTheProfilesRoot: a capture's write errors name the
// capture directory inside the profiles root and the --captures flag, and
// never --output, which moves pages, not captures.
func TestCaptureMessagesNameTheProfilesRoot(t *testing.T) {
	s, d, _ := newCaptureServer(t)
	s.writeErr = failOp("intent", syscall.ENOSPC)
	r := drive(t, s, d, capCase{})
	e := captureErr(t, r.err)
	if e.Code != capture.CodeDisk {
		t.Fatalf("code %s, want disk", e.Code)
	}
	text := e.Message + " " + e.Next
	for _, want := range []string{"inside the profiles root", "--captures"} {
		if !strings.Contains(text, want) {
			t.Errorf("%q does not name %q", text, want)
		}
	}
	if strings.Contains(text, s.profiles.Name()) {
		t.Errorf("%q names the profiles root's path", text)
	}
	if strings.Contains(text, "--output") || strings.Contains(text, "output root") {
		t.Errorf("%q mentions the output root", text)
	}
	s.writeErr = nil
	m := manifestFor(t, d, "adhoc", r.id, "run")
	if m.Error == nil || m.Error.Message != e.Message {
		t.Fatalf("failure manifest error %+v, want the same message", m.Error)
	}

	// The hard-link refusal advises --captures too.
	s2, d2, _ := newCaptureServer(t)
	s2.link = refuseProbeLinks
	e2 := captureErr(t, drive(t, s2, d2, capCase{}).err)
	text = e2.Message + " " + e2.Next
	if !strings.Contains(text, "--captures") || strings.Contains(text, "--output") {
		t.Errorf("hard-link refusal %q", text)
	}
}
