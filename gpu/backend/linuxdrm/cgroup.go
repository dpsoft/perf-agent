package linuxdrm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const procRoot = "/proc"

func lookupCgroupPath(pid uint32) (string, bool) {
	return lookupCgroupPathFrom(procRoot, pid)
}

func lookupCgroupPathFrom(root string, pid uint32) (string, bool) {
	path := filepath.Join(root, strconv.FormatUint(uint64(pid), 10), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return parseCgroupPath(string(data))
}

func parseCgroupPath(raw string) (string, bool) {
	var fallback string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		path := strings.TrimSpace(parts[2])
		if path == "" {
			continue
		}
		if parts[1] == "" {
			return path, true
		}
		if fallback == "" {
			fallback = path
		}
	}
	if fallback == "" {
		return "", false
	}
	return fallback, true
}
