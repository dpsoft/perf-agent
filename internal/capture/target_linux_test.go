package capture

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ourSelf is this test process as the server.
func ourSelf() SelfID {
	return SelfID{UID: os.Getuid(), GID: os.Getgid(), PID: os.Getpid()}
}

// fakeStat is a /proc/<pid>/stat line with the given comm, state and
// start time (field 22).
func fakeStat(pid int, comm, state string, start uint64) string {
	return fmt.Sprintf("%d (%s) %s 1 1 1 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 %d 1000 100\n", pid, comm, state, start)
}

// fakeStatus is a /proc/<pid>/status excerpt.
func fakeStatus(uids, gids, capPrm string) string {
	return "Name:\tfake\nUmask:\t0022\nState:\tS (sleeping)\n" +
		"Uid:\t" + uids + "\nGid:\t" + gids + "\n" +
		"CapInh:\t0000000000000000\nCapPrm:\t" + capPrm + "\nCapEff:\t" + capPrm + "\n"
}

// fakeProc builds a proc tree with one process: its stat, status and
// maps (each skipped when empty), an exe link (none when exe is empty)
// and a root link to "/".
func fakeProc(t *testing.T, pid int, stat, status, maps, exe string) Proc {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"stat": stat, "status": status, "maps": maps} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if exe != "" {
		if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/", filepath.Join(dir, "root")); err != nil {
		t.Fatal(err)
	}
	return Proc{Root: root}
}

// ids is "n n n n", the four ids of a status Uid or Gid line.
func ids(n int) string { return strings.Repeat(strconv.Itoa(n)+"\t", 3) + strconv.Itoa(n) }

