# Namespace-aware `--pid` + k8s pprof labels — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `--pid <N>` work from inside a Kubernetes pod (translate via /proc/<pid>/status NSpid) and attach k8s identity labels (pod_uid, container_id, plus best-effort downward-API names) to every emitted pprof sample. Library mode gets `WithLabels` and `WithLabelEnricher` options; CLI uses defaults unchanged.

**Architecture:** Two new internal packages (`internal/nspid` for namespace translation, `internal/k8slabels` for cgroup parsing + env-var reading), `Labels map[string]string` plumbed through `pprof.BuildersOptions` to every emitted `profile.Sample`, and a label-enrichment hook in `perfagent.Options` so library callers can override or extend the defaults. No BPF changes, no new external dependencies, no k8s API client. Cgroup v2 only.

**Tech Stack:** Go 1.26, stdlib only (`os`, `strings`, `strconv`, `regexp`), Linux-only. Existing `pprof.NewProfileBuilders` is the single integration point at the pprof layer.

**Companion docs:**
- `docs/superpowers/specs/2026-04-30-k8s-pid-namespace-and-pprof-labels-design.md` — design spec.

---

## File Structure

**New files:**
- `internal/nspid/nspid.go` — `Translate(pidInOurView int) (hostPID int, err error)`. Reads `/proc/<pid>/status`, parses `NSpid:` line, returns the outermost (host) PID.
- `internal/nspid/nspid_test.go` — table tests using a temp dir as a fake `/proc`.
- `internal/k8slabels/cgroup_parse.go` — pure functions: `parseV2CgroupPath([]byte) (string, bool)`, `extractPodUID(path) string`, `extractContainerID(path) string`. No I/O.
- `internal/k8slabels/cgroup_parse_test.go` — table tests covering containerd, criO, docker, kubelet-direct, non-k8s paths, hybrid v1+v2 file content.
- `internal/k8slabels/env.go` — `downwardAPIEnv() map[string]string`. Reads `POD_NAME`, `POD_NAMESPACE`, `CONTAINER_NAME`. Skips empty/unset.
- `internal/k8slabels/env_test.go` — sets/unsets env vars via `t.Setenv`, asserts the map contents.
- `internal/k8slabels/k8slabels.go` — `FromPID(procRoot string, hostPID int) (map[string]string, error)`. Composes cgroup labels + env labels.
- `internal/k8slabels/k8slabels_test.go` — uses fake /proc fixtures.

**Modified files:**
- `pprof/pprof.go` — add `Labels map[string]string` field to `BuildersOptions`; in `newSample`, clone the map onto every emitted sample's `Label`.
- `pprof/pprof_test.go` — one new test that asserts a sample gets the configured labels.
- `profile/profiler.go` — `NewProfiler` gains a trailing `labels map[string]string` param; passes it to `BuildersOptions`.
- `offcpu/profiler.go` — same: `NewProfiler` gains a trailing `labels` param.
- `unwind/dwarfagent/common.go` — `newSession` gains a trailing `labels` param; stored on `*session`; passed to `BuildersOptions` in the collect path.
- `unwind/dwarfagent/agent.go` — `NewProfilerWithMode` and `NewProfilerWithHooks` gain a trailing `labels` param; passed to `newSession`.
- `unwind/dwarfagent/offcpu.go` — `NewOffCPUProfilerWithHooks` and `NewOffCPUProfiler` same.
- `perfagent/options.go` — new `WithLabels(map[string]string)` and `WithLabelEnricher(func(int) map[string]string)` options; default enricher = `internal/k8slabels.FromPID`.
- `perfagent/agent.go` — at `Start`, after capability promotion and before profiler construction: translate `config.PID` via `internal/nspid.Translate`, run the enricher, merge with `WithLabels`, pass the result down to whichever profiler is built.
- `main.go` — no change. The CLI sets `WithPID(pid)` and the library handles everything.

**Test layout note:** All packages above have unit tests that run without root. The existing root-gated integration tests in `test/` are not modified by this plan; an optional integration verification step is in Task 11 but the test itself is gated to skip in non-pod environments.

---

## Task 1: `internal/nspid` — host PID translation

