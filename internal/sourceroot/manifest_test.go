package sourceroot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func writeManifest(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "source-manifest.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestManifestGivesBuildVerified(t *testing.T) {
	dir := sourceTree(t)
	path := writeManifest(t, map[string]any{
		"format":    ManifestFormat,
		"build_ids": []string{"b1"},
		"commit":    strings.Repeat("a", 40),
		"files":     map[string]string{"src/app.c": strings.ToUpper(sha256Hex(appC))},
	})
	m, err := ReadManifest(path, DefaultLimits().MaxManifestBytes)
	if err != nil {
		t.Fatal(err)
	}
	r := openRoots(t, []string{dir}, []Manifest{m}, nil, DefaultLimits())
	res := r.Resolve("/build/app/src/app.c", "b1", "")
	if res.Status != StatusBuildVerified || res.VerifiedBy != "manifest" || res.Commit != strings.Repeat("a", 40) || res.Reason != "" {
		t.Fatalf("Resolve = %+v, want build_verified by the manifest", res)
	}

	// Another build is not covered by this manifest: no manifest evidence.
	if res := r.Resolve("/build/app/src/app.c", "b2", ""); res.Status != StatusSourceUnverified || res.Reason != "git probe disabled" {
		t.Fatalf("Resolve(b2) = %+v, want no manifest evidence", res)
	}
	// A file the manifest does not list gets none either.
	if res := r.Resolve("/build/app/src/util.c", "b1", ""); res.Status != StatusSourceUnverified || res.Reason != "git probe disabled" {
		t.Fatalf("Resolve(util.c) = %+v, want no manifest evidence", res)
	}

	writeFile(t, dir, "src/app.c", strings.Replace(appC, "line 3", "line 8", 1))
	res = r.Resolve("/build/app/src/app.c", "b1", "")
	if res.Status != StatusSourceUnverified || res.Reason != "file differs from the source manifest" {
		t.Fatalf("Resolve after a changed byte = %+v, want source_unverified %q", res, "file differs from the source manifest")
	}
}

func TestManifestMatchesACaptureID(t *testing.T) {
	dir := sourceTree(t)
	m := Manifest{Format: ManifestFormat, CaptureIDs: []string{"cap-1"}, Files: map[string]string{"src/util.c": sha256Hex("int util(void) { return 0; }\n")}}
	r := openRoots(t, []string{dir}, []Manifest{m}, nil, DefaultLimits())
	if res := r.ResolveBasename("util.c", "", "cap-1"); res.Status != StatusBuildVerified {
		t.Fatalf("ResolveBasename(util.c, cap-1) = %+v, want build_verified", res)
	}
	if res := r.ResolveBasename("util.c", "", ""); res.Status != StatusSourceUnverified {
		t.Fatalf("ResolveBasename(util.c) without an ID = %+v, want source_unverified", res)
	}
}

func TestReadManifestRefusesBadInput(t *testing.T) {
	good := sha256Hex("x")
	cases := map[string]any{
		"format":        map[string]any{"format": "perf-agent-source-manifest/v2", "build_ids": []string{"b1"}, "files": map[string]string{"a.c": good}},
		"no ids":        map[string]any{"format": ManifestFormat, "files": map[string]string{"a.c": good}},
		"bad digest":    map[string]any{"format": ManifestFormat, "build_ids": []string{"b1"}, "files": map[string]string{"a.c": "abc"}},
		"absolute":      map[string]any{"format": ManifestFormat, "build_ids": []string{"b1"}, "files": map[string]string{"/a.c": good}},
		"dotdot":        map[string]any{"format": ManifestFormat, "build_ids": []string{"b1"}, "files": map[string]string{"../a.c": good}},
		"unclean":       map[string]any{"format": ManifestFormat, "build_ids": []string{"b1"}, "files": map[string]string{"src//a.c": good}},
		"bad commit":    map[string]any{"format": ManifestFormat, "build_ids": []string{"b1"}, "commit": "HEAD", "files": map[string]string{"a.c": good}},
		"empty id":      map[string]any{"format": ManifestFormat, "build_ids": []string{""}, "files": map[string]string{"a.c": good}},
		"not an object": []string{"x"},
	}
	for name, v := range cases {
		if _, err := ReadManifest(writeManifest(t, v), 1<<20); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	p := writeManifest(t, map[string]any{"format": ManifestFormat, "build_ids": []string{"b1"}, "files": map[string]string{"a.c": good}})
	if _, err := ReadManifest(p, 1<<20); err != nil {
		t.Fatalf("valid manifest: %v", err)
	}
	if _, err := ReadManifest(p, 10); err == nil || !strings.Contains(err.Error(), "10") {
		t.Fatalf("over the size limit: err = %v, want the limit named", err)
	}
	if err := os.WriteFile(p, []byte(`{"format":"`+ManifestFormat+`","build_ids":["b1"],"files":{}} trailing`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(p, 1<<20); err == nil {
		t.Fatal("trailing data: want an error")
	}
}

func TestReadManifestRefusesAFIFOWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "m.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	errc := make(chan error, 1)
	go func() {
		_, err := ReadManifest(fifo, 1<<20)
		errc <- err
	}()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("err = %v, want not a regular file", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO manifest blocked")
	}
}
