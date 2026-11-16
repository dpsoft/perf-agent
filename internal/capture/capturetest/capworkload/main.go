//go:build linux

// Command capworkload is the scripted workload the capture tests profile.
// The first argument selects the behaviour; see run.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	prSetDumpable = 4
	spawnSetsid   = "spawn-setsid"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: capworkload MODE [ARGS...]")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "capworkload:", err)
		os.Exit(2)
	}
}

func run(mode string, args []string) error {
	var perr error
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		perr = errors.Join(perr, fmt.Errorf("%s needs argument %d", mode, i+1))
		return ""
	}
	dur := func(i int) time.Duration {
		d, err := time.ParseDuration(arg(i))
		if err != nil {
			perr = errors.Join(perr, err)
		}
		return d
	}
	num := func(i int) int {
		n, err := strconv.Atoi(arg(i))
		if err != nil {
			perr = errors.Join(perr, err)
		}
		return n
	}
	// Arguments are parsed up front so a bad one is an error, never a
	// zero duration.
	switch mode {
	case "busy", "read-zero", "sleep", "fork-and-exit", spawnSetsid, "ignore-sigterm":
		dur(0)
	case "double-fork-setsid":
		dur(0)
		if len(args) > 1 {
			dur(1)
		}
	case "flood-then-sleep":
		num(0)
		dur(1)
	case "exit-after":
		dur(0)
		num(1)
	case "drop-dumpable-after", "setresgid-after":
		dur(0)
		dur(1)
	case "flood-output":
		num(0)
	}
	if perr != nil {
		return perr
	}
	switch mode {
	case "busy":
		spin(dur(0))
	case "read-zero":
		return readZero(dur(0))
	case "sleep":
		time.Sleep(dur(0))
	case "exit-after":
		time.Sleep(dur(0))
		os.Exit(num(1))
	case "fork-and-exit":
		return forkAndExit(dur(0))
	case "double-fork-setsid":
		grandchild := dur(0)
		if len(args) > 1 {
			grandchild = dur(1)
		}
		return doubleForkSetsid(dur(0), grandchild)
	case spawnSetsid:
		return spawnDetached(dur(0))
	case "drop-dumpable-after":
		time.Sleep(dur(0))
		if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); e != 0 {
			return e
		}
		time.Sleep(dur(1))
	case "setresgid-after":
		return setresgidAfter(dur(0), dur(1))
	case "ignore-sigterm":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(dur(0))
	case "flood-output":
		flood(num(0))
	case "flood-then-sleep":
		flood(num(0))
		fmt.Println("flooded")
		time.Sleep(dur(1))
	case "report":
		return report()
	case "exec":
		if len(args) == 0 {
			return fmt.Errorf("exec needs a path")
		}
		return syscall.Exec(args[0], args, os.Environ())
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
	return nil
}

// spin keeps a CPU busy in user space for d. It is a named function of its
// own so a real profile of the workload has a frame to look for.
//
//go:noinline
func spin(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
	}
}

// readZero reads /dev/zero for d, so most of its CPU time is spent in the
// kernel: a real profile with kernel stacks has kernel frames to show.
//
//go:noinline
func readZero(d time.Duration) error {
	f, err := os.Open("/dev/zero")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 1<<20)
	for end := time.Now().Add(d); time.Now().Before(end); {
		if _, err := f.Read(buf); err != nil {
			return err
		}
	}
	return nil
}

// forkAndExit leaves a sleeping child in this process group and exits.
func forkAndExit(d time.Duration) error {
	c := exec.Command(os.Args[0], "sleep", d.String())
	if err := c.Start(); err != nil {
		return err
	}
	fmt.Println(c.Process.Pid)
	return nil
}

// doubleForkSetsid runs an intermediate that starts a sleeper (for
// grandchild) in a new session and exits, so the sleeper is orphaned and
// outside this group. It prints the sleeper's pid, then sleeps for d
// itself.
func doubleForkSetsid(d, grandchild time.Duration) error {
	c := exec.Command(os.Args[0], spawnSetsid, grandchild.String())
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return err
	}
	time.Sleep(d)
	return nil
}

func spawnDetached(d time.Duration) error {
	c := exec.Command(os.Args[0], "sleep", d.String())
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return err
	}
	fmt.Println(c.Process.Pid)
	return nil
}

// setresgidAfter changes the real, effective and saved gid to a
// supplementary one after a delay. It prints skip when there is none or the
// kernel refuses.
func setresgidAfter(after, d time.Duration) error {
	groups, _ := syscall.Getgroups()
	egid := syscall.Getegid()
	i := slices.IndexFunc(groups, func(g int) bool { return g != egid })
	if i < 0 {
		fmt.Println("skip")
		return nil
	}
	time.Sleep(after)
	if err := syscall.Setresgid(groups[i], groups[i], groups[i]); err != nil {
		fmt.Println("skip")
		return nil
	}
	time.Sleep(d)
	return nil
}

// flood writes mib MiB to each of stdout and stderr, the lines containing
// invalid UTF-8.
func flood(mib int) {
	line := []byte("flood \xff\xfe\xc3 " + strings.Repeat("y", 1000) + "\n")
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		w := bufio.NewWriterSize(f, 64<<10)
		for n := 0; n < mib<<20; n += len(line) {
			_, _ = w.Write(line)
		}
		_ = w.Flush()
	}
}

// report prints the process attributes tests inspect, read from /proc.
func report() error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	nnp := -1
	for line := range strings.Lines(string(status)) {
		if v, ok := strings.CutPrefix(line, "NoNewPrivs:"); ok {
			nnp, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return err
	}
	// The comm field may hold spaces and parentheses; the rest starts after
	// the last ')'.
	i := strings.LastIndex(string(stat), ")")
	if i < 0 {
		return errors.New("malformed /proc/self/stat")
	}
	fields := strings.Fields(string(stat)[i+1:])
	tty := -1
	if len(fields) > 4 {
		tty, _ = strconv.Atoi(fields[4])
	}
	env := slices.Sorted(slices.Values(os.Environ()))
	return json.NewEncoder(os.Stdout).Encode(struct {
		NoNewPrivs int      `json:"nonewprivs"`
		TTYNr      int      `json:"tty_nr"`
		Env        []string `json:"env"`
	}{nnp, tty, env})
}
