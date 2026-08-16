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

func TestProfileSampleLabelsMergeWithBuilderLabels(t *testing.T) {
	builders := NewProfileBuilders(BuildersOptions{
		SampleRate: 99,
		Labels:     map[string]string{"pod_uid": "pod-a", "gpu_kernel": "from_builder"},
	})

	builders.AddSample(&ProfileSample{
		SampleType: SampleTypeCpu,
		Stack:      FramesFromNames([]string{"main"}),
		Value:      100,
		Labels:     map[string]string{"gpu_kernel": "from_sample"},
	})

	for _, b := range builders.Builders {
		require.Len(t, b.Profile.Sample, 1)
		labels := b.Profile.Sample[0].Label
		assert.Equal(t, []string{"pod-a"}, labels["pod_uid"],
			"builder-level labels with no per-sample override must survive")
		assert.Equal(t, []string{"from_sample"}, labels["gpu_kernel"],
			"per-sample labels must win over builder-level labels of the same key")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./pprof/ -run "TestProfileSampleLabels" -v
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
go test ./pprof/ -run "TestProfileSampleLabels" -v
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

### Task 2: Document the capability set — why each cap, and when `CAP_SYS_ADMIN` stops being needed

**No behaviour change.** The requested capability set stays exactly as it is. What is wrong today is the *documentation*: `unwind/ehmaps/openable.go` attributes `/proc/<pid>/map_files` access to `CAP_SYS_ADMIN` while `perfagent/agent.go` already attributes it correctly to `CAP_CHECKPOINT_RESTORE`, and nothing anywhere explains what each capability is actually for.

The reason to document rather than drop is precise, and worth stating in the code so nobody has to rediscover it. The two capabilities that replace `CAP_SYS_ADMIN` did **not** arrive together:

- `CAP_PERFMON` — kernel **5.8** — `perf_event_open`, including `pid=-1`
- `CAP_CHECKPOINT_RESTORE` — kernel **5.9** — `/proc/<pid>/map_files`

The project currently documents a floor of **kernel 5.8+** (`README.md:115`, `test/README.md:162`, `TESTING.md:230`). On exactly 5.8, `CAP_CHECKPOINT_RESTORE` does not exist, so dropping `CAP_SYS_ADMIN` would break symbolization there. A single kernel minor version is the entire obstacle.

Current deployments target 6.x, where both capabilities are long available and `CAP_SYS_ADMIN` is unambiguously redundant. So the condition for dropping it is not "wait for kernels to catch up" — it is "raise the documented floor from 5.8 to 5.9+". This task records that, so the drop later is a deliberate decision against a stated condition rather than a rediscovery. Raising the floor is a project decision and is explicitly **not** part of this task.

Do not edit the `setcap` lines in `README.md`, `bench/README.md`, or the `examples/` READMEs. They stay as-is because the code still requests the full set.

**Files:**
- Modify: `perfagent/agent.go` — the capability comment block above the `SetFlag` call (line ~296)
- Modify: `unwind/ehmaps/openable.go:15-16` — the stale comment
- Modify: `README.md` — add a subsection under the existing capability guidance

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: nothing. Documentation only; no exported or package-private identifiers are added or changed.

- [ ] **Step 1: Correct the stale comment in `unwind/ehmaps/openable.go`**

Replace lines 15–16, which currently read:

```go
// Requires CAP_SYS_ADMIN to read /proc/<pid>/map_files. perf-agent's
// standard cap set covers this.
```

with:

```go
// Reading /proc/<pid>/map_files required CAP_SYS_ADMIN before kernel 5.9, and
// CAP_CHECKPOINT_RESTORE from 5.9 onward. perf-agent's standard cap set holds
// both, so this works either way.
```

- [ ] **Step 2: Expand the capability comment block in `perfagent/agent.go`**

Replace the existing comment block above the `caps.SetFlag(...)` call. Leave the `SetFlag` call itself untouched:

```go
	// Capabilities raised to Effective. The set is deliberately the historical
	// superset so that pre-5.8 kernels keep working:
	//
	//   CAP_BPF                - load eBPF programs and create maps
	//   CAP_PERFMON            - perf_event_open, stack traces, tracing attachment
	//   CAP_SYS_PTRACE         - read /proc/<pid>/maps and /proc/<pid>/mem of other processes
	//   CAP_CHECKPOINT_RESTORE - follow /proc/<pid>/map_files/ symlinks (blazesym symbolization)
	//   CAP_SYS_ADMIN          - only needed on kernels older than 5.8/5.9; see below
	//
	// CAP_SYS_ADMIN is retained for backward compatibility, not because current
	// kernels need it. Two capabilities were introduced to carve out its roles,
	// but they did not arrive together:
	//
	//   - CAP_PERFMON (5.8) covers perf_event_open, including pid=-1 for
	//     system-wide profiling.
	//   - CAP_CHECKPOINT_RESTORE (5.9) covers /proc/<pid>/map_files.
	//
	// That one-version gap is the whole reason this is still here. The project
	// documents a floor of kernel 5.8, and on exactly 5.8 CAP_CHECKPOINT_RESTORE
	// does not exist, so dropping CAP_SYS_ADMIN would break symbolization there.
	//
	// On kernel >= 5.9 the minimal working set is:
	//
	//   cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep
	//
	// Dropping CAP_SYS_ADMIN matters for per-pod and sidecar deployments, where
	// it is the near-root capability that gets a workload rejected. The condition
	// is not "wait for newer kernels" - deployments already target 6.x. It is:
	// raise the documented floor from 5.8 to 5.9+, then verify the minimal set
	// for both --pid and -a runs (-a is the one that exercises
	// perf_event_open(pid=-1)), then drop it here.
```

- [ ] **Step 3: Verify it still builds and the existing tests pass**

```bash
go build ./... && go test ./perfagent/ ./unwind/... -count=1
```

Expected: builds clean, and behaviour is identical because only comments changed. Packages requiring root may fail as they did before — note them, do not fix them here.

- [ ] **Step 4: Document the capability set in `README.md`**

Find the existing capability guidance near `README.md:116` (`- Root, OR \`setcap ...\``). Immediately after that line, add:

```markdown
<details>
<summary>What each capability is for, and when <code>cap_sys_admin</code> can be dropped</summary>

| Capability | Why it is needed |
|---|---|
| `cap_bpf` | Load eBPF programs and create maps |
| `cap_perfmon` | `perf_event_open`, stack traces, tracing attachment |
| `cap_sys_ptrace` | Read `/proc/<pid>/maps` and `/proc/<pid>/mem` of the target |
| `cap_checkpoint_restore` | Follow `/proc/<pid>/map_files/` symlinks during symbolization |
| `cap_sys_admin` | Only on kernels older than 5.8/5.9 — see below |

`cap_sys_admin` is kept for backward compatibility. Two capabilities were added
to the kernel to carve out the roles perf-agent used it for — but they did not
arrive in the same release:

- **`CAP_PERFMON` (kernel 5.8)** covers `perf_event_open`, including `pid=-1`
  for system-wide profiling.
- **`CAP_CHECKPOINT_RESTORE` (kernel 5.9)** covers `/proc/<pid>/map_files`.

perf-agent's documented floor is kernel 5.8, and on exactly 5.8
`cap_checkpoint_restore` does not exist — so dropping `cap_sys_admin` there would
break symbolization. That single kernel minor version is why the full set is
still the default.

On **kernel 5.9 or newer** the minimal set is:

```bash
sudo setcap cap_bpf,cap_perfmon,cap_sys_ptrace,cap_checkpoint_restore+ep ./perf-agent
```

If you run 6.x — as most deployments now do — this is the set to use. It matters
most for per-pod and sidecar deployments, where `cap_sys_admin` is the near-root
capability that gets a workload rejected by admission policy.

</details>
```

- [ ] **Step 5: Confirm no `setcap` line was changed**

```bash
grep -rn "cap_sys_admin" --include="*.md" . | grep -v docs/superpowers
```

Expected: the pre-existing lines in `README.md` (3), `bench/README.md` (1), `examples/README.md` (1), `examples/rust-pgo/README.md` (1) and `examples/flamegraph/README.md` (1) are all still present and unmodified, plus the new occurrences inside the block added in Step 4. Nothing was removed.

- [ ] **Step 6: Commit**

```bash
git add perfagent/agent.go unwind/ehmaps/openable.go README.md
git commit -m "docs(caps): explain the capability set and when CAP_SYS_ADMIN is redundant

No behaviour change - the requested capability set is unchanged, because
dropping CAP_SYS_ADMIN would break kernels older than 5.8/5.9.

What was wrong was the documentation. unwind/ehmaps/openable.go attributed
/proc/<pid>/map_files access to CAP_SYS_ADMIN while perfagent/agent.go already
attributed it correctly to CAP_CHECKPOINT_RESTORE, and nothing explained what
any of the capabilities were for.

Records the two kernel versions that made CAP_SYS_ADMIN redundant - CAP_PERFMON
in 5.8 for perf_event_open including pid=-1, CAP_CHECKPOINT_RESTORE in 5.9 for
map_files - and the minimal set for kernel >= 5.9, so dropping it later is a
deliberate decision against stated conditions."
```

---

## Phase gate

Phase 1 is complete when:

1. `go test ./pprof/ ./perfagent/ -count=1` passes.
2. `go build ./...` succeeds.
3. Task 2 changed no `setcap` invocation and no capability code — `git diff` for Task 2 touches comments and `README.md` only.

The original spec gate for Task 2 (a setcap'd binary without `cap_sys_admin` completing `--pid` and `-a` runs) no longer applies, because the capability set is unchanged. That verification becomes the entry condition for a future task that actually drops `CAP_SYS_ADMIN`, once the minimum supported kernel is ≥ 5.9.

Both tasks are independent of each other and of the `gpu/` package. Task 1 is a behaviour change and should land on `main` as its own PR; Task 2 is documentation and can land separately.

Once the gate passes, write the Phase 2 plan (the continuous core: bounded ring, correlation-ID index, sink backpressure, conformance suite, and the ported canonical model).
