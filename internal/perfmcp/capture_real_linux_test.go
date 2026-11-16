package perfmcp

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/capture/capturetest"
)

// The variables of the real-capture tests. An unset PERF_AGENT_BIN (or,
// for the GPU test, PERF_AGENT_GPU_SHIM) skips; once set, anything that
// keeps the test from running a real capture fails it.
const (
	perfAgentBinEnv = "PERF_AGENT_BIN"
	gpuShimEnv      = "PERF_AGENT_GPU_SHIM"
	// cudaWorkloadEnv overrides the CUDA workload, which is otherwise
	// nvidia/testdata/cuda_workload beside the shim.
	cudaWorkloadEnv = "PERF_AGENT_CUDA_WORKLOAD"
)

// realOpts is what a real capture test needs of the server.
type realOpts struct {
	kernelStacks bool
	gpuShim      string
}

// realServer is a server over the setcap'd perf-agent PERF_AGENT_BIN names,
// served over pipes and initialized. It skips only when the variable is
// unset: a binary that fails verification, a test process that may not
// capture, or a server that comes up with capture disabled fails the test
// with the reason. At cleanup it fails the test for any launched command
// still alive.
func realServer(t *testing.T, o realOpts) (*jobEnv, *testClient) {
	t.Helper()
	bin := os.Getenv(perfAgentBinEnv)
	if bin == "" {
		t.Skip(perfAgentBinEnv + " is not set")
	}
	b, err := capture.VerifyBinary(bin)
	if err != nil {
		t.Fatalf("%s=%s cannot be used: %v", perfAgentBinEnv, bin, err)
	}
	status, err := capture.ReadSelfStatus()
	if err != nil {
		t.Fatalf("cannot read this test's own status: %v", err)
	}
	if err := capture.CheckSelf(status); err != nil {
		t.Fatalf("this test process may not run captures: %v", err)
	}
	if err := capture.CheckBounding(status, b.Caps); err != nil {
		t.Fatalf("this test process may not run %s: %v", bin, err)
	}
	if o.kernelStacks && !b.Caps.Has(capture.CapSyslog) {
		t.Fatalf("%s lacks cap_syslog, so kernel frames would come out unsymbolized: %s", perfAgentBinEnv, capture.SetcapLine)
	}
	cfg := testConfig(t)
	cfg.Capture = CaptureConfig{PerfAgent: bin, GPUShim: o.gpuShim}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := &jobEnv{t: t, s: s, profiles: cfg.ProfilesDir, faults: newFaultGates()}
	t.Cleanup(e.noLeftovers)
	t.Cleanup(func() { _ = s.Close() })
	if s.captureDisabled != "" {
		t.Fatalf("capture is disabled: %s", s.captureDisabled)
	}
	t.Logf("perf-agent %s (%s %s)", bin, b.Version, b.Revision)
	return e, attachClient(t, s)
}

// profileOf is the capture's opened profile of mode.
func profileOf(t *testing.T, r captureResult, mode capture.Mode) *openResult {
	t.Helper()
	i := slices.IndexFunc(r.Profiles, func(p profileResult) bool { return p.Mode == string(mode) })
	if i < 0 {
		t.Fatalf("capture %s has no %s profile: %+v", r.Capture.ID, mode, r.Profiles)
	}
	if p := r.Profiles[i]; p.Open == nil {
		t.Fatalf("the %s profile %s did not open: %s", mode, p.Path, p.OpenError)
	}
	return r.Profiles[i].Open
}

// topFunctions are the first rows of top by cumulative value over the
// default view of o, of domain when it is not "".
func topFunctions(t *testing.T, c *testClient, o *openResult, domain string, limit int) []functionRow {
	t.Helper()
	args := map[string]any{"handle": o.Handle, "view": o.DefaultView, "rows": "functions", "limit": limit}
	if domain != "" {
		args["domain"] = domain
	}
	var top topResult[functionRow]
	c.callOK("top", args, &top)
	return top.Rows
}

