package mcpflags

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

func parse(t *testing.T, kind Kind, args ...string) (perfmcp.Config, string, error) {
	t.Helper()
	fs := flag.NewFlagSet("prog", flag.ContinueOnError)
	var stderr bytes.Buffer
	fs.SetOutput(&stderr)
	v := Register(fs, kind)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	cfg, err := v.Config(fs, &stderr, "v1")
	return cfg, stderr.String(), err
}

func TestStdioParsesAsBefore(t *testing.T) {
	profiles := t.TempDir()
	defLim := perfmcp.DefaultLimits()
	rows := defLim
	rows.MaxRows = 7
	rows.Source.GitTimeout = 2 * time.Second
	ttl := defLim
	ttl.Capture.TTL = 48 * time.Hour
	tests := []struct {
		name    string
		args    []string
		want    perfmcp.Config
		stderr  string
		usage   string
		stderrN string
	}{
		{name: "minimal", args: []string{"--profiles", profiles},
			want: perfmcp.Config{ProfilesDir: profiles, SourceGit: true, Limits: defLim, Version: "v1", Capture: perfmcp.CaptureConfig{Owner: "stdio"}}},
		{name: "roots and limits", args: []string{"--profiles", profiles, "--source", "/a", "--source", "/b", "--source-manifest", "/m.json",
			"--source-git=false", "--output", "/out", "--limit", "MaxRows=7", "--limit", "Source.GitTimeout=2s"},
			want: perfmcp.Config{ProfilesDir: profiles, SourceDirs: []string{"/a", "/b"}, SourceManifests: []string{"/m.json"}, OutputDir: "/out",
				Limits: rows, Version: "v1", Capture: perfmcp.CaptureConfig{Owner: "stdio"}}},
		{name: "capture flags", args: []string{"--profiles", profiles, "--perf-agent", "/opt/pa", "--captures", "runs",
			"--capture-command", "/usr/bin/true", "--gpu-shim", "/opt/shim.so", "--capture-ttl", "48h", "--limit", "Capture.TTL=1h"},
			want: perfmcp.Config{ProfilesDir: profiles, SourceGit: true, Limits: ttl, Version: "v1",
				Capture: perfmcp.CaptureConfig{PerfAgent: "/opt/pa", Dir: "runs", GPUShim: "/opt/shim.so", Owner: "stdio", Commands: []string{"/usr/bin/true"}}}},
		{name: "no profiles", args: nil, usage: "prog: --profiles DIR is required: the directory profiles are read from"},
		{name: "relative shim", args: []string{"--profiles", profiles, "--gpu-shim", "lib/shim.so"},
			usage: `prog: --gpu-shim "lib/shim.so" is not an absolute path; name the file by its absolute path`},
		{name: "relative command", args: []string{"--profiles", profiles, "--capture-command", "bin/x"},
			usage: `prog: --capture-command "bin/x" is not an absolute path; name each allowed program by its absolute path`},
		{name: "bad limit", args: []string{"--profiles", profiles, "--limit", "MaxRows=x"}, usage: `prog: --limit MaxRows=x: "x" is not an integer`},
		{name: "zero limit", args: []string{"--profiles", profiles, "--limit", "MaxRows=0"}, usage: "prog: limit MaxRows must be positive, got 0"},
		{name: "extra argument", args: []string{"--profiles", profiles, "stray"}, usage: `prog: unexpected argument "stray"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := parse(t, Stdio, tc.args...)
			if tc.usage != "" {
				ue, ok := errors.AsType[*UsageError](err)
				if !ok || ue.Msg != tc.usage {
					t.Fatalf("err = %v, want usage error %q", err, tc.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Log == nil {
				t.Error("no logger")
			}
			cfg.Log = nil
			if !reflect.DeepEqual(cfg, tc.want) {
				t.Errorf("config\n got %+v\nwant %+v", cfg, tc.want)
			}
		})
	}
}

func TestHubDefaultsOpenHandlesTo32AndOwnerHub(t *testing.T) {
	cfg, _, err := parse(t, Hub, "--profiles", t.TempDir(), "--perf-agent", "/opt/pa")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxOpenHandles != 32 || cfg.Capture.Owner != "hub" {
		t.Errorf("MaxOpenHandles %d, Owner %q", cfg.Limits.MaxOpenHandles, cfg.Capture.Owner)
	}
	if cfg.Capture.Dir != "" {
		t.Errorf("Capture.Dir = %q, want it left to perfmcp's owner default", cfg.Capture.Dir)
	}
	stdio, _, err := parse(t, Stdio, "--profiles", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if stdio.Limits.MaxOpenHandles != perfmcp.DefaultLimits().MaxOpenHandles {
		t.Errorf("stdio MaxOpenHandles = %d", stdio.Limits.MaxOpenHandles)
	}
}

func TestHubLimitOverridesTheOpenHandlesDefault(t *testing.T) {
	cfg, _, err := parse(t, Hub, "--profiles", t.TempDir(), "--limit", "MaxOpenHandles=8")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Limits.MaxOpenHandles != 8 {
		t.Errorf("MaxOpenHandles = %d, want 8", cfg.Limits.MaxOpenHandles)
	}
}

func TestHubUsageNamesItsCapturesDefault(t *testing.T) {
	for kind, want := range map[Kind]string{Stdio: "<profiles>/captures)", Hub: "<profiles>/captures-hub)"} {
		fs := flag.NewFlagSet("prog", flag.ContinueOnError)
		Register(fs, kind)
		if got := fs.Lookup("captures").Usage; !strings.HasSuffix(got, "(default "+want) {
			t.Errorf("%s usage %q", kind, got)
		}
	}
}

func TestCaptureFlagsWithoutPerfAgentWarnOnce(t *testing.T) {
	_, stderr, err := parse(t, Stdio, "--profiles", t.TempDir(), "--captures", "runs", "--gpu-shim", "/opt/shim.so")
	if err != nil {
		t.Fatal(err)
	}
	const want = "prog: --captures, --gpu-shim have no effect without --perf-agent: capture is disabled\n"
	if stderr != want {
		t.Errorf("stderr %q, want %q", stderr, want)
	}
	_, stderr, err = parse(t, Stdio, "--profiles", t.TempDir())
	if err != nil || stderr != "" {
		t.Errorf("no capture flag: err %v, stderr %q", err, stderr)
	}
}

func TestCapturesDirRules(t *testing.T) {
	profiles, outside := t.TempDir(), t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(outside, filepath.Join(profiles, "away")))
	must(os.Mkdir(filepath.Join(profiles, "inside"), 0o700))
	must(os.Symlink(".", filepath.Join(profiles, "self")))
	must(os.WriteFile(filepath.Join(profiles, "file"), nil, 0o600))
	for _, c := range []struct{ dir, want string }{
		{filepath.Join(outside, "runs"), "outside the profiles root"},
		{profiles, "the profiles root itself"},
		{"self", "the profiles root itself"},
		{"file", "not a directory"},
		{"away/runs", "outside the profiles root"},
	} {
		if _, err := CheckCapturesDir(profiles, c.dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", c.dir, err, c.want)
		}
	}
	for _, c := range []struct{ dir, want string }{
		{"caps", "caps"},
		{filepath.Join(profiles, "inside", "runs"), "inside/runs"},
	} {
		if got, err := CheckCapturesDir(profiles, c.dir); err != nil || got != c.want {
			t.Errorf("%s: got %q, %v; want %q", c.dir, got, err, c.want)
		}
	}
}

func TestEveryLimitFieldCanBeSetByFlag(t *testing.T) {
	l := perfmcp.DefaultLimits()
	fields := LimitFields(&l)
	if len(fields) < 30 {
		t.Errorf("only %d settable limits", len(fields))
	}
	for name, f := range fields {
		switch k := f.Kind(); {
		case f.Type() == reflect.TypeFor[time.Duration](), k >= reflect.Int && k <= reflect.Int64:
		default:
			t.Errorf("%s has kind %v", name, k)
		}
	}
}
