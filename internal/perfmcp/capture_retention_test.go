package perfmcp

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// retentionServer is a capture server whose retention keeps keep unkept
// captures per series, with no age limit and room to spare, logging to
// the returned buffer.
func retentionServer(t *testing.T, keep int) (*Server, *captureDir, *testClock, *lockedBuffer) {
	t.Helper()
	s, d, clk := newCaptureServer(t)
	logs := &lockedBuffer{}
	s.log = slog.New(slog.NewTextHandler(logs, nil))
	s.lim.Capture.KeepPerSeries = keep
	s.lim.Capture.TTL = 0
	s.lim.Capture.MaxStoredBytes = 1 << 40
	return s, d, clk, logs
}

// aged is one capture a retention test commits.
type aged struct {
	series, label string
	// age is how long before the clock's now it started.
	age      time.Duration
	keep     bool
	compare  string
	modes    []capture.Mode
	content  map[string][]byte
	failedAt faultPoint // a failure manifest instead, failing here
}

// commitAged commits c (or its failure) and returns its id.
func commitAged(t *testing.T, s *Server, d *captureDir, c aged) int {
	t.Helper()
	meta := &manifest{
		StoppedBy: "duration", ProfiledDuration: "10s",
		Start: s.clock().Add(-c.age).UTC(), Kept: c.keep, CompareWith: c.compare,
	}
	if c.failedAt != 0 {
		s.captureFault = inject(shot{c.failedAt, 1, errInjected})
		defer func() { s.captureFault = nil }()
	}
	r := drive(t, s, d, capCase{series: c.series, label: c.label, modes: c.modes, content: c.content, meta: meta})
	if c.failedAt == 0 && (!r.committed || r.err != nil) {
		t.Fatalf("capture %s/%s: committed %v, err %v", c.series, c.label, r.committed, r.err)
	}
	if c.failedAt != 0 && !hasManifest(t, d, cmpSeries(c.series), r.id, cmpLabel(c.label)) {
		t.Fatalf("capture %s/%s failed at %v without a manifest: %v", c.series, c.label, c.failedAt, r.err)
	}
	return r.id
}

func cmpSeries(s string) string { return cmpDefault(s, "adhoc") }
func cmpLabel(s string) string  { return cmpDefault(s, "run") }
func cmpDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// retainNow runs retention as a job does, holding the engine, with
// baseline as the call's baseline.
func retainNow(t *testing.T, s *Server, d *captureDir, baseline string) []string {
	t.Helper()
	ev, err := retainOnce(t, s, d, baseline)
	if err != nil {
		t.Fatalf("retention: %v (evicted %q)", err, ev)
	}
	return ev
}

func retainOnce(t *testing.T, s *Server, d *captureDir, baseline string) ([]string, error) {
	t.Helper()
	if err := s.engine.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer s.engine.release()
	return s.retain(d, s.protected(d, baseline), s.clock())
}

// seriesNames lists series.
func seriesNames(t *testing.T, d *captureDir, series string) []string {
	t.Helper()
	ser, err := d.root.OpenRoot(series)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	return dirNames(t, ser, ".")
}

// noneOf fails when any name of capture id in series is left, quarantined
// copies included.
func noneOf(t *testing.T, d *captureDir, series string, id int) {
	t.Helper()
	for _, n := range seriesNames(t, d, series) {
		if strings.HasPrefix(n, formatID(id)+"-") || strings.HasPrefix(n, quarantinePrefix) {
			t.Errorf("%s/%s is left after %s was evicted", series, n, formatID(id))
		}
	}
}

