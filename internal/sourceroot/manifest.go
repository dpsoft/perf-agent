package sourceroot

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// ManifestFormat is the format string of a version 1 source manifest.
const ManifestFormat = "perf-agent-source-manifest/v1"

// Manifest records the source files a build was made from. A file found in
// a source root at a path the manifest lists, with the listed SHA-256, is
// the source of every build and capture the manifest names.
type Manifest struct {
	Format     string   `json:"format"`
	BuildIDs   []string `json:"build_ids,omitzero"`
	CaptureIDs []string `json:"capture_ids,omitzero"`
	Commit     string   `json:"commit,omitempty"`
	// Files maps a slash path relative to its source root to the
	// lowercase hex SHA-256 of the file's bytes.
	Files map[string]string `json:"files"`
}

// ReadManifest reads and checks the manifest at p, refusing a file over limit
// bytes or one that is not a regular file (without blocking on a FIFO).
// Digests are stored lowercase.
func ReadManifest(p string, limit int64) (Manifest, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest %s is not a regular file", p)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest: %w", err)
	}
	if int64(len(raw)) > limit {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest %s is larger than %d bytes", p, limit)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest %s: %w", p, err)
	}
	files := make(map[string]string, len(m.Files))
	for k, v := range m.Files {
		files[k] = strings.ToLower(v)
	}
	m.Files = files
	if err := m.validate(); err != nil {
		return Manifest{}, fmt.Errorf("sourceroot: source manifest %s: %w", p, err)
	}
	return m, nil
}

func (m Manifest) validate() error {
	if m.Format != ManifestFormat {
		return fmt.Errorf("format %q, want %q", m.Format, ManifestFormat)
	}
	if len(m.BuildIDs) == 0 && len(m.CaptureIDs) == 0 {
		return errors.New("names no build or capture ID")
	}
	if slices.Contains(m.BuildIDs, "") || slices.Contains(m.CaptureIDs, "") {
		return errors.New("an empty build or capture ID")
	}
	if m.Commit != "" && !validOID(m.Commit) {
		return fmt.Errorf("commit %q is not a full lowercase hex object id", m.Commit)
	}
	for p, sum := range m.Files {
		if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") {
			return fmt.Errorf("file %q is not a clean path relative to a source root", p)
		}
		if slices.Contains(strings.Split(p, "/"), ".git") {
			return fmt.Errorf("file %q is inside git metadata, which is never source", p)
		}
		if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 || strings.ToLower(sum) != sum {
			return fmt.Errorf("file %q: %q is not a lowercase hex SHA-256", p, sum)
		}
	}
	return nil
}

// names reports whether the manifest covers the build or the capture.
func (m Manifest) names(buildID, captureID string) bool {
	return (buildID != "" && slices.Contains(m.BuildIDs, buildID)) ||
		(captureID != "" && slices.Contains(m.CaptureIDs, captureID))
}
