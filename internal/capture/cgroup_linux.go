package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// wOK is access(2)'s W_OK, which the syscall package does not define.
const wOK = 2

// DelegatedCgroup decides whether a launched workload can get its own
// cgroup under the server's: selfCgroup is /proc/self/cgroup and mount the
// cgroup v2 mount. It needs the unified "0::" entry, the server's cgroup
// directory writable (to create the child), cgroup.kill present (to end
// it) and cgroup.procs writable (starting a process in a cgroup needs
// write access to the common ancestor's cgroup.procs). It returns the
// parent directory, or why there is none.
func DelegatedCgroup(selfCgroup []byte, mount string) (parent, why string) {
	rel, found := "", false
	for line := range strings.Lines(string(selfCgroup)) {
		if p, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "0::"); ok {
			rel, found = p, true
			break
		}
	}
	if !found {
		return "", "no cgroup v2 (0::) entry in /proc/self/cgroup"
	}
	if !filepath.IsAbs(rel) {
		return "", fmt.Sprintf("the cgroup v2 path %q is not absolute", rel)
	}
	dir := filepath.Join(mount, rel)
	if err := syscall.Access(dir, wOK); err != nil {
		return "", fmt.Sprintf("cgroup %s is not writable by this user: %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cgroup.kill")); err != nil {
		return "", fmt.Sprintf("cgroup %s has no cgroup.kill (Linux 5.14 or later): %v", dir, err)
	}
	if err := syscall.Access(filepath.Join(dir, "cgroup.procs"), wOK); err != nil {
		return "", fmt.Sprintf("%s/cgroup.procs is not writable by this user: %v", dir, err)
	}
	return dir, ""
}

// CreateCgroup creates the child cgroup name under parent and returns its
// path. name is one path element, and the directory must not exist yet.
func CreateCgroup(parent, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("cgroup name %q is not one path element", name)
	}
	path := filepath.Join(parent, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// KillCgroup SIGKILLs every process in the cgroup at path, descendants
// that left the session or process group included.
func KillCgroup(path string) error {
	f, err := os.OpenFile(filepath.Join(path, "cgroup.kill"), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString("1")
	return errors.Join(err, f.Close())
}

// RemoveCgroup removes the cgroup at path; one already gone is not an
// error. The kernel refuses while it still has processes.
func RemoveCgroup(path string) error {
	if err := syscall.Rmdir(path); err != nil && !errors.Is(err, syscall.ENOENT) {
		return &os.PathError{Op: "rmdir", Path: path, Err: err}
	}
	return nil
}

// cgroupPopulated reports whether the cgroup at path, or one below it,
// still has a process.
func cgroupPopulated(path string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.events"))
	if err != nil {
		return false, err
	}
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "populated "); ok {
			return v != "0", nil
		}
	}
	return false, fmt.Errorf("%s/cgroup.events has no populated line", path)
}

// cgroupProcs lists the processes directly in the cgroup at path.
func cgroupProcs(path string) []int {
	b, err := os.ReadFile(filepath.Join(path, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var pids []int
	for f := range strings.FieldsSeq(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
