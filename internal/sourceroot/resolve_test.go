package sourceroot

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const appC = "line 1\nline 2\nline 3\nline 4\nline 5\nline 6\n"

// writeFile creates dir/rel with content, making parent directories.
func writeFile(t *testing.T, dir, rel, content string) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// sourceTree is a root holding src/app.c, src/util.c and other/src/app.c.
func sourceTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, "src/app.c", appC)
	writeFile(t, dir, "src/util.c", "int util(void) { return 0; }\n")
	writeFile(t, dir, "other/src/app.c", "other\n")
	return dir
}

func openRoots(t *testing.T, dirs []string, manifests []Manifest, probe Prober, lim Limits) *Roots {
	t.Helper()
	r, err := Open(dirs, manifests, probe, lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestResolveTakesTheLongestSuffix(t *testing.T) {
	dir := sourceTree(t)
	r := openRoots(t, []string{dir, t.TempDir()}, nil, nil, DefaultLimits())
	res := r.Resolve("/build/app/src/util.c", "", "")
	if res.Status != StatusSourceUnverified || res.Root != dir || res.Path != "src/util.c" {
		t.Fatalf("Resolve = %+v, want src/util.c in %s", res, dir)
	}
	// "src/app.c" matches at the "src/app.c" length in this root only;
	// other/src/app.c would need the shorter "app.c".
	res = r.Resolve("/build/app/src/app.c", "", "")
	if res.Path != "src/app.c" || len(res.Candidates) != 0 {
		t.Fatalf("Resolve(app.c) = %+v, want src/app.c and no candidates", res)
	}
	if res.SHA256 != sha256Hex(appC) {
		t.Fatalf("SHA256 = %s, want %s", res.SHA256, sha256Hex(appC))
	}
}

func TestResolveIsAmbiguousAcrossRoots(t *testing.T) {
	a, b := sourceTree(t), sourceTree(t)
	r := openRoots(t, []string{a, b}, nil, nil, DefaultLimits())
	res := r.Resolve("/build/app/src/app.c", "", "")
	want := []string{filepath.Join(a, "src", "app.c"), filepath.Join(b, "src", "app.c")}
	if res.Status != StatusAmbiguous || !slices.Equal(res.Candidates, want) || res.Path != "" {
		t.Fatalf("Resolve = %+v, want ambiguous between %v", res, want)
	}
	if _, err := r.Lines(res, 1, 0); err == nil {
		t.Fatal("Lines of an ambiguous resolution: want an error")
	}
	if n := len(r.Opened()); n != 0 {
		t.Fatalf("Opened() has %d files after an ambiguous resolution, want 0", n)
	}
}

func TestResolveDedupesOneFileSeenThroughTwoRoots(t *testing.T) {
	dir := sourceTree(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	r := openRoots(t, []string{dir, link}, nil, nil, DefaultLimits())
	if res := r.Resolve("src/util.c", "", ""); res.Status != StatusSourceUnverified || res.Root != dir {
		t.Fatalf("Resolve(src/util.c) = %+v, want one file in %s", res, dir)
	}
	if res := r.Resolve("src/app.c", "", ""); res.Status != StatusSourceUnverified {
		t.Fatalf("Resolve(src/app.c) = %+v, want one file", res)
	}
}

func TestResolveUnresolved(t *testing.T) {
	r := openRoots(t, []string{sourceTree(t)}, nil, nil, DefaultLimits())
	for _, name := range []string{"/nowhere/x.c", "", "/", "..", "/build/src"} {
		if res := r.Resolve(name, "", ""); res.Status != StatusUnresolved || res.Reason == "" {
			t.Errorf("Resolve(%q) = %+v, want unresolved with a reason", name, res)
		}
	}
}

func TestResolveCannotEscapeARoot(t *testing.T) {
	outside := t.TempDir()
	secret := writeFile(t, outside, "secret.c", "secret\n")
	dir := sourceTree(t)
	if err := os.Symlink(secret, filepath.Join(dir, "src", "evil.c")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "src", "out")); err != nil {
		t.Fatal(err)
	}
	r := openRoots(t, []string{dir}, nil, nil, DefaultLimits())
	for _, name := range []string{"../../etc/passwd", "/build/src/evil.c", "/build/src/out/secret.c", "../" + filepath.Base(outside) + "/secret.c"} {
		if res := r.Resolve(name, "", ""); res.Status != StatusUnresolved {
			t.Errorf("Resolve(%q) = %+v, want unresolved", name, res)
		}
	}
	if res := r.ResolveBasename("secret.c", "", ""); res.Status != StatusUnresolved {
		t.Errorf("ResolveBasename(secret.c) = %+v, want unresolved", res)
	}
	if n := len(r.Opened()); n != 0 {
		t.Fatalf("Opened() = %d files, want nothing read", n)
	}
}

func TestResolveSkipsGitMetadata(t *testing.T) {
	dir := sourceTree(t)
	writeFile(t, dir, ".git/config", "[core]\n")
	r := openRoots(t, []string{dir}, nil, nil, DefaultLimits())
	if res := r.Resolve("/x/.git/config", "", ""); res.Status != StatusUnresolved {
		t.Fatalf("Resolve(.git/config) = %+v, want unresolved", res)
	}
	if res := r.ResolveBasename("config", "", ""); res.Status != StatusUnresolved {
		t.Fatalf("ResolveBasename(config) = %+v, want unresolved", res)
	}
}

func TestBasenameLookupIsAmbiguousNotAGuess(t *testing.T) {
	dir := sourceTree(t)
	r := openRoots(t, []string{dir}, nil, nil, DefaultLimits())
	res := r.ResolveBasename("app.c", "", "")
	want := []string{filepath.Join(dir, "other", "src", "app.c"), filepath.Join(dir, "src", "app.c")}
	if res.Status != StatusAmbiguous || !slices.Equal(res.Candidates, want) {
		t.Fatalf("ResolveBasename(app.c) = %+v, want ambiguous between %v", res, want)
	}
	res = r.ResolveBasename("util.c", "", "")
	if res.Status != StatusSourceUnverified || res.Path != "src/util.c" {
		t.Fatalf("ResolveBasename(util.c) = %+v, want src/util.c", res)
	}
	if res := r.ResolveBasename("nope.c", "", ""); res.Status != StatusUnresolved {
		t.Fatalf("ResolveBasename(nope.c) = %+v, want unresolved", res)
	}
}

func TestBasenameWalkLimit(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxWalkEntries = 1
	r := openRoots(t, []string{sourceTree(t)}, nil, nil, lim)
	for _, base := range []string{"util.c", "app.c"} {
		res := r.ResolveBasename(base, "", "")
		if res.Status != StatusUnresolved || res.Reason != "source walk limit reached" {
			t.Fatalf("ResolveBasename(%s) = %+v, want unresolved %q", base, res, "source walk limit reached")
		}
	}
}

func TestNoProberMeansUnverified(t *testing.T) {
	r := openRoots(t, []string{sourceTree(t)}, nil, nil, DefaultLimits())
	res := r.Resolve("src/app.c", "b1", "")
	if res.Status != StatusSourceUnverified || res.Reason != "git probe disabled" || res.Commit != "" {
		t.Fatalf("Resolve = %+v, want source_unverified %q", res, "git probe disabled")
	}
}

func TestLinesAreBoundedAndValidUTF8(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 2048)
	writeFile(t, dir, "f.c", "one\ntwo\r\nthree\nfour\n"+long+"\nbad \xff\xfe end\n€uro\n")
	r := openRoots(t, []string{dir}, nil, nil, DefaultLimits())
	res := r.Resolve("f.c", "", "")

	got, err := r.Lines(res, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{N: 2, Text: "two"}, {N: 3, Text: "three"}, {N: 4, Text: "four"}}
	if !slices.Equal(got, want) {
		t.Fatalf("Lines(3, 1) = %+v, want %+v", got, want)
	}

	got, err = r.Lines(res, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Truncated || len(got[0].Text) != 512 {
		t.Fatalf("2 KiB line: %d lines, truncated %v, %d bytes; want one truncated 512-byte line", len(got), got[0].Truncated, len(got[0].Text))
	}

	got, err = r.Lines(res, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text != "bad �� end" {
		t.Fatalf("invalid bytes: %q", got[0].Text)
	}

	// Clamped at both ends; the context is capped at MaxContext.
	got, err = r.Lines(res, 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 7 || got[0].N != 1 || got[6].N != 7 || got[6].Text != "€uro" {
		t.Fatalf("Lines(1, 1000) = %d lines (%+v ... %+v), want 1..7", len(got), got[0], got[len(got)-1])
	}
	for _, bad := range []int64{0, -1, 8} {
		if _, err := r.Lines(res, bad, 0); err == nil {
			t.Errorf("Lines(%d): want an error", bad)
		}
	}
}

func TestLinesCutOnACharacterBoundary(t *testing.T) {
	dir := t.TempDir()
	lim := DefaultLimits()
	lim.MaxLineBytes = 5
	writeFile(t, dir, "f.c", "ab€cd\n")
	r := openRoots(t, []string{dir}, nil, nil, lim)
	got, err := r.Lines(r.Resolve("f.c", "", ""), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text != "ab€" || !got[0].Truncated {
		t.Fatalf("Lines = %+v, want %q truncated", got, "ab€")
	}
}

func TestLinesComeFromTheSnapshot(t *testing.T) {
	dir := sourceTree(t)
	m := Manifest{Format: ManifestFormat, BuildIDs: []string{"b1"}, Files: map[string]string{"src/app.c": sha256Hex(appC)}}
	r := openRoots(t, []string{dir}, []Manifest{m}, nil, DefaultLimits())
	res := r.Resolve("/build/app/src/app.c", "b1", "")
	if res.Status != StatusBuildVerified {
		t.Fatalf("Resolve = %+v, want build_verified", res)
	}

	// Replace the verified file atomically with other bytes.
	tmp := writeFile(t, dir, "src/.app.c.new", "changed 1\nchanged 2\nchanged 3\n")
	if err := os.Rename(tmp, filepath.Join(dir, "src", "app.c")); err != nil {
		t.Fatal(err)
	}

	got, err := r.Lines(res, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "line 2" {
		t.Fatalf("Lines after replacement = %+v, want the verified snapshot's %q", got, "line 2")
	}
	if res.SHA256 != sha256Hex(appC) {
		t.Fatalf("resolution digest changed: %s", res.SHA256)
	}

	again := r.Resolve("/build/app/src/app.c", "b1", "")
	if again.Status != StatusSourceUnverified || again.Reason != "file differs from the source manifest" {
		t.Fatalf("Resolve after replacement = %+v, want the new bytes unverified", again)
	}
	got, err = r.Lines(again, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Text != "changed 2" {
		t.Fatalf("new Lines = %+v, want %q", got, "changed 2")
	}

	opened := r.Opened()
	if len(opened) != 2 || os.SameFile(opened[0], opened[1]) {
		t.Fatalf("Opened() = %d files, want the two distinct snapshots", len(opened))
	}
}

func TestOpenedReportsEachSnapshotOnce(t *testing.T) {
	dir := sourceTree(t)
	r := openRoots(t, []string{dir}, nil, nil, DefaultLimits())
	r.Resolve("src/app.c", "", "")
	r.Resolve("src/app.c", "", "")
	r.ResolveBasename("util.c", "", "")
	opened := r.Opened()
	if len(opened) != 2 {
		t.Fatalf("Opened() = %d files, want 2", len(opened))
	}
	fi, err := os.Stat(filepath.Join(dir, "src", "app.c"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(opened[0], fi) {
		t.Fatal("Opened()[0] is not src/app.c")
	}
}

func TestNonRegularFilesAreRefusedWithoutBlocking(t *testing.T) {
	dir := sourceTree(t)
	fifo := filepath.Join(dir, "src", "pipe.c")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	r := openRoots(t, []string{dir}, nil, nil, DefaultLimits())
	done := make(chan Resolution, 2)
	go func() {
		done <- r.Resolve("/b/src/pipe.c", "", "")
		done <- r.ResolveBasename("pipe.c", "", "")
	}()
	for range 2 {
		select {
		case res := <-done:
			if res.Status != StatusUnresolved {
				t.Fatalf("FIFO resolved as %+v, want unresolved", res)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("resolving a FIFO blocked")
		}
	}

	// The handle check refuses a FIFO even when it is swapped in after the
	// lookup: the open does not block and the type comes from the handle.
	errc := make(chan error, 1)
	go func() {
		_, err := r.snapshot(0, "src/pipe.c")
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("snapshot of a FIFO: err = %v, want not a regular file", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("opening a FIFO blocked")
	}
	if n := len(r.Opened()); n != 0 {
		t.Fatalf("Opened() = %d after refusing a FIFO, want 0", n)
	}
}

func TestFileOverTheSizeLimitIsUnverified(t *testing.T) {
	dir := sourceTree(t)
	lim := DefaultLimits()
	lim.MaxFileBytes = 10 // "line 1\nlin"
	m := Manifest{Format: ManifestFormat, BuildIDs: []string{"b1"}, Files: map[string]string{"src/app.c": sha256Hex(appC)}}
	r := openRoots(t, []string{dir}, []Manifest{m}, nil, lim)
	res := r.Resolve("src/app.c", "b1", "")
	if res.Status != StatusSourceUnverified || !strings.Contains(res.Reason, "10-byte") || res.SHA256 != "" {
		t.Fatalf("Resolve = %+v, want source_unverified over the 10-byte limit and no digest", res)
	}
	got, err := r.Lines(res, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []Line{{N: 1, Text: "line 1"}, {N: 2, Text: "lin", Truncated: true}}
	if !slices.Equal(got, want) {
		t.Fatalf("Lines = %+v, want %+v", got, want)
	}
}

func TestOpenRefusesBadInput(t *testing.T) {
	if _, err := Open([]string{filepath.Join(t.TempDir(), "missing")}, nil, nil, DefaultLimits()); err == nil {
		t.Error("missing root: want an error")
	}
	lim := DefaultLimits()
	lim.MaxLineBytes = 0
	if _, err := Open([]string{t.TempDir()}, nil, nil, lim); err == nil {
		t.Error("zero MaxLineBytes: want an error")
	}
	bad := Manifest{Format: "other/v9", BuildIDs: []string{"b1"}}
	if _, err := Open([]string{t.TempDir()}, []Manifest{bad}, nil, DefaultLimits()); err == nil {
		t.Error("bad manifest: want an error")
	}
}

func TestDirsAreAbsolute(t *testing.T) {
	dir := sourceTree(t)
	t.Chdir(filepath.Dir(dir))
	r := openRoots(t, []string{filepath.Base(dir)}, nil, nil, DefaultLimits())
	if got := r.Dirs(); !slices.Equal(got, []string{dir}) {
		t.Fatalf("Dirs() = %v, want [%s]", got, dir)
	}
	if res := r.Resolve("src/util.c", "", ""); res.Root != dir {
		t.Fatalf("Root = %q, want %q", res.Root, dir)
	}
}
