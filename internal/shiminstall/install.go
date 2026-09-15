// Package shiminstall places the CUPTI adapter the agent carries into a
// directory targets can load it from.
//
// It exists so the shim and the consumer that decodes its USDT payload ship
// as one artifact. Record layouts are frozen per version, so a shim placed
// out of band -- by a node image or by config management -- can be older or
// newer than the agent with nothing in the pod spec to reveal it, and the
// symptom is a decode failure rather than a version warning.
package shiminstall

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Name is the filename targets name in CUDA_INJECTION64_PATH.
const Name = "libperfagent-gpu-nvidia.so"

// Install copies src into destDir as Name and reports the resulting path.
//
// replaced is false when the destination already held identical content. A
// restarting agent on an unchanged node must not churn the inode, because
// every process that mapped the old one keeps it -- so a needless replace
// would leave those processes on an inode the agent is no longer attached
// to, silently, until something noticed the profile had thinned.
//
// The write is a temp file plus rename(2) within destDir, never a write in
// place. The file is mapped executable by live processes; truncating it
// under them is a crash, not a version skew. rename within one filesystem
// is atomic, so a target resolving the path mid-install sees either the old
// file or the new one and never a partial.
func Install(src, destDir string) (dest string, replaced bool, err error) {
	dest = filepath.Join(destDir, Name)

	srcSum, err := sum(src)
	if err != nil {
		return "", false, fmt.Errorf("shiminstall: read source: %w", err)
	}
	switch dstSum, serr := sum(dest); {
	case serr == nil && dstSum == srcSum:
		return dest, false, nil
	case serr != nil && !errors.Is(serr, fs.ErrNotExist):
		return "", false, fmt.Errorf("shiminstall: read destination: %w", serr)
	}

	if err = os.MkdirAll(destDir, 0o755); err != nil {
		return "", false, fmt.Errorf("shiminstall: create %s: %w", destDir, err)
	}
	tmp, err := os.CreateTemp(destDir, Name+".tmp-*")
	if err != nil {
		return "", false, fmt.Errorf("shiminstall: temp file: %w", err)
	}
	tmpName := tmp.Name()
	// A temp file left in the shim directory is a second inode carrying
	// the right-looking bytes, and CUDA_INJECTION64_PATH is a plain string
	// somebody can point at it. Remove it on every failure path.
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: open source: %w", err)
	}
	_, cerr := io.Copy(tmp, in)
	_ = in.Close()
	if cerr != nil {
		_ = tmp.Close()
		err = cerr
		return "", false, fmt.Errorf("shiminstall: copy: %w", cerr)
	}
	if err = tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: chmod: %w", err)
	}
	// Durable before it is visible: a crash between rename and writeback
	// would otherwise leave targets pointed at a truncated file.
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: sync: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", false, fmt.Errorf("shiminstall: close: %w", err)
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return "", false, fmt.Errorf("shiminstall: rename into place: %w", err)
	}
	return dest, true, nil
}

func sum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
