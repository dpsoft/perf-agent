//go:build linux

package capture

import (
	"cmp"
	"math"
	"os"
	"syscall"
)

// FreeBytes is the space an unprivileged process may still write on the
// filesystem that holds the open file f (a directory, say): the blocks
// available to non-root users, which leaves out the reserve only root may
// use, counted in the fragment size the kernel reports them in.
func FreeBytes(f *os.File) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Fstatfs(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return freeBytes(st.Bavail, int64(st.Frsize), int64(st.Bsize)), nil
}

// freeBytes is avail blocks of frsize bytes (bsize when the filesystem
// reports no fragment size), saturating at the largest int64 rather than
// overflowing.
func freeBytes(avail uint64, frsize, bsize int64) int64 {
	size := cmp.Or(frsize, bsize)
	if size <= 0 {
		return 0
	}
	if avail > uint64(math.MaxInt64/size) {
		return math.MaxInt64
	}
	return int64(avail) * size
}
