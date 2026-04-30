// Package nspid translates a PID from any Linux PID namespace into the
// outermost (host) kernel PID. Required when perf-agent runs in a sidecar
// or other non-host PID namespace: the user-visible PID number is local to
// that namespace, but BPF (and perf_event_open) operate on host PIDs.
package nspid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Translate maps a PID visible from the agent's own /proc to the outermost
// (host) kernel PID by reading /proc/<pid>/status's NSpid: line.
//
// If NSpid contains a single column, the agent is already in the host PID
// namespace and the input is returned unchanged.
func Translate(pidInOurView int) (int, error) {
	return translateAt("/proc", pidInOurView)
}

func translateAt(procRoot string, pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("nspid: invalid pid %d", pid)
	}
	statusPath := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, fmt.Errorf("nspid: read %s: %w (process exited or namespace mismatch?)", statusPath, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		if len(fields) == 0 {
			break
		}
		host, perr := strconv.Atoi(fields[0])
		if perr != nil {
			return 0, fmt.Errorf("nspid: parse host pid in %q: %w", line, perr)
		}
		return host, nil
	}
	return 0, errors.New("nspid: no NSpid: line in status (kernel < 4.1 or no PID namespace support)")
}
