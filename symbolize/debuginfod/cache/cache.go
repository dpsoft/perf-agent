// Package cache stores debuginfod-fetched artifacts on disk under a
// .build-id/<NN>/<rest>{.debug,} layout that blazesym's debug_dirs walker
// recognizes natively.
package cache

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Kind selects the artifact flavor. KindDebuginfo files are placed where
// blazesym's debug-link / build-id resolver finds them automatically.
// KindExecutable files are returned by the dispatcher to blazesym.
type Kind int

const (
	KindDebuginfo Kind = iota
	KindExecutable
)

// Cache wraps a directory containing the .build-id index.
// The Index field is added in Task 9.
type Cache struct {
	Dir string
	// Index will be added in Task 9.
}

// pathFor returns the path within Dir for (buildID, kind), or "" if buildID
// is too short to split into the standard <NN>/<rest> layout (minimum 4 chars).
func pathFor(buildID string, kind Kind) string {
	if len(buildID) < 4 {
		return ""
	}
	prefix := buildID[:2]
	rest := buildID[2:]
	if kind == KindDebuginfo {
		rest += ".debug"
	}
	return filepath.Join(".build-id", prefix, rest)
}

// AbsPath returns the absolute path within Dir for (buildID, kind).
// Returns "" when buildID is invalid (too short).
func (c *Cache) AbsPath(buildID string, kind Kind) string {
	rel := pathFor(buildID, kind)
	if rel == "" {
		return ""
	}
	return filepath.Join(c.Dir, rel)
}

// Has reports whether the artifact is on disk.
func (c *Cache) Has(buildID string, kind Kind) bool {
	abs := c.AbsPath(buildID, kind)
	if abs == "" {
		return false
	}
	_, err := os.Stat(abs)
	return err == nil
}

// WriteAtomic streams body to a tmp file in the same directory as the
// final destination, then renames into place. Returns the absolute final
// path on success. On any error the tmp file is removed before returning.
func (c *Cache) WriteAtomic(kind Kind, buildID string, body io.Reader) (_ string, err error) {
	rel := pathFor(buildID, kind)
	if rel == "" {
		return "", fmt.Errorf("invalid build-id %q", buildID)
	}
	abs := filepath.Join(c.Dir, rel)
	dir := filepath.Dir(abs)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "fetch-*.tmp")
	if err != nil {
		return "", fmt.Errorf("createtemp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = io.Copy(tmp, body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("copy: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", fmt.Errorf("close tmp: %w", err)
	}
	if err = os.Rename(tmpName, abs); err != nil {
		return "", fmt.Errorf("rename: %w", err)
	}
	return abs, nil
}

// ErrNoIndex is returned when an operation requires a configured Index.
var ErrNoIndex = errors.New("cache: no index configured")
