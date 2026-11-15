package capture

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var neverEmitted = []string{
	"-a", "--all", "--pmu", "--per-pid", "--perf-data-output", "--flamegraph-output",
	"--metrics-listen", "--debuginfod", "--debuginfod-url", "--debuginfod-prefetch",
	"--symbol-cache", "--gpu-telemetry", "--continuous", "--gpu-ring-bytes",
	"--gpu-stack-memo", "--gpu-per-execution-samples",
}

func TestBuildArgvEveryCombination(t *testing.T) {
	known := map[string]Flag{}
	for _, f := range Flags {
		known[f.Name] = f
	}
	all := []Mode{ModeCPU, ModeOffCPU, ModeGPU}
	for mask := 1; mask < 8; mask++ {
		var modes []Mode
		for i, m := range all {
			if mask&(1<<i) != 0 {
				modes = append(modes, m)
			}
		}
		for _, ks := range []bool{false, true} {
			for _, unwind := range []string{"fp", "dwarf", "auto"} {
				s := Settings{Modes: modes, Duration: 10 * time.Second, SampleRate: 99, Unwind: unwind, KernelStacks: ks}
				argv, err := BuildArgv(ArgvSpec{Binary: "/b/perf-agent", PID: 7, Settings: s, Staging: "/s", GPUShim: "/shim.so", CaptureID: "x/001"})
				if err != nil {
					t.Fatal(err)
				}
				if argv[0] != "/b/perf-agent" {
					t.Fatalf("argv[0] = %q", argv[0])
				}
				for i, a := range argv[1:] {
					if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
						t.Fatalf("%v: single-dash argument %q", argv, a)
					}
					if name, ok := strings.CutPrefix(a, "--"); ok {
						f, found := known[name]
						if !found {
							t.Fatalf("%v: flag %s not in Flags", argv, a)
						}
						if f.Bool {
							if next := i + 2; next < len(argv) && !strings.HasPrefix(argv[next], "--") {
								t.Fatalf("%v: bool flag %s followed by value %q", argv, a, argv[next])
							}
						}
					}
					if slices.Contains(neverEmitted, a) {
						t.Fatalf("%v: emitted %s", argv, a)
					}
				}
				for _, must := range []string{"--no-debuginfod", "--duration"} {
					if !slices.Contains(argv, must) {
						t.Fatalf("%v: missing %s", argv, must)
					}
				}
				for _, c := range []struct {
					m    Mode
					flag string
				}{{ModeCPU, "--profile"}, {ModeOffCPU, "--offcpu"}, {ModeGPU, "--gpu"}} {
					if got, want := slices.Contains(argv, c.flag), slices.Contains(modes, c.m); got != want {
						t.Fatalf("%v: %s present=%v want %v", argv, c.flag, got, want)
					}
				}
				if slices.Contains(modes, ModeGPU) {
					i := slices.Index(argv, "--gpu-shim")
					j := slices.Index(argv, "--gpu-executions-output")
					if i < 0 || argv[i+1] != "/shim.so" || j < 0 || argv[j+1] != "/s/gpu.execlog" {
						t.Fatalf("%v: gpu shim/execlog", argv)
					}
				} else if slices.Contains(argv, "--gpu-shim") {
					t.Fatalf("%v: gpu-shim without gpu", argv)
				}
				if slices.Contains(argv, "--kernel-stacks") != ks {
					t.Fatalf("%v: kernel-stacks", argv)
				}
			}
		}
	}
}

func TestBuildArgvExact(t *testing.T) {
	got, err := BuildArgv(ArgvSpec{
		Binary: "/b/perf-agent", PID: 41234,
		Settings:  Settings{Modes: []Mode{ModeCPU}, Duration: 10 * time.Second, SampleRate: 99, Unwind: "auto"},
		Staging:   "/p/captures/s/.staging-ab",
		CaptureID: "s/001",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/b/perf-agent", "--pid", "41234", "--duration", "10s", "--sample-rate", "99", "--unwind", "auto",
		"--no-debuginfod", "--profile", "--profile-output", "/p/captures/s/.staging-ab/cpu.pb.gz",
		"--tag", "perf_agent_mcp_capture=s/001"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestCanonicalOrdersModes(t *testing.T) {
	got := Settings{Modes: []Mode{ModeGPU, ModeCPU}}.Canonical().Modes
	if want := []Mode{ModeCPU, ModeGPU}; !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestExpectedFiles(t *testing.T) {
	got := ExpectedFiles([]Mode{ModeGPU, ModeCPU})
	if want := []string{"cpu.pb.gz", "gpu.pb.gz", ExeclogFile}; !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestBuildArgvExactOffCPUAndGPU(t *testing.T) {
	base := ArgvSpec{Binary: "/b/pa", PID: 5, Staging: "/s", GPUShim: "/shim.so", CaptureID: "c/1"}
	cases := []struct {
		name string
		mode []Mode
		ks   bool
		want []string
	}{
		{"offcpu", []Mode{ModeOffCPU}, false, []string{"/b/pa", "--pid", "5", "--duration", "5s", "--sample-rate", "49", "--unwind", "fp",
			"--no-debuginfod", "--offcpu", "--offcpu-output", "/s/offcpu.pb.gz", "--tag", "perf_agent_mcp_capture=c/1"}},
		{"gpu", []Mode{ModeGPU}, true, []string{"/b/pa", "--pid", "5", "--duration", "5s", "--sample-rate", "49", "--unwind", "fp",
			"--no-debuginfod", "--gpu", "--gpu-shim", "/shim.so", "--gpu-output", "/s/gpu.pb.gz",
			"--gpu-executions-output", "/s/gpu.execlog", "--kernel-stacks", "--tag", "perf_agent_mcp_capture=c/1"}},
	}
	for _, c := range cases {
		a := base
		a.Settings = Settings{Modes: c.mode, Duration: 5 * time.Second, SampleRate: 49, Unwind: "fp", KernelStacks: c.ks}
		got, err := BuildArgv(a)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s:\ngot  %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestBuildArgvRejectsBadUnwind(t *testing.T) {
	for _, u := range []string{"", "FP", "--evil", "fp,dwarf"} {
		_, err := BuildArgv(ArgvSpec{Binary: "/b", Settings: Settings{Modes: []Mode{ModeCPU}, Unwind: u}})
		e, ok := errors.AsType[*Error](err)
		if !ok || e.Code != CodeInvalidArgument {
			t.Errorf("unwind %q: err = %v", u, err)
		}
	}
}

func TestPerfAgentEnvExact(t *testing.T) {
	want := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LC_ALL=C"}
	if !slices.Equal(PerfAgentEnv, want) {
		t.Fatalf("PerfAgentEnv = %q, want %q", PerfAgentEnv, want)
	}
	for _, kv := range PerfAgentEnv {
		if k, _, _ := strings.Cut(kv, "="); k != "PATH" && k != "LC_ALL" {
			t.Errorf("unexpected environment key %q", k)
		}
	}
}
