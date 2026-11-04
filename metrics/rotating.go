package metrics

import (
	"errors"
	"fmt"
	"os"
)

// RotatingWriter bounds local JSONL storage to three files of at most MaxBytes
// each: Path, Path.1 and Path.2. One Write is one complete record. It is intended
// for the collector's serial writes and is not safe for concurrent use.
type RotatingWriter struct {
	Path     string
	MaxBytes int64
	file     *os.File
	size     int64
}

func NewRotatingWriter(path string, maxBytes int64) (*RotatingWriter, error) {
	if maxBytes <= 0 {
		return nil, errors.New("JSONL size limit must be positive")
	}
	w := &RotatingWriter{Path: path, MaxBytes: maxBytes}
	if err := w.validateFiles(); err != nil {
		return nil, err
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *RotatingWriter) open() error {
	file, err := os.OpenFile(w.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return errors.New("rotating JSONL output must be a regular file")
	}
	w.file, w.size = file, info.Size()
	return nil
}
func (w *RotatingWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.MaxBytes {
		return 0, fmt.Errorf("JSONL record exceeds %d-byte rotation limit", w.MaxBytes)
	}
	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.MaxBytes {
		if err := w.validateFiles(); err != nil {
			return 0, err
		}
		if err := w.file.Close(); err != nil {
			w.file = nil
			return 0, err
		}
		w.file = nil
		if err := os.Rename(w.Path+".1", w.Path+".2"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if err := os.Rename(w.Path, w.Path+".1"); err != nil {
			return 0, err
		}
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}
func (w *RotatingWriter) Close() error {
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *RotatingWriter) validateFiles() error {
	for _, suffix := range []string{"", ".1", ".2"} {
		info, err := os.Lstat(w.Path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("rotating JSONL path must be a regular file: %s", w.Path+suffix)
		}
		if info.Size() > w.MaxBytes {
			return fmt.Errorf("existing JSONL file exceeds %d-byte limit: %s", w.MaxBytes, w.Path+suffix)
		}
	}
	return nil
}
