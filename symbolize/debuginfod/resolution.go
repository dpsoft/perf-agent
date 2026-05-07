package debuginfod

import (
	"debug/elf"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// hasDwarf reports whether the ELF at path has a non-empty .debug_info
// section (the cheapest "DWARF is present" signal).
func hasDwarf(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sec := f.Section(".debug_info")
	return sec != nil && sec.Size > 0
}

// binaryReadable reports whether path can be opened (no read of contents).
// Distinguishes "binary on disk" from "sidecar can't see peer's filesystem".
func binaryReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// hasResolvableDebuglink returns true when path's .gnu_debuglink section
// names a file that exists in standard search paths plus any caller-supplied
// extras (e.g., the debuginfod cache dir).
func hasResolvableDebuglink(path string, extraDirs []string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sec := f.Section(".gnu_debuglink")
	if sec == nil {
		return false
	}
	data, err := sec.Data()
	if err != nil {
		return false
	}
	// Layout: NUL-terminated filename, padded to 4 bytes, then crc32.
	end := 0
	for end < len(data) && data[end] != 0 {
		end++
	}
	if end == 0 {
		return false
	}
	name := string(data[:end])
	candidates := append([]string{
		filepath.Join(filepath.Dir(path), name),
		filepath.Join(filepath.Dir(path), ".debug", name),
		filepath.Join("/usr/lib/debug", filepath.Dir(path), name),
		filepath.Join("/usr/lib/debug", name),
	}, extraDirsExpand(extraDirs, name)...)
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return true
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
	}
	return false
}

func extraDirsExpand(dirs []string, name string) []string {
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, filepath.Join(d, name))
	}
	return out
}
