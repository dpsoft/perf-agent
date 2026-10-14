//go:build !(linux && (amd64 || arm64))

package pyunwind

import (
	"fmt"
	"runtime"
)

// tlsBase refuses on every architecture without a measured glibc `struct
// pthread` layout, for the same reason glibcTSDOffsets does: the TSD walk
// that layout drives cannot be guessed at. A wrong guess does not fail --
// it reads a pointer out of the wrong byte of the pthread struct and
// returns a plausible, wrong PyThreadState.
//
// linux/amd64 and linux/arm64 each have a measured layout and a real
// implementation in their own file; this is what is left. Attach would
// refuse such a target anyway (glibcTSDOffsets is consulted before the TLS
// base is ever read), so this arm exists to keep the package building
// everywhere else rather than to be reached in practice.
func (r *ProcReader) tlsBase() (uint64, error) {
	return 0, fmt.Errorf("%w: %s", ErrUnsupportedArch, runtime.GOARCH)
}