**Files:**
- Create: `internal/nspid/nspid.go`
- Test: `internal/nspid/nspid_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/nspid/nspid_test.go
package nspid

import (
	"os"
	"path/filepath"
	"testing"
)

func writeStatus(t *testing.T, root string, pid int, contents string) {
	t.Helper()
	dir := filepath.Join(root, "proc", "1")
	_ = pid // pid arg is for clarity; we always write under /proc/1 for the synthetic fixture
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTranslate_HostNamespace(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, 1, "Name:\tcat\nNSpid:\t12345\n")
	got, err := translateAt(filepath.Join(root, "proc"), 1)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got != 12345 {
		t.Errorf("hostPID = %d, want 12345", got)
	}
}

func TestTranslate_SharedPodNamespace(t *testing.T) {
	root := t.TempDir()
	// Two-column NSpid: outermost (host) first, then in-pod ns.
	writeStatus(t, root, 1, "Name:\tjava\nNSpid:\t12345\t5\n")
	got, err := translateAt(filepath.Join(root, "proc"), 1)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got != 12345 {
		t.Errorf("hostPID = %d, want 12345 (outermost column)", got)
	}
}

func TestTranslate_MissingProcess(t *testing.T) {
	root := t.TempDir()
	// /proc/1/status doesn't exist
	_, err := translateAt(filepath.Join(root, "proc"), 1)
	if err == nil {
		t.Fatal("expected error for missing /proc/<pid>/status")
	}
}

func TestTranslate_MissingNSpidLine(t *testing.T) {
	root := t.TempDir()
	writeStatus(t, root, 1, "Name:\told_kernel\n") // no NSpid line at all
	_, err := translateAt(filepath.Join(root, "proc"), 1)
	if err == nil {
		t.Fatal("expected error for missing NSpid line")
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
cd /home/diego/github/perf-agent/.worktrees/k8s-pid-labels
GOTOOLCHAIN=auto go test ./internal/nspid/... 2>&1 | tail -5
```
Expected: build failure (`undefined: translateAt`).

- [ ] **Step 3: Write the implementation**

```go
// internal/nspid/nspid.go
// Package nspid translates a PID from any Linux PID namespace into the
// outermost (host) kernel PID. Required when perf-agent runs in a sidecar
// or other non-host PID namespace: the user-visible PID number is local to
// that namespace, but BPF (and perf_event_open) operate on host PIDs.
package nspid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Translate maps a PID visible from the agent's own /proc to the outermost
// (host) kernel PID by reading /proc/<pid>/status's NSpid: line.
//
// If NSpid contains a single column, the agent is already in the host PID
// namespace and the input is returned unchanged.
func Translate(pidInOurView int) (int, error) {
	return translateAt("/proc", pidInOurView)
}

func translateAt(procRoot string, pid int) (int, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("nspid: invalid pid %d", pid)
	}
	statusPath := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, fmt.Errorf("nspid: read %s: %w (process exited or namespace mismatch?)", statusPath, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		if len(fields) == 0 {
			break
		}
		host, perr := strconv.Atoi(fields[0])
		if perr != nil {
			return 0, fmt.Errorf("nspid: parse host pid in %q: %w", line, perr)
		}
		return host, nil
	}
	return 0, errors.New("nspid: no NSpid: line in status (kernel < 4.1 or no PID namespace support)")
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/nspid/... -count=1 2>&1 | tail -5
```
Expected: `ok  github.com/dpsoft/perf-agent/internal/nspid`.

- [ ] **Step 5: Commit**

```bash
git add internal/nspid/
git commit -m "internal/nspid: translate PID to host kernel PID via /proc/<pid>/status NSpid"
```

---

## Task 2: `internal/k8slabels` — cgroup path parsing (pure)

**Files:**
- Create: `internal/k8slabels/cgroup_parse.go`
- Test: `internal/k8slabels/cgroup_parse_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/k8slabels/cgroup_parse_test.go
package k8slabels

import "testing"

func TestParseV2CgroupPath(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantOK  bool
	}{
		{
			name:   "pure v2 single line",
			input:  "0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope\n",
			want:   "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope",
			wantOK: true,
		},
		{
			name: "hybrid v1+v2: only the 0:: line is used",
			input: "12:devices:/kubepods/burstable/pod-abc/container-xyz\n" +
				"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope\n",
			want:   "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234.slice/cri-containerd-abc.scope",
			wantOK: true,
		},
		{
			name:   "v1 only: no 0:: line",
			input:  "12:devices:/some/v1/path\n2:cpu,cpuacct:/foo\n",
			want:   "",
			wantOK: false,
		},
		{
			name:   "empty file",
			input:  "",
			want:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseV2CgroupPath([]byte(tc.input))
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("parseV2CgroupPath() = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestExtractPodUID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{
			// systemd-style with dashes
			path: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc.scope",
			want: "12345678-1234-1234-1234-123456789abc",
		},
		{
			// cgroupfs-style without dashes
			path: "/kubepods/burstable/pod12345678-1234-1234-1234-123456789abc/cri-containerd-abc.scope",
			want: "12345678-1234-1234-1234-123456789abc",
		},
		{
			// no kubepods → no UID
			path: "/system.slice/myservice.scope",
			want: "",
		},
		{
			// kubepods but no podXXX segment
			path: "/kubepods.slice/kubepods-besteffort.slice",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := extractPodUID(tc.path)
			if got != tc.want {
				t.Errorf("extractPodUID(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

func TestExtractContainerID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{
			path: "/kubepods.slice/.../cri-containerd-abc123def456.scope",
			want: "abc123def456",
		},
		{
			path: "/kubepods.slice/.../crio-9f8e7d6c5b4a.scope",
			want: "9f8e7d6c5b4a",
		},
		{
			path: "/kubepods.slice/.../docker-deadbeef.scope",
			want: "deadbeef",
		},
		{
			path: "/kubepods/burstable/pod-abc/abc123def456",
			want: "abc123def456", // raw container-id leaf (cgroupfs driver)
		},
		{
			path: "/kubepods.slice/kubepods-burstable.slice", // no leaf yet
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got := extractContainerID(tc.path)
			if got != tc.want {
				t.Errorf("extractContainerID(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/k8slabels/... 2>&1 | tail -5
```
Expected: build failure (`undefined: parseV2CgroupPath`, etc.).