func TestRetentionKeepsPerSeriesNewestFirst(t *testing.T) {
	s, d, _, logs := retentionServer(t, 2)
	for i := range 5 {
		commitAged(t, s, d, aged{series: "a", age: time.Duration(5-i) * time.Hour})
	}
	commitAged(t, s, d, aged{series: "b", age: 9 * time.Hour})
	// A kept capture is not one of the unkept ones the series keeps.
	for range 3 {
		commitAged(t, s, d, aged{series: "k", label: "cand"})
	}
	commitAged(t, s, d, aged{series: "k", label: "base", keep: true})
	ev := retainNow(t, s, d, "")
	want := []string{"a/001-run", "a/002-run", "a/003-run", "k/001-cand"}
	if !slices.Equal(ev, want) {
		t.Fatalf("evicted %q, want %q", ev, want)
	}
	for _, id := range []int{1, 2, 3} {
		noneOf(t, d, "a", id)
	}
	noneOf(t, d, "k", 1)
	for _, c := range []struct {
		series, label string
		id            int
	}{{"a", "run", 4}, {"a", "run", 5}, {"b", "run", 1}, {"k", "cand", 2}, {"k", "cand", 3}, {"k", "base", 4}} {
		if !hasManifest(t, d, c.series, c.id, c.label) {
			t.Errorf("%s/%s-%s was evicted", c.series, formatID(c.id), c.label)
		}
	}
	invariant(t, d, nil)
	for _, e := range want {
		if !strings.Contains(logs.String(), e) {
			t.Errorf("eviction of %s not logged:\n%s", e, logs.String())
		}
	}
	// A second pass finds nothing more to do.
	if ev := retainNow(t, s, d, ""); len(ev) != 0 {
		t.Errorf("second pass evicted %q", ev)
	}
	// A manifest of series a copied into series b is not b's capture:
	// retention leaves it, though none of the files it names is in b.
	src := filepath.Join(d.root.Name(), "a", manifestName(5, "run"))
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(d.root.Name(), "b", manifestName(5, "run"))
	mustWrite(t, stray, b)
	if ev := retainNow(t, s, d, ""); len(ev) != 0 {
		t.Errorf("evicted %q", ev)
	}
	if _, err := os.Lstat(stray); err != nil {
		t.Errorf("the copied manifest went: %v", err)
	}
}

// TestRetentionKeepsTheNewestThatRemain: a capture an earlier step of the
// pass removed is not one of the series' newest it keeps.
func TestRetentionKeepsTheNewestThatRemain(t *testing.T) {
	s, d, _, _ := retentionServer(t, 2)
	s.lim.Capture.TTL = time.Hour
	commitAged(t, s, d, aged{age: 10 * time.Minute})
	commitAged(t, s, d, aged{age: 2 * time.Hour}) // the clock moved back meanwhile
	commitAged(t, s, d, aged{age: 5 * time.Minute})
	if ev := retainNow(t, s, d, ""); !slices.Equal(ev, []string{"adhoc/002-run"}) {
		t.Fatalf("evicted %q", ev)
	}
}

