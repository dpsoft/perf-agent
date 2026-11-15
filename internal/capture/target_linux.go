package capture

import (
	"bufio"
	"bytes"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func (p Proc) path(pid int, name string) string {
	return filepath.Join(p.Root, strconv.Itoa(pid), name)
}

func (p Proc) ownerOf(path string) (int, error) {
	if p.owner != nil {
		return p.owner(path)
	}
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return int(st.Uid), nil
}

// procStat is what the target rule reads from /proc/<pid>/stat.
type procStat struct {
	comm  string
	state byte
	start uint64
}

// readStat parses /proc/<pid>/stat. The comm field is in parentheses and
// may hold spaces and parentheses itself, so the fields are counted from
// the last ')'. The start time goes through the hook.
func readStat(p Proc, pid int) (procStat, error) {
	b, err := os.ReadFile(p.path(pid, "stat"))
	if err != nil {
		return procStat{}, err
	}
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return procStat{}, fmt.Errorf("pid %d: malformed stat", pid)
	}
	// After the comm: state is field 3 and starttime field 22.
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("pid %d: stat has %d fields after the comm", pid, len(fields))
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, fmt.Errorf("pid %d: stat start time: %w", pid, err)
	}
	if p.StartTimeHook != nil {
		start = p.StartTimeHook(pid, start)
	}
	return procStat{comm: string(b[open+1 : end]), state: fields[0][0], start: start}, nil
}

// exited reports a state in which the task has exited: a zombie or dead.
func (s procStat) exited() bool {
	return s.state == 'Z' || s.state == 'X' || s.state == 'x'
}

// StartTime returns pid's start time in clock ticks since boot
// (/proc/<pid>/stat field 22), through p.StartTimeHook when set.
func StartTime(p Proc, pid int) (uint64, error) {
	s, err := readStat(p, pid)
	return s.start, err
}

// Alive reports whether pid is still the process whose start time was
// recorded, and has not exited. A stopped (T, t) or uninterruptible (D)
// task is alive; a zombie, which its parent has not reaped yet, is not.
func Alive(p Proc, pid int, start uint64) bool {
	s, err := readStat(p, pid)
	return err == nil && s.start == start && !s.exited()
}

// gone reports an error that means the process no longer exists.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// vanished reports target_not_found when pid is gone or is another
// process than the one started at start, and target_exited when it has
// exited; nil while it is still that live process.
func vanished(p Proc, pid int, start uint64) *Error {
	s, err := readStat(p, pid)
	switch {
	case gone(err), err == nil && s.start != start:
		return targetNotFound(pid)
	case err == nil && s.exited():
		return targetExited(pid)
	}
	return nil
}

func targetExited(pid int) *Error {
	return Errorf(CodeTargetExited, "start it again", "pid %d has exited", pid)
}

func targetNotFound(pid int) *Error {
	return Errorf(CodeTargetNotFound, "check the pid, or capture a command instead", "no process %d", pid)
}

const notYoursNext = "profile a process you started as your own user, or capture it as a command"

