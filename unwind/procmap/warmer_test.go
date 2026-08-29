package procmap

import (
	"os"
	"path/filepath"
	"testing"
)

const oneMapping = "00400000-00401000 r-xp 00000000 fd:01 111 /usr/bin/shortlived\n"

func writePID(t *testing.T, root string, pid int, maps string) string {
	t.Helper()
	dir := filepath.Join(root, itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "maps"), []byte(maps), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// The point of the whole thing: a process that exits during the capture is
// still resolvable afterwards, because the sweep read its maps while it was
// alive. Issue #56.
func TestASweptPIDStaysResolvableAfterItExits(t *testing.T) {
	tmp := t.TempDir()
	dir := writePID(t, tmp, 4321, oneMapping)

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()
	w := NewWarmer(r, 0)

	seen, warmed := w.Sweep()
	if seen != 1 || warmed != 1 {
		t.Fatalf("sweep: seen=%d warmed=%d, want 1 and 1", seen, warmed)
	}

	if err := os.RemoveAll(dir); err != nil { // the process exits
		t.Fatal(err)
	}

	m, ok := r.Lookup(4321, 0x400500)
	if !ok {
		t.Fatal("a swept PID must stay resolvable after it exits")
	}
	if m.Path != "/usr/bin/shortlived" {
		t.Fatalf("got %q", m.Path)
	}
}

// The control for the test above. Without the sweep the same PID resolves to
// nothing — so that test is measuring the sweep and not the fixture.
func TestWithoutASweepTheSamePIDIsLost(t *testing.T) {
	tmp := t.TempDir()
	dir := writePID(t, tmp, 4321, oneMapping)

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup(4321, 0x400500); ok {
		t.Fatal("expected the unswept PID to be unresolvable")
	}
}

// Repeat sweeps must not re-read /proc for processes already known. This is
// what makes a timer affordable: the cost is one read per NEW process, not one
// read per process per sweep.
func TestRepeatSweepsDoNotRereadKnownPIDs(t *testing.T) {
	tmp := t.TempDir()
	writePID(t, tmp, 4321, oneMapping)

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()
	w := NewWarmer(r, 0)

	w.Sweep()
	after := r.populateCountForTest(4321)
	for range 5 {
		w.Sweep()
	}
	if got := r.populateCountForTest(4321); got != after {
		t.Fatalf("populate count moved on repeat sweeps: %d -> %d", after, got)
	}

	sweeps, seen, warmed := w.Stats()
	if sweeps != 6 {
		t.Fatalf("sweeps=%d, want 6", sweeps)
	}
	if seen != 6 {
		t.Fatalf("pidsSeen=%d, want 6 (one PID, six sweeps)", seen)
	}
	if warmed != 1 {
		t.Fatalf("pidsWarmed=%d, want 1 — only the first sweep reads", warmed)
	}
}

// Non-numeric entries under /proc (self, sys, net, ...) are not PIDs and must
// not be offered to the cache.
func TestSweepIgnoresNonPIDEntries(t *testing.T) {
	tmp := t.TempDir()
	writePID(t, tmp, 4321, oneMapping)
	for _, name := range []string{"self", "sys", "net", "thread-self"} {
		if err := os.MkdirAll(filepath.Join(tmp, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()

	seen, warmed := NewWarmer(r, 0).Sweep()
	if seen != 1 || warmed != 1 {
		t.Fatalf("seen=%d warmed=%d, want 1 and 1", seen, warmed)
	}
}

// Refresh replaces a warmed snapshot when the process is alive and its maps
// have changed. Without this a long-lived process would be stuck with whatever
// the first sweep saw, and a library it loaded later would never resolve —
// which would make the #56 fix a regression for everything that dlopens.
func TestRefreshReplacesAWarmedSnapshotWhileTheProcessLives(t *testing.T) {
	tmp := t.TempDir()
	writePID(t, tmp, 4321, oneMapping)

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()
	NewWarmer(r, 0).Sweep()

	// The process dlopens something: a second executable mapping appears.
	const grown = oneMapping +
		"00500000-00501000 r-xp 00000000 fd:01 222 /usr/lib/late.so\n"
	if err := os.WriteFile(filepath.Join(tmp, "4321", "maps"), []byte(grown), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok := r.Lookup(4321, 0x500500); ok {
		t.Fatal("the warmed snapshot should predate the new mapping")
	}
	if !r.Refresh(4321) {
		t.Fatal("Refresh should succeed for a live process")
	}
	m, ok := r.Lookup(4321, 0x500500)
	if !ok || m.Path != "/usr/lib/late.so" {
		t.Fatalf("after Refresh: ok=%v m=%+v, want /usr/lib/late.so", ok, m)
	}
}

// And the half that makes Refresh worth having over Invalidate: when the
// process is gone, the warmed snapshot SURVIVES. Invalidate-then-Lookup would
// drop it and resolve nothing, reintroducing #56 in a window narrow enough to
// be mistaken for something else.
func TestRefreshKeepsTheWarmedSnapshotWhenTheProcessIsGone(t *testing.T) {
	tmp := t.TempDir()
	dir := writePID(t, tmp, 4321, oneMapping)

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()
	NewWarmer(r, 0).Sweep()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if r.Refresh(4321) {
		t.Fatal("Refresh must report failure for a process that is gone")
	}
	m, ok := r.Lookup(4321, 0x400500)
	if !ok || m.Path != "/usr/bin/shortlived" {
		t.Fatalf("warmed snapshot lost: ok=%v m=%+v", ok, m)
	}
}

// Start/Stop is the timer wrapper around Sweep and nothing more. The first
// sweep happens on Start rather than one interval later, so a capture shorter
// than the interval still warms.
func TestStartSweepsImmediatelyAndStopIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	writePID(t, tmp, 4321, oneMapping)

	r := NewResolver(WithProcRoot(tmp))
	defer r.Close()
	w := NewWarmer(r, 0)

	w.Stop() // before Start: must not panic or block
	w.Start()
	w.Start() // idempotent
	sweeps, _, warmed := w.Stats()
	if sweeps < 1 || warmed != 1 {
		t.Fatalf("Start did not sweep immediately: sweeps=%d warmed=%d", sweeps, warmed)
	}
	w.Stop()
	w.Stop() // idempotent
}
