package debuginfod

import "github.com/dpsoft/perf-agent/unwind/procmap"

// readBuildID returns the GNU build-id (lowercase hex) of the ELF at
// mapsFile, falling back to symbolicPath. Empty string when neither path
// resolves (or the ELF has no .note.gnu.build-id).
//
// mapsFile is the kernel-resolved /proc/<pid>/map_files/<va>-<va> symlink,
// present even when symbolicPath isn't reachable from the agent's
// filesystem. symbolicPath is the path string from /proc/<pid>/maps.
func readBuildID(mapsFile, symbolicPath string) string {
	if mapsFile != "" {
		if id, _ := procmap.ReadBuildID(mapsFile); id != "" {
			return id
		}
	}
	if symbolicPath != "" && symbolicPath != mapsFile {
		if id, _ := procmap.ReadBuildID(symbolicPath); id != "" {
			return id
		}
	}
	return ""
}
