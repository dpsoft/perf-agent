package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
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