- [ ] **Step 3: Write the implementation**

```go
// internal/k8slabels/cgroup_parse.go
package k8slabels

import (
	"path/filepath"
	"regexp"
	"strings"
)

// parseV2CgroupPath scans a /proc/<pid>/cgroup file body and returns the
// cgroup v2 path (the line beginning with "0::"). Hybrid hosts (cgroup v1
// + v2 mounted) include both formats; pure v1 hosts have no 0:: line.
func parseV2CgroupPath(body []byte) (string, bool) {
	for line := range strings.SplitSeq(string(body), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimRight(rest, "\r"), true
		}
	}
	return "", false
}

// podUIDRE matches the pod-UID segment in a kubepods cgroup path. Two
// flavors are produced by kubelet drivers:
//
//   - cgroupfs driver: pod<UID> with dashes (e.g. pod12345678-1234-...)
//   - systemd driver: kubepods-burstable-pod<UID>.slice with underscores
//     in place of dashes (e.g. ...pod12345678_1234_1234_1234_...slice)
//
// The regex captures both variants; canonicalisation (underscores → dashes)
// happens after extraction.
var podUIDRE = regexp.MustCompile(`pod([0-9a-fA-F]{8}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{4}[-_][0-9a-fA-F]{12})`)

func extractPodUID(cgroupPath string) string {
	if !strings.Contains(cgroupPath, "kubepods") {
		return ""
	}
	m := podUIDRE.FindStringSubmatch(cgroupPath)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "_", "-")
}

// containerIDRuntimePrefixes is the set of leaf-segment prefixes used by
// supported container runtimes when running under k8s. Order matters only
// for documentation: each is checked exhaustively.
var containerIDRuntimePrefixes = []string{
	"cri-containerd-",
	"crio-",
	"docker-",
}

func extractContainerID(cgroupPath string) string {
	leaf := filepath.Base(cgroupPath)
	if leaf == "" || leaf == "/" {
		return ""
	}
	// Strip the .scope suffix (systemd driver) before checking prefixes.
	stripped := strings.TrimSuffix(leaf, ".scope")
	for _, prefix := range containerIDRuntimePrefixes {
		if rest, ok := strings.CutPrefix(stripped, prefix); ok {
			return rest
		}
	}
	// cgroupfs driver: leaf is the raw container ID. Heuristic: looks like
	// a hex blob (≥12 chars, all hex). Avoids matching "kubepods-burstable.slice".
	if len(stripped) >= 12 && isHex(stripped) {
		return stripped
	}
	return ""
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/k8slabels/... -count=1 -run "TestParseV2|TestExtractPod|TestExtractContainer" 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/k8slabels/cgroup_parse.go internal/k8slabels/cgroup_parse_test.go
git commit -m "internal/k8slabels: cgroup v2 path + pod UID + container ID parsers"
```

---

## Task 3: `internal/k8slabels` — downward-API env-var reader

**Files:**
- Create: `internal/k8slabels/env.go`
- Test: `internal/k8slabels/env_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/k8slabels/env_test.go
package k8slabels

import (
	"reflect"
	"testing"
)

func TestDownwardAPIEnv_AllSet(t *testing.T) {
	t.Setenv("POD_NAME", "my-app-7d8f5c-xkz2q")
	t.Setenv("POD_NAMESPACE", "production")
	t.Setenv("CONTAINER_NAME", "my-app")
	got := downwardAPIEnv()
	want := map[string]string{
		"pod_name":       "my-app-7d8f5c-xkz2q",
		"namespace":      "production",
		"container_name": "my-app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("downwardAPIEnv() = %v, want %v", got, want)
	}
}

func TestDownwardAPIEnv_AllUnset(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	got := downwardAPIEnv()
	if len(got) != 0 {
		t.Errorf("downwardAPIEnv() with all empty = %v, want empty map", got)
	}
}

func TestDownwardAPIEnv_PartialSet(t *testing.T) {
	t.Setenv("POD_NAME", "my-app-xkz2q")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "my-app")
	got := downwardAPIEnv()
	want := map[string]string{
		"pod_name":       "my-app-xkz2q",
		"container_name": "my-app",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("downwardAPIEnv() = %v, want %v", got, want)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/k8slabels/... -count=1 -run TestDownwardAPI 2>&1 | tail -5
```
Expected: build failure (`undefined: downwardAPIEnv`).

