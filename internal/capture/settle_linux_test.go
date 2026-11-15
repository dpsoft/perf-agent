package capture

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// Synthetic PIDs above Linux's pid_max: a stray signal reaches nothing.
const synthetic = 1_000_000_000

// fakeChild writes a /proc/<pid>/stat for a child of ppid in root.
func fakeChild(t *testing.T, root string, pid, ppid int, state string, start uint64) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stat := strings.Replace(fakeStat(pid, "probe", state, start), ") "+state+" 1 ", fmt.Sprintf(") %s %d ", state, ppid), 1)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
}

// echild is a wait4 for a reaper with no child left.
func echild(*syscall.WaitStatus) (int, error) { return 0, syscall.ECHILD }

// stillChildren is a wait4 for a reaper whose children are all running.
func stillChildren(*syscall.WaitStatus) (int, error) { return 0, nil }

func fakeReaper(root string, wait4 func(*syscall.WaitStatus) (int, error)) *reaper {
	return &reaper{proc: Proc{Root: root}, self: os.Getpid(), starts: map[int]uint64{}, live: map[int]bool{}, wait4: wait4}
}

func TestSettleIsCompleteWhenNoChildIsLeft(t *testing.T) {
	r := fakeReaper(t.TempDir(), echild)
	if left := r.settle(); len(left) != 0 || len(r.reasons) != 0 {
		t.Errorf("survivors %v, reasons %q", left, r.reasons)
	}
}

// One empty /proc snapshot is not quiescence: a descendant adopted while
// the snapshot was read is still found, and reported if it outlives the
// grace.
func TestSettleSeesAdoptionDuringTheProcSnapshot(t *testing.T) {
	root := t.TempDir()
	const old, adopted = synthetic, synthetic + 1
	fakeChild(t, root, old, os.Getpid(), "Z", 11)
	added := false
	r := fakeReaper(root, stillChildren)
	r.proc.StartTimeHook = func(pid int, s uint64) uint64 {
		if pid == old && !added {
			added = true
			fakeChild(t, root, adopted, os.Getpid(), "S", 12)
		}
		return s
	}
	left := r.settle()
	if len(left) != 1 || left[0].PID != adopted {
		t.Errorf("survivors = %+v, want the adopted %d", left, adopted)
	}
	if len(r.reasons) == 0 {
		t.Error("cleanup would read complete")
	}
}

// A listed zombie keeps the cleanup going until it is reaped, and only
// ECHILD after that ends it.
func TestSettleWaitsForAListedZombieToBeReaped(t *testing.T) {
	root := t.TempDir()
	const z = synthetic + 2
	fakeChild(t, root, z, os.Getpid(), "Z", 13)
	calls, reaped := 0, false
	r := fakeReaper(root, func(*syscall.WaitStatus) (int, error) {
		calls++
		switch {
		case reaped:
			return 0, syscall.ECHILD
		case calls < 4:
			return 0, nil
		}
		reaped = true
		_ = os.RemoveAll(filepath.Join(root, strconv.Itoa(z)))
		return z, nil
	})
	if left := r.settle(); len(left) != 0 || len(r.reasons) != 0 {
		t.Errorf("survivors %v, reasons %q", left, r.reasons)
	}
	if !reaped {
		t.Error("settle ended while the zombie was still listed")
	}
}