func TestRetentionByStoredBytes(t *testing.T) {
	const reserve = 8 << 10
	setup := func(t *testing.T) (*Server, *captureDir, map[string]int64) {
		s, d, _, _ := retentionServer(t, 100)
		s.lim.Capture.MaxProfileBytes, s.lim.Capture.MaxExeclogBytes = reserve/2, reserve/2
		// Series old was written least recently; its first capture is kept.
		commitAged(t, s, d, aged{series: "old", label: "base", keep: true, age: 9 * time.Hour})
		commitAged(t, s, d, aged{series: "old", age: 3 * time.Hour})
		commitAged(t, s, d, aged{series: "new", age: 2 * time.Hour})
		commitAged(t, s, d, aged{series: "old", age: 90 * time.Minute})
		commitAged(t, s, d, aged{series: "new", age: 10 * time.Minute})
		sizes := map[string]int64{}
		for _, c := range []string{"old/002-run", "old/003-run", "new/001-run", "new/002-run"} {
			series, capName, _ := strings.Cut(c, "/")
			for _, n := range seriesNames(t, d, series) {
				if strings.HasPrefix(n, capName) {
					fi, err := os.Lstat(filepath.Join(d.root.Name(), series, n))
					if err != nil {
						t.Fatal(err)
					}
					sizes[c] += fi.Size()
				}
			}
		}
		return s, d, sizes
	}
	used := func(t *testing.T, d *captureDir) int64 {
		n, err := dirBytes(d.root)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	t.Run("one byte over", func(t *testing.T) {
		s, d, _ := setup(t)
		s.lim.Capture.MaxStoredBytes = used(t, d) + reserve - 1
		if s.storeHasRoom(used(t, d)) {
			t.Fatal("the store has room before retention")
		}
		if ev := retainNow(t, s, d, ""); !slices.Equal(ev, []string{"old/002-run"}) {
			t.Fatalf("evicted %q", ev)
		}
		if !s.storeHasRoom(used(t, d)) {
			t.Error("no room after retention")
		}
		invariant(t, d, nil)
	})
	t.Run("past the least recently written series", func(t *testing.T) {
		s, d, sizes := setup(t)
		s.lim.Capture.MaxStoredBytes = used(t, d) + reserve - sizes["old/002-run"] - sizes["old/003-run"] - 1
		want := []string{"old/002-run", "old/003-run", "new/001-run"}
		if ev := retainNow(t, s, d, ""); !slices.Equal(ev, want) {
			t.Fatalf("evicted %q, want %q", ev, want)
		}
		if !hasManifest(t, d, "old", 1, "base") || !hasManifest(t, d, "new", 2, "run") {
			t.Error("the kept capture or the newest one went")
		}
		invariant(t, d, nil)
	})
	t.Run("not enough room is no error", func(t *testing.T) {
		s, d, _ := setup(t)
		s.lim.Capture.MaxStoredBytes = reserve
		ev := retainNow(t, s, d, "new/002")
		if want := []string{"old/002-run", "old/003-run", "new/001-run"}; !slices.Equal(ev, want) {
			t.Fatalf("evicted %q, want %q", ev, want)
		}
		if s.storeHasRoom(used(t, d)) {
			t.Error("room although the kept capture and the baseline stay")
		}
		invariant(t, d, nil)
	})
}

func TestDirBytes(t *testing.T) {
	_, d, _ := newCaptureServer(t)
	base, err := dirBytes(d.root)
	if err != nil {
		t.Fatal(err)
	}
	dir := d.root.Name()
	if err := os.MkdirAll(filepath.Join(dir, "x", "y"), 0o700); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "x", "a"), make([]byte, 100))
	mustWrite(t, filepath.Join(dir, "x", "y", "b"), make([]byte, 50))
	// A second name of a file takes no more room, and a symbolic link is
	// not followed.
	if err := os.Link(filepath.Join(dir, "x", "a"), filepath.Join(dir, "x", "a2")); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(filepath.Dir(dir), "big")
	mustWrite(t, big, make([]byte, 1<<20))
	if err := os.Symlink(big, filepath.Join(dir, "x", "big")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(dir), filepath.Join(dir, "x", "up")); err != nil {
		t.Fatal(err)
	}
	got, err := dirBytes(d.root)
	if err != nil {
		t.Fatal(err)
	}
	if got-base != 150 {
		t.Errorf("dirBytes grew by %d, want 150", got-base)
	}
}

func TestRetentionTTL(t *testing.T) {
	setup := func(t *testing.T, ttl time.Duration) (*Server, *captureDir) {
		s, d, _, _ := retentionServer(t, 100)
		s.lim.Capture.TTL = ttl
		commitAged(t, s, d, aged{label: "base", keep: true, age: 48 * time.Hour})
		commitAged(t, s, d, aged{age: 2 * time.Hour})
		commitAged(t, s, d, aged{age: 30 * time.Minute})
		// A failure manifest records no start: its age is the manifest's.
		old := commitAged(t, s, d, aged{failedAt: faultAfterSeq})
		commitAged(t, s, d, aged{failedAt: faultAfterSeq})
		then := time.Now().Add(-3 * time.Hour)
		if err := os.Chtimes(filepath.Join(d.root.Name(), "adhoc", manifestName(old, "run")), then, then); err != nil {
			t.Fatal(err)
		}
		return s, d
	}
	t.Run("by start time", func(t *testing.T) {
		s, d := setup(t, time.Hour)
		if ev := retainNow(t, s, d, ""); !slices.Equal(ev, []string{"adhoc/002-run", "adhoc/004-run"}) {
			t.Fatalf("evicted %q", ev)
		}
		invariant(t, d, nil)
	})
	t.Run("0 disables it", func(t *testing.T) {
		s, d := setup(t, 0)
		if ev := retainNow(t, s, d, ""); len(ev) != 0 {
			t.Fatalf("evicted %q", ev)
		}
	})
}