- [ ] **Step 3: Write the implementation**

```go
// internal/k8slabels/env.go
package k8slabels

import "os"

// downwardAPIEnv returns labels read from canonical Kubernetes downward-API
// environment variables. Unset or empty variables are silently skipped, so
// callers can apply this function on any host without producing spurious
// empty-string labels.
func downwardAPIEnv() map[string]string {
	out := make(map[string]string, 3)
	for envName, labelKey := range map[string]string{
		"POD_NAME":       "pod_name",
		"POD_NAMESPACE":  "namespace",
		"CONTAINER_NAME": "container_name",
	} {
		if v := os.Getenv(envName); v != "" {
			out[labelKey] = v
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/k8slabels/... -count=1 -run TestDownwardAPI 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/k8slabels/env.go internal/k8slabels/env_test.go
git commit -m "internal/k8slabels: downward-API env-var reader"
```

---

## Task 4: `internal/k8slabels` — `FromPID` assembly

**Files:**
- Create: `internal/k8slabels/k8slabels.go`
- Test: `internal/k8slabels/k8slabels_test.go`

- [ ] **Step 1: Write failing tests**

```go
// internal/k8slabels/k8slabels_test.go
package k8slabels

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCgroup(t *testing.T, procRoot string, pid int, content string) {
	t.Helper()
	dir := filepath.Join(procRoot, "1") // we use pid=1 in the fake /proc
	_ = pid
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFromPID_KubepodsContainerd(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir()
	writeCgroup(t, root, 1,
		"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc123def456.scope\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if got["pod_uid"] != "12345678-1234-1234-1234-123456789abc" {
		t.Errorf("pod_uid = %q", got["pod_uid"])
	}
	if got["container_id"] != "abc123def456" {
		t.Errorf("container_id = %q", got["container_id"])
	}
	if got["cgroup_path"] == "" {
		t.Errorf("cgroup_path missing")
	}
	if _, ok := got["pod_name"]; ok {
		t.Errorf("pod_name should not be set when env unset")
	}
}

func TestFromPID_NotKubernetes(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir()
	writeCgroup(t, root, 1, "0::/user.slice/user-1000.slice/session-1.scope\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if got["cgroup_path"] != "/user.slice/user-1000.slice/session-1.scope" {
		t.Errorf("cgroup_path = %q", got["cgroup_path"])
	}
	if _, ok := got["pod_uid"]; ok {
		t.Errorf("pod_uid should not be set on non-k8s host")
	}
	if _, ok := got["container_id"]; ok {
		t.Errorf("container_id should not be set on non-k8s host")
	}
}

func TestFromPID_V1Only(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir()
	writeCgroup(t, root, 1, "12:devices:/some/v1/path\n2:cpu,cpuacct:/foo\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("v1-only host should produce no labels, got %v", got)
	}
}

func TestFromPID_WithDownwardAPI(t *testing.T) {
	t.Setenv("POD_NAME", "my-app-xkz2q")
	t.Setenv("POD_NAMESPACE", "production")
	t.Setenv("CONTAINER_NAME", "my-app")
	root := t.TempDir()
	writeCgroup(t, root, 1,
		"0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod12345678_1234_1234_1234_123456789abc.slice/cri-containerd-abc.scope\n")
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID: %v", err)
	}
	if got["pod_name"] != "my-app-xkz2q" {
		t.Errorf("pod_name = %q", got["pod_name"])
	}
	if got["namespace"] != "production" {
		t.Errorf("namespace = %q", got["namespace"])
	}
	if got["container_name"] != "my-app" {
		t.Errorf("container_name = %q", got["container_name"])
	}
}

func TestFromPID_ProcessGone(t *testing.T) {
	t.Setenv("POD_NAME", "")
	t.Setenv("POD_NAMESPACE", "")
	t.Setenv("CONTAINER_NAME", "")
	root := t.TempDir() // no /1 dir created
	got, err := FromPID(root, 1)
	if err != nil {
		t.Fatalf("FromPID should not error when /proc/<pid>/cgroup is missing: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("missing cgroup file should produce no labels, got %v", got)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

```bash
GOTOOLCHAIN=auto go test ./internal/k8slabels/... -count=1 -run TestFromPID 2>&1 | tail -5
```
Expected: build failure (`undefined: FromPID`).

- [ ] **Step 3: Write the implementation**

```go
// internal/k8slabels/k8slabels.go
// Package k8slabels derives Kubernetes identity labels from a target
// process's cgroup path and the agent's own downward-API environment.
//
// All work is read-only file I/O against /proc and the agent's process
// environment — no Kubernetes API calls, no kubelet, no container runtime
// sockets. Cgroup v2 is required; v1-only hosts produce no k8s labels.
package k8slabels

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// FromPID reads /proc/<hostPID>/cgroup, parses the v2 path, derives k8s
// identity labels, and merges in any present downward-API env labels.
//
// procRoot is "/proc" in production; tests pass a temp dir.
//
// On a host where /proc/<hostPID>/cgroup doesn't exist (process exited),
// FromPID returns an empty map and a nil error — the caller's BPF setup
// will surface the "process gone" error on its own path.
func FromPID(procRoot string, hostPID int) (map[string]string, error) {
	if hostPID <= 0 {
		return nil, fmt.Errorf("k8slabels: invalid pid %d", hostPID)
	}
	out := make(map[string]string, 6)

	cgroupPath := filepath.Join(procRoot, strconv.Itoa(hostPID), "cgroup")
	body, err := os.ReadFile(cgroupPath)
	switch {
	case err == nil:
		// proceed
	case errors.Is(err, os.ErrNotExist):
		// process gone or non-Linux fixture; merge env-only labels and return.
		for k, v := range downwardAPIEnv() {
			out[k] = v
		}
		return out, nil
	default:
		return nil, fmt.Errorf("k8slabels: read %s: %w", cgroupPath, err)
	}

	if v2Path, ok := parseV2CgroupPath(body); ok {
		out["cgroup_path"] = v2Path
		if uid := extractPodUID(v2Path); uid != "" {
			out["pod_uid"] = uid
		}
		if cid := extractContainerID(v2Path); cid != "" {
			out["container_id"] = cid
		}
	}

	for k, v := range downwardAPIEnv() {
		out[k] = v
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to confirm they pass**

```bash
GOTOOLCHAIN=auto go test ./internal/k8slabels/... -count=1 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/k8slabels/k8slabels.go internal/k8slabels/k8slabels_test.go
git commit -m "internal/k8slabels: FromPID assembles cgroup + downward-API labels"
```

---

## Task 5: `pprof` — `Labels` field + per-sample plumbing

**Files:**
- Modify: `pprof/pprof.go`
- Test: `pprof/pprof_test.go`

- [ ] **Step 1: Write failing test**

Append to `pprof/pprof_test.go`:

```go
func TestNewProfileBuilders_StaticLabelsOnSample(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Labels: map[string]string{
			"pod_uid":      "12345678-1234-1234-1234-123456789abc",
			"container_id": "abc123def456",
		},
	})
	builders.AddSample(&ProfileSample{
		Pid:         42,
		Aggregation: SampleAggregated,
		SampleType:  SampleTypeCpu,
		Stack:       []Frame{FrameFromName("main.foo")},
		Value:       1,
	})
	for _, b := range builders.Builders {
		if len(b.Profile.Sample) == 0 {
			t.Fatal("no samples emitted")
		}
		got := b.Profile.Sample[0].Label
		if got["pod_uid"] == nil || got["pod_uid"][0] != "12345678-1234-1234-1234-123456789abc" {
			t.Errorf("pod_uid label missing or wrong: %v", got)
		}
		if got["container_id"] == nil || got["container_id"][0] != "abc123def456" {
			t.Errorf("container_id label missing or wrong: %v", got)
		}
	}
}
```

- [ ] **Step 2: Run test to confirm it fails**

```bash
GOTOOLCHAIN=auto go test ./pprof/... -count=1 -run TestNewProfileBuilders_StaticLabelsOnSample 2>&1 | tail -5
```
Expected: build failure (`unknown field Labels in struct literal`) or test failure (no Labels field, no labels on samples).

- [ ] **Step 3: Add the field + plumb through `newSample`**

In `pprof/pprof.go`, modify `BuildersOptions`:

```go
type BuildersOptions struct {
	SampleRate    int64
	PerPIDProfile bool
	Comments      []string          // Profile-level comments/tags
	Labels        map[string]string // Per-sample static labels (e.g. pod_uid, container_id)
	Resolver      *procmap.Resolver // nil → fallback to name-based Location dedup
}
```

In `pprof/pprof.go`, modify `(p *ProfileBuilder).newSample`:

```go
func (p *ProfileBuilder) newSample(inputSample *ProfileSample) *profile.Sample {
	sample := new(profile.Sample)
	if inputSample.SampleType == SampleTypeCpu || inputSample.SampleType == SampleTypeOffCpu {
		sample.Value = []int64{0}
	} else {
		sample.Value = []int64{0, 0}
	}
	sample.Location = make([]*profile.Location, len(inputSample.Stack))
	if len(p.opt.Labels) > 0 {
		sample.Label = make(map[string][]string, len(p.opt.Labels))
		for k, v := range p.opt.Labels {
			sample.Label[k] = []string{v}
		}
	}
	return sample
}
```

`*ProfileBuilder` already carries `opt BuildersOptions`. Verify by reading `pprof/pprof.go` around `type ProfileBuilder struct`. If `opt` isn't there yet, add it during this step (the field is set in `BuilderForSample` which already passes `opt`).

- [ ] **Step 4: Run tests to confirm they pass**

```bash
GOTOOLCHAIN=auto go test ./pprof/... -count=1 2>&1 | tail -5
```
Expected: PASS (all existing pprof tests + the new one).

- [ ] **Step 5: Commit**

```bash
git add pprof/pprof.go pprof/pprof_test.go
git commit -m "pprof: BuildersOptions.Labels attaches static labels to every emitted sample"
```

---

## Task 6: Plumb `labels` through profilers

**Files:**
- Modify: `profile/profiler.go`
- Modify: `offcpu/profiler.go`
- Modify: `unwind/dwarfagent/common.go`
- Modify: `unwind/dwarfagent/agent.go`
- Modify: `unwind/dwarfagent/offcpu.go`

This task is mechanical pass-through; tests in Tasks 5 + 8 cover it end-to-end.

- [ ] **Step 1: `profile.NewProfiler` gains `labels` parameter**

In `profile/profiler.go`, change the signature and pass to `BuildersOptions`. Find:

```go
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
```

Change to:

```go
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, labels map[string]string) (*Profiler, error) {
```

Add `labels` to the struct:

```go
type Profiler struct {
	objs       *perfObjects
	symbolizer *blazesym.Symbolizer
	resolver   *procmap.Resolver
	perfSet    *perfevent.Set
	tags       []string
	sampleRate int
	labels     map[string]string
}
```

Set it in the constructor before `return &Profiler{...}`:

```go
return &Profiler{
	objs:       objs,
	symbolizer: symbolizer,
	resolver:   procmap.NewResolver(),
	perfSet:    perfSet,
	tags:       tags,
	sampleRate: sampleRate,
	labels:     labels,
}, nil
```

In the `Collect` method (find the `pprof.NewProfileBuilders(pprof.BuildersOptions{...})` call), add `Labels: pr.labels` to the literal.

- [ ] **Step 2: `offcpu.NewProfiler` gains `labels` parameter**

In `offcpu/profiler.go`, mirror the change. Existing signature:

```go
func NewProfiler(pid int, systemWide bool, tags []string) (*Profiler, error) {
```

becomes:

```go
func NewProfiler(pid int, systemWide bool, tags []string, labels map[string]string) (*Profiler, error) {
```

Store on the struct, pass to `BuildersOptions` at the `NewProfileBuilders` call site.

- [ ] **Step 3: `dwarfagent.newSession` gains `labels` parameter**

In `unwind/dwarfagent/common.go`:

```go
func newSession(objs sessionObjs, pid int, systemWide bool, cpus []uint, tags []string, logPrefix string, hooks *Hooks, mode Mode) (*session, error) {
```

becomes:

```go
func newSession(objs sessionObjs, pid int, systemWide bool, cpus []uint, tags []string, logPrefix string, hooks *Hooks, mode Mode, labels map[string]string) (*session, error) {
```

Add `labels map[string]string` field on `*session`. In the existing `pprof.NewProfileBuilders(pprof.BuildersOptions{...})` call (line ~328), add `Labels: s.labels`.

- [ ] **Step 4: `dwarfagent.NewProfilerWithMode` and friends gain `labels` parameter**

In `unwind/dwarfagent/agent.go`:

```go
func NewProfilerWithMode(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks, mode Mode) (*Profiler, error) {
```

becomes:

```go
func NewProfilerWithMode(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int, hooks *Hooks, mode Mode, labels map[string]string) (*Profiler, error) {
```

Pass `labels` through to `newSession`. Update `NewProfilerWithHooks` and `NewProfiler` (the no-hooks wrappers) to accept and forward the param.

In `unwind/dwarfagent/offcpu.go`, mirror for `NewOffCPUProfilerWithHooks` and `NewOffCPUProfiler`.

- [ ] **Step 5: Update perfagent's call sites**

In `perfagent/agent.go`, every `profile.NewProfiler(...)`, `offcpu.NewProfiler(...)`, `dwarfagent.NewProfilerWithMode(...)`, `dwarfagent.NewOffCPUProfilerWithHooks(...)` call gains a trailing argument: pass `nil` for now (Task 7 will compute the real labels and replace these).

- [ ] **Step 6: Update bench's call sites**

In `bench/cmd/scenario/main.go`, the `dwarfagent.NewProfilerWithMode` calls gain a trailing `nil` for labels. (Bench bypasses perfagent.Agent; labels are not relevant in the bench scenarios.)

- [ ] **Step 7: Build the entire module to confirm no compile errors**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go build ./... 2>&1 | tail -10
```
Expected: clean build.

- [ ] **Step 8: Run all unit tests**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./... -count=1 2>&1 | tail -15
```
Expected: all packages PASS.

- [ ] **Step 9: Commit**

```bash
git add profile/profiler.go offcpu/profiler.go unwind/dwarfagent/common.go unwind/dwarfagent/agent.go unwind/dwarfagent/offcpu.go perfagent/agent.go bench/cmd/scenario/main.go
git commit -m "profile/offcpu/dwarfagent: plumb labels map through profilers to pprof"
```

---

## Task 7: `perfagent` — `WithLabels` and `WithLabelEnricher` options

**Files:**
- Modify: `perfagent/options.go`
- Test: create `perfagent/options_test.go` (or augment if it exists)

- [ ] **Step 1: Read existing options.go to confirm patterns**

```bash
grep -n "^func With\|^type Option\|^type Config" /home/diego/github/perf-agent/.worktrees/k8s-pid-labels/perfagent/options.go
```

Confirm the `Option func(*Config)` style and how `Config` is shaped.

- [ ] **Step 2: Add fields to `Config`**

Append these fields to the `Config` struct in `perfagent/options.go`:

```go
// Labels are static per-sample pprof labels. Merged on top of the enricher
// output (Labels wins on key collision). Set via WithLabels.
Labels map[string]string

// LabelEnricher computes additional per-sample labels at agent startup
// from the resolved host PID. Default is internal/k8slabels.FromPID.
// Override via WithLabelEnricher; pass nil to disable defaults entirely.
// LabelEnricherSet records whether the user explicitly called
// WithLabelEnricher (so passing nil to disable is distinguishable from
// not calling it at all).
LabelEnricher    func(hostPID int) map[string]string
LabelEnricherSet bool
```

- [ ] **Step 3: Add the option constructors**

In `perfagent/options.go`:

```go
// WithLabels attaches static per-sample labels to every emitted pprof
// sample. Merged with WithLabelEnricher output; static labels win on key
// collision.
func WithLabels(labels map[string]string) Option {
	return func(c *Config) {
		if c.Labels == nil {
			c.Labels = make(map[string]string, len(labels))
		}
		for k, v := range labels {
			c.Labels[k] = v
		}
	}
}

// WithLabelEnricher overrides the default label enricher (which derives
// k8s identity labels from /proc/<hostPID>/cgroup and downward-API env
// vars). Pass nil to disable all enricher-sourced labels — only labels
// from WithLabels will be attached.
func WithLabelEnricher(fn func(hostPID int) map[string]string) Option {
	return func(c *Config) {
		c.LabelEnricher = fn
		c.LabelEnricherSet = true
	}
}
```

- [ ] **Step 4: Write a unit test**

Create or extend `perfagent/options_test.go`:

```go
package perfagent

import (
	"testing"
)

func TestWithLabels_AddsToConfig(t *testing.T) {
	cfg := DefaultConfig()
	WithLabels(map[string]string{"service": "api"})(cfg)
	if cfg.Labels["service"] != "api" {
		t.Errorf("service label = %q", cfg.Labels["service"])
	}
}

func TestWithLabels_MergesAcrossCalls(t *testing.T) {
	cfg := DefaultConfig()
	WithLabels(map[string]string{"service": "api"})(cfg)
	WithLabels(map[string]string{"version": "1.2.3"})(cfg)
	if cfg.Labels["service"] != "api" || cfg.Labels["version"] != "1.2.3" {
		t.Errorf("merged labels = %v", cfg.Labels)
	}
}

func TestWithLabelEnricher_StoresAndMarksSet(t *testing.T) {
	cfg := DefaultConfig()
	called := false
	WithLabelEnricher(func(int) map[string]string {
		called = true
		return map[string]string{"x": "y"}
	})(cfg)
	if !cfg.LabelEnricherSet {
		t.Fatal("LabelEnricherSet should be true after WithLabelEnricher")
	}
	got := cfg.LabelEnricher(0)
	if !called || got["x"] != "y" {
		t.Errorf("enricher not stored correctly")
	}
}

func TestWithLabelEnricher_NilDisables(t *testing.T) {
	cfg := DefaultConfig()
	WithLabelEnricher(nil)(cfg)
	if !cfg.LabelEnricherSet {
		t.Fatal("LabelEnricherSet should be true even when fn is nil")
	}
	if cfg.LabelEnricher != nil {
		t.Errorf("LabelEnricher should be nil")
	}
}
```

- [ ] **Step 5: Run options tests**

```bash
GOTOOLCHAIN=auto go test ./perfagent/... -count=1 -run "TestWithLabels|TestWithLabelEnricher" 2>&1 | tail -5
```
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add perfagent/options.go perfagent/options_test.go
git commit -m "perfagent: WithLabels and WithLabelEnricher options"
```

---

## Task 8: `perfagent` — wire NSpid translation + label collection

**Files:**
- Modify: `perfagent/agent.go`

- [ ] **Step 1: Add imports + helper at the top of `perfagent/agent.go`**

In the import block:

```go
"github.com/dpsoft/perf-agent/internal/k8slabels"
"github.com/dpsoft/perf-agent/internal/nspid"
```

- [ ] **Step 2: Add a helper that resolves host PID + final label set**

Append this method (anywhere convenient, but adjacent to `Start` is logical):

```go
// resolveTarget translates the configured PID to its host-namespace
// counterpart and computes the final per-sample label set by running the
// configured enricher (default: internal/k8slabels.FromPID) and merging
// the static labels from WithLabels on top.
//
// If config.PID is 0 (system-wide -a mode), no translation runs and
// labels come solely from WithLabels and from any user-supplied enricher
// invoked with hostPID=0.
func (a *Agent) resolveTarget() (hostPID int, labels map[string]string, err error) {
	hostPID = a.config.PID
	if hostPID > 0 {
		hostPID, err = nspid.Translate(a.config.PID)
		if err != nil {
			return 0, nil, fmt.Errorf("resolve target pid: %w", err)
		}
	}

	labels = make(map[string]string, 8)

	// Default enricher unless the caller explicitly disabled or replaced.
	enricher := a.config.LabelEnricher
	if !a.config.LabelEnricherSet {
		enricher = func(pid int) map[string]string {
			if pid <= 0 {
				return nil
			}
			out, err := k8slabels.FromPID("/proc", pid)
			if err != nil {
				log.Printf("k8slabels.FromPID(%d): %v (continuing without k8s labels)", pid, err)
				return nil
			}
			return out
		}
	}
	if enricher != nil {
		for k, v := range enricher(hostPID) {
			labels[k] = v
		}
	}
	for k, v := range a.config.Labels {
		labels[k] = v // WithLabels wins on key collision
	}
	return hostPID, labels, nil
}
```

- [ ] **Step 3: Call `resolveTarget` early in `Start`**

In `(a *Agent) Start(ctx context.Context) error`, after the capability promotion (`caps.SetFlag(...)` block) and rlimit, but **before** any profiler is constructed, add:

```go
hostPID, labels, err := a.resolveTarget()
if err != nil {
	return err
}
// hostPID replaces config.PID for downstream BPF setup;
// labels are passed to every profiler constructor.
```

Then for every profiler call site in `Start` (search for `profile.NewProfiler`, `offcpu.NewProfiler`, `dwarfagent.NewProfilerWithMode`, `dwarfagent.NewOffCPUProfilerWithHooks`):
- Replace the `a.config.PID` argument with `hostPID`.
- Replace the trailing `nil` (added in Task 6) with `labels`.

- [ ] **Step 4: Build + run all tests**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go test ./... -count=1 2>&1 | tail -15
```
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add perfagent/agent.go
git commit -m "perfagent: translate --pid via nspid; collect k8s labels via enricher in Start"
```

---

## Task 9: Smoke-verify locally

**Files:** none (build + run only).

- [ ] **Step 1: Build the agent binary**

```bash
LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release \
GOTOOLCHAIN=auto \
CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
CGO_LDFLAGS="-L /home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
go build -o perf-agent .
```
Expected: clean build, `perf-agent` binary in worktree root.

- [ ] **Step 2: Apply caps to the binary**

```bash
sudo setcap cap_sys_ptrace,cap_sys_admin,cap_perfmon,cap_bpf,cap_checkpoint_restore=ep ./perf-agent
getcap ./perf-agent
```
Expected: caps printed.

- [ ] **Step 3: Run a single-PID profile against any local process and verify labels in the pprof**

```bash
# Start a long-running target
sleep 600 &
PID=$!

# Profile it for 3s
./perf-agent --profile --pid $PID --duration 3s --profile-output /tmp/k8s-labels.pb.gz

# Inspect labels in the pprof
go tool pprof -raw /tmp/k8s-labels.pb.gz | grep -E "label|cgroup_path" | head -10

kill $PID 2>/dev/null
```
Expected: at least one `cgroup_path` label appears (target's host cgroup). On a non-k8s host, no `pod_uid` or `container_id` should appear. No errors during the run.

- [ ] **Step 4: Confirm CLI and library both work — write a tiny lib smoke (optional, no commit)**

If on a host with kubepods cgroups (kind, minikube, real k8s), repeat Step 3 and verify `pod_uid` is set.

---

## Self-review

- [x] Spec coverage: every section of the spec maps to a task.
  - Goal 1 (namespace-aware PID) → Task 1 + Task 8.
  - Goal 2 (cgroup-derived labels) → Tasks 2, 4.
  - Goal 3 (downward-API best-effort) → Task 3.
  - Goal 4 (CLI + lib parity) → Task 7 + Task 8.
  - Non-goals not implemented: BPF cgroup-id allowlist, k8s API watcher, cgroup v1, `--tid`. Confirmed.
- [x] No placeholders. All steps include actual code or actual commands.
- [x] Type consistency: `labels map[string]string` used everywhere; `enricher func(int) map[string]string` consistent across Task 7 and Task 8.
- [x] DRY: cgroup-related parsing lives in `internal/k8slabels` (one place).
- [x] TDD: Tasks 1, 2, 3, 4, 5, 7 are tests-first.
- [x] Frequent commits: each task ends with one commit; Task 6 is the only multi-file mechanical commit.
