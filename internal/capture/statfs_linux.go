//go:build linux

package capture

import "syscall"

// FreeBytes is the space an unprivileged process may still write on the
// filesystem that holds path: the blocks available to non-root users,
// which leaves out the reserve only root may use.
func FreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