// TestRetentionNeverDeletesProtected: past every limit, retention still
// keeps a kept capture, the series' newest compare_with, the call's
// baseline, a capture with a file open in the store, a capture an intent
// names, and one whose file the captureProtect hook names.
func TestRetentionNeverDeletesProtected(t *testing.T) {
	s, d, _, _ := retentionServer(t, 1)
	s.lim.Capture.TTL = time.Minute
	s.lim.Capture.MaxStoredBytes = s.lim.Capture.MaxProfileBytes + s.lim.Capture.MaxExeclogBytes
	s.ownerAlive = func(intent) bool { return true }
	old := time.Hour
	commitAged(t, s, d, aged{label: "base", keep: true, age: old})                                                    // 001 kept
	commitAged(t, s, d, aged{label: "cmp", age: old})                                                                 // 002 the newest compare_with
	commitAged(t, s, d, aged{label: "call", age: old})                                                                // 003 the call's baseline
	commitAged(t, s, d, aged{label: "open", age: old, content: map[string][]byte{"cpu.pb.gz": pmfixture.CPUBasic()}}) // 004 open
	commitAged(t, s, d, aged{label: "hook", age: old})                                                                // 005 captureProtect
	commitAged(t, s, d, aged{label: "plain", age: old, compare: "001"})                                               // 006
	commitAged(t, s, d, aged{label: "cand", age: old, compare: "002"})                                                // 007
	commitAged(t, s, d, aged{label: "newest", age: old})                                                              // 008
	// 009: committed, but its cleanup crashed, so its intent is still
	// there (the next recovery would finish it; retention runs first here).
	s.captureFault = inject(shot{faultAfterManifestLink, 1, errCrash})
	r := drive(t, s, d, capCase{label: "intent", meta: &manifest{Start: s.clock().Add(-old)}})
	s.captureFault = nil
	if r.id != 9 || !hasManifest(t, d, "adhoc", 9, "intent") {
		t.Fatalf("capture 009: %+v", r)
	}

	if _, err := s.store.open(filepath.Join(d.rel, "adhoc", finalName(4, "open", "cpu.pb.gz")), ""); err != nil {
		t.Fatal(err)
	}
	hook := manifestFor(t, d, "adhoc", 5, "hook").Files[0]
	s.captureProtect = func() []fileKey { return []fileKey{{Dev: hook.Dev, Ino: hook.Ino}} }

	p := s.protected(d, "adhoc/003")
	for _, k := range []string{"adhoc/001", "adhoc/002", "adhoc/003", "adhoc/009"} {
		if !p.captures[k] {
			t.Errorf("%s is not protected: %v", k, p.captures)
		}
	}
	// A rollback this server could not finish yet protects its capture.
	s.capMu.Lock()
	s.stranded = append(s.stranded, &captureTxn{series: "adhoc", id: 6})
	s.capMu.Unlock()
	if !s.protected(d, "").captures["adhoc/006"] {
		t.Error("a stranded capture is not protected")
	}
	s.capMu.Lock()
	s.stranded = nil
	s.capMu.Unlock()
	open := manifestFor(t, d, "adhoc", 4, "open").Files[0]
	for _, k := range []fileKey{{open.Dev, open.Ino}, {hook.Dev, hook.Ino}} {
		if !p.files[k] {
			t.Errorf("file %v is not protected: %v", k, p.files)
		}
	}

	want := []string{"adhoc/006-plain", "adhoc/007-cand", "adhoc/008-newest"}
	if ev := retainNow(t, s, d, "adhoc/003"); !slices.Equal(ev, want) {
		t.Fatalf("evicted %q, want %q", ev, want)
	}
	for id, label := range map[int]string{1: "base", 2: "cmp", 3: "call", 4: "open", 5: "hook", 9: "intent"} {
		if !hasManifest(t, d, "adhoc", id, label) {
			t.Errorf("protected capture %03d-%s was evicted", id, label)
		}
	}
	for id, label := range map[int]string{1: "base", 2: "cmp", 3: "call", 4: "open", 5: "hook", 9: "intent"} {
		ser, _ := d.root.OpenRoot("adhoc")
		if err := verifyFiles(ser, manifestFor(t, d, "adhoc", id, label)); err != nil {
			t.Errorf("protected capture %03d-%s lost a file: %v", id, label, err)
		}
		_ = ser.Close()
	}

	// Without the hook, the baseline and the open handle, those go too, and
	// so does 002, which no capture compares with any more.
	s.captureProtect = nil
	s.store = newStore(s.profiles, s.lim.Model, s.lim.MaxOpenHandles, newPins())
	if ev := retainNow(t, s, d, ""); !slices.Equal(ev, []string{"adhoc/002-cmp", "adhoc/003-call", "adhoc/004-open", "adhoc/005-hook"}) {
		t.Fatalf("evicted %q", ev)
	}
}