// waitState polls /proc/<pid>/stat until its state is one of want.
func waitState(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err == nil {
			_, after, _ := strings.Cut(string(b[strings.LastIndexByte(string(b), ')')+1:]), " ")
			if strings.ContainsAny(after[:1], want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d never reached state %s", pid, want)
}

func TestPreflightAcceptsAChildOfOurs(t *testing.T) {
	c := startChild(t, "sleep")
	st, e := Preflight(DefaultProc(), c.PID, ourSelf())
	if e != nil {
		t.Fatalf("a child of ours: %v", e)
	}
	start, err := StartTime(DefaultProc(), c.PID)
	if err != nil {
		t.Fatal(err)
	}
	if st.PID != c.PID || st.StartTime != start || start == 0 {
		t.Fatalf("state pid %d start %d, want pid %d start %d (non-zero)", st.PID, st.StartTime, c.PID, start)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Base(self)[:min(15, len(filepath.Base(self)))]; st.Comm != want {
		t.Fatalf("comm %q, want %q", st.Comm, want)
	}
	if st.Exe != self {
		t.Fatalf("exe %q, want %q", st.Exe, self)
	}
	uid, gid := os.Getuid(), os.Getgid()
	if st.UIDs != [4]int{uid, uid, uid, uid} || st.GIDs != [4]int{gid, gid, gid, gid} || st.StatusOwner != uid || st.CapPrm != 0 {
		t.Fatalf("ids %v %v, status owner %d, CapPrm %#x; want all %d / %d and no capabilities", st.UIDs, st.GIDs, st.StatusOwner, st.CapPrm, uid, gid)
	}
	if st.BuildID == "" || len(st.BuildID)%2 != 0 || strings.Trim(st.BuildID, "0123456789abcdef") != "" {
		t.Fatalf("build-id %q, want lower-case hex", st.BuildID)
	}
	// The same file reached through another exe link has the same build-id.
	p := fakeProc(t, 7, fakeStat(7, "x", "S", 1), "", "", self)
	if got := ReadBuildID(p, 7); got != st.BuildID {
		t.Fatalf("build-id through a fake exe link %q, want %q", got, st.BuildID)
	}
	if got := ReadBuildID(fakeProc(t, 8, "", "", "", ""), 8); got != "" {
		t.Fatalf("build-id without an exe %q, want none", got)
	}
}

func TestPreflightRefuses(t *testing.T) {
	c := startChild(t, "sleep")
	self := ourSelf()
	exclude := self
	exclude.Exclude = func(pid int) bool { return pid == c.PID }
	otherGID := self
	otherGID.GID++

	gone := startChild(t, "sleep")
	if err := gone.reap(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		pid  int
		self SelfID
		code Code
		want string
	}{
		{"pid 1", 1, self, CodeTargetRefused, "pid 1"},
		{"the server itself", os.Getpid(), self, CodeTargetRefused, "this server"},
		{"an excluded child", c.PID, exclude, CodeTargetRefused, "perf-agent or a reaper"},
		{"a reaped child", gone.PID, self, CodeTargetNotFound, strconv.Itoa(gone.PID)},
		{"another gid", c.PID, otherGID, CodeTargetRefused, "gid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, e := Preflight(DefaultProc(), tc.pid, tc.self)
			targetError(t, e, tc.code, tc.want)
		})
	}

	t.Run("a kernel thread", func(t *testing.T) {
		if b, err := os.ReadFile("/proc/2/comm"); err != nil || strings.TrimSpace(string(b)) != "kthreadd" {
			t.Skipf("pid 2 is not kthreadd here (%q, %v)", b, err)
		}
		_, e := Preflight(DefaultProc(), 2, self)
		targetError(t, e, CodeTargetRefused, "kernel thread")
	})

	uid, gid := os.Getuid(), os.Getgid()
	for _, tc := range []struct {
		name, status, want string
	}{
		{"a saved uid", fakeStatus(fmt.Sprintf("%d\t%d\t%d\t%d", uid, uid, uid+1, uid), ids(gid), zero), "saved uid"},
		{"an fs gid", fakeStatus(ids(uid), fmt.Sprintf("%d\t%d\t%d\t%d", gid, gid, gid, gid+1), zero), "filesystem gid"},
		{"capabilities", fakeStatus(ids(uid), ids(gid), "0000000000080000"), "cap_sys_ptrace"},
		{"three uids", fakeStatus(fmt.Sprintf("%d\t%d\t%d", uid, uid, uid), ids(gid), zero), "Uid"},
		{"no CapPrm", "Uid:\t" + ids(uid) + "\nGid:\t" + ids(gid) + "\n", "CapPrm"},
		{"Gid twice", fakeStatus(ids(uid), ids(gid), zero) + "Gid:\t" + ids(gid) + "\n", "Gid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeProc(t, 42, fakeStat(42, "fake", "S", 9), tc.status, "", "/bin/true")
			_, e := Preflight(p, 42, self)
			targetError(t, e, CodeTargetRefused, tc.want)
		})
	}
}

func TestPreflightRefusesANonDumpableChild(t *testing.T) {
	c := startChild(t, "nondumpable")
	if reason, skip := strings.CutPrefix(c.Line, "skip "); skip {
		t.Skip(reason)
	}
	_, e := Preflight(DefaultProc(), c.PID, ourSelf())
	targetError(t, e, CodeTargetRefused, "not dumpable")
}

// TestPreflightRefusesAChangedGID uses a child that moved its effective
// gid to a supplementary group, which needs a host that permits it; the
// SelfID case in TestPreflightRefuses covers the rule everywhere.
func TestPreflightRefusesAChangedGID(t *testing.T) {
	c := startChild(t, "setresgid")
	if reason, skip := strings.CutPrefix(c.Line, "skip "); skip {
		t.Skip(reason)
	}
	_, e := Preflight(DefaultProc(), c.PID, ourSelf())
	targetError(t, e, CodeTargetRefused, "effective gid")
}

// TestPreflightNeverConsultsTheProcDirOwner pins §6.3: dumpability is the
// exe link and the status owner, never the /proc/<pid> directory owner.
func TestPreflightNeverConsultsTheProcDirOwner(t *testing.T) {
	c := startChild(t, "sleep")
	dir := filepath.Join("/proc", strconv.Itoa(c.PID))
	status := filepath.Join(dir, "status")
	uid := os.Getuid()
	for _, tc := range []struct {
		name              string
		dirOwner, stOwner int
		ok                bool
	}{
		{"directory root, status ours", 0, uid, true},
		{"directory ours, status root", uid, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			p := DefaultProc()
			p.owner = func(path string) (int, error) {
				asked = append(asked, path)
				switch filepath.Clean(path) {
				case dir:
					return tc.dirOwner, nil
				case status:
					return tc.stOwner, nil
				}
				return uid, nil
			}
			st, e := Preflight(p, c.PID, ourSelf())
			switch {
			case tc.ok && e != nil:
				t.Fatalf("refused: %v", e)
			case !tc.ok:
				targetError(t, e, CodeTargetRefused, "not dumpable")
			case st.StatusOwner != tc.stOwner:
				t.Fatalf("status owner %d, want %d", st.StatusOwner, tc.stOwner)
			}
			if !slices.Contains(asked, status) {
				t.Fatalf("the status owner was never asked (asked %q)", asked)
			}
			for _, a := range asked {
				if filepath.Clean(a) == dir {
					t.Fatalf("the /proc/<pid> directory owner was asked (asked %q)", asked)
				}
			}
		})
	}
}

