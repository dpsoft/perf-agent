package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/shiminstall"
)

func TestFindTargetsAllowsAnEmptyNodeWhenNotRequiringOne(t *testing.T) {
	// A shim nothing has mapped. A collector on a node with no GPU pods
	// yet is the NORMAL case, not an error.
	shim := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(shim, []byte("\x7fELF not really"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := findTargets(shim, 200*time.Millisecond, false)
	if err != nil {
		t.Fatalf("findTargets(requireOne=false) = %v; an empty node must not be an error", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want no targets", got)
	}
}

func TestFindTargetsStillFailsWhenOneIsRequired(t *testing.T) {
	// -pid and single-shot launch runs still want the old behaviour: if the
	// caller says a target must exist and none does, that is a
	// misconfiguration worth refusing.
	shim := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(shim, []byte("\x7fELF not really"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := findTargets(shim, 200*time.Millisecond, true); err == nil {
		t.Fatal("findTargets(requireOne=true) returned nil error with no targets")
	}
}

func TestCollectorModeRefusesLaunchFlags(t *testing.T) {
	// Collector mode never starts the workload, so every flag that
	// configures a child is meaningless and must be refused rather than
	// ignored -- a silently-ignored -period would have the profile read at
	// a sampling rate it was not taken at.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opt := defineFlags(fs)
	if err := fs.Parse([]string{"-mode=collector", "-shim-dir=/tmp/x", "-period=4"}); err != nil {
		t.Fatal(err)
	}
	if err := validateMode(fs, opt); err == nil {
		t.Fatal("collector mode accepted -period; launch-only flags must be refused")
	}
}

func TestCollectorModeRequiresAShimDir(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opt := defineFlags(fs)
	if err := fs.Parse([]string{"-mode=collector"}); err != nil {
		t.Fatal(err)
	}
	if err := validateMode(fs, opt); err == nil {
		t.Fatal("collector mode accepted an empty -shim-dir")
	}
}

func TestAgentModeRejectsShimDir(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opt := defineFlags(fs)
	if err := fs.Parse([]string{"-shim-dir=/tmp/x"}); err != nil {
		t.Fatal(err)
	}
	if err := validateMode(fs, opt); err == nil {
		t.Fatal("agent mode accepted -shim-dir; it installs nothing and -shim names the file")
	}
}

func TestAgentModeIsTheDefaultAndUnchanged(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opt := defineFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if *opt.mode != "agent" {
		t.Errorf("default mode = %q, want agent", *opt.mode)
	}
	if err := validateMode(fs, opt); err != nil {
		t.Errorf("default invocation rejected: %v", err)
	}
}

func TestUnknownModeIsRefused(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opt := defineFlags(fs)
	if err := fs.Parse([]string{"-mode=daemonset"}); err != nil {
		t.Fatal(err)
	}
	if err := validateMode(fs, opt); err == nil {
		t.Fatal("an unknown -mode was accepted")
	}
}

// stageShimMapping writes procRoot/<pid>/maps naming shimPath with the
// device and inode a uprobe keys on.
func stageShimMapping(t *testing.T, procRoot string, pid uint32, shimPath string) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(shimPath, &st); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	maps := fmt.Sprintf("7f0000000000-7f0000001000 r-xp 00000000 %02x:%02x %d %s\n",
		(st.Dev>>8)&0xfff, st.Dev&0xff, st.Ino, shimPath)
	if err := os.WriteFile(filepath.Join(dir, "maps"), []byte(maps), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorInstallsThenAttachesToEveryMappedInode(t *testing.T) {
	// The upgrade case, which is the one most likely to be skipped: an
	// older shim still has a mapper, so a collector that attached only to
	// the current file would stop covering it -- with no error, just a
	// thinner profile.
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), shiminstall.Name)
	if err := os.WriteFile(src, []byte("shim-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, shiminstall.Name+".v1")
	if err := os.WriteFile(old, []byte("shim-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	procRoot := t.TempDir()
	stageShimMapping(t, procRoot, 42, old)

	files, err := collectorShimFiles(src, dir, procRoot)
	if err != nil {
		t.Fatalf("collectorShimFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d shim files, want 2 (installed current + mapped old)", len(files))
	}
	var sawCurrent, sawOld bool
	for _, f := range files {
		if f.Path == filepath.Join(dir, shiminstall.Name) {
			sawCurrent = true
		}
		if f.Path == old {
			sawOld = true
		}
	}
	if !sawCurrent {
		t.Error("the installed shim is not in the attach set")
	}
	if !sawOld {
		t.Error("an older shim with a live mapper is not in the attach set; every " +
			"workload already running would stop being covered")
	}
}

func TestCollectorAttachSetIsJustTheCurrentShimOnAQuietNode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), shiminstall.Name)
	if err := os.WriteFile(src, []byte("shim-v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	files, err := collectorShimFiles(src, dir, t.TempDir())
	if err != nil {
		t.Fatalf("collectorShimFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("got %d, want exactly 1 on a node with nothing running", len(files))
	}
}