// logRows logs rows as an agent would read them.
func logRows(t *testing.T, what string, rows []functionRow) {
	t.Helper()
	for i, r := range rows {
		t.Logf("%s %2d. %-60s cum %s self %s (%s) [%s]", what, i+1, r.Name, r.Cumulative.Value, r.Self.Value, r.Cumulative.Unit, r.Domain)
	}
}

// rowNamed reports whether some row's name contains any of names.
func rowNamed(rows []functionRow, names ...string) bool {
	return slices.ContainsFunc(rows, func(r functionRow) bool {
		return slices.ContainsFunc(names, func(n string) bool { return strings.Contains(r.Name, n) })
	})
}

// A real capture of a launched workload spinning in user space, as an
// agent runs one: capture, capture_wait until done, then top over the
// opened profile, whose rows include the workload's own function.
func TestRealCapture(t *testing.T) {
	_, c := realServer(t, realOpts{})
	r, _ := waitResult(t, c, startCapture(t, c, map[string]any{
		"command": []any{capturetest.Workload(t), "busy", "5s"}, "duration": "2s",
	}))
	if r.Derived != derivedReady || r.Capture.StoppedBy != "duration" {
		t.Fatalf("capture %+v, derived %q (%s)", r.Capture, r.Derived, r.DeriveError)
	}
	t.Logf("capture %s/%s: profiled %s, attach latency %s, perf-agent %+v", r.Capture.Series, r.Capture.ID,
		r.Capture.ProfiledDuration, r.Capture.AttachLatency, r.Capture.PerfAgent)
	rows := topFunctions(t, c, profileOf(t, r, capture.ModeCPU), "", 10)
	logRows(t, "top", rows)
	if !rowNamed(rows, "main.spin") {
		t.Fatal("no top row is the workload's main.spin")
	}
}

// With kernel_stacks the real capture notes that the file holds kernel
// addresses, and a workload reading /dev/zero has symbolized kernel
// frames: cap_syslog let perf-agent read the kernel's symbols.
func TestRealKernelStacks(t *testing.T) {
	_, c := realServer(t, realOpts{kernelStacks: true})
	r, _ := waitResult(t, c, startCapture(t, c, map[string]any{
		"command": []any{capturetest.Workload(t), "read-zero", "5s"}, "duration": "2s", "kernel_stacks": true,
	}))
	if r.Derived != derivedReady || !r.Capture.KernelStacks {
		t.Fatalf("capture %+v, derived %q (%s)", r.Capture, r.Derived, r.DeriveError)
	}
	if !slices.Contains(r.Notes, kernelNote) {
		t.Errorf("notes %q lack %q", r.Notes, kernelNote)
	}
	rows := topFunctions(t, c, profileOf(t, r, capture.ModeCPU), "kernel", 10)
	logRows(t, "kernel", rows)
	if !slices.ContainsFunc(rows, func(r functionRow) bool { return !strings.HasPrefix(r.Name, "0x") }) {
		t.Fatalf("no symbolized kernel frame among %d kernel rows", len(rows))
	}
}

