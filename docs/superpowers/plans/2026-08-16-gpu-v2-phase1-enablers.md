# GPU V2 Phase 1 — Enablers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Land the two small, independent changes on `main` that unblock every later GPU V2 phase — per-sample pprof labels, and removal of the redundant `CAP_SYS_ADMIN` requirement.

**Architecture:** Both changes are to existing, shipping code and carry no GPU dependency. Task 1 adds a `Labels` field to `ProfileSample` and folds it into the sample dedup hash, so GPU metadata can live in labels rather than synthetic stack frames. Task 2 extracts the capability set into a testable function and drops `CAP_SYS_ADMIN`, which `CAP_PERFMON` and `CAP_CHECKPOINT_RESTORE` have superseded since kernels 5.8 and 5.9.

**Tech Stack:** Go 1.25, `github.com/google/pprof/profile`, `github.com/cespare/xxhash/v2`, `kernel.org/pub/linux/libs/security/libcap/cap`, testify (`assert` + `require`).

**Spec:** `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` (§8 Output representation, §11 Deployment, §14 Phase 1)

## Global Constraints

- Go 1.25.0+.
- CGO is required to build or test this repo. Every `go test` / `go build` invocation must be prefixed with the environment block in "Build environment" below, or it fails to link blazesym.
- Output format stays pprof. No new wire format, no OTLP emitter.
- Capability claims assume kernel ≥ 5.9. The dev host runs 6.19.
- Do not commit `CLAUDE.md`.
- No `Co-Authored-By` lines in commit messages.

## Build environment

Every test and build command in this plan assumes this has been exported in the shell first:

```bash
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release
```

`make test-unit` will not work: it runs `go generate` first, which needs `llvm-strip`. The eBPF bytecode is committed, so run `go test` directly as shown in each step.

## Scope note

The spec's §14 said the first plan would cover Phases 1 and 2. On review, Phase 2 (the continuous core) is a separate subsystem with a different deliverable — it creates the `gpu/` package on this branch, while Phase 1 modifies existing shipping code destined for `main`. Splitting them keeps each plan independently shippable and testable. Phase 2 gets its own plan once Phase 1's gate passes.

---

### Task 1: Per-sample pprof labels

GPU context currently has nowhere to go except synthetic stack frames, which makes it part of stack identity. With PC sampling that produces one flame-graph leaf per sampled instruction. `BuildersOptions.Labels` already exists but is builder-level and constant — every sample in a profile gets the same map. This task gives an individual sample its own labels.

The subtle part is step 6: `CreateSampleOrAddValue` currently hashes only the stack's location IDs, so two samples with the same stack but different labels would silently merge and one label set would win.

**Files:**
- Modify: `pprof/pprof.go` — `ProfileSample` struct (line ~85), `newSample` (line ~441), `CreateSampleOrAddValue` (line ~263), imports (line ~3)
- Test: `pprof/pprof_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `ProfileSample.Labels map[string]string` — optional per-sample labels. Per-sample keys override `BuildersOptions.Labels` keys of the same name.
  - `hashLabels(seed uint64, labels map[string]string) uint64` — package-private, order-independent fold of labels into a dedup hash.

- [ ] **Step 1: Write the failing test for per-sample labels**

Append to `pprof/pprof_test.go`:

```go
func TestProfileSampleLabels(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"main"}),
		Value:      100,
		Labels:     map[string]string{"gpu_kernel": "flash_attn_fwd"},
	})

	for _, b := range builders.Builders {
		require.Len(t, b.Profile.Sample, 1)
		assert.Equal(t, []string{"flash_attn_fwd"}, b.Profile.Sample[0].Label["gpu_kernel"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./pprof/ -run TestProfileSampleLabels -v
```

Expected: compile failure — `unknown field Labels in struct literal of type ProfileSample`.

- [ ] **Step 3: Add the field and merge it in `newSample`**

In `pprof/pprof.go`, add the field to `ProfileSample`:

```go
type ProfileSample struct {
	Pid         uint32
	SampleType  SampleType
	Aggregation SampleAggregation
	Stack       []Frame
	Value       uint64
	Value2      uint64
	// Labels are per-sample pprof labels, merged over BuildersOptions.Labels.
	// Keys present in both win here. Use for context that must not fragment
	// stack identity (GPU stall reason, PC, queue, device, correlation ID).
	Labels map[string]string
}
```

Replace the label block in `newSample`:

```go
	if len(p.opt.Labels) > 0 || len(inputSample.Labels) > 0 {
		sample.Label = make(map[string][]string, len(p.opt.Labels)+len(inputSample.Labels))
		for k, v := range p.opt.Labels {
			sample.Label[k] = []string{v}
		}
		for k, v := range inputSample.Labels {
			sample.Label[k] = []string{v}
		}
	}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./pprof/ -run TestProfileSampleLabels -v
```

Expected: PASS.

- [ ] **Step 5: Write the failing test for label-aware dedup**

This is the correctness case. Omitting `Aggregation` selects the zero value (`false`), which routes through `CreateSampleOrAddValue` — the deduplicating path. Append to `pprof/pprof_test.go`:

```go
func TestSameStackDifferentLabelsNotMerged(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	stack := []string{"main", "compute"}

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames(stack),
		Value:      10,
		Labels:     map[string]string{"gpu_stall": "long_scoreboard"},
	})
	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames(stack),
		Value:      20,
		Labels:     map[string]string{"gpu_stall": "barrier"},
	})

	for _, b := range builders.Builders {
		require.Len(t, b.Profile.Sample, 2,
			"samples with identical stacks but different labels must not be merged")
	}
}

