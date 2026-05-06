// Package cache stores debuginfod-fetched artifacts on disk under a
// .build-id/<NN>/<rest>{.debug,} layout that blazesym's debug_dirs walker
// recognizes natively.
package cache

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoIndex is returned when an operation requires a configured Index.
var ErrNoIndex = errors.New("cache: no index configured")

// Kind selects the artifact flavor. KindDebuginfo files are placed where
// blazesym's debug-link / build-id resolver finds them automatically.
// KindExecutable files are returned by the dispatcher to blazesym.
type Kind int

const (
	KindDebuginfo Kind = iota
	KindExecutable
)

// Cache wraps a directory containing the .build-id index.
type Cache struct {
	Dir      string
	Index    Index
	MaxBytes int64 // 0 means unbounded
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
func (c *Cache) WriteAtomic(buildID string, kind Kind, body io.Reader) (_ string, err error) {
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
	// Record in the index (best-effort: a stale index can be rebuilt by
	// Prewarm; failing here would mean throwing away a successful fetch).
	size, _ := fileSize(abs)
	if c.Index != nil {
		_ = c.Index.Touch(buildID, kind, size)
	}
	return abs, nil
}

// fileSize returns the size in bytes of the file at path p.
func fileSize(p string) (int64, error) {
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Evict deletes LRU entries until total cache size ≤ MaxBytes. Safe to
// call at any time. No-op when Index is nil or MaxBytes ≤ 0.
func (c *Cache) Evict() error {
	if c.Index == nil || c.MaxBytes <= 0 {
		return nil
	}
	evicted, err := c.Index.EvictTo(c.MaxBytes)
	if err != nil {
		return err
	}
	for _, e := range evicted {
		abs := c.AbsPath(e.BuildID, e.Kind)
		if abs == "" {
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			// Continue evicting; index/file drift can be reconciled by
			// Prewarm on next start.
		}
	}
	return nil
}

// Prewarm walks Dir and records each existing artifact in the Index.
// Recovers from index loss (e.g., crash) and lets a fresh process inherit
// a populated cache from a previous run.
func (c *Cache) Prewarm() error {
	if c.Index == nil {
		return ErrNoIndex
	}
	root := filepath.Join(c.Dir, ".build-id")
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		buildID, kind, ok := parsePath(c.Dir, path)
		if !ok {
			return nil
		}
		st, err := os.Stat(path)
		if err != nil {
			return nil
		}
		return c.Index.Touch(buildID, kind, st.Size())
	})
}

// parsePath inverts pathFor for files under <dir>/.build-id/<NN>/<rest>{.debug,}.
// Returns ok=false for paths outside that layout.
func parsePath(dir, path string) (buildID string, kind Kind, ok bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	if len(parts) != 3 || parts[0] != ".build-id" {
		return
	}
	prefix := parts[1]
	rest := parts[2]
	if strings.HasSuffix(rest, ".debug") {
		kind = KindDebuginfo
		rest = strings.TrimSuffix(rest, ".debug")
	} else {
		kind = KindExecutable
	}
	return prefix + rest, kind, true
}

// Close closes the index. No-op when Index is nil.
func (c *Cache) Close() error {
	if c.Index == nil {
		return nil
	}
	return c.Index.Close()
}
