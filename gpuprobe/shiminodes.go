package gpuprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// ShimFile identifies a shim by the pair a uprobe actually keys on.
//
// Path is carried for logs and for link.OpenExecutable; it is NOT the
// identity. Two paths holding identical bytes are two files to a uprobe,
// and the dev/ino pair is what says so.
type ShimFile struct {
	Path string
	Dev  uint64
	Ino  uint64
}

// ShimIdentity returns the (dev, ino) a uprobe will key on for path.
func ShimIdentity(path string) (dev, ino uint64, err error) { return enrollShimIdentity(path) }

// ShimInodesInUse returns every shim file in dir that some process has
// mapped, with the pids mapping it.
//
// A rename(2) upgrade leaves the previous inode alive for everything that
// already mapped it -- that survival is precisely what makes replacing the
// file safe -- so an agent holding one link on the current path silently
// stops covering every workload that was already running. The answer here
// is bounded by live shim versions, normally one and transiently two, not
// by the number of targets.
func ShimInodesInUse(dir string) (map[ShimFile][]int, error) {
	return shimInodesInUseIn("/proc", dir)
}

func shimInodesInUseIn(procRoot, dir string) (map[ShimFile][]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read shim dir %s: %w", dir, err)
	}
	byIno := map[uint64]ShimFile{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		dev, ino, ierr := enrollShimIdentity(p)
		if ierr != nil {
			continue // not a readable regular file; nothing a uprobe could attach to
		}
		byIno[ino] = ShimFile{Path: p, Dev: dev, Ino: ino}
	}
	out := map[ShimFile][]int{}
	if len(byIno) == 0 {
		return out, nil
	}

	procEntries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	for _, e := range procEntries {
		pid, perr := strconv.ParseUint(e.Name(), 10, 32)
		if perr != nil {
			continue // not a pid directory
		}
		for ino, f := range byIno {
			// Errors are the normal case here, not the exception: a
			// process can exit between ReadDir and this read, and one
			// owned by another user is unreadable. Neither says anything
			// about whether it was a target, and neither may stop the
			// scan -- a scan that gave up on the first EACCES would report
			// no targets on any machine with a second tenant.
			ok, merr := procMapsHaveInode(procRoot, uint32(pid), f.Dev, ino) //nolint:gosec // bounded by ParseUint above
			if merr != nil || !ok {
				continue
			}
			out[f] = append(out[f], int(pid)) //nolint:gosec // bounded by ParseUint above
		}
	}
	// Sorted so a caller's log, and a test's expectation, do not depend on
	// readdir order.
	for f := range out {
		sort.Ints(out[f])
	}
	return out, nil
}