// Preflight applies the §6.3 target rule to pid, as the kernel's
// ptrace_may_access credential test without Yama would, and returns what
// it saw for the rechecks. In order: the process exists and has not
// exited; it is not pid 1, the server or one of the server's children;
// its exe link is readable, which the kernel allows only for a dumpable
// task it would let the server ptrace-read (never a kernel thread);
// /proc/<pid>/status is owned by the server's uid, which it is not for a
// non-dumpable task; its four uids and four gids are the server's; and it
// holds no permitted capability. The /proc/<pid> directory owner is never
// consulted: it does not follow dumpability.
func Preflight(p Proc, pid int, self SelfID) (TargetState, *Error) {
	if pid <= 0 {
		return TargetState{}, Errorf(CodeInvalidArgument, "pass a positive pid", "pid %d is not a process id", pid)
	}
	s, err := readStat(p, pid)
	switch {
	case gone(err):
		return TargetState{}, targetNotFound(pid)
	case err != nil:
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "cannot read pid %d's stat: %v", pid, err)
	case s.exited():
		return TargetState{}, targetExited(pid)
	}

	switch {
	case pid == 1:
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "pid 1 is never a target")
	case pid == self.PID:
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "pid %d is this server", pid)
	case self.Exclude != nil && self.Exclude(pid):
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "pid %d is this server's perf-agent or a reaper", pid)
	}

	exe, err := os.Readlink(p.path(pid, "exe"))
	if err != nil {
		// An exit between the stat and the readlink also fails it.
		if e := vanished(p, pid, s.start); e != nil {
			return TargetState{}, e
		}
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext,
			"pid %d is not dumpable, a kernel thread, or not yours (its exe link is unreadable: %v)", pid, err)
	}

	statusPath := p.path(pid, "status")
	owner, err := p.ownerOf(statusPath)
	if err != nil {
		if gone(err) {
			return TargetState{}, targetNotFound(pid)
		}
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "cannot stat pid %d's status: %v", pid, err)
	}
	if owner != self.UID {
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext,
			"pid %d is not dumpable: its status is owned by uid %d, not this server's uid %d", pid, owner, self.UID)
	}

	b, err := os.ReadFile(statusPath)
	if err != nil {
		if gone(err) {
			return TargetState{}, targetNotFound(pid)
		}
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "cannot read pid %d's status: %v", pid, err)
	}
	st, err := parseTargetStatus(b)
	if err != nil {
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext, "pid %d: %v", pid, err)
	}
	if e := checkIDs(pid, "uid", st.UIDs, self.UID); e != nil {
		return TargetState{}, e
	}
	if e := checkIDs(pid, "gid", st.GIDs, self.GID); e != nil {
		return TargetState{}, e
	}
	if st.CapPrm != 0 {
		return TargetState{}, Errorf(CodeTargetRefused, notYoursNext,
			"pid %d holds capabilities (CapPrm: %s)", pid, strings.Join(CapSet(st.CapPrm).Names(), ","))
	}

	st.PID = pid
	st.StartTime = s.start
	st.StatusOwner = owner
	st.Comm = s.comm
	st.Exe = exe
	st.BuildID = ReadBuildID(p, pid)
	return st, nil
}

// idNames are the four ids of a status Uid or Gid line, in order.
var idNames = [4]string{"real", "effective", "saved", "filesystem"}

func checkIDs(pid int, kind string, got [4]int, want int) *Error {
	for i, id := range got {
		if id != want {
			return Errorf(CodeTargetRefused, notYoursNext,
				"pid %d's %s %s is %d, not this server's %s %d", pid, idNames[i], kind, id, kind, want)
		}
	}
	return nil
}

// parseTargetStatus reads the Uid, Gid and CapPrm lines of a
// /proc/<pid>/status. A missing, repeated or malformed line is an error.
func parseTargetStatus(status []byte) (TargetState, error) {
	read := map[string]bool{"Uid": true, "Gid": true, "CapPrm": true}
	fields := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok || !read[k] {
			continue
		}
		if _, dup := fields[k]; dup {
			return TargetState{}, fmt.Errorf("status has %s twice", k)
		}
		fields[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return TargetState{}, fmt.Errorf("cannot read status: %w", err)
	}
	var st TargetState
	for _, f := range []struct {
		key string
		dst *[4]int
	}{{"Uid", &st.UIDs}, {"Gid", &st.GIDs}} {
		vals := strings.Fields(fields[f.key])
		if len(vals) != 4 {
			return TargetState{}, fmt.Errorf("cannot read %s from status", f.key)
		}
		for i, v := range vals {
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return TargetState{}, fmt.Errorf("cannot read %s from status", f.key)
			}
			f.dst[i] = int(n)
		}
	}
	capPrm, err := strconv.ParseUint(fields["CapPrm"], 16, 64)
	if err != nil {
		return TargetState{}, fmt.Errorf("cannot read CapPrm from status")
	}
	st.CapPrm = capPrm
	return st, nil
}