func TestSameStackSameLabelsStillMerged(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{SampleRate: 99})
	stack := []string{"main", "compute"}
	labels := map[string]string{"gpu_stall": "barrier"}

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames(stack),
		Value:      10,
		Labels:     labels,
	})
	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames(stack),
		Value:      20,
		Labels:     labels,
	})

	for _, b := range builders.Builders {
		require.Len(t, b.Profile.Sample, 1,
			"identical stack and identical labels must still aggregate")
	}
}

func TestLabelHashIsOrderIndependent(t *testing.T) {
	a := hashLabels(1234, map[string]string{"x": "1", "y": "2"})
	b := hashLabels(1234, map[string]string{"y": "2", "x": "1"})
	assert.Equal(t, a, b, "label hash must not depend on map iteration order")
}
```

- [ ] **Step 6: Run the tests to verify they fail**

```bash
go test ./pprof/ -run 'TestSameStack|TestLabelHash' -v
```

Expected: `TestSameStackDifferentLabelsNotMerged` FAILS with `expected 2, got 1` (labels are not in the hash, so the samples merge). `TestLabelHashIsOrderIndependent` fails to compile — `undefined: hashLabels`.

- [ ] **Step 7: Fold labels into the dedup hash**

In `pprof/pprof.go`, add `encoding/binary` and `slices` to the import block:

```go
import (
	"encoding/binary"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/cespare/xxhash/v2"
	"github.com/google/pprof/profile"

	"github.com/klauspost/compress/gzip"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)
```

In `CreateSampleOrAddValue`, replace the single hash line:

```go
	h := xxhash.Sum64(uint64Bytes(p.tmpLocationIDs))
	if len(inputSample.Labels) > 0 {
		h = hashLabels(h, inputSample.Labels)
	}
```

Add the helper next to `CreateSampleOrAddValue`:

```go
// hashLabels folds per-sample labels into the sample dedup hash so that two
// samples sharing a stack but carrying different labels are kept apart rather
// than silently merged. Keys are sorted, so the result does not depend on Go's
// randomized map iteration order.
func hashLabels(seed uint64, labels map[string]string) uint64 {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	d := xxhash.New()
	var seedBuf [8]byte
	binary.LittleEndian.PutUint64(seedBuf[:], seed)
	_, _ = d.Write(seedBuf[:])
	for _, k := range keys {
		_, _ = d.WriteString(k)
		_, _ = d.WriteString("\x00")
		_, _ = d.WriteString(labels[k])
		_, _ = d.WriteString("\x00")
	}
	return d.Sum64()
}
```

- [ ] **Step 8: Run the full pprof test suite**

```bash
go test ./pprof/ -count=1 -v
```

Expected: PASS, including the pre-existing tests. If `TestSampleAggregation` or `TestProfileComments` now fail, the label merge changed behaviour for samples with no labels — check that the `len(...) > 0` guards in both `newSample` and `CreateSampleOrAddValue` are present.

- [ ] **Step 9: Verify no other caller is disturbed**

```bash
go build ./... && go test ./profile/ ./offcpu/ ./cpu/ -count=1
```

Expected: builds clean; those suites behave exactly as before (`ProfileSample.Labels` is optional and nil for every existing caller).

- [ ] **Step 10: Commit**

```bash
git add pprof/pprof.go pprof/pprof_test.go
git commit -m "feat(pprof): per-sample labels, folded into the dedup hash