// TestCompareNamesWhatChanged pins the §6.3 recheck: credentials,
// dumpability, CapPrm and the start time. An exec of an ordinary program
// changes none of them and is not a change.
func TestCompareNamesWhatChanged(t *testing.T) {
	was := TargetState{
		PID: 5, StartTime: 100,
		UIDs: [4]int{1000, 1000, 1000, 1000}, GIDs: [4]int{1000, 1000, 1000, 1000},
		StatusOwner: 1000, Comm: "app", Exe: "/usr/bin/app", BuildID: "ab",
	}
	if e := was.Compare(was); e != nil {
		t.Fatalf("equal states: %v", e)
	}
	execd := was
	execd.Comm, execd.Exe, execd.BuildID = "other", "/usr/bin/other", "cd"
	if e := was.Compare(execd); e != nil {
		t.Fatalf("an exec with the same credentials: %v", e)
	}
	for _, tc := range []struct {
		name    string
		change  func(*TargetState)
		code    Code
		changed string
	}{
		{"start time", func(s *TargetState) { s.StartTime++; s.GIDs[1] = 0 }, CodePIDReused, ""},
		{"effective gid", func(s *TargetState) { s.GIDs[1] = 10 }, CodeTargetChanged, "gid"},
		{"saved uid", func(s *TargetState) { s.UIDs[2] = 0 }, CodeTargetChanged, "uid"},
		{"status owner", func(s *TargetState) { s.StatusOwner = 0 }, CodeTargetChanged, "dumpable"},
		{"CapPrm", func(s *TargetState) { s.CapPrm = 1 << CapSysAdmin }, CodeTargetChanged, "capabilities"},
		{"uid and gid", func(s *TargetState) { s.UIDs[0], s.GIDs[3] = 0, 0 }, CodeTargetChanged, "uid,gid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := was
			tc.change(&now)
			e := was.Compare(now)
			targetError(t, e, tc.code, strconv.Itoa(was.PID))
			if e.Changed != tc.changed {
				t.Fatalf("changed %q, want %q", e.Changed, tc.changed)
			}
		})
	}
}

func TestStartTimeReadsField22AfterTheLastParen(t *testing.T) {
	p := fakeProc(t, 9, fakeStat(9, "a) S (b 1 2", "S", 4242), "", "", "")
	got, err := StartTime(p, 9)
	if err != nil || got != 4242 {
		t.Fatalf("start time %d, %v; want 4242", got, err)
	}
	for _, bad := range []string{"9 (x) S 1 2\n", "9 (x S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 77\n", fakeStat(9, "x", "S", 0)[:20] + "zz 1 2\n"} {
		p := fakeProc(t, 9, bad, "", "", "")
		if got, err := StartTime(p, 9); err == nil {
			t.Fatalf("stat %q: start time %d, want an error", bad, got)
		}
	}
}

func TestStartTimeHookFeedsReuse(t *testing.T) {
	c := startChild(t, "sleep")
	actual, err := StartTime(DefaultProc(), c.PID)
	if err != nil {
		t.Fatal(err)
	}
	hooked := DefaultProc()
	hooked.StartTimeHook = func(pid int, start uint64) uint64 {
		if pid != c.PID {
			t.Errorf("hook called for pid %d, want %d", pid, c.PID)
		}
		return start + 7
	}
	if got, err := StartTime(hooked, c.PID); err != nil || got != actual+7 {
		t.Fatalf("hooked start time %d, %v; want %d", got, err, actual+7)
	}
	was, e := Preflight(DefaultProc(), c.PID, ourSelf())
	if e != nil {
		t.Fatal(e)
	}
	now, e := Preflight(hooked, c.PID, ourSelf())
	if e != nil {
		t.Fatal(e)
	}
	if now.StartTime != actual+7 {
		t.Fatalf("hooked preflight start time %d, want %d", now.StartTime, actual+7)
	}
	targetError(t, was.Compare(now), CodePIDReused, "reused")
}