// Compare is the recheck: was is the preflight, now a later one of the
// same pid. A changed start time is pid_reused. A changed uid, gid,
// status owner (dumpability) or CapPrm is target_changed, with Changed
// naming each. The exe, comm and build-id may change by an ordinary exec,
// which gains nothing the ids and capabilities would not show.
func (was TargetState) Compare(now TargetState) *Error {
	if now.StartTime != was.StartTime {
		return Errorf(CodePIDReused, "capture again", "pid %d was reused: its start time changed", was.PID)
	}
	var changed []string
	if now.UIDs != was.UIDs {
		changed = append(changed, "uid")
	}
	if now.GIDs != was.GIDs {
		changed = append(changed, "gid")
	}
	if now.StatusOwner != was.StatusOwner {
		changed = append(changed, "dumpable")
	}
	if now.CapPrm != was.CapPrm {
		changed = append(changed, "capabilities")
	}
	if changed == nil {
		return nil
	}
	e := Errorf(CodeTargetChanged, "capture again once the target has settled",
		"pid %d changed since the preflight: %s", was.PID, strings.Join(changed, ", "))
	e.Changed = strings.Join(changed, ",")
	return e
}

// ReadBuildID returns the hex GNU build-id of pid's executable, or "" when
// it cannot be read or has none.
func ReadBuildID(p Proc, pid int) string {
	f, err := elf.Open(p.path(pid, "exe"))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sec := f.Section(".note.gnu.build-id")
	if sec == nil {
		return ""
	}
	data, err := sec.Data()
	// The note header is namesz, descsz and type, then the name "GNU\0".
	const header = 16
	if err != nil || len(data) < header || string(data[12:16]) != "GNU\x00" {
		return ""
	}
	n := int(f.ByteOrder.Uint32(data[4:8]))
	if n == 0 || n > len(data)-header {
		return ""
	}
	return hex.EncodeToString(data[header : header+n])
}

// devNumbers splits a stat st_dev into the major and minor numbers that
// /proc/<pid>/maps prints, as glibc's major() and minor() do.
func devNumbers(dev uint64) (major, minor uint64) {
	major = (dev>>8)&0xfff | (dev>>32)&^0xfff
	minor = dev&0xff | (dev>>12)&^0xff
	return major, minor
}

// CheckShimMapped refuses a gpu capture of pid when pid has not mapped
// the shim's inode: perf-agent attaches by inode, so a copy at another
// path would capture nothing. A mapping matches on the shim's dev:ino, or
// on its inode when the mapped path, resolved in the target's root, is
// the shim: btrfs reports a different dev in maps than stat does.
func CheckShimMapped(p Proc, pid int, s Shim) *Error {
	f, err := os.Open(p.path(pid, "maps"))
	if err != nil {
		if gone(err) {
			return targetNotFound(pid)
		}
		return Errorf(CodeTargetRefused, notYoursNext, "cannot read pid %d's maps: %v", pid, err)
	}
	defer func() { _ = f.Close() }()
	major, minor := devNumbers(s.Dev)
	dev := fmt.Sprintf("%02x:%02x", major, minor)
	ino := strconv.FormatUint(s.Ino, 10)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// address perms offset dev inode path
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[4] != ino {
			continue
		}
		if fields[3] == dev {
			return nil
		}
		path := strings.Join(fields[5:], " ")
		var st syscall.Stat_t
		if filepath.IsAbs(path) && syscall.Stat(filepath.Join(p.path(pid, "root"), path), &st) == nil &&
			uint64(st.Dev) == s.Dev && uint64(st.Ino) == s.Ino {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return Errorf(CodeTargetRefused, notYoursNext, "cannot read pid %d's maps: %v", pid, err)
	}
	return Errorf(CodeGPUNotInjected, "restart it with CUDA_INJECTION64_PATH="+s.Path+", or capture it as a command",
		"the target did not load the CUPTI adapter at start; restart it with CUDA_INJECTION64_PATH=%s, or capture it as a command", s.Path)
}
