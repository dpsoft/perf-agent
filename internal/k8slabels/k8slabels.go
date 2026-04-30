// Package k8slabels derives Kubernetes identity labels from a target
// process's cgroup path and the agent's own downward-API environment.
//
// All work is read-only file I/O against /proc and the agent's process
// environment — no Kubernetes API calls, no kubelet, no container runtime
// sockets. Cgroup v2 is required; v1-only hosts produce no k8s labels.
package k8slabels

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// FromPID reads /proc/<hostPID>/cgroup, parses the v2 path, derives k8s
// identity labels, and merges in any present downward-API env labels.
//
// procRoot is "/proc" in production; tests pass a temp dir.
//
// On a host where /proc/<hostPID>/cgroup doesn't exist (process exited),
// FromPID returns an empty map and a nil error — the caller's BPF setup
// will surface the "process gone" error on its own path.
func FromPID(procRoot string, hostPID int) (map[string]string, error) {
	if hostPID <= 0 {
		return nil, fmt.Errorf("k8slabels: invalid pid %d", hostPID)
	}
	out := make(map[string]string, 6)

	cgroupPath := filepath.Join(procRoot, strconv.Itoa(hostPID), "cgroup")
	body, err := os.ReadFile(cgroupPath)
	switch {
	case err == nil:
		// proceed
	case errors.Is(err, os.ErrNotExist):
		// process gone or non-Linux fixture; merge env-only labels and return.
		for k, v := range downwardAPIEnv() {
			out[k] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("k8slabels: read %s: %w", cgroupPath, err)
	}

	if v2Path, ok := parseV2CgroupPath(body); ok {
		out["cgroup_path"] = v2Path
		if uid := extractPodUID(v2Path); uid != "" {
			out["pod_uid"] = uid
		}
		if cid := extractContainerID(v2Path); cid != "" {
			out["container_id"] = cid
		}
	}

	for k, v := range downwardAPIEnv() {
		out[k] = v
	}
	return out, nil
}