// replaceFile is one way a file a manifest records stops being that file.
type replaceFile struct {
	name string
	do   func(t *testing.T, path string)
	// late changes the file between the check and the move aside.
	late bool
}

var replacements = []replaceFile{
	{name: "replaced by another file with the same bytes", do: func(t *testing.T, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, path+".new", b)
		if err := os.Rename(path+".new", path); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "rewritten in place", do: rewriteInPlace},
	{name: "hard-linked elsewhere", do: func(t *testing.T, path string) {
		if err := os.Link(path, filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "mine")); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "a symbolic link", do: func(t *testing.T, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(path))), "theirs")
		mustWrite(t, other, b)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, path); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "rewritten after its check", do: rewriteInPlace, late: true},
}

// rewriteInPlace changes the file's first byte, keeping its inode and size.
func rewriteInPlace(t *testing.T, path string) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("U"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRetentionSparesAReplacedFile: a capture whose file is not the one
// its manifest records any more keeps that file byte for byte, and its
// manifest; retention notes it and goes on with the next capture.
func TestRetentionSparesAReplacedFile(t *testing.T) {
	modes := []capture.Mode{capture.ModeCPU, capture.ModeGPU}
	for _, rp := range replacements {
		t.Run(rp.name, func(t *testing.T) {
			s, d, _, logs := retentionServer(t, 1)
			// Both the TTL and the keep count select 001; it is judged,
			// and noted, once.
			s.lim.Capture.TTL = time.Hour
			commitAged(t, s, d, aged{modes: modes, age: 2 * time.Hour})
			commitAged(t, s, d, aged{modes: modes})
			commitAged(t, s, d, aged{modes: modes})
			target := finalName(1, "run", "gpu.execlog")
			path := filepath.Join(d.root.Name(), "adhoc", target)
			var before []byte
			change := func() {
				rp.do(t, path)
				var err error
				if before, err = os.ReadFile(path); err != nil {
					t.Fatal(err)
				}
			}
			if rp.late {
				quarantineHook = func(_ *os.Root, name string) {
					if name == target {
						change()
					}
				}
				defer func() { quarantineHook = nil }()
			} else {
				change()
			}
			ev := retainNow(t, s, d, "")
			quarantineHook = nil
			if !slices.Equal(ev, []string{"adhoc/002-run"}) {
				t.Fatalf("evicted %q", ev)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("the replaced file changed: %v", err)
			}
			if !rp.late {
				for _, o := range []string{"cpu.pb.gz", "gpu.pb.gz"} {
					if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", finalName(1, "run", o))); err != nil {
						t.Errorf("%s of a capture with a replaced file went: %v", o, err)
					}
				}
			}
			if !hasManifest(t, d, "adhoc", 1, "run") {
				t.Fatal("the manifest of a capture with a replaced file went")
			}
			for _, n := range seriesNames(t, d, "adhoc") {
				if strings.HasPrefix(n, quarantinePrefix) {
					t.Errorf("%s left aside", n)
				}
			}
			if n := strings.Count(logs.String(), "left adhoc/"+target); n != 1 {
				t.Errorf("the spared file is noted %d times:\n%s", n, logs.String())
			}
			// The next pass spares it again.
			if ev := retainNow(t, s, d, ""); len(ev) != 0 {
				t.Errorf("second pass evicted %q", ev)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Error("the second pass changed the replaced file")
			}
		})
	}
}

