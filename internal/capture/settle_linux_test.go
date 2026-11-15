package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
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
