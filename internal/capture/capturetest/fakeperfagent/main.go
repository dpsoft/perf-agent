// Command fakeperfagent stands in for perf-agent in tests. Its behaviour
// comes from fake-mode.json beside the executable, because it runs with a
// scrubbed environment.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest/fakemode"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == fakemode.GrandchildArg {
		time.Sleep(10 * time.Second)
		return
	}
	os.Exit(run())
}

func run() int {
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	dir := filepath.Dir(exe)
	var mf fakemode.File
	if b, err := os.ReadFile(filepath.Join(dir, "fake-mode.json")); err != nil {
		return fail(err)
	} else if err := json.Unmarshal(b, &mf); err != nil {
		return fail(err)
	}

	fs := flag.NewFlagSet("fakeperfagent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vals := map[string]*string{}
	for _, f := range capture.Flags {
		if f.Bool {
			fs.Bool(f.Name, false, "")
		} else {
			vals[f.Name] = fs.String(f.Name, "", "")
		}
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakeperfagent:", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "fakeperfagent: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	var duration time.Duration
	if v := *vals["duration"]; v != "" {
		if duration, err = time.ParseDuration(v); err != nil {
			fmt.Fprintln(os.Stderr, "fakeperfagent: bad --duration:", err)
			return 2
		}
	}

	rec := fakemode.Record{Argv: os.Args, Env: os.Environ(), PID: os.Getpid(), PGID: syscall.Getpgrp()}
	if mf.Mode == fakemode.ForkGrandchild {
		gc := exec.Command(exe, fakemode.GrandchildArg)
		if err := gc.Start(); err != nil {
			return fail(err)
		}
		rec.Grandchild = gc.Process.Pid
		go func() { _ = gc.Wait() }()
	}
	if mf.Record != "" {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(mf.Record, b, 0o644); err != nil {
			return fail(err)
		}
	}

	sigs := make(chan os.Signal, 1)
	switch mf.Mode {
	case fakemode.Exit1Stderr:
		fmt.Fprintln(os.Stderr, "failed to set capabilities")
		return 1
	case fakemode.Exit2:
		fmt.Fprintln(os.Stderr, "usage: fakeperfagent")
		return 2
	case fakemode.Exit127Loader:
		fmt.Fprintln(os.Stderr, "error while loading shared libraries: libblazesym_c.so")
		return 127
	case fakemode.IgnoreSIGINT:
		signal.Ignore(syscall.SIGINT)
	default:
		signal.Notify(sigs, syscall.SIGINT)
	}

	time.Sleep(time.Duration(mf.ReadyAfter))
	if mf.Mode == fakemode.NeverReady {
		<-sigs
		return 1
	}
	fmt.Printf(capture.ReadinessFormat+"\n", duration)

	if mf.Mode == fakemode.SpamStdout {
		spam(os.Stdout)
		spam(os.Stderr)
	}

	var timeout <-chan time.Time
	if duration > 0 {
		timeout = time.After(duration)
	}
	select {
	case <-sigs:
		if mf.Mode == fakemode.HangAfterSIGINT {
			time.Sleep(10 * time.Minute)
			return 1
		}
	case <-timeout:
	}

	if err := writeOutputs(mf.Mode, dir, vals); err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "fakeperfagent:", err)
	return 1
}

// spam writes 4 MiB of text lines.
func spam(w io.Writer) {
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for range 4 * 1024 {
		_, _ = w.Write(line)
	}
}

// writeOutputs creates the files the argv asked for from the fixtures
// beside the executable.
func writeOutputs(mode fakemode.Mode, dir string, vals map[string]*string) error {
	if mode == fakemode.NoFile {
		return nil
	}
	fix := func(name string) string { return filepath.Join(dir, "fixtures", name) }
	outs := []struct{ flag, fixture string }{
		{"profile-output", fakemode.FixtureCPU},
		{"offcpu-output", fakemode.FixtureCPU},
		{"gpu-output", fakemode.FixtureGPU},
		{"gpu-executions-output", fakemode.FixtureExeclog},
	}
	if mode == fakemode.NoGPUSamples {
		outs[2].fixture, outs[3].fixture = fakemode.FixtureGPUEmpty, fakemode.FixtureExeclogEmpty
	}
	for i, o := range outs {
		dst := *vals[o.flag]
		if dst == "" {
			continue
		}
		var err error
		switch {
		case mode == fakemode.SymlinkOutput && i == 0:
			err = os.Symlink(fix(o.fixture), dst)
		case mode == fakemode.HardlinkOutput && i == 0:
			err = os.Link(fix(o.fixture), dst)
		case mode == fakemode.HugeFile && i == 0:
			err = sparse(dst, 256<<20)
		default:
			err = copyFile(fix(o.fixture), dst)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func sparse(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