BuildersOptions.Labels is builder-level and constant, so every sample in a
profile receives the same map. GPU context varies per sample and had nowhere
to go but synthetic stack frames, which makes it part of stack identity — with
PC sampling that yields one flame-graph leaf per sampled instruction.

ProfileSample.Labels adds per-sample labels, merged over the builder-level map
with per-sample keys winning. CreateSampleOrAddValue hashed only the stack's
location IDs, so samples sharing a stack but carrying different labels would
have merged silently; labels are now folded into that hash with sorted keys so
the result is independent of map iteration order."
```

---

### Task 2: Drop the redundant `CAP_SYS_ADMIN` requirement

perf-agent requests `CAP_SYS_ADMIN` and every documented `setcap` line includes it. On kernel ≥ 5.9 it is redundant: `CAP_PERFMON` (5.8) covers `perf_event_open` including `pid=-1`, and `CAP_CHECKPOINT_RESTORE` (5.9) covers `/proc/<pid>/map_files`. `CAP_SYS_ADMIN` is near-root and is what gets a per-pod agent rejected, so removing it is a prerequisite for the §11 deployment model.

The capability set is currently an inline call inside `Start()`, which is untestable. This task extracts it first.

**Files:**
- Modify: `perfagent/agent.go` — capability comment block (line ~296) and `SetFlag` call (line ~302)
- Modify: `unwind/ehmaps/openable.go:15` — stale comment
- Modify: `README.md` (lines 40, 116, 366), `bench/README.md:28`, `examples/README.md:15`, `examples/rust-pgo/README.md:12`, `examples/flamegraph/README.md:11`
- Test: `perfagent/agent_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: `requiredCapabilities() []cap.Value` — package-private in `perfagent`, returns the capability set the agent raises to Effective.

- [ ] **Step 1: Write the failing test**

Append to `perfagent/agent_test.go`:

```go
func TestRequiredCapabilitiesExcludesSysAdmin(t *testing.T) {
	caps := requiredCapabilities()

	assert.NotContains(t, caps, cap.SYS_ADMIN,
		"CAP_SYS_ADMIN is redundant on kernel >= 5.9: CAP_PERFMON covers "+
			"perf_event_open including pid=-1, CAP_CHECKPOINT_RESTORE covers "+
			"/proc/<pid>/map_files")

	assert.Contains(t, caps, cap.BPF)
	assert.Contains(t, caps, cap.PERFMON)
	assert.Contains(t, caps, cap.SYS_PTRACE)
	assert.Contains(t, caps, cap.CHECKPOINT_RESTORE)
}
```

Ensure `perfagent/agent_test.go` imports `kernel.org/pub/linux/libs/security/libcap/cap` and `github.com/stretchr/testify/assert`.

- [ ] **Step 2: Run the test to verify it fails**

```bash
go test ./perfagent/ -run TestRequiredCapabilitiesExcludesSysAdmin -v
```

Expected: compile failure — `undefined: requiredCapabilities`.

- [ ] **Step 3: Extract the capability set and drop `SYS_ADMIN`**

In `perfagent/agent.go`, add above the function containing the `SetFlag` call:

```go
// requiredCapabilities is the capability set the agent raises to Effective.
//
//	CAP_BPF                - load eBPF programs and create maps
//	CAP_PERFMON            - perf_event_open (including pid=-1 for system-wide),
//	                         stack traces, tracing attachment. Since kernel 5.8
//	                         this covers what CAP_SYS_ADMIN used to be needed for.
//	CAP_SYS_PTRACE         - read /proc/<pid>/maps and /proc/<pid>/mem of other processes
//	CAP_CHECKPOINT_RESTORE - follow /proc/<pid>/map_files/ symlinks for blazesym
//	                         symbolization. Added in kernel 5.9 precisely so this
//	                         does not require CAP_SYS_ADMIN.
func requiredCapabilities() []cap.Value {
	return []cap.Value{cap.BPF, cap.PERFMON, cap.SYS_PTRACE, cap.CHECKPOINT_RESTORE}
}
```

