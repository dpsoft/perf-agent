package debuginfod

import "testing"

func TestReadBuildIDPrefersMapsFile(t *testing.T) {
	for _, p := range []string{"/usr/bin/grep", "/usr/bin/ls", "/bin/ls"} {
		id := readBuildID(p, "/this/path/should/not/be/read")
		if id != "" {
			return // PASS — read from mapsFile
		}
	}
	t.Skip("no system binary with build-id available")
}

func TestReadBuildIDFallsBackToSymbolicPath(t *testing.T) {
	for _, p := range []string{"/usr/bin/grep", "/usr/bin/ls", "/bin/ls"} {
		id := readBuildID("/nonexistent/path", p)
		if id != "" {
			return
		}
	}
	t.Skip("no system binary with build-id available")
}

func TestReadBuildIDNothingFound(t *testing.T) {
	if id := readBuildID("/nonexistent/a", "/nonexistent/b"); id != "" {
		t.Fatalf("readBuildID = %q", id)
	}
}