// A descendant stuck in its exit (PF_EXITING, in D state, as a wedged
// GPU process may be) has ended for the rechecks but is still there: it is
// a survivor, named by identity.
func TestSettleReportsADescendantStuckInItsExit(t *testing.T) {
	root := t.TempDir()
	const stuck = synthetic + 3
	fakeChild(t, root, stuck, os.Getpid(), "D", 14)
	statPath := filepath.Join(root, strconv.Itoa(stuck), "stat")
	b, err := os.ReadFile(statPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statPath, []byte(strings.Replace(string(b), " 4194304 ", " 4194308 ", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	r := fakeReaper(root, stillChildren)
	left := r.settle()
	if len(left) != 1 || left[0].PID != stuck || left[0].State != "D" {
		t.Errorf("survivors = %+v, want %d in D state", left, stuck)
	}
}

// Without ECHILD the tree is not proven quiet, even when no child is listed.
func TestSettleWithoutECHILDIsIncomplete(t *testing.T) {
	r := fakeReaper(t.TempDir(), stillChildren)
	r.settle()
	if len(r.reasons) == 0 {
		t.Error("cleanup would read complete though children remain")
	}
}

// fakeCgroup is a directory shaped like a cgroup: cgroup.procs, and
// cgroup.events when events is not empty.
func fakeCgroup(t *testing.T, dir, events string, procs ...int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range procs {
		fmt.Fprintln(&b, p)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if events != "" {
		if err := os.WriteFile(filepath.Join(dir, "cgroup.events"), []byte(events), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The cgroup is empty only when its cgroup.events says so for the whole
// subtree; a populated nested cgroup or an unreadable events file is
// incomplete, with the nested processes named.
func TestSettleNeedsTheCgroupProvenEmpty(t *testing.T) {
	for _, tc := range []struct {
		name, events string
		nested       []int
	}{
		{"nested populated", "populated 1\n", []int{synthetic + 3}},
		{"unreadable events", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cg := filepath.Join(t.TempDir(), "perf-agent-mcp-1")
			fakeCgroup(t, cg, tc.events)
			if tc.nested != nil {
				fakeCgroup(t, filepath.Join(cg, "nested"), "populated 1\n", tc.nested...)
			}
			r := fakeReaper(t.TempDir(), echild)
			r.inCgroup, r.spec.Cgroup = true, cg
			left := r.settle()
			if len(r.reasons) == 0 {
				t.Fatal("cleanup would read complete")
			}
			for _, pid := range tc.nested {
				if !slices.ContainsFunc(left, func(s Survivor) bool { return s.PID == pid }) {
					t.Errorf("survivors %+v miss the nested %d", left, pid)
				}
			}
		})
	}
}

// Removal is depth-first, and a failure is a reason, never swallowed.
func TestRemovingTheCgroupIsDepthFirstAndReported(t *testing.T) {
	parent := t.TempDir()
	cg := filepath.Join(parent, "perf-agent-mcp-2")
	if err := os.MkdirAll(filepath.Join(cg, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := fakeReaper(t.TempDir(), echild)
	r.spec.Cgroup = cg
	r.removeCgroup()
	if _, err := os.Stat(cg); !os.IsNotExist(err) || len(r.reasons) != 0 {
		t.Errorf("cgroup left: %v; reasons %q", err, r.reasons)
	}

	cg = filepath.Join(parent, "perf-agent-mcp-3")
	fakeCgroup(t, filepath.Join(cg, "a"), "populated 0\n")
	r = fakeReaper(t.TempDir(), echild)
	r.spec.Cgroup = cg
	r.removeCgroup()
	if len(r.reasons) == 0 || !strings.Contains(strings.Join(r.reasons, ";"), "remove") {
		t.Errorf("reasons %q, want the removal failure", r.reasons)
	}
}

// The reaper removes only a cgroup this server creates.
func TestTheReaperLeavesAForeignCgroup(t *testing.T) {
	cg := filepath.Join(t.TempDir(), "user.slice")
	if err := os.Mkdir(cg, 0o700); err != nil {
		t.Fatal(err)
	}
	r := fakeReaper(t.TempDir(), echild)
	r.spec.Cgroup = cg
	r.removeCgroup()
	if _, err := os.Stat(cg); err != nil {
		t.Errorf("removed a foreign cgroup: %v", err)
	}
	if len(r.notes) == 0 {
		t.Error("leaving it was not noted")
	}
}

// The lock is held until both known children are reaped, even after a
// reaped event that reported one of them as a survivor.
func TestTheLockWaitsForTheKnownChildren(t *testing.T) {
	r := fakeReaper(t.TempDir(), echild)
	r.exits = make(chan exitNote, 1)
	// A real, already waited-for process gives the child a handle to
	// release; its PID is replaced by a synthetic one.
	cmd := exec.Command(capturetest.Workload(t), "exit-after", "0s", "0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	r.perf = &child{cmd: cmd, who: WhoPerfAgent, pid: synthetic + 4}
	done := make(chan struct{})
	go func() {
		r.awaitKnown()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("returned while perf-agent was not reaped")
	case <-time.After(100 * time.Millisecond):
	}
	r.exits <- exitNote{pid: synthetic + 4}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not return once perf-agent was reaped")
	}
}

// The handshake tells an attempted exec from a launcher that failed or
// exited before it: only the marker, then EOF, is an exec attempted.
func TestTheHandshakeNeedsTheExecMarker(t *testing.T) {
	for _, tc := range []struct {
		name, wrote, want string
	}{
		{"exec", string(execMarker), ""},
		{"exec failed", string(execMarker) + "exec /x: no such file or directory", "no such file"},
		{"exited before exec", "", "exited before"},
		{"prctl failed", "prctl(PR_SET_NO_NEW_PRIVS): EINVAL", "prctl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specR, specW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			syncR, syncW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			go func() {
				// The launcher's side: read the spec, write, exit or exec.
				_, _ = io.ReadAll(specR)
				_ = specR.Close()
				_, _ = syncW.Write([]byte(tc.wrote))
				_ = syncW.Close()
			}()
			err = handshake(specW, syncR, []byte(`{"Path":"/x"}`))
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("handshake = %v, want an attempted exec", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("handshake = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Without the subreaper, cleanup is incomplete even with a cgroup: a
// descendant moved out of it, or into another session, is out of reach.
func TestNoSubreaperIsIncompleteEvenWithACgroup(t *testing.T) {
	cg := filepath.Join(t.TempDir(), "perf-agent-mcp-4")
	fakeCgroup(t, cg, "populated 0\n")
	r := fakeReaper(t.TempDir(), echild)
	r.inCgroup, r.spec.Cgroup = true, cg
	r.subreaper = syscall.EPERM
	r.settle()
	if !strings.Contains(strings.Join(r.reasons, ";"), "process group only") {
		t.Errorf("reasons %q, want process group only", r.reasons)
	}
}

// Reasons are one line: a multi-part removal error is joined with "; ".
func TestCleanupReasonsAreOneLine(t *testing.T) {
	cg := filepath.Join(t.TempDir(), "perf-agent-mcp-5")
	fakeCgroup(t, filepath.Join(cg, "a"), "populated 0\n")
	fakeCgroup(t, filepath.Join(cg, "b"), "populated 0\n")
	r := fakeReaper(t.TempDir(), echild)
	r.spec.Cgroup = cg
	r.removeCgroup()
	if len(r.reasons) == 0 {
		t.Fatal("no removal failure reported")
	}
	for _, why := range r.reasons {
		if strings.ContainsRune(why, '\n') {
			t.Errorf("reason %q spans lines", why)
		}
	}
}

// A sub-cgroup the workload made unreadable is still removed.
func TestRemoveCgroupRestoresAccessToASubCgroup(t *testing.T) {
	capturetest.SkipIfPrivileged(t)
	cg := filepath.Join(t.TempDir(), "perf-agent-mcp-6")
	sub := filepath.Join(cg, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	if err := RemoveCgroup(cg); err != nil {
		t.Errorf("RemoveCgroup = %v", err)
	}
	if _, err := os.Stat(cg); !os.IsNotExist(err) {
		t.Errorf("%s left: %v", cg, err)
	}
}