// A real GPU capture of a CUDA workload with the CUPTI shim injected:
// the GPU profile has the workload's kernels under a [gpu:launch] frame,
// and a second capture in the series compared with the first has a
// compare.
func TestRealGPUCapture(t *testing.T) {
	if os.Getenv(perfAgentBinEnv) == "" {
		t.Skip(perfAgentBinEnv + " is not set")
	}
	shim := os.Getenv(gpuShimEnv)
	if shim == "" {
		t.Skip(gpuShimEnv + " is not set")
	}
	if _, err := capture.VerifyShim(shim); err != nil {
		t.Fatalf("%s=%s cannot be used: %v", gpuShimEnv, shim, err)
	}
	workload := cmp.Or(os.Getenv(cudaWorkloadEnv), filepath.Join(filepath.Dir(shim), "nvidia", "testdata", "cuda_workload"))
	if err := capture.CheckCommandFile(workload); err != nil {
		t.Fatalf("the CUDA workload %s (%s) cannot run: %v", workload, cudaWorkloadEnv, err)
	}
	_, c := realServer(t, realOpts{gpuShim: shim})
	cmd := []any{workload, "20000", "200"}

	base, _ := waitResult(t, c, startCapture(t, c, map[string]any{
		"command": cmd, "modes": []any{"cpu", "gpu"}, "duration": "3s", "series": "gpu", "label": "base",
	}))
	if base.Derived != derivedReady {
		t.Fatalf("base %+v, derived %q (%s)", base.Capture, base.Derived, base.DeriveError)
	}
	t.Logf("base %s/%s: modes %v, profiled %s, stopped by %s, notes %q", base.Capture.Series, base.Capture.ID,
		base.Capture.Modes, base.Capture.ProfiledDuration, base.Capture.StoppedBy, base.Notes)
	gpu := profileOf(t, base, capture.ModeGPU)
	kernels := topFunctions(t, c, gpu, "gpu-kernel", 10)
	logRows(t, "gpu kernel", kernels)
	if !rowNamed(kernels, "perfagent_axpy", "perfagent_scale") {
		t.Fatal("no GPU kernel row is perfagent_axpy or perfagent_scale")
	}

	// A launch with no CPU sample near it hangs under [gpu:launch
	// unsampled]; one with a sampled CPU stack hangs under [gpu:launch].
	var nodes topResult[nodeRow]
	c.callOK("top", map[string]any{"handle": gpu.Handle, "view": gpu.DefaultView, "rows": "nodes", "domain": "gpu-kernel", "limit": 50}, &nodes)
	launched := false
	for _, n := range nodes.Rows {
		var path pathResult
		c.callOK("path", map[string]any{"handle": gpu.Handle, "view": gpu.DefaultView, "node": n.Node}, &path)
		var names []string
		for _, f := range path.Frames {
			names = append(names, f.Name)
		}
		t.Logf("path to %s (cum %s): %s", n.Name, n.Cumulative.Value, strings.Join(names, " -> "))
		if slices.Contains(names, "[gpu:launch]") {
			launched = true
			break
		}
	}
	if !launched {
		t.Fatalf("no path to any of %d GPU kernel nodes has a [gpu:launch] frame", len(nodes.Rows))
	}

	cand, _ := waitResult(t, c, startCapture(t, c, map[string]any{
		"command": cmd, "series": "gpu", "label": "cand", "compare_with": base.Capture.ID,
	}))
	if cand.Derived != derivedReady || cand.Capture.CompareWith != base.Capture.ID {
		t.Fatalf("cand %+v, derived %q (%s)", cand.Capture, cand.Derived, cand.DeriveError)
	}
	if cand.Compare == nil || cand.Compare.Base.ID != base.Capture.ID || len(cand.Compare.Views) == 0 {
		t.Fatalf("cand compare %+v", cand.Compare)
	}
	t.Logf("compare %s -> %s: comparability %v, notes %q", cand.Compare.Base.ID, cand.Compare.New.ID, cand.Compare.Comparability, cand.Compare.Notes)
	for _, v := range cand.Compare.Views {
		t.Logf("view %s (%s): base total %s, new total %s, %d rows", v.View, v.Unit, v.BaseTotal, v.NewTotal, len(v.Rows))
		for i, row := range v.Rows[:min(len(v.Rows), 5)] {
			t.Logf("  %d. %s: %v, cum %s -> %s", i+1, row.Name, row.Interpretation, row.BaseCum, row.NewCum)
		}
	}
	if !slices.ContainsFunc(cand.Compare.Views, func(v compareView) bool {
		return slices.ContainsFunc(v.Rows, func(r diffNodeRow) bool { return strings.Contains(r.Name, "[gpu:kernel:") })
	}) {
		t.Error("no compare view has a GPU kernel row")
	}
}
