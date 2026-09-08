package gpuprobe

import (
	"fmt"
	"os"
	"sort"
	"strconv"
)

// ProcessesMappingShim returns every process on this host that has the shim at
// shimPath mapped, which is the set of processes whose probes an attachment to
// that file can ever fire in.
//
// WHY DISCOVERY IS A SEPARATE PROBLEM FROM ATTACHMENT
// ---------------------------------------------------
// Attaching to one process needs a pid, and the deployments that need this
// most are the ones with no way to learn it. A sidecar shares a process
// namespace with the application container; it is not the application's
// parent, nothing hands it a pid, and the kubelet -- which does know -- has no
// channel to tell it. A node collector has the same problem multiplied by
// every pod on the node. Asking an operator to supply -pid in a pod spec is
// asking them for a number that does not exist until after the pod is
// scheduled.
//
// The shim's own mapping answers it without asking anybody. A process that
// maps the shim inode was started with CUDA_INJECTION64_PATH pointing at it
// and reached cuInit; a process that does not, was not, and no probe attached
// to that file can ever fire in it. So the mapping is not a heuristic for
// "is this a target" -- it is the definition of one.
//
// IDENTITY IS (dev, ino), NOT THE PATH
// ------------------------------------
// The same rule the uprobe itself follows, and it is the reason discovery
// cannot be done by matching path strings out of /proc/<pid>/maps. A uprobe
// attaches to an inode. Two byte-identical copies of the shim at two paths are
// two files, and a process mapping the second one will never fire probes
// attached to the first, however identical its maps line looks. Conversely a
// process that mapped this exact inode still counts after the file is renamed
// or deleted, and its maps line then says something else entirely.
//
// This has a direct consequence for Kubernetes that belongs in the caller's
// documentation rather than being discovered later: an emptyDir per pod gives
// every pod its OWN copy of the shim and therefore its own inode, so one
// attachment covers one pod. Covering a whole node from one attachment
// requires the pods to share one file.
//
// WHAT IT DOES NOT DO
// -------------------
// It reads what is mapped now. A process that starts later will not be in the
// answer, and does not need to be: the startup rendezvous (enroll.go) catches
// a process during its own cuInit, which is strictly better than any poll
// could be. Rediscovery exists for the ones the rendezvous could not serve --
// a process already past cuInit when this agent arrived -- and for noticing
// that every target has gone.
//
// Processes that cannot be read are SKIPPED rather than failing the scan. A
// host runs processes belonging to other users, and a scan that gave up on the
// first EACCES would report no targets on any machine with a second tenant --
// the failure would look exactly like "nothing is being profiled".
func ProcessesMappingShim(shimPath string) ([]int, error) {
	return processesMappingShimIn("/proc", shimPath)
}

func processesMappingShimIn(procRoot, shimPath string) ([]int, error) {
	dev, ino, err := enrollShimIdentity(shimPath)
	if err != nil {
		return nil, fmt.Errorf("identify shim: %w", err)
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	var found []int
	for _, e := range entries {
		pid, err := strconv.ParseUint(e.Name(), 10, 32)
		if err != nil {
			continue // not a pid directory
		}
		// Errors are the normal case here, not the exception: a process that
		// exits between ReadDir and this read is gone (ESRCH/ENOENT), and one
		// owned by another user without ptrace access is unreadable (EACCES).
		// Neither says anything about whether it was a target, and neither may
		// stop the scan.
		ok, err := procMapsHaveInode(procRoot, uint32(pid), dev, ino) //nolint:gosec // bounded by ParseUint above
		if err != nil || !ok {
			continue
		}
		found = append(found, int(pid)) //nolint:gosec // bounded by ParseUint above
	}
	// Sorted so a caller's log, and a test's expectation, do not depend on
	// readdir order.
	sort.Ints(found)
	return found, nil
}

// RegisterTarget installs CFI tables for a process discovered after Attach.
//
// Rediscovery needs it, and only rediscovery: a process that starts normally
// enrols itself during cuInit, which is earlier and more reliable than any
// scan. What this covers is the process a scan finds that the rendezvous could
// not serve -- one already past cuInit when this agent arrived, or one whose
// rendezvous was refused or throttled.
//
// Idempotent: the registry never recompiles a pid it already holds, so calling
// this on every rescan for every target costs a map lookup per pid rather than
// a CFI compile. That is what makes a polling caller cheap enough to be
// correct.
//
// Returns how many binaries were registered for the pid and any error. Zero
// with no error is the ordinary answer for a pid already held -- not a
// failure, and a caller that logged it as one would report every rescan as
// broken.
func (c *Consumer) RegisterTarget(pid int) (int, error) {
	if c == nil || c.unwind == nil || pid <= 0 {
		return 0, nil
	}
	return c.unwind.registerNow(uint32(pid)) //nolint:gosec // bounds-checked above
}