// evictionOps records, in a clean eviction of one three-file capture,
// every named write and fault point retention reaches.
func evictionOps(t *testing.T) (ops map[string]int, points map[faultPoint]int, moves int) {
	t.Helper()
	s, d, _ := partialSetup(t)
	ops, points = map[string]int{}, map[faultPoint]int{}
	var seq []string
	s.writeErr = func(op string) error { ops[op]++; seq = append(seq, op); return nil }
	s.captureFault = func(p faultPoint) error { points[p]++; return nil }
	quarantineAfterHook = func() error { moves++; return nil }
	defer func() { quarantineAfterHook = nil }()
	if ev := retainNow(t, s, d, ""); !slices.Equal(ev, []string{"adhoc/001-run"}) {
		t.Fatalf("evicted %q", ev)
	}
	// The files' removals are durable before the manifest goes.
	rm, sync := "retention remove", "fsync retention"
	if want := []string{rm, rm, rm, sync, rm, sync}; !slices.Equal(seq, want) {
		t.Fatalf("writes %q, want %q", seq, want)
	}
	return ops, points, moves
}

// partialSetup is a series whose first capture (three files) retention
// evicts.
func partialSetup(t *testing.T) (*Server, *captureDir, *testClock) {
	t.Helper()
	s, d, clk, _ := retentionServer(t, 1)
	modes := []capture.Mode{capture.ModeCPU, capture.ModeGPU}
	commitAged(t, s, d, aged{modes: modes})
	commitAged(t, s, d, aged{})
	return s, d, clk
}

