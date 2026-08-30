//go:build !(linux && amd64)

package pyunwind

import (
	"fmt"
	"runtime"
)

// tlsBase refuses on every architecture but linux/amd64, for the same
// reason glibcTSDOffsets does: this package has measured glibc's `struct
// pthread` layout on amd64 only, and the TSD walk that layout drives cannot
// be guessed at. A wrong guess does not fail -- it reads a pointer out of
// the wrong byte of the pthread struct and returns a plausible, wrong
// PyThreadState.
//
// Attach would refuse such a target anyway (glibcTSDOffsets is consulted
// before the TLS base is ever read), so this arm exists to keep the package
// building for arm64 rather than to be reached in practice.
func (r *ProcReader) tlsBase() (uint64, error) {
	return 0, fmt.Errorf("%w: %s", ErrUnsupportedArch, runtime.GOARCH)
}
