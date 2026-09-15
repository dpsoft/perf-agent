# GPU Collector Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one agent per node profile every GPU pod on that node, attaching once to a shared `hostPath` shim inode.

**Architecture:** The agent writes the shim it carries to a `hostPath` directory, then creates **one** `UprobeMulti` link with `PID: 0` against that inode — which covers processes created later, measured on hardware 2026-09-15. Processes already running when it starts are handed to the consumer as `EagerPIDs`. Because replacement uses `rename(2)`, an upgrade leaves an older inode with live mappers, so the agent holds one link per distinct mapped inode rather than exactly one.

**Tech Stack:** Go 1.26, `cilium/ebpf` (`link.UprobeMulti`), `gpuprobe`, `internal/k8slabels`, CUPTI via the injected shim. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-10-gpu-collector-daemonset-design.md`

## Scope

**This plan targets `cmd/gpu-cuda-profile`.** Every piece the spec names already lives there. The `perf-agent` binary has no GPU flags at all — `--gpu`, `--gpu-shim` and `--gpu-output` appear in `examples/kubernetes/pytorch-gpu-profile.yaml` but are not implemented — so wiring GPU into the main agent is a **follow-on, not part of this plan**. Task 8's manifest therefore invokes `gpu-cuda-profile` by name; renaming or folding it into `perf-agent` is tracked separately.

## Global Constraints

- **Init PID namespace required.** `hostPID: true`. No PID translation in either direction; `/proc` cannot supply one (see the companion namespace design).
- **Kernel floor 6.2+.** `uprobe_multi` on 6.6+; `perf_uprobe` plus `CAP_SYS_ADMIN` below it.
- **`CAP_SYS_ADMIN` acceptable where a mechanism needs it.** Explicit capabilities preferred over blanket `privileged`; `privileged: true` is refused.
- **Uprobe attachment keys on `(dev, ino)`.** A copy of the shim at another path is a different file to a uprobe, however identical its bytes. Verified: a decoy on an identical-bytes copy contributed **zero** samples.
- **Never write the shim in place.** It is mapped executable by live processes; truncating under them is a crash. Temp file plus `rename(2)` in the same directory.
- **Silence is the defect.** Every "not profiled" path must be counted and reported. CUDA injection fails open and silent, so an unprofiled workload is otherwise indistinguishable from one that ran nothing.
- **Build/test flags** (blazesym is required for `gpuprobe`):
  ```
  LD_LIBRARY_PATH="/home/diego/github/blazesym/target/release:$LD_LIBRARY_PATH" \
  CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" \
  CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" \
  go test ./...
  ```
  LD_LIBRARY_PATH is required and was missing from the first draft of this
  block: -Wl,-Bstatic does not fully static-link blazesym here, so the test
  binary needs the .so at RUN time. `make test-unit` sets it; a bare run
  fails with "libblazesym_c.so: cannot open shared object file".

## File Structure

| File | Responsibility |
|---|---|
| `internal/shiminstall/install.go` (create) | Copy the carried shim to a directory: build-id compare, temp + `rename(2)`. No knowledge of BPF. |
| `internal/shiminstall/install_test.go` (create) | Fixture tests for skip-when-same, atomic replace, old-inode survival. |
| `gpuprobe/discover.go` (modify) | `ProcessesMappingShim` gains a directory-wide variant returning `(inode → pids)`. |
| `gpuprobe/consumer.go` (modify) | Accept N shim paths and hold one link per inode instead of exactly one. |
| `cmd/gpu-cuda-profile/main.go` (modify) | `-mode`, `-shim-dir`; allow attach with zero targets; unprofiled-process report. |
| `cmd/gpu-cuda-profile/collector_test.go` (create) | Mode preconditions and the zero-target path. |
| `test/gpu_collector_test.go` (create) | The two regression tests that were the spike: `PID: 0` late-attach, and the inode rule. |
| `examples/kubernetes/gpu-collector-daemonset.yaml` (create) | The DaemonSet plus the two lines an application pod adds. |

---

### Task 1: Attach with zero targets

**Files:**
- Modify: `cmd/gpu-cuda-profile/main.go` (`waitForTargets`, the `*opt.discover` case)
- Test: `cmd/gpu-cuda-profile/collector_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: `findTargets(shimPath string, within time.Duration, requireOne bool) ([]int, error)` — replaces `waitForTargets`. With `requireOne=false` an empty result is success.