// evictionState is the invariant oracle for a state an interrupted
// eviction left: as invariant, except that a committed capture may have
// lost some of its files (never a file that is not the one recorded).
func evictionState(t *testing.T, d *captureDir, notes []string) {
	t.Helper()
	ser, err := d.root.OpenRoot("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	ms, bad, err := seriesManifests(ser)
	if err != nil || len(bad) != 0 {
		t.Fatalf("manifests: %v %v", bad, err)
	}
	owned := map[string]bool{}
	for _, m := range ms {
		for _, f := range m.Files {
			owned[f.Name] = true
			if why, err := matches(ser, f.Name, f); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil || why != "" {
				t.Errorf("%s: %s", f.Name, cmpOrErr(why, err))
			}
		}
	}
	for _, n := range dirNames(t, ser, ".") {
		_, _, kind, ok := parseName(n)
		switch {
		case n == seqFile, owned[n], ok && kind == kindManifest:
		case strings.HasPrefix(n, "."):
			if !noted(notes, "orphan_temp", n) {
				t.Errorf("%s: a leftover nothing noted (notes %q)", n, notes)
			}
		default:
			t.Errorf("%s: no manifest owns it", n)
		}
	}
}

// TestRetentionFinishesAPartialEviction: an eviction that stops part-way
// (a crash, or EIO/ENOSPC, at any step) leaves the manifest until the
// capture's files are gone, so the state stays valid, and the next pass
// finishes it. Only a crash between moving the manifest itself aside and
// unlinking it leaves a quarantined name, which recovery reports.
func TestRetentionFinishesAPartialEviction(t *testing.T) {
	ops, points, moves := evictionOps(t)
	if moves != 4 || ops["retention remove"] != 4 || points[faultRetentionHash] < 4 {
		t.Fatalf("a clean eviction: %d moves, ops %v, points %v", moves, ops, points)
	}
	check := func(t *testing.T, s *Server, d *captureDir, clk *testClock, err error, crash bool) {
		t.Helper()
		var notes []string
		if crash {
			if !errors.Is(err, errCrash) {
				t.Fatalf("no crash: %v", err)
			}
			notes = crashRecover(t, s, d, clk)
		} else if err == nil {
			t.Fatal("no error")
		}
		evictionState(t, d, notes)
		// Whether the pass had begun to remove 001: a name of it gone or
		// moved aside.
		touched := !hasManifest(t, d, "adhoc", 1, "run")
		for _, n := range seriesNames(t, d, "adhoc") {
			touched = touched || strings.HasPrefix(n, quarantinePrefix)
		}
		for _, o := range []string{"cpu.pb.gz", "gpu.pb.gz", "gpu.execlog"} {
			_, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", finalName(1, "run", o)))
			touched = touched || errors.Is(err, fs.ErrNotExist)
		}
		s.writeErr, s.captureFault = nil, nil
		quarantineAfterHook = nil
		// No limit selects 001 now: only an eviction already begun is
		// finished.
		s.lim.Capture.KeepPerSeries = 100
		retainNow(t, s, d, "")
		if crash {
			notes = crashRecover(t, s, d, clk)
		}
		if hasManifest(t, d, "adhoc", 1, "run") == touched {
			t.Fatalf("eviction begun %v, but the manifest is there %v after the next pass", touched, !touched)
		}
		invariant(t, d, notes)
		for _, n := range seriesNames(t, d, "adhoc") {
			if strings.HasPrefix(n, quarantinePrefix) && !noted(notes, "orphan_temp", n) {
				t.Errorf("%s left aside and not noted", n)
			}
		}
		if r := drive(t, s, d, capCase{}); r.id != 3 {
			t.Errorf("the next capture got id %d, want 3", r.id)
		}
	}
	for n := 1; n <= moves; n++ {
		t.Run(fmt.Sprintf("crash after move %d", n), func(t *testing.T) {
			s, d, clk := partialSetup(t)
			defer quarantineCrash(n)()
			_, err := retainOnce(t, s, d, "")
			check(t, s, d, clk, err, true)
			if n < moves {
				for _, name := range seriesNames(t, d, "adhoc") {
					if strings.HasPrefix(name, quarantinePrefix) {
						t.Errorf("%s: a capture file moved aside was not finished", name)
					}
				}
			}
		})
	}
	for op, count := range ops {
		for n := 1; n <= count; n++ {
			for _, e := range []error{errCrash, syscall.EIO, syscall.ENOSPC} {
				t.Run(fmt.Sprintf("%s %d %v", op, n, e), func(t *testing.T) {
					s, d, clk := partialSetup(t)
					s.writeErr = failOpAt(op, n, e)
					_, err := retainOnce(t, s, d, "")
					check(t, s, d, clk, err, errors.Is(e, errCrash))
				})
			}
		}
	}
	for p, count := range points {
		for n := 1; n <= count; n++ {
			for _, e := range []error{errCrash, errInjected} {
				t.Run(fmt.Sprintf("%v %d %v", p, n, e), func(t *testing.T) {
					s, d, clk := partialSetup(t)
					s.captureFault = inject(shot{p, n, e})
					_, err := retainOnce(t, s, d, "")
					check(t, s, d, clk, err, errors.Is(e, errCrash))
				})
			}
		}
	}
}

func TestRetentionDoesNotReuseIDs(t *testing.T) {
	s, d, _, _ := retentionServer(t, 1)
	s.lim.Capture.TTL = time.Hour
	for range 3 {
		commitAged(t, s, d, aged{age: 2 * time.Hour})
	}
	ser, err := d.root.OpenRoot("adhoc")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ser.Close() }()
	if got := seqNext(t, ser); got != "4" {
		t.Fatalf(".seq = %q", got)
	}
	if ev := retainNow(t, s, d, ""); len(ev) != 3 {
		t.Fatalf("evicted %q", ev)
	}
	if names := seriesNames(t, d, "adhoc"); !slices.Equal(names, []string{seqFile}) {
		t.Fatalf("series holds %v", names)
	}
	if got := seqNext(t, ser); got != "4" {
		t.Errorf(".seq = %q after retention", got)
	}
	if r := drive(t, s, d, capCase{}); r.id != 4 || !r.committed {
		t.Errorf("the next capture: id %d, committed %v, err %v", r.id, r.committed, r.err)
	}
}