// TestAliveSeesExitAndReuse: alive is the recorded instance in a state
// other than Z, X or x. A child that exited is not alive while its parent
// has not reaped it yet, and is not found once it has.
func TestAliveSeesExitAndReuse(t *testing.T) {
	p := DefaultProc()
	c := startChild(t, "sleep")
	start, err := StartTime(p, c.PID)
	if err != nil {
		t.Fatal(err)
	}
	if !Alive(p, c.PID, start) {
		t.Fatal("a running child is not alive")
	}

	if err := syscall.Kill(c.PID, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	waitState(t, c.PID, "T")
	if !Alive(p, c.PID, start) {
		t.Fatal("a stopped child is not alive")
	}
	if err := syscall.Kill(c.PID, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}

	c.exit()
	waitExitedNoReap(t, c.PID)
	if Alive(p, c.PID, start) {
		t.Fatal("an exited, unreaped child is alive")
	}
	_, e := Preflight(p, c.PID, ourSelf())
	targetError(t, e, CodeTargetExited, "exited")

	if err := c.reap(); err != nil {
		t.Fatal(err)
	}
	if Alive(p, c.PID, start) {
		t.Fatal("a reaped child is alive")
	}
	_, e = Preflight(p, c.PID, ourSelf())
	targetError(t, e, CodeTargetNotFound, strconv.Itoa(c.PID))

	// The same PID with another start time is another process.
	c2 := startChild(t, "sleep")
	start2, err := StartTime(p, c2.PID)
	if err != nil {
		t.Fatal(err)
	}
	reused := p
	reused.StartTimeHook = func(_ int, s uint64) uint64 { return s + 1 }
	if Alive(reused, c2.PID, start2) || Alive(p, c2.PID, start2+1) {
		t.Fatal("a process with another start time is alive as the recorded one")
	}

	for state, alive := range map[string]bool{"R": true, "S": true, "D": true, "T": true, "t": true, "I": true, "Z": false, "X": false, "x": false} {
		fp := fakeProc(t, 11, fakeStat(11, "w", state, 500), "", "", "")
		if got := Alive(fp, 11, 500); got != alive {
			t.Errorf("state %s: alive %v, want %v", state, got, alive)
		}
	}
}

func TestCheckShimMapped(t *testing.T) {
	shimFile := filepath.Join(t.TempDir(), "libcupti_shim.so")
	if err := os.WriteFile(shimFile, []byte("shim"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "libother.so")
	if err := os.WriteFile(other, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(shimFile, &st); err != nil {
		t.Fatal(err)
	}
	shim := Shim{Path: shimFile, Dev: uint64(st.Dev), Ino: st.Ino}
	devField := fmt.Sprintf("%02x:%02x", unix.Major(shim.Dev), unix.Minor(shim.Dev))
	line := func(dev string, ino uint64, path string) string {
		return fmt.Sprintf("7f0000000000-7f0000001000 r-xp 00000000 %s %d    %s\n", dev, ino, path)
	}
	head := line("fd:01", 1111, "/usr/bin/app") + "7ffd00000000-7ffd00021000 rw-p 00000000 00:00 0    [stack]\n"

	for _, tc := range []struct {
		name, maps string
		ok         bool
	}{
		{"the shim's dev:ino", head + line(devField, shim.Ino, shimFile), true},
		// btrfs reports one dev in maps and another to stat for the same
		// inode; the mapped path, resolved in the target's root, is the shim.
		{"another dev, the shim's path", head + line("00:99", shim.Ino, shimFile), true},
		{"another inode", head + line(devField, shim.Ino+1, shimFile), false},
		{"another dev, another file", head + line("00:99", shim.Ino, other), false},
		{"not mapped", head, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fakeProc(t, 33, fakeStat(33, "app", "S", 1), "", tc.maps, "")
			e := CheckShimMapped(p, 33, shim)
			if tc.ok {
				if e != nil {
					t.Fatalf("mapped: %v", e)
				}
				return
			}
			targetError(t, e, CodeGPUNotInjected, "CUDA_INJECTION64_PATH="+shimFile)
		})
	}

	t.Run("a live process and its shared library", func(t *testing.T) {
		sleep, err := exec.LookPath("sleep")
		if err != nil {
			t.Skip(err)
		}
		cmd := exec.Command(sleep, "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		lib := mappedLibrary(t, cmd.Process.Pid)
		var st syscall.Stat_t
		if err := syscall.Stat(lib, &st); err != nil {
			t.Fatal(err)
		}
		if e := CheckShimMapped(DefaultProc(), cmd.Process.Pid, Shim{Path: lib, Dev: uint64(st.Dev), Ino: st.Ino}); e != nil {
			t.Fatalf("%s mapped by %s: %v", lib, sleep, e)
		}
	})
}

// mappedLibrary waits for pid to map a shared library and returns its path.
func mappedLibrary(t *testing.T, pid int) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/maps")
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) == 6 && strings.Contains(filepath.Base(fields[5]), ".so") {
				_ = f.Close()
				return fields[5]
			}
		}
		_ = f.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Skipf("pid %d mapped no shared library (a static sleep?)", pid)
	return ""
}