A collector starts before the workloads it profiles. Today `waitForTargets`
fatals after `-wait-for-shim` when nothing maps the shim, so the command
cannot start first. This is the change the spike had to work around.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
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
	// -pid and single-shot launch runs still want the old behaviour: if
	// the caller says a target must exist and none does, that is a
	// misconfiguration worth refusing.
	shim := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(shim, []byte("\x7fELF not really"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := findTargets(shim, 200*time.Millisecond, true); err == nil {
		t.Fatal("findTargets(requireOne=true) returned nil error with no targets")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/gpu-cuda-profile/ -run TestFindTargets -v` (with the CGO flags from Global Constraints)
Expected: FAIL — `undefined: findTargets`.

- [ ] **Step 3: Write minimal implementation**

Replace `waitForTargets` in `cmd/gpu-cuda-profile/main.go`:

```go
// findTargets waits for processes mapping shimPath.
//
// requireOne distinguishes two callers with opposite needs. A launch or
// -pid run that finds nothing is misconfigured and should say so. A
// collector that finds nothing has simply started before the workloads,
// which is the normal case on a freshly booted node -- and refusing there
// would make the agent unable to be the first thing up, which is exactly
// when it is most useful.
func findTargets(shimPath string, within time.Duration, requireOne bool) ([]int, error) {
	deadline := time.Now().Add(within)
	for {
		found, err := gpuprobe.ProcessesMappingShim(shimPath)
		if err != nil {
			return nil, err
		}
		if len(found) > 0 {
			return found, nil
		}
		if !time.Now().Before(deadline) {
			if !requireOne {
				return nil, nil
			}
			return nil, fmt.Errorf(
				"no process on this host maps %s after %s. The CUDA driver loads the shim "+
					"during cuInit, from CUDA_INJECTION64_PATH in each process's own "+
					"environment, so a process not started with it pointing at THIS FILE "+
					"can never be a target -- and a copy of the shim at another path is a "+
					"different file to a uprobe, however identical its bytes. Check that "+
					"the application was started with CUDA_INJECTION64_PATH=%s, that it "+
					"has reached cuInit, and that this process can see it (a sidecar needs "+
					"shareProcessNamespace, a node agent needs hostPID)",
				shimPath, within, shimPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
```

Update the discover case to pass `requireOne` based on mode (Task 4 adds the flag; until then pass `true` to preserve behaviour):

```go
	case *opt.discover:
		var derr error
		targets, derr = findTargets(shimPath, *opt.waitForShim, true)
		if derr != nil {
			log.Fatalf("discover targets: %v", derr)
		}
		log.Printf("discovered %d process(es) mapping %s: %v", len(targets), shimPath, targets)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/gpu-cuda-profile/ -v`
Expected: PASS, including the existing flag-totality test.

- [ ] **Step 5: Commit**

```bash
git add cmd/gpu-cuda-profile/main.go cmd/gpu-cuda-profile/collector_test.go
git commit -m "gpu: an empty node is not an error for a collector

waitForTargets fatals when nothing maps the shim, so the command cannot
start before the workloads it profiles. A collector always does -- the
PID:0 spike had to start a workload first purely to work around this.

requireOne keeps the old behaviour for launch and -pid runs, where
finding nothing really is a misconfiguration."
```

---

### Task 2: Install the carried shim

**Files:**
- Create: `internal/shiminstall/install.go`
- Test: `internal/shiminstall/install_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `shiminstall.Install(src, destDir string) (dest string, replaced bool, err error)`; `shiminstall.ErrSameBuildID` is not returned — `replaced=false` signals the skip.

- [ ] **Step 1: Write the failing test**

```go
package shiminstall

import (
	"os"
	"path/filepath"
	"testing"
)

// elfWithBuildID writes a minimal file whose bytes differ per id. Install
// compares build-ids via the same reader gpuprobe uses; for these tests
// distinct content is enough to make the ids differ.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestInstallCopiesWhenAbsent(t *testing.T) {
	src := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	dst := t.TempDir()
	write(t, src, "shim-v1")

	got, replaced, err := Install(src, dst)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !replaced {
		t.Error("replaced = false on first install; want true")
	}
	if want := filepath.Join(dst, "libperfagent-gpu-nvidia.so"); got != want {
		t.Errorf("dest = %q, want %q", got, want)
	}
	b, _ := os.ReadFile(got)
	if string(b) != "shim-v1" {
		t.Errorf("content = %q, want shim-v1", b)
	}
}

func TestInstallSkipsWhenIdentical(t *testing.T) {
	src := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	dst := t.TempDir()
	write(t, src, "shim-v1")

	first, _, err := Install(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	inoBefore := inode(t, first)

	_, replaced, err := Install(src, dst)
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if replaced {
		t.Error("replaced = true for identical content; a restart must not churn the inode")
	}
	if inode(t, first) != inoBefore {
		t.Error("inode changed on a no-op install; every live mapper would be orphaned")
	}
}

func TestInstallReplacesAtomicallyAndLeavesTheOldInodeIntact(t *testing.T) {
	src := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	dst := t.TempDir()
	write(t, src, "shim-v1")
	first, _, err := Install(src, dst)
	if err != nil {
		t.Fatal(err)
	}

	// Hold the old file open, standing in for a process that mapped it.
	held, err := os.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	oldIno := inode(t, first)

	write(t, src, "shim-v2")
	second, replaced, err := Install(src, dst)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !replaced {
		t.Fatal("replaced = false for changed content")
	}
	if inode(t, second) == oldIno {
		t.Error("inode unchanged after replace; the file was written IN PLACE, which " +
			"corrupts every live mapping rather than versioning it")
	}
	// The holder still reads the old bytes: that is what makes rename safe.
	buf := make([]byte, len("shim-v1"))
	if _, err := held.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "shim-v1" {
		t.Errorf("old fd sees %q, want shim-v1 — the replace was not atomic", buf)
	}
	b, _ := os.ReadFile(second)
	if string(b) != "shim-v2" {
		t.Errorf("new content = %q, want shim-v2", b)
	}
}

func TestInstallLeavesNoTempFileBehind(t *testing.T) {
	src := filepath.Join(t.TempDir(), "libperfagent-gpu-nvidia.so")
	dst := t.TempDir()
	write(t, src, "shim-v1")
	if _, _, err := Install(src, dst); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dest holds %v; want only the shim. A stray temp file is a second "+
			"inode a target could be pointed at by accident", names)
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(interface{ Ino() uint64 })
	_ = st
	_ = ok
	return statIno(t, path)
}
```

Add the platform helper in the same file:

```go
func statIno(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Ino
}
```

and import `"syscall"`. Delete the `inode` indirection if the linter objects — `statIno` is the real one.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/shiminstall/ -v`
Expected: FAIL — `undefined: Install`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package shiminstall places the CUPTI adapter the agent carries into a
// directory targets can load it from.
//
// It exists so the shim and the consumer that decodes its USDT payload
// ship as one artifact. Record layouts are frozen per version, so a shim
// placed out of band -- by a node image or config management -- can be
// older or newer than the agent with nothing in the pod spec to reveal it.
package shiminstall

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Name is the filename targets name in CUDA_INJECTION64_PATH.
const Name = "libperfagent-gpu-nvidia.so"

// Install copies src into destDir as Name and reports the resulting path.
//
// replaced is false when the destination already held identical content:
// a restarting agent on an unchanged node must not churn the inode,
// because every process that mapped the old one keeps it and would need a
// second uprobe link to stay covered.
//
// The write is a temp file plus rename(2) within destDir, never a write in
// place. The file is mapped executable by live processes; truncating it
// under them is a crash, not a version skew. rename within one filesystem
// is atomic, so a target resolving the path during an install sees either
// the old file or the new one, never a partial.
func Install(src, destDir string) (dest string, replaced bool, err error) {
	dest = filepath.Join(destDir, Name)

	srcSum, err := sum(src)
	if err != nil {
		return "", false, fmt.Errorf("shiminstall: read source: %w", err)
	}
	switch dstSum, serr := sum(dest); {
	case serr == nil && dstSum == srcSum:
		return dest, false, nil
	case serr != nil && !errors.Is(serr, fs.ErrNotExist):
		return "", false, fmt.Errorf("shiminstall: read destination: %w", serr)
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", false, fmt.Errorf("shiminstall: create %s: %w", destDir, err)
	}
	tmp, err := os.CreateTemp(destDir, Name+".tmp-*")
	if err != nil {
		return "", false, fmt.Errorf("shiminstall: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: open source: %w", err)
	}
	_, cerr := io.Copy(tmp, in)
	_ = in.Close()
	if cerr != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: copy: %w", cerr)
	}
	if err = tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: chmod: %w", err)
	}
	// Durable before it is visible: a crash between rename and writeback
	// would otherwise leave targets pointed at a truncated file.
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", false, fmt.Errorf("shiminstall: sync: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return "", false, fmt.Errorf("shiminstall: close: %w", err)
	}
	if err = os.Rename(tmpName, dest); err != nil {
		return "", false, fmt.Errorf("shiminstall: rename into place: %w", err)
	}
	return dest, true, nil
}

func sum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/shiminstall/ -v`
Expected: PASS, all four.

- [ ] **Step 5: Commit**

```bash
git add internal/shiminstall/
git commit -m "shiminstall: place the carried shim, atomically

The shim and the consumer that decodes its USDT payload ship as one
artifact so they cannot drift; record layouts are frozen per version and
a shim placed out of band has nothing in the pod spec to reveal a
mismatch.

Temp file plus rename(2), never a write in place: the file is mapped
executable by live processes and truncating it under them is a crash. A
test holds the old file open and asserts it still reads the old bytes,
which is the property that makes the replace safe.

Identical content is a no-op, asserted by inode: a restart that churned
the inode would orphan every live mapper."
```

---

### Task 3: Discover mapped shim inodes, not just one path

**Files:**
- Modify: `gpuprobe/discover.go`
- Test: `gpuprobe/discover_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `gpuprobe.ShimInodesInUse(dir string) (map[ShimFile][]int, error)` where `type ShimFile struct { Path string; Dev, Ino uint64 }`.

A `rename(2)` upgrade leaves the previous inode alive for everything that
mapped it. One link on the new path silently stops covering every workload
already running, so the agent needs the set of inodes that still have
mappers — bounded by live shim versions, not by pod count.

- [ ] **Step 1: Write the failing test**

```go
func TestShimInodesInUseGroupsByInodeNotPath(t *testing.T) {
	dir := t.TempDir()
	// Two shim files in the same directory, identical bytes, different
	// inodes -- exactly what an upgrade leaves behind.
	oldShim := filepath.Join(dir, "libperfagent-gpu-nvidia.so.old")
	newShim := filepath.Join(dir, "libperfagent-gpu-nvidia.so")
	for _, p := range []string{oldShim, newShim} {
		if err := os.WriteFile(p, []byte("\x7fELF fake"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	procRoot := t.TempDir()
	stageShimMapping(t, procRoot, 11, oldShim)
	stageShimMapping(t, procRoot, 22, newShim)
	stageShimMapping(t, procRoot, 33, newShim)

	got, err := shimInodesInUseIn(procRoot, dir)
	if err != nil {
		t.Fatalf("shimInodesInUseIn: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d inodes, want 2 (one per file)", len(got))
	}
	for f, pids := range got {
		switch f.Path {
		case oldShim:
			if len(pids) != 1 || pids[0] != 11 {
				t.Errorf("old inode pids = %v, want [11]", pids)
			}
		case newShim:
			if len(pids) != 2 {
				t.Errorf("new inode pids = %v, want two", pids)
			}
		default:
			t.Errorf("unexpected path %q", f.Path)
		}
	}
}

func TestShimInodesInUseSkipsFilesNothingMapped(t *testing.T) {
	dir := t.TempDir()
	unused := filepath.Join(dir, "libperfagent-gpu-nvidia.so")
	if err := os.WriteFile(unused, []byte("\x7fELF fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := shimInodesInUseIn(t.TempDir(), dir)
	if err != nil {
		t.Fatalf("shimInodesInUseIn: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v; a file with no mappers needs no link", got)
	}
}
```

`stageShimMapping` already exists in `gpuprobe/discover_nspid_test.go` on the
superseded branch; reproduce it here rather than depending on that branch:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./gpuprobe/ -run TestShimInodesInUse -v`
Expected: FAIL — `undefined: shimInodesInUseIn`.

- [ ] **Step 3: Write minimal implementation**

In `gpuprobe/discover.go`:

```go
// ShimFile identifies a shim by the pair a uprobe actually keys on.
//
// Path is carried for logs and for link.OpenExecutable; it is NOT the
// identity. Two paths with identical bytes are two files to a uprobe, and
// the dev/ino pair is what says so.
type ShimFile struct {
	Path string
	Dev  uint64
	Ino  uint64
}

// ShimInodesInUse returns every shim file in dir that some process has
// mapped, with the pids mapping it.
//
// A rename(2) upgrade leaves the previous inode alive for everything that
// already mapped it -- that survival is what makes the replace safe -- so
// an agent holding one link on the current path stops covering every
// workload that was already running. The answer here is bounded by live
// shim versions, normally one and transiently two, not by pod count.
func ShimInodesInUse(dir string) (map[ShimFile][]int, error) {
	return shimInodesInUseIn("/proc", dir)
}

func shimInodesInUseIn(procRoot, dir string) (map[ShimFile][]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read shim dir %s: %w", dir, err)
	}
	byIno := map[uint64]ShimFile{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		dev, ino, ierr := enrollShimIdentity(p)
		if ierr != nil {
			continue // not a readable regular file; not a shim we can attach to
		}
		byIno[ino] = ShimFile{Path: p, Dev: dev, Ino: ino}
	}
	if len(byIno) == 0 {
		return map[ShimFile][]int{}, nil
	}

	procEntries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", procRoot, err)
	}
	out := map[ShimFile][]int{}
	for _, e := range procEntries {
		pid, perr := strconv.ParseUint(e.Name(), 10, 32)
		if perr != nil {
			continue
		}
		for ino, f := range byIno {
			// Errors are the normal case: a process can exit between
			// ReadDir and this read, and one owned by another user is
			// unreadable. Neither says anything about whether it was a
			// target, and neither may stop the scan.
			ok, merr := procMapsHaveInode(procRoot, uint32(pid), f.Dev, ino) //nolint:gosec // bounded by ParseUint
			if merr != nil || !ok {
				continue
			}
			out[f] = append(out[f], int(pid)) //nolint:gosec // bounded by ParseUint
		}
	}
	for f := range out {
		sort.Ints(out[f])
	}
	return out, nil
}
```

`enrollShimIdentity` already returns `(dev, ino, error)` in this file; if its
signature differs, adapt the call rather than changing it.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./gpuprobe/ -v -run 'TestShimInodes|TestProcessesMappingShim'`
Expected: PASS, and the existing discovery tests unchanged.

- [ ] **Step 5: Commit**

```bash
git add gpuprobe/discover.go gpuprobe/discover_test.go
git commit -m "gpuprobe: report shim inodes in use, not just one path

A rename(2) upgrade leaves the previous inode alive for everything that
already mapped it -- which is exactly what makes replacing the file safe
-- so one link on the current path silently stops covering every workload
already running.

Grouped by (dev, ino) rather than by path, because that is what a uprobe
keys on: two files with identical bytes are two attach targets."
```

---

### Task 4: Collector mode

**Files:**
- Modify: `cmd/gpu-cuda-profile/main.go` (`options`, `defineFlags`, `launchOnlyInAttachMode`)
- Test: `cmd/gpu-cuda-profile/collector_test.go`

**Interfaces:**
- Consumes: `findTargets` (Task 1), `shiminstall.Install` (Task 2).
- Produces: `-mode agent|collector` and `-shim-dir`; `collectorPreconditions() error`.

- [ ] **Step 1: Write the failing test**

```go
func TestCollectorModeRefusesLaunchFlags(t *testing.T) {
	// Collector mode never starts the workload, so every flag that
	// configures a child is meaningless and must be refused rather than
	// ignored -- a silently-ignored -period would have the profile read
	// at a sampling rate it was not taken at.
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opt := defineFlags(fs)
	if err := fs.Parse([]string{"-mode=collector", "-period=4"}); err != nil {
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/gpu-cuda-profile/ -run 'TestCollectorMode|TestAgentModeIs' -v`
Expected: FAIL — `opt.mode undefined`, `undefined: validateMode`.

- [ ] **Step 3: Write minimal implementation**

Add to `options` and `defineFlags`:

```go
	mode    *string
	shimDir *string
```

```go
		mode: fs.String("mode", "agent",
			"agent | collector. agent profiles one workload, launched or attached, and is "+
				"the historical behaviour. collector runs one per NODE: it installs the "+
				"shim it carries into -shim-dir, attaches once to that inode with PID 0, "+
				"and profiles every process that maps it -- including ones that start "+
				"later. collector requires the init PID namespace (hostPID: true), "+
				"because BPF reports init-namespace pids and /proc cannot translate them"),
		shimDir: fs.String("shim-dir", "",
			"collector mode: the directory to install the carried shim into and attach "+
				"to. Mounted read-write here and read-only by the pods that load it. "+
				"Required in collector mode; meaningless in agent mode, where -shim "+
				"names the file directly"),
```

```go
// validateMode refuses combinations rather than ignoring them.
//
// The same discipline as launchOnlyInAttachMode and for the same reason: a
// flag accepted here and quietly ignored would have the profile
// interpreted under a configuration it was not taken with, and a refusal
// costs one restart where a wrong conclusion costs much more.
func validateMode(fs *flag.FlagSet, opt *options) error {
	switch *opt.mode {
	case "agent":
		if *opt.shimDir != "" {
			return errors.New("-shim-dir is collector-only; agent mode names the file with -shim")
		}
		return nil
	case "collector":
		if *opt.shimDir == "" {
			return errors.New("collector mode requires -shim-dir: the directory it installs the shim into and attaches to")
		}
		var refused []string
		fs.Visit(func(f *flag.Flag) {
			if why, ok := launchOnlyInAttachMode[f.Name]; ok {
				refused = append(refused, fmt.Sprintf("-%s (%s)", f.Name, why))
			}
		})
		if len(refused) > 0 {
			sort.Strings(refused)
			return fmt.Errorf("collector mode never starts a workload, so these configure nothing: %s",
				strings.Join(refused, ", "))
		}
		return nil
	default:
		return fmt.Errorf("-mode %q: want agent or collector", *opt.mode)
	}
}
```

Call it immediately after `fs.Parse` in `main`, and add `"mode"` and
`"shim-dir"` to `attachSafeFlags` so the existing totality test passes.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/gpu-cuda-profile/ -v`
Expected: PASS, including the flag-totality test that enumerates every
registered flag.

- [ ] **Step 5: Commit**

```bash
git add cmd/gpu-cuda-profile/
git commit -m "gpu: add collector mode

Explicit rather than inferred. The two deployments differ in shim
delivery (emptyDir vs hostPath) and in pod-spec requirements (hostPID),
so a binary that guessed would be guessing about the operator's manifest,
silently.

Launch-only flags are refused in collector mode for the same reason they
are refused in attach mode: a silently-ignored -period has the profile
read at a sampling rate it was not taken at."
```

---

### Task 5: Install and attach on start

**Files:**
- Modify: `cmd/gpu-cuda-profile/main.go` (the mode dispatch before `gpuprobe.Attach`)
- Modify: `gpuprobe/consumer.go` (`Config.ShimPath` gains `Config.ShimFiles`)
- Test: `cmd/gpu-cuda-profile/collector_test.go`

**Interfaces:**
- Consumes: `shiminstall.Install` (Task 2), `gpuprobe.ShimInodesInUse` (Task 3), `findTargets` (Task 1).
- Produces: `gpuprobe.Config.ShimFiles []ShimFile` — when non-empty it supersedes `ShimPath`, and the consumer creates one link per entry.

- [ ] **Step 1: Write the failing test**

```go
func TestCollectorInstallsThenAttachesToEveryMappedInode(t *testing.T) {
	// The upgrade case, which is the one most likely to be skipped: an
	// older shim still has a mapper, so a collector that attached only to
	// the current file would stop covering it.
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), shiminstall.Name)
	if err := os.WriteFile(src, []byte("shim-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-existing older shim, still mapped by something.
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
		t.Error("an older shim with a live mapper is not in the attach set; " +
			"every workload already running would stop being covered")
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/gpu-cuda-profile/ -run TestCollector -v`
Expected: FAIL — `undefined: collectorShimFiles`.

- [ ] **Step 3: Write minimal implementation**

```go
// collectorShimFiles installs the carried shim and returns every inode the
// agent must attach to: the one just installed, plus any older shim in the
// same directory that still has a live mapper.
//
// The second half is not defensive. Replacement is rename(2), so processes
// that mapped the previous file keep it -- that survival is what makes the
// replace safe -- and attaching only to the current path would silently
// stop covering every workload already running.
func collectorShimFiles(carried, dir, procRoot string) ([]gpuprobe.ShimFile, error) {
	dest, replaced, err := shiminstall.Install(carried, dir)
	if err != nil {
		return nil, err
	}
	log.Printf("gpu collector: shim %s (%s)", dest,
		map[bool]string{true: "installed", false: "already current"}[replaced])

	dev, ino, err := gpuprobe.ShimIdentity(dest)
	if err != nil {
		return nil, fmt.Errorf("identify installed shim: %w", err)
	}
	files := []gpuprobe.ShimFile{{Path: dest, Dev: dev, Ino: ino}}

	inUse, err := shimInodesInUseIn(procRoot, dir)
	if err != nil {
		return nil, err
	}
	for f, pids := range inUse {
		if f.Ino == ino && f.Dev == dev {
			continue // already have it
		}
		log.Printf("gpu collector: also attaching to %s (inode %d), mapped by %d process(es) from a previous version",
			f.Path, f.Ino, len(pids))
		files = append(files, f)
	}
	return files, nil
}
```

Export the identity helper from `gpuprobe/discover.go` so the command can
use it, and export `shimInodesInUseIn` as a package-level test seam the same
way `ProcessesMappingShim` wraps it:

```go
// ShimIdentity returns the (dev, ino) a uprobe will key on for path.
func ShimIdentity(path string) (dev, ino uint64, err error) { return enrollShimIdentity(path) }
```

In `gpuprobe/consumer.go`, add to `Config`:

```go
	// ShimFiles is the set of shim inodes to attach to. When non-empty it
	// supersedes ShimPath.
	//
	// More than one is the upgrade case and nothing else: rename(2) leaves
	// the previous inode alive for its existing mappers, so covering them
	// needs a link per inode. Bounded by live shim versions, not by the
	// number of targets.
	ShimFiles []ShimFile
```

and in the attach path (`consumer.go:1760`), loop over `cfg.ShimFiles` when
set, creating one `UprobeMulti` per file with `PID: 0`, appending each link
to the consumer's link set so `Close` releases all of them. When
`ShimFiles` is empty, keep the existing single-path behaviour verbatim.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/gpu-cuda-profile/ ./gpuprobe/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/gpu-cuda-profile/ gpuprobe/
git commit -m "gpu: collector installs the shim, then attaches per inode

One link per mapped inode rather than one per node, because rename(2)
leaves the previous shim alive for everything that mapped it. Attaching
only to the current path would silently drop every workload that was
already running -- the failure has no error, just a thinner profile.

Bounded by live shim versions, normally one and transiently two, not by
the number of targets."
```

---

### Task 6: Say what is not being profiled

**Files:**
- Modify: `cmd/gpu-cuda-profile/main.go`
- Test: `cmd/gpu-cuda-profile/collector_test.go`

**Interfaces:**
- Consumes: `gpuprobe.ShimInodesInUse` (Task 3).
- Produces: `unprofiledGPUProcesses(procRoot, shimDir string) (int, error)`.

CUDA injection fails open and silent. A pod that never mounted the shim,
and a pod that started before the agent wrote it, both run unprofiled with
no error anywhere — indistinguishable from a node with no GPU work.

- [ ] **Step 1: Write the failing test**

```go
func TestUnprofiledGPUProcessesCountsThoseMappingNoShim(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, shiminstall.Name)
	if err := os.WriteFile(shim, []byte("shim"), 0o755); err != nil {
		t.Fatal(err)
	}
	procRoot := t.TempDir()
	stageShimMapping(t, procRoot, 10, shim)              // profiled
	stageCUDAProcessWithoutShim(t, procRoot, 20)         // NOT profiled
	stageCUDAProcessWithoutShim(t, procRoot, 30)         // NOT profiled

	n, err := unprofiledGPUProcesses(procRoot, dir)
	if err != nil {
		t.Fatalf("unprofiledGPUProcesses: %v", err)
	}
	if n != 2 {
		t.Fatalf("got %d, want 2 — a GPU process mapping no shim is unprofiled and "+
			"looks exactly like an idle node unless it is counted", n)
	}
}
```

with the fixture:

```go
// stageCUDAProcessWithoutShim writes a maps file that maps libcuda but no
// shim: a GPU workload nobody opted in.
func stageCUDAProcessWithoutShim(t *testing.T, procRoot string, pid uint32) {
	t.Helper()
	dir := filepath.Join(procRoot, strconv.FormatUint(uint64(pid), 10))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	maps := "7f0000000000-7f0000001000 r-xp 00000000 fd:01 999 /usr/lib64/libcuda.so.610.57.04\n"
	if err := os.WriteFile(filepath.Join(dir, "maps"), []byte(maps), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/gpu-cuda-profile/ -run TestUnprofiled -v`
Expected: FAIL — `undefined: unprofiledGPUProcesses`.

- [ ] **Step 3: Write minimal implementation**

```go
// unprofiledGPUProcesses counts processes that have libcuda mapped but no
// shim from shimDir.
//
// Each one is a workload running unprofiled, for one of two reasons that
// look identical from here: its pod never declared the mount and the
// environment variable, or it reached cuInit before this agent installed
// the shim. Both are silent -- CUDA injection fails open -- so the count
// is the only thing standing between "nobody opted in" and "the node is
// idle".
func unprofiledGPUProcesses(procRoot, shimDir string) (int, error) {
	inUse, err := shimInodesInUseIn(procRoot, shimDir)
	if err != nil {
		return 0, err
	}
	profiled := map[int]struct{}{}
	for _, pids := range inUse {
		for _, p := range pids {
			profiled[p] = struct{}{}
		}
	}

	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", procRoot, err)
	}
	n := 0
	for _, e := range entries {
		pid, perr := strconv.Atoi(e.Name())
		if perr != nil {
			continue
		}
		if _, ok := profiled[pid]; ok {
			continue
		}
		body, rerr := os.ReadFile(filepath.Join(procRoot, e.Name(), "maps"))
		if rerr != nil {
			continue // exited, or not ours to read; says nothing either way
		}
		if strings.Contains(string(body), "libcuda.so") {
			n++
		}
	}
	return n, nil
}
```

Call it once after attach and report:

```go
	if n, err := unprofiledGPUProcesses("/proc", *opt.shimDir); err == nil && n > 0 {
		log.Printf("gpu collector: %d GPU process(es) map libcuda but no shim from %s "+
			"and are NOT being profiled. Either their pods do not declare the volume and "+
			"CUDA_INJECTION64_PATH, or they reached cuInit before this agent installed the "+
			"shim. Injection fails open and silent, so this count is the only signal.",
			n, *opt.shimDir)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/gpu-cuda-profile/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/gpu-cuda-profile/
git commit -m "gpu: count GPU processes that map no shim

CUDA injection fails open and silent, so a pod that never declared the
mount and the env var, and a pod that reached cuInit before the agent
installed the shim, both run unprofiled with no error anywhere -- and a
node full of them looks exactly like an idle node.

The count is the only signal that separates the two."
```

---

### Task 7: The two regression tests the spike was

**Files:**
- Create: `test/gpu_collector_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing; this is the gate.

These were run by hand on 2026-09-15 and passed. They go in the tree
because the design has no meaning without them, and because both have a
way of passing for the wrong reason if written carelessly.

- [ ] **Step 1: Write the failing test**

```go
//go:build linux

package test

// TestPIDZeroCoversAProcessStartedAfterAttach is the collector's load-bearing
// claim: one link per node covers pods scheduled later.
//
// TWO conditions make this test mean anything, and it is worthless without
// either:
//
//   - Two workloads. With one, a pass cannot distinguish PID:0 from a
//     lucky single-target attach.
//   - Rediscovery OFF. At the 5s default, the second workload appearing
//     proves nothing: a rescan could have found and registered it, and the
//     output is identical either way.
func TestPIDZeroCoversAProcessStartedAfterAttach(t *testing.T) {
	requireGPU(t)
	requireBPFCaps(t)

	shim := stageShim(t)
	a := startWorkload(t, shim, 200000) // alive across the window
	agent := startCollector(t, shim, collectorOpts{rediscoverEvery: time.Hour})
	waitForAttach(t, agent)

	b := startWorkload(t, shim, 60000) // starts AFTER the link exists
	profile := agent.waitAndCollect(t)

	pids := gpuPIDsIn(t, profile)
	if !pids[a.pid] {
		t.Fatalf("workload A (%d) missing; the harness is broken, not the feature", a.pid)
	}
	if !pids[b.pid] {
		t.Fatalf("workload B (%d) is absent. It started after the link was created and "+
			"rediscovery was off, so PID:0 does not cover processes created after "+
			"attach -- which is the whole collector design", b.pid)
	}
	if agent.discovered(t, b.pid) {
		t.Fatal("B was discovered, so this run did not test PID:0 at all")
	}
}

// TestADifferentInodeIsNotCovered is the control that makes the test above
// interpretable. Identical bytes, different inode, must contribute nothing.
func TestADifferentInodeIsNotCovered(t *testing.T) {
	requireGPU(t)
	requireBPFCaps(t)

	attached, decoy := stageTwoIdenticalShims(t)
	c := startWorkload(t, attached, 200000)
	agent := startCollector(t, attached, collectorOpts{rediscoverEvery: time.Hour})
	waitForAttach(t, agent)

	d := startWorkload(t, decoy, 60000)
	profile := agent.waitAndCollect(t)

	pids := gpuPIDsIn(t, profile)
	if !pids[c.pid] {
		t.Fatalf("workload C (%d) missing; harness broken", c.pid)
	}
	if pids[d.pid] {
		t.Fatalf("workload D (%d) appears despite mapping a different inode with "+
			"identical bytes. The (dev, ino) rule the whole hostPath layout rests on "+
			"does not hold", d.pid)
	}
}
```

Implement the helpers (`stageShim`, `startWorkload`, `startCollector`,
`waitForAttach`, `gpuPIDsIn`, `stageTwoIdenticalShims`) against the existing
`test/` harness conventions; `requireGPU` and `requireBPFCaps` follow the
skip-with-a-stated-reason pattern already used in `unwind/ehmaps`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./test/ -run 'TestPIDZero|TestADifferentInode' -v`
Expected: FAIL or SKIP with a stated reason. On a box without the GPU or
without caps it must SKIP **loudly**: a gate that skips silently in CI is
how `main` merged red before.

- [ ] **Step 3: Make it pass**

Run against the real GPU. Reference results from the 2026-09-15 spike, which
these reproduce: A 296,364 samples / B 120,000 samples with B undiscovered;
C 298,844 samples / D absent.

- [ ] **Step 4: Verify**

Run both with `-count=2` to confirm they are not order-dependent.

- [ ] **Step 5: Commit**

```bash
git add test/gpu_collector_test.go
git commit -m "test: PID:0 covers later processes, and a copy does not

Both were run by hand before the design was written and both go in the
tree, because the collector has no meaning without them and both pass for
the wrong reason if written carelessly.

Two workloads, because one cannot distinguish PID:0 from a lucky
single-target attach. Rediscovery off, because at the 5s default the
second workload appearing proves nothing -- a rescan finds it and the
output looks identical.

The inode control is what makes the first test interpretable: identical
bytes at another path must contribute zero."
```

---

### Task 8: The DaemonSet, and what a pod adds

**Files:**
- Create: `examples/kubernetes/gpu-collector-daemonset.yaml`
- Modify: `examples/kubernetes/README.md`

**Interfaces:**
- Consumes: `-mode collector`, `-shim-dir` (Task 4).
- Produces: nothing.

- [ ] **Step 1: Write the manifest**

```yaml
# One agent per NODE. Contrast pytorch-gpu-profile.yaml, which puts one
# agent in every profiled POD.
#
# The shim lives once on the node and every GPU pod maps THE SAME INODE, so
# the agent attaches once with PID: 0 and covers pods scheduled later --
# measured, see the design doc. The sidecar's emptyDir gives each pod its
# own copy and therefore its own inode, which is right for a sidecar
# sitting inside the pod and wrong for an agent that is not.
#
# This does NOT make profiling zero-touch. The application pod still
# declares the mount and CUDA_INJECTION64_PATH (see the bottom of this
# file). Injecting those without editing the pod needs a mutating webhook,
# which is deferred.
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: perf-agent-gpu-collector
spec:
  selector:
    matchLabels: { app: perf-agent-gpu-collector }
  template:
    metadata:
      labels: { app: perf-agent-gpu-collector }
    spec:
      # BPF reports init-namespace pids and /proc cannot translate them, so
      # the agent must BE in that namespace. Parca and the OTel profiler
      # both take this too; unlike them we do not also take privileged.
      hostPID: true
      nodeSelector:
        nvidia.com/gpu.present: "true"
      containers:
        - name: perf-agent
          image: ghcr.io/dpsoft/perf-agent:latest   # carries the shim
          args:
            - -mode=collector
            - -shim-dir=/var/lib/perf-agent/gpu
            - -out=/profiles/gpu.pb.gz
            - -duration=60s
          securityContext:
            privileged: false
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
              add:
                - BPF                  # load programs, create maps and links
                - PERFMON              # perf_event_open, including system-wide
                - SYS_PTRACE           # read another process's memory
                - CHECKPOINT_RESTORE   # /proc/<pid>/map_files
                # - SYSLOG             # add for kernel stacks: without it
                #                      # /proc/kallsyms reads zeros and kernel
                #                      # symbolization is silently skipped
                # - SYS_ADMIN          # add on kernels 6.2-6.5, where
                #                      # uprobe_multi does not exist and the
                #                      # perf_uprobe PMU path needs it
          volumeMounts:
            # READ-WRITE here: the agent installs the shim it carries.
            - { name: shim, mountPath: /var/lib/perf-agent/gpu }
            - { name: btf, mountPath: /sys/kernel/btf, readOnly: true }
            - { name: profiles, mountPath: /profiles }
      volumes:
        - name: shim
          hostPath: { path: /var/lib/perf-agent/gpu, type: DirectoryOrCreate }
        - name: btf
          hostPath: { path: /sys/kernel/btf, type: Directory }
        - name: profiles
          emptyDir: {}     # swap for a PVC or an uploader in a real deployment
---
# What an application pod adds to be profiled. Two things, no containers.
#
# A pod that omits this is simply not profiled, and nothing fails: CUDA
# injection fails open and silent. The collector logs how many GPU
# processes mapped no shim, which is the only way to tell that apart from
# an idle node.
#
#     volumeMounts:
#       - name: perfagent-shim
#         mountPath: /var/lib/perf-agent/gpu
#         readOnly: true              # READ-ONLY in the app; only the agent writes
#     env:
#       - name: CUDA_INJECTION64_PATH
#         value: /var/lib/perf-agent/gpu/libperfagent-gpu-nvidia.so
#     volumes:
#       - name: perfagent-shim
#         hostPath:
#           path: /var/lib/perf-agent/gpu
#           type: Directory
#
# CUPTI is still yours to supply: the driver does not ship it and
# nvidia-container-toolkit never injects it, so it must be in the
# application image. PyTorch images usually have it via pip.
```

- [ ] **Step 2: Validate it parses**

Run: `python3 -c "import yaml; list(yaml.safe_load_all(open('examples/kubernetes/gpu-collector-daemonset.yaml')))"`
Expected: no output, exit 0.

- [ ] **Step 3: Add the README contrast**

Add a section to `examples/kubernetes/README.md` stating: sidecar =
`emptyDir`, one inode per pod, agent inside the pod; collector =
`hostPath`, one inode per node, agent beside them. Neither is zero-touch.

- [ ] **Step 4: Verify**

Run: `kubectl apply --dry-run=client -f examples/kubernetes/gpu-collector-daemonset.yaml`
Expected: accepted. Skip if no cluster is reachable and say so.

- [ ] **Step 5: Commit**

```bash
git add examples/kubernetes/
git commit -m "examples: a GPU collector DaemonSet

One agent per node against the sidecar's one per pod. The shim lives once
on the node so every GPU pod maps the same inode and the agent attaches
once with PID:0, covering pods scheduled later.

States plainly that this is not zero-touch: the application pod still
declares the mount and CUDA_INJECTION64_PATH, and a pod that omits them
fails open and silent."
```

---

## Self-Review

**Spec coverage.** Installation → Task 2; atomic replace and the build-id
skip → Task 2; the two-inode upgrade consequence → Tasks 3 and 5; `PID: 0`
single attach → Task 5; attach-with-zero-targets → Task 1; mode selection →
Task 4; the unprofiled/ordering diagnostic → Task 6; the pod-spec contract
and the DaemonSet → Task 8; the verified claim as regression tests → Task 7.

**Two spec items deliberately have no task.** Per-pod attribution via
`k8slabels` is already implemented and needs no change under this design —
the collector is in the init namespace, so the pids it labels are directly
resolvable. Eviction (spec open question 4) is unexamined and stays open;
nothing here makes it worse.

**Placeholder scan.** Task 3 says "adapt the call rather than changing it"
about `enrollShimIdentity`, and Task 7 says to implement harness helpers
against existing `test/` conventions. Both are pointers at code the
implementer must read, not deferred decisions — flagged rather than hidden.

**Type consistency.** `ShimFile{Path, Dev, Ino}` is defined in Task 3 and
used in Tasks 5 and 6. `shiminstall.Install(src, destDir) (dest string,
replaced bool, err error)` is defined in Task 2 and used in Task 5.
`findTargets(shimPath, within, requireOne)` is defined in Task 1 and
referenced in Task 5. `shimInodesInUseIn(procRoot, dir)` is the test seam
under `ShimInodesInUse(dir)` in Task 3 and is used directly in Tasks 5 and 6.

## Follow-on, not in this plan

- **`perf-agent` has no GPU flags.** `--gpu`, `--gpu-shim` and
  `--gpu-output` appear in `examples/kubernetes/pytorch-gpu-profile.yaml`
  and are not implemented anywhere. Either the GPU path moves into that
  binary or `gpu-cuda-profile` is promoted and renamed. Until then both
  example manifests name a command that does not match the flags the
  sidecar example uses.
- **Zero-touch injection** (mutating webhook or GPU-operator hook).
- **Eviction** of labels and module bytes when a pod ends.