// TestFailedManifestsAreRetainedLikeCaptures: failed and interrupted
// manifests count toward a series' keep count and the TTL like captures,
// and are evicted by the identity of the manifest.
func TestFailedManifestsAreRetainedLikeCaptures(t *testing.T) {
	s, d, clk, logs := retentionServer(t, 3)
	commitAged(t, s, d, aged{})                                  // 001 ok
	commitAged(t, s, d, aged{failedAt: faultAfterSeq})           // 002 failed, no intent
	commitAged(t, s, d, aged{failedAt: faultAfterStaging})       // 003 failed, rolled back
	s.captureFault = inject(shot{faultAfterIntent, 1, errCrash}) // 004 interrupted
	drive(t, s, d, capCase{})
	s.captureFault = nil
	crashRecover(t, s, d, clk)
	if m := manifestFor(t, d, "adhoc", 4, "run"); m.Outcome != captureInterrupted {
		t.Fatalf("004 is %s", m.Outcome)
	}
	commitAged(t, s, d, aged{}) // 005 ok

	if ev := retainNow(t, s, d, ""); !slices.Equal(ev, []string{"adhoc/001-run", "adhoc/002-run"}) {
		t.Fatalf("evicted %q", ev)
	}
	invariant(t, d, nil)

	// 003's manifest is three hours old by its own time.
	s.lim.Capture.TTL = time.Hour
	then := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(d.root.Name(), "adhoc", manifestName(3, "run")), then, then); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(d.root.Name(), "adhoc", manifestName(4, "run")), then, then); err != nil {
		t.Fatal(err)
	}
	// 004's manifest is rewritten after retention read it: it is not the
	// manifest retention judged, so it stays.
	target := manifestName(4, "run")
	quarantineHook = func(_ *os.Root, name string) {
		if name == target {
			rewriteInPlace(t, filepath.Join(d.root.Name(), "adhoc", target))
		}
	}
	ev := retainNow(t, s, d, "")
	quarantineHook = nil
	if !slices.Equal(ev, []string{"adhoc/003-run"}) {
		t.Fatalf("evicted %q", ev)
	}
	if _, err := os.Lstat(filepath.Join(d.root.Name(), "adhoc", target)); err != nil {
		t.Fatalf("the rewritten manifest went: %v", err)
	}
	if !strings.Contains(logs.String(), "left adhoc/"+target) {
		t.Errorf("the rewritten manifest is not noted:\n%s", logs.String())
	}
}

// TestRetentionRemovesNothingWhenItCannotTellWhatToKeep: a series that
// cannot be read may hold an intent or a baseline; retention then removes
// nothing anywhere.
func TestRetentionRemovesNothingWhenItCannotTellWhatToKeep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory of mode 0")
	}
	s, d, _, _ := retentionServer(t, 1)
	for range 3 {
		commitAged(t, s, d, aged{series: "a"})
	}
	commitAged(t, s, d, aged{series: "b"})
	b := filepath.Join(d.root.Name(), "b")
	if err := os.Chmod(b, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(b, 0o700) }()
	ev, err := retainOnce(t, s, d, "")
	if err == nil || len(ev) != 0 || captureErr(t, err).Code != capture.CodeDisk {
		t.Fatalf("evicted %q, err %v", ev, err)
	}
	for id := 1; id <= 3; id++ {
		if !hasManifest(t, d, "a", id, "run") {
			t.Errorf("a/%s was evicted", formatID(id))
		}
	}
}