Replace the existing comment block and `SetFlag` call with:

```go
	caps := cap.GetProc()
	if err := caps.SetFlag(cap.Effective, true, requiredCapabilities()...); err != nil {
		return fmt.Errorf("set capabilities: %w", err)
	}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
go test ./perfagent/ -run TestRequiredCapabilitiesExcludesSysAdmin -v
```

Expected: PASS.

- [ ] **Step 5: Fix the stale comment in `unwind/ehmaps/openable.go`**

Replace lines 15–16:

```go
// Requires CAP_CHECKPOINT_RESTORE to read /proc/<pid>/map_files (kernel 5.9+;
// before that it needed CAP_SYS_ADMIN). perf-agent's standard cap set covers this.
```

- [ ] **Step 6: Update every documented `setcap` invocation**

Remove `cap_sys_admin,` from each of these, leaving `cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep`:

- `README.md:40`, `README.md:116`, `README.md:366`
- `bench/README.md:28` (note this one orders them `cap_perfmon,cap_bpf,cap_sys_admin,...`)
- `examples/README.md:15`
- `examples/rust-pgo/README.md:12`
- `examples/flamegraph/README.md:11`

Verify none remain:

```bash
grep -rn "cap_sys_admin" --include="*.md" . | grep -v docs/superpowers
```

Expected: no output.

- [ ] **Step 7: Run the unit suites**

```bash
go build ./... && go test ./perfagent/ ./unwind/... -count=1
```

Expected: builds clean. Pre-existing failures unrelated to capabilities may appear in packages that need root — note them, do not fix them here.

- [ ] **Step 8: Verify on hardware — this is the phase gate**

Build and grant the reduced set. Do **not** put the binary in `/tmp`: it is mounted `nosuid` and file capabilities do not survive exec there.

```bash
go build -o ./perf-agent .
sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
getcap ./perf-agent
```

Expected from `getcap`: `cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore=ep` with no `cap_sys_admin`.

Then run both modes as a non-root user, with no `sudo`:

```bash
./perf-agent --pid $$ --profile --duration 5s
./perf-agent -a --profile --duration 5s
```

Expected: both complete and write a `.pb.gz`. The system-wide run is the one that actually exercises `perf_event_open(pid=-1)` — the capability `CAP_SYS_ADMIN` was believed to be required for. If it fails with `EACCES` or `EPERM`, stop: check `cat /proc/sys/kernel/perf_event_paranoid` and report the value rather than restoring `CAP_SYS_ADMIN`.

- [ ] **Step 9: Commit**

```bash
git add perfagent/agent.go perfagent/agent_test.go unwind/ehmaps/openable.go \
        README.md bench/README.md examples/README.md \
        examples/rust-pgo/README.md examples/flamegraph/README.md
git commit -m "feat(caps): drop redundant CAP_SYS_ADMIN

CAP_PERFMON (kernel 5.8) covers perf_event_open including pid=-1 for
system-wide profiling, and CAP_CHECKPOINT_RESTORE (kernel 5.9) was added
specifically so /proc/<pid>/map_files could be read without CAP_SYS_ADMIN.
The agent has been requesting a near-root capability it has not needed for
years, and every documented setcap line propagated it.

Extracts the set into requiredCapabilities() so it can be asserted in a test,
and corrects the comment in unwind/ehmaps/openable.go, which still attributed
map_files access to CAP_SYS_ADMIN while perfagent/agent.go already documented
it correctly.

Verified on kernel 6.19: a binary with only cap_bpf, cap_perfmon,
cap_sys_ptrace and cap_checkpoint_restore completes both --pid and -a runs."
```

---

## Phase gate

Phase 1 is complete when:

1. `go test ./pprof/ ./perfagent/ -count=1` passes.
2. A setcap'd binary carrying no `cap_sys_admin` completes both a `--pid` and an `-a` profile run as a non-root user (Task 2, Step 8).
3. `grep -rn "cap_sys_admin" --include="*.md" .` returns nothing outside `docs/superpowers/`.

Both tasks are independent of each other and of the `gpu/` package. They are intended to land on `main` as two separate PRs, not as part of the GPU branch — nothing in them references GPU code.

Once the gate passes, write the Phase 2 plan (the continuous core: bounded ring, correlation-ID index, sink backpressure, conformance suite, and the ported canonical model).
