package ehmaps

import (
	"fmt"
	"os"
)

// openableBinary returns a path to the binary backing the symbolic path
// (typically inside a target process) that we can open from the agent's
// namespace. Tries /proc/<pid>/map_files/<startHex>-<limitHex> first
// (kernel-resolved symlink — works across mount namespaces and survives
// unlinked-but-mapped binaries), then falls back to the symbolic path.
// Returns "" when neither resolves.
//
// Requires CAP_SYS_ADMIN to read /proc/<pid>/map_files. perf-agent's
// standard cap set covers this.
//
// Note: the probe (open + close) and the caller's subsequent use of the
// returned path are not atomic. Callers must still handle errors from
// any later open on the returned path.
func openableBinary(pid uint32, start, limit uint64, symbolicPath string) string {
	candidates := [2]string{
		fmt.Sprintf("/proc/%d/map_files/%x-%x", pid, start, limit),
		symbolicPath,
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_ = f.Close()
		return p
	}
	return ""
}
