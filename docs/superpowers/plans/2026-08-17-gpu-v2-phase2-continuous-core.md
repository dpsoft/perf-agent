# GPU V2 Phase 2 — Continuous Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `gpu/` core so it survives a fast producer — bounded memory, indexed joins, accounted losses — before any vendor shim exists to stress it.

**Architecture:** A clean-slate `gpu/` package on `gpu-v2`. The canonical event model and the join ladder are ported from PR #10 (branch `gpu-profiling-spec`, `54502dd9`) with revisions; the storage beneath them is new. PR #10's `Timeline` appends to five unbounded slices and joins them with linear scans at `Snapshot()` — invisible at fixture scale, fatal at CUPTI rates. This replaces that with a bounded FIFO cache with time-based eviction, a correlation-ID index, and a sink that can report loss.

**Tech Stack:** Go 1.26, `github.com/google/pprof/profile` via the repo's `pprof` package, testify (`assert` + `require`). No eBPF, no CGO-dependent GPU code in this phase.

**Spec:** `docs/superpowers/specs/2026-08-16-gpu-profiling-v2-design.md` (§7 Canonical event model, §8 Output representation, §9 Overhead control, §10 Correlation and joins, §13 Testing, §14 Phase 2)

## Global Constraints

- Go 1.26.0+ (go.mod declares `go 1.26.0`; CI pins `1.26`).
- CGO is required to build or test this repo. Export the block in "Build environment" before any `go` command.
- Output format stays pprof. No new wire format, no OTLP emitter.
- All `*_ns` fields in the normalized model are CPU-monotonic. Producers convert before emitting; the core never converts.
- A heuristic join is never reported as exact. Heuristic and ambiguous joins are always marked.
- No event is ever dropped silently. Every loss path increments a counter that reaches the snapshot.
- Do not commit `CLAUDE.md`.
- No `Co-Authored-By` lines in commit messages.
- Use `git -c user.name="diego" -c user.email="diegolparra@gmail.com" commit`.

## Build environment

```bash
export CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include"
export CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -lblazesym_c"
export LD_LIBRARY_PATH=/home/diego/github/blazesym/target/release
```

Do not run `make test-unit` — it runs `go generate`, which needs `llvm-strip` (not installed). Run `go test` directly.

## Prerequisite — read before Task 1

**Task 5 cannot start until `ProfileSample.Labels` is available on this branch.** That field is Phase 1's Task 1, currently on the unmerged branch `feat/pprof-sample-labels` (commit `aca4df91`). `gpu-v2` was branched from `main` before it existed.

Resolve it in one of two ways before reaching Task 5, and record which:

- **Preferred:** `feat/pprof-sample-labels` merges to `main`, then rebase `gpu-v2` onto `main`.
- **If that PR is still open:** merge `feat/pprof-sample-labels` into `gpu-v2` directly. Note this in the ledger so the eventual rebase is expected to be a no-op.

Verify before starting Task 5:

```bash
grep -n "Labels map\[string\]string" pprof/pprof.go
```

Expected: the field exists on `ProfileSample`. If it does not, stop and resolve the prerequisite.

Tasks 1–4 and 6 have no dependency on it.

## Source material

Port from worktree `/home/diego/github/perf-agent/.worktrees/gpu-profiling-spec` at `54502dd9`:

- `gpu/types.go` — canonical model. Port with the §7 revisions in Task 1.
- `gpu/timeline.go` — the join ladder (`findLaunchByCorrelation`, `findLaunchHeuristic`, `launchKernelNamesCompatible`, `findLaunchForEvent`, `isJoinCandidateEvent`, `samplesForExec`). Port the *decision logic* in Task 4; do **not** port its storage.

Read those files. Do not copy the whole package.

---

### Task 1: Canonical event model

Port the model, applying the §7 changes: split the fat `GPUSample`, add `GPUModule`, and give the sink error returns.

**Files:**
- Create: `gpu/types.go`
- Test: `gpu/types_test.go`

**Interfaces:**
- Produces (later tasks depend on these exact names):
  - `GPUBackendID string`; constants `BackendLinuxDRM`, `BackendHIP`, `BackendCUPTI`, `BackendReplay`
  - `CorrelationID struct { Backend GPUBackendID; Value string }` — comparable, usable as a map key
  - `ClockDomain uint8`; `ClockDomainCPUMonotonic`; `NormalizeClockDomain(ClockDomain) ClockDomain`; `ValidateSupportedClockDomain(ClockDomain) error`
  - `LaunchContext struct { PID, TID uint32; TimeNs uint64; CPUStack []pp.Frame; Tags map[string]string }`
  - `GPUKernelLaunch struct { Correlation CorrelationID; Queue GPUQueueRef; KernelName string; ClockDomain ClockDomain; TimeNs uint64; Launch LaunchContext }`
  - `GPUKernelExec struct { Execution GPUExecutionRef; Correlation CorrelationID; Queue GPUQueueRef; KernelName string; ClockDomain ClockDomain; StartNs, EndNs uint64 }`
  - `GPUPCSample struct { Correlation CorrelationID; Module ModuleRef; ClockDomain ClockDomain; TimeNs uint64; PCOffset uint64; StallReason string; Count uint64 }`
  - `ModuleRef struct { Backend GPUBackendID; CRC uint64 }`
  - `GPUModule struct { Ref ModuleRef; SizeBytes uint64; LoadedNs uint64 }`
  - `GPUCapability uint8`; `CapabilityLaunchTrace`, `CapabilityExecTimeline`, `CapabilityPCSampling`
  - `EventSink interface` — every method returns `error`
  - `Backend interface { ID() GPUBackendID; Capabilities() []GPUCapability; Start(context.Context, EventSink) error; Stop(context.Context) error; Close() error }`

- [ ] **Step 1: Write the failing tests**

Create `gpu/types_test.go`:

```go
package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCorrelationIDUsableAsMapKey(t *testing.T) {
	m := map[CorrelationID]int{}
	a := CorrelationID{Backend: BackendCUPTI, Value: "1234"}
	b := CorrelationID{Backend: BackendCUPTI, Value: "1234"}
	c := CorrelationID{Backend: BackendHIP, Value: "1234"}

	m[a] = 1
	m[c] = 2

	assert.Equal(t, 1, m[b], "equal correlation IDs must hash to the same bucket")
	assert.Len(t, m, 2, "same value under a different backend is a different key")
}

func TestNormalizeClockDomainDefaultsToCPUMonotonic(t *testing.T) {
	assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(ClockDomain(0)))
	assert.Equal(t, ClockDomainCPUMonotonic, NormalizeClockDomain(ClockDomainCPUMonotonic))
}

func TestValidateSupportedClockDomainRejectsDeviceClocks(t *testing.T) {
	require.NoError(t, ValidateSupportedClockDomain(ClockDomainCPUMonotonic))
	require.NoError(t, ValidateSupportedClockDomain(ClockDomain(0)),
		"zero value normalizes to cpu-monotonic and must be accepted")
	assert.Error(t, ValidateSupportedClockDomain(ClockDomainGPUDevice),
		"producers must convert device clocks before emitting")
	assert.Error(t, ValidateSupportedClockDomain(ClockDomainSynced))
}

func TestPCSamplesAreSeparateFromExecutions(t *testing.T) {
	// GPUPCSample must not be reachable by widening GPUKernelExec: PC data is
	// capability-gated and only CUPTI-class backends emit it.
	s := GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "7"},
		Module:      ModuleRef{Backend: BackendCUPTI, CRC: 0xdeadbeef},
		PCOffset:    0x1a40,
		StallReason: "long_scoreboard",
		Count:       3,
	}
	assert.Equal(t, uint64(0x1a40), s.PCOffset)
	assert.Equal(t, uint64(3), s.Count)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./gpu/ -count=1 -v
```

Expected: build failure — the `gpu` package does not exist.

- [ ] **Step 3: Write `gpu/types.go`**

Port from `.worktrees/gpu-profiling-spec/gpu/types.go`, keeping its `GPUBackendID`, `GPUDeviceRef`, `GPUQueueRef`, `GPUExecutionRef`, `CorrelationID`, `ClockDomain` (with its JSON marshalling, `NormalizeClockDomain`, `ValidateSupportedClockDomain`), `LaunchContext`, `GPUKernelLaunch`, `GPUKernelExec`, `GPUTimelineEvent`, `WorkloadAttribution`, `JoinStats`, and the capability model.

Apply these changes:

1. Backend constants: keep `BackendLinuxDRM`, `BackendHIP`, `BackendReplay`. Drop `BackendLinuxKFD`, `BackendAMDSample`, `BackendStream`, `BackendHostReplay` — those backends are not in V2. Add `BackendCUPTI GPUBackendID = "cupti"`.
2. Strip PC/source fields from the sample type. `GPUSample` in PR #10 carries `PC`, `Function`, `File`, `Line`, `StallReason` for a model that never produced them. Delete `GPUSample` entirely and add:

```go
// ModuleRef identifies a GPU binary (a cubin for CUPTI) by content hash. It is
// symbolization metadata, not an event: the bytes travel out of band and are
// resolved centrally.
type ModuleRef struct {
	Backend GPUBackendID `json:"backend"`
	CRC     uint64       `json:"crc"`
}

// GPUModule records that a GPU binary was loaded. Emitted on its own lifecycle,
// replayed to consumers that attach late.
type GPUModule struct {
	Ref       ModuleRef `json:"ref"`
	SizeBytes uint64    `json:"size_bytes"`
	LoadedNs  uint64    `json:"loaded_ns"`
}

// GPUPCSample is one program-counter sample with its stall reason, attributed to
// a kernel launch by correlation ID. Capability-gated: only backends advertising
// CapabilityPCSampling emit these, so no backend pays for CUPTI's richness.
//
// PCOffset is an offset within Module, not an absolute address — it means
// nothing without the module, which is why the two are separate types.
type GPUPCSample struct {
	Correlation CorrelationID `json:"correlation"`
	Module      ModuleRef     `json:"module"`
	ClockDomain ClockDomain   `json:"clock_domain,omitempty"`
	TimeNs      uint64        `json:"time_ns"`
	PCOffset    uint64        `json:"pc_offset"`
	StallReason string        `json:"stall_reason,omitempty"`
	Count       uint64        `json:"count"`
}
```

3. Capabilities: keep `CapabilityLaunchTrace`, `CapabilityExecTimeline`, `CapabilityPCSampling`, `CapabilityLifecycleTimeline`, and their name maps. Drop `CapabilityDeviceCounters`, `CapabilityStallReasons`, `CapabilitySourceMap` — stall reasons now ride on `GPUPCSample`, and source mapping is Phase 6.
4. `EventSink` methods all return `error`, so a producer can be told to stop:

```go
// EventSink receives normalized events from a backend. Every method returns an
// error so a backend can be pushed back on: a sink at capacity returns
// ErrSinkFull, and the backend is expected to drop and count rather than block.
type EventSink interface {
	EmitLaunch(GPUKernelLaunch) error
	EmitExec(GPUKernelExec) error
	EmitPCSample(GPUPCSample) error
	EmitModule(GPUModule) error
	EmitEvent(GPUTimelineEvent) error
}
```

5. `Backend` drops `EventBackends()` — that existed to support the multiplexed stdin paths V2 removes.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./gpu/ -count=1 -v
```

Expected: PASS, 4/4.

- [ ] **Step 5: Commit**

```bash
git add gpu/types.go gpu/types_test.go
git commit -m "feat(gpu): canonical event model

Ports the vendor-neutral model from the gpu-profiling-spec exploration with the
revisions in spec section 7.

GPUSample carried PC, function, file, line and stall reason for a model that
never produced any of them. It is replaced by GPUPCSample, gated behind
CapabilityPCSampling, plus GPUModule/ModuleRef for the binary a PC offset is
meaningless without. Backends that cannot sample PCs no longer carry the fields,
and CUPTI's richness is not truncated at the boundary.

EventSink methods now return error so a backend can be pushed back on instead of
a sink silently absorbing everything."
```

---

### Task 2: Bounded launch cache

The structure that decides how late a PC sample can arrive and still be attributable. PR #10 keeps every launch forever in a slice; this keeps a bounded, time-evicted FIFO with an index, and counts what it drops.

**Files:**
- Create: `gpu/launchcache.go`
- Test: `gpu/launchcache_test.go`

**Interfaces:**
- Consumes: `CorrelationID`, `GPUKernelLaunch` (Task 1)
- Produces:
  - `LaunchCacheConfig struct { Capacity int; HorizonNs uint64 }`
  - `NewLaunchCache(LaunchCacheConfig) *LaunchCache`
  - `(*LaunchCache) Put(GPUKernelLaunch)`
  - `(*LaunchCache) Get(CorrelationID) (GPUKernelLaunch, bool)`
  - `(*LaunchCache) Len() int`
  - `(*LaunchCache) Stats() LaunchCacheStats`
  - `LaunchCacheStats struct { Live int; EvictedCapacity, EvictedHorizon, Replaced uint64 }`

- [ ] **Step 1: Write the failing tests**

Create `gpu/launchcache_test.go`:

```go
package gpu

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func launch(value string, timeNs uint64) GPUKernelLaunch {
	return GPUKernelLaunch{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: value},
		KernelName:  "k_" + value,
		TimeNs:      timeNs,
		Launch:      LaunchContext{PID: 1, TID: 1, TimeNs: timeNs},
	}
}

func TestLaunchCacheGetsWhatItPut(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	c.Put(launch("a", 10))

	got, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	require.True(t, ok)
	assert.Equal(t, "k_a", got.KernelName)
}

func TestLaunchCacheMissIsNotAnError(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "nope"})
	assert.False(t, ok, "a miss must be reported, not fabricated")
}

func TestLaunchCacheEvictsOldestOverCapacity(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 2})
	c.Put(launch("a", 10))
	c.Put(launch("b", 20))
	c.Put(launch("c", 30))

	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	assert.False(t, ok, "oldest entry must be evicted first")
	_, ok = c.Get(CorrelationID{Backend: BackendCUPTI, Value: "c"})
	assert.True(t, ok)

	assert.Equal(t, uint64(1), c.Stats().EvictedCapacity)
	assert.Equal(t, 2, c.Stats().Live)
}

func TestLaunchCacheEvictsBeyondHorizon(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 100, HorizonNs: 50})
	c.Put(launch("old", 10))
	c.Put(launch("new", 100)) // 100-10 = 90 > horizon 50

	_, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "old"})
	assert.False(t, ok, "entries older than the horizon must be evicted")
	assert.Equal(t, uint64(1), c.Stats().EvictedHorizon)
}

func TestLaunchCacheMemoryIsBounded(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 16})
	for i := 0; i < 100000; i++ {
		// Unique correlation per launch: a repeated ID would exercise
		// replacement rather than the capacity bound this test exists for.
		c.Put(launch(strconv.Itoa(i), uint64(i)))
	}
	assert.LessOrEqual(t, c.Len(), 16, "cache must stay bounded under sustained load")
	assert.Greater(t, c.Stats().EvictedCapacity, uint64(0), "evictions must be counted, not silent")
}

func TestLaunchCacheReplacesDuplicateCorrelation(t *testing.T) {
	c := NewLaunchCache(LaunchCacheConfig{Capacity: 4})
	c.Put(launch("a", 10))
	c.Put(launch("a", 20))

	got, ok := c.Get(CorrelationID{Backend: BackendCUPTI, Value: "a"})
	require.True(t, ok)
	assert.Equal(t, uint64(20), got.TimeNs, "a repeated correlation ID must take the newer launch")
	assert.Equal(t, 1, c.Stats().Live, "a replacement must not grow the cache")
	assert.Equal(t, uint64(1), c.Stats().Replaced)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./gpu/ -count=1 -run TestLaunchCache -v
```

Expected: build failure — `undefined: NewLaunchCache`.

- [ ] **Step 3: Write `gpu/launchcache.go`**

```go
package gpu

import "sync"

// LaunchCacheConfig bounds the cache two ways. Capacity caps memory; HorizonNs
// caps how far back an attribution can reach.
//
// HorizonNs is the load-bearing tunable for PC sampling: samples arrive
// asynchronously, sometimes seconds after the launch that produced them, and a
// launch evicted before its samples land makes those samples unattributable.
// Too small silently loses attribution; too large costs memory. Zero disables
// horizon eviction and leaves only the capacity bound.
type LaunchCacheConfig struct {
	Capacity  int
	HorizonNs uint64
}

// LaunchCacheStats reports what the cache holds and what it dropped. Every
// eviction path is counted: a cache that loses attributions silently is
// indistinguishable from a correlation bug.
type LaunchCacheStats struct {
	Live            int    `json:"live"`
	EvictedCapacity uint64 `json:"evicted_capacity,omitempty"`
	EvictedHorizon  uint64 `json:"evicted_horizon,omitempty"`
	Replaced        uint64 `json:"replaced,omitempty"`
}

const defaultLaunchCacheCapacity = 65536

// LaunchCache is a bounded FIFO of recent launches indexed by correlation ID.
// Put and Get are O(1) amortized. It replaces the unbounded slice plus linear
// scan the earlier prototype used, which was quadratic at snapshot time.
type LaunchCache struct {
	mu       sync.Mutex
	cfg      LaunchCacheConfig
	byCorr   map[CorrelationID]GPUKernelLaunch
	order    []CorrelationID // insertion order; entries before head are dead
	head     int
	newestNs uint64
	stats    LaunchCacheStats
}

func NewLaunchCache(cfg LaunchCacheConfig) *LaunchCache {
	if cfg.Capacity <= 0 {
		cfg.Capacity = defaultLaunchCacheCapacity
	}
	return &LaunchCache{
		cfg:    cfg,
		byCorr: make(map[CorrelationID]GPUKernelLaunch, cfg.Capacity),
		order:  make([]CorrelationID, 0, cfg.Capacity),
	}
}

func (c *LaunchCache) Put(l GPUKernelLaunch) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if l.TimeNs > c.newestNs {
		c.newestNs = l.TimeNs
	}
	if _, exists := c.byCorr[l.Correlation]; exists {
		// Same correlation seen again: take the newer launch without growing
		// the FIFO. The stale order entry is skipped when it reaches the head.
		c.byCorr[l.Correlation] = l
		c.stats.Replaced++
		c.evictLocked()
		return
	}
	c.byCorr[l.Correlation] = l
	c.order = append(c.order, l.Correlation)
	c.evictLocked()
}

func (c *LaunchCache) Get(id CorrelationID) (GPUKernelLaunch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, ok := c.byCorr[id]
	return l, ok
}

func (c *LaunchCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.byCorr)
}

func (c *LaunchCache) Stats() LaunchCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Live = len(c.byCorr)
	return s
}

// evictLocked drops entries past the horizon first, then past capacity. The
// caller must hold c.mu.
func (c *LaunchCache) evictLocked() {
	if c.cfg.HorizonNs > 0 {
		for c.head < len(c.order) {
			id := c.order[c.head]
			l, live := c.byCorr[id]
			if !live {
				c.head++ // stale order entry from a replacement
				continue
			}
			if c.newestNs <= l.TimeNs || c.newestNs-l.TimeNs <= c.cfg.HorizonNs {
				break
			}
			delete(c.byCorr, id)
			c.head++
			c.stats.EvictedHorizon++
		}
	}
	for len(c.byCorr) > c.cfg.Capacity && c.head < len(c.order) {
		id := c.order[c.head]
		c.head++
		if _, live := c.byCorr[id]; !live {
			continue
		}
		delete(c.byCorr, id)
		c.stats.EvictedCapacity++
	}
	c.compactLocked()
}

// compactLocked reclaims the dead prefix of order once it dominates the slice,
// so a long-running agent does not grow order without bound.
func (c *LaunchCache) compactLocked() {
	if c.head < 1024 || c.head*2 < len(c.order) {
		return
	}
	rest := c.order[c.head:]
	c.order = append(c.order[:0], rest...)
	c.head = 0
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./gpu/ -count=1 -run TestLaunchCache -v
```

Expected: PASS, 6/6.

- [ ] **Step 5: Add the race check**

```bash
go test ./gpu/ -count=1 -race -run TestLaunchCache
```

Expected: PASS, no race reported. The cache is written by producer goroutines and read at snapshot time, so it must be safe without external locking.

- [ ] **Step 6: Commit**

```bash
git add gpu/launchcache.go gpu/launchcache_test.go
git commit -m "feat(gpu): bounded launch cache with time-based eviction

The structure that decides how late a PC sample can arrive and still be
attributable. The earlier prototype kept every launch in an unbounded slice and
searched it linearly for every execution and every event - invisible against
fixtures, quadratic and unbounded at real rates.

Bounded FIFO indexed by correlation ID: O(1) put and get, capacity and horizon
eviction, and a counter on every drop path. A cache that loses attributions
silently is indistinguishable from a correlation bug, so nothing is dropped
without being counted."
```

---

### Task 3: Accounting sink

`EventSink` gained error returns in Task 1. This is the implementation that turns them into backpressure and a loss record.

**Files:**
- Create: `gpu/sink.go`
- Test: `gpu/sink_test.go`

**Interfaces:**
- Consumes: `EventSink` and all event types (Task 1). Also the test helper
  `launch(value string, timeNs uint64) GPUKernelLaunch` defined in Task 2's
  `gpu/launchcache_test.go` — same package, so it is already in scope. Do not
  redefine it; Task 2 must be complete first.
- Produces:
  - `ErrSinkFull` — sentinel error
  - `SinkStats struct { Launches, Execs, PCSamples, Modules, Events uint64; DroppedFull, DroppedInvalid uint64 }`
  - `NewCountingSink(inner EventSink, capacity int) *CountingSink`
  - `(*CountingSink) Stats() SinkStats`

- [ ] **Step 1: Write the failing tests**

Create `gpu/sink_test.go`:

```go
package gpu

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSink struct {
	launches int
	execs    int
}

func (r *recordingSink) EmitLaunch(GPUKernelLaunch) error   { r.launches++; return nil }
func (r *recordingSink) EmitExec(GPUKernelExec) error       { r.execs++; return nil }
func (r *recordingSink) EmitPCSample(GPUPCSample) error     { return nil }
func (r *recordingSink) EmitModule(GPUModule) error         { return nil }
func (r *recordingSink) EmitEvent(GPUTimelineEvent) error   { return nil }

func TestCountingSinkForwardsAndCounts(t *testing.T) {
	inner := &recordingSink{}
	s := NewCountingSink(inner, 10)

	require.NoError(t, s.EmitLaunch(launch("a", 10)))
	require.NoError(t, s.EmitExec(GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		StartNs:     20, EndNs: 30,
	}))

	assert.Equal(t, 1, inner.launches)
	assert.Equal(t, 1, inner.execs)
	assert.Equal(t, uint64(1), s.Stats().Launches)
	assert.Equal(t, uint64(1), s.Stats().Execs)
}

func TestCountingSinkReturnsErrSinkFullAtCapacity(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 2)

	require.NoError(t, s.EmitLaunch(launch("a", 10)))
	require.NoError(t, s.EmitLaunch(launch("b", 20)))
	err := s.EmitLaunch(launch("c", 30))

	require.Error(t, err, "a sink at capacity must push back, not absorb silently")
	assert.True(t, errors.Is(err, ErrSinkFull))
	assert.Equal(t, uint64(1), s.Stats().DroppedFull, "the drop must be counted")
}

func TestCountingSinkRejectsUnsupportedClockDomain(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 10)

	l := launch("a", 10)
	l.ClockDomain = ClockDomainGPUDevice
	err := s.EmitLaunch(l)

	require.Error(t, err, "producers must convert device clocks before emitting")
	assert.Equal(t, uint64(1), s.Stats().DroppedInvalid)
	assert.Equal(t, uint64(0), s.Stats().Launches, "a rejected event is not counted as accepted")
}

func TestCountingSinkZeroCapacityIsUnbounded(t *testing.T) {
	s := NewCountingSink(&recordingSink{}, 0)
	for i := 0; i < 1000; i++ {
		require.NoError(t, s.EmitLaunch(launch("x", uint64(i))))
	}
	assert.Equal(t, uint64(0), s.Stats().DroppedFull)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./gpu/ -count=1 -run TestCountingSink -v
```

Expected: build failure — `undefined: NewCountingSink`.

- [ ] **Step 3: Write `gpu/sink.go`**

```go
package gpu

import (
	"errors"
	"fmt"
	"sync"
)

// ErrSinkFull is returned when the sink has reached capacity. A backend
// receiving it should drop the event and count it locally rather than block:
// blocking a producer inside a profiled application is worse than losing a
// sample, and the loss is visible in SinkStats either way.
var ErrSinkFull = errors.New("gpu: sink full")

// SinkStats is the ingestion-side loss record. JoinStats reports what could not
// be correlated; this reports what never arrived.
type SinkStats struct {
	Launches  uint64 `json:"launches,omitempty"`
	Execs     uint64 `json:"execs,omitempty"`
	PCSamples uint64 `json:"pc_samples,omitempty"`
	Modules   uint64 `json:"modules,omitempty"`
	Events    uint64 `json:"events,omitempty"`

	DroppedFull    uint64 `json:"dropped_full,omitempty"`
	DroppedInvalid uint64 `json:"dropped_invalid,omitempty"`
}

// CountingSink wraps a sink with admission control and accounting. capacity is
// the total accepted-event budget; zero means unbounded.
type CountingSink struct {
	mu       sync.Mutex
	inner    EventSink
	capacity int
	accepted int
	stats    SinkStats
}

func NewCountingSink(inner EventSink, capacity int) *CountingSink {
	return &CountingSink{inner: inner, capacity: capacity}
}

func (s *CountingSink) Stats() SinkStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// admit applies the clock-domain contract and the capacity bound. It returns
// nil when the event may proceed, and has already counted the drop otherwise.
func (s *CountingSink) admit(domain ClockDomain) error {
	if err := ValidateSupportedClockDomain(domain); err != nil {
		s.stats.DroppedInvalid++
		return fmt.Errorf("gpu: rejected event: %w", err)
	}
	if s.capacity > 0 && s.accepted >= s.capacity {
		s.stats.DroppedFull++
		return ErrSinkFull
	}
	s.accepted++
	return nil
}

func (s *CountingSink) EmitLaunch(l GPUKernelLaunch) error {
	s.mu.Lock()
	if err := s.admit(l.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.Launches++
	s.mu.Unlock()
	return s.inner.EmitLaunch(l)
}

func (s *CountingSink) EmitExec(e GPUKernelExec) error {
	s.mu.Lock()
	if err := s.admit(e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.Execs++
	s.mu.Unlock()
	return s.inner.EmitExec(e)
}

func (s *CountingSink) EmitPCSample(p GPUPCSample) error {
	s.mu.Lock()
	if err := s.admit(p.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.PCSamples++
	s.mu.Unlock()
	return s.inner.EmitPCSample(p)
}

func (s *CountingSink) EmitModule(m GPUModule) error {
	s.mu.Lock()
	if s.capacity > 0 && s.accepted >= s.capacity {
		s.stats.DroppedFull++
		s.mu.Unlock()
		return ErrSinkFull
	}
	s.accepted++
	s.stats.Modules++
	s.mu.Unlock()
	return s.inner.EmitModule(m)
}

func (s *CountingSink) EmitEvent(e GPUTimelineEvent) error {
	s.mu.Lock()
	if err := s.admit(e.ClockDomain); err != nil {
		s.mu.Unlock()
		return err
	}
	s.stats.Events++
	s.mu.Unlock()
	return s.inner.EmitEvent(e)
}
```

Note `EmitModule` deliberately skips the clock check: `GPUModule` carries a load timestamp but is symbolization metadata rather than a timeline event, and has no `ClockDomain` field.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./gpu/ -count=1 -run TestCountingSink -v
```

Expected: PASS, 4/4.

- [ ] **Step 5: Commit**

```bash
git add gpu/sink.go gpu/sink_test.go
git commit -m "feat(gpu): accounting sink with backpressure

EventSink methods return error as of the model change; this is what makes that
mean something. CountingSink applies the clock-domain contract at the boundary,
enforces a capacity budget, and counts every rejection.

A backend receiving ErrSinkFull drops and counts rather than blocking - blocking
a producer inside a profiled application is worse than losing a sample, and
either way the loss is visible. JoinStats reports what could not be correlated;
SinkStats reports what never arrived."
```

---

### Task 4: Indexed timeline and join ladder

Port the join *decisions* from PR #10; replace the linear scans with `LaunchCache` lookups.

**Files:**
- Create: `gpu/timeline.go`
- Test: `gpu/timeline_test.go`

**Interfaces:**
- Consumes: Task 1 types, `LaunchCache` (Task 2), `SinkStats` (Task 3). Also the
  test helper `launch(...)` from Task 2's `gpu/launchcache_test.go` — in scope
  already, do not redefine. This task defines `execFor(...)`, which Task 5 reuses.
- Produces:
  - `TimelineConfig struct { LaunchCache LaunchCacheConfig; LaunchEventJoinWindowNs uint64 }`
  - `NewTimeline(TimelineConfig) *Timeline` — implements `EventSink`
  - `(*Timeline) Snapshot() Snapshot`
  - `ExecutionView struct { Launch *GPUKernelLaunch; Exec GPUKernelExec; PCSamples []GPUPCSample; Join JoinKind; Heuristic, Ambiguous bool }`
  - `JoinKind string`; `JoinExact`, `JoinHeuristic`
  - `Snapshot struct { Executions []ExecutionView; Events []GPUTimelineEvent; Modules []GPUModule; JoinStats JoinStats; LaunchCache LaunchCacheStats }`

- [ ] **Step 1: Write the failing tests**

Create `gpu/timeline_test.go`:

```go
package gpu

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execFor(value string, startNs, endNs uint64) GPUKernelExec {
	return GPUKernelExec{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: value},
		KernelName:  "k_" + value,
		StartNs:     startNs,
		EndNs:       endNs,
	}
}

func TestTimelineJoinsExactByCorrelationID(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.NotNil(t, snap.Executions[0].Launch)
	assert.Equal(t, JoinExact, snap.Executions[0].Join)
	assert.False(t, snap.Executions[0].Heuristic, "an exact join must never be marked heuristic")
	assert.Equal(t, uint64(1), snap.JoinStats.ExactExecutionJoinCount)
}

func TestTimelineReportsUnmatchedExecution(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(execFor("ghost", 20, 30)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch, "an unmatched execution must not invent a launch")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
}

func TestTimelineAttachesPCSamplesByCorrelation(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		PCOffset:    0x1a40, StallReason: "long_scoreboard", Count: 5, TimeNs: 25,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.Len(t, snap.Executions[0].PCSamples, 1)
	assert.Equal(t, uint64(0x1a40), snap.Executions[0].PCSamples[0].PCOffset)
}

func TestTimelineDegradesWhenLaunchEvicted(t *testing.T) {
	// A PC sample whose launch has aged out must become unattributed, never
	// mis-attributed to a different launch.
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 1}})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitLaunch(launch("b", 20))) // evicts "a"
	require.NoError(t, tl.EmitExec(execFor("a", 30, 40)))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	assert.Nil(t, snap.Executions[0].Launch, "an evicted launch must not be replaced by another")
	assert.Equal(t, uint64(1), snap.JoinStats.UnmatchedExecutionCount)
	assert.Equal(t, uint64(1), snap.LaunchCache.EvictedCapacity,
		"the eviction that caused the miss must be visible in the snapshot")
}

func TestTimelineSnapshotIsBoundedUnderLoad(t *testing.T) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 64}})
	for i := 0; i < 50000; i++ {
		// Unique correlation per launch. Reusing one ID would make every Put a
		// replacement, leaving Live at 1 and passing this assertion vacuously.
		require.NoError(t, tl.EmitLaunch(launch(strconv.Itoa(i), uint64(i))))
	}
	snap := tl.Snapshot()
	assert.Equal(t, 64, snap.LaunchCache.Live, "cache must fill to capacity and hold there")
	assert.Greater(t, snap.LaunchCache.EvictedCapacity, uint64(0), "evictions must be counted")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./gpu/ -count=1 -run TestTimeline -v
```

Expected: build failure — `undefined: NewTimeline`.

- [ ] **Step 3: Write `gpu/timeline.go`**

Read `.worktrees/gpu-profiling-spec/gpu/timeline.go` first and port the decision logic — the exact-then-heuristic ordering, `JoinStats` accounting, and the `Heuristic`/`Ambiguous` marking. Do not port `findLaunchByCorrelation` (replaced by `LaunchCache.Get`), the unbounded slices, or `samplesForExec`'s linear scan.

Structure:

- `Timeline` holds a `*LaunchCache`, a bounded `[]GPUKernelExec`, a `map[CorrelationID][]GPUPCSample` of pending samples, a bounded `[]GPUTimelineEvent`, a `[]GPUModule`, a `SinkStats`-style eviction counter, and a `sync.Mutex`. Every `Emit*` takes the lock — Task 3's `CountingSink` is a separate concern and does not serialize this.
- `EmitLaunch` → `cache.Put`.
- `EmitExec` → append to the bounded exec slice, evicting oldest and counting when over capacity.
- `EmitPCSample` → append into the pending-samples map keyed by correlation.
- `EmitModule` → append.
- `Snapshot` → for each exec: `cache.Get(exec.Correlation)`; hit is `JoinExact`; miss falls through to the ported heuristic (queue + kernel-name compatible + `launch.TimeNs <= exec.StartNs`) over the cache's live entries, marking `Heuristic` and `Ambiguous` when more than one candidate matched. Attach `pendingSamples[exec.Correlation]`. Fill `JoinStats` and `LaunchCache` stats.

Requirements the tests pin:

- an exact join must set `Join = JoinExact` and leave `Heuristic` false
- a miss must leave `Launch` nil rather than attaching a different launch
- eviction counts must reach `Snapshot().LaunchCache`

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./gpu/ -count=1 -run TestTimeline -v
```

Expected: PASS, 5/5.

- [ ] **Step 5: Race check**

```bash
go test ./gpu/ -count=1 -race
```

Expected: PASS. Unlike the earlier prototype — which had no synchronization and was safe only by the accident that its emitters touched different slices — `Timeline` is explicitly guarded.

- [ ] **Step 6: Commit**

```bash
git add gpu/timeline.go gpu/timeline_test.go
git commit -m "feat(gpu): indexed timeline with the ported join ladder

Keeps the join decisions from the earlier prototype - exact correlation ID
first, then a bounded queue/kernel-name heuristic, with heuristic and ambiguous
joins always marked so a guess is never presented as vendor truth. Replaces the
storage underneath: correlation lookups go through the bounded LaunchCache
instead of scanning every launch for every execution and every event.

A PC sample whose launch has aged out degrades to unattributed rather than
attaching to a neighbouring launch, and the eviction that caused it is visible
in the snapshot.

Ingestion is mutex-guarded. The prototype had no synchronization at all and was
safe only because its emitters happened to touch different slices."
```

---

### Task 5: pprof projection — frames and labels

**Blocked on the prerequisite above.** Verify `ProfileSample.Labels` exists before starting.

**Files:**
- Create: `gpu/projection.go`
- Test: `gpu/projection_test.go`

**Interfaces:**
- Consumes: `Snapshot`, `ExecutionView` (Task 4); `pp.ProfileSample`, `pp.Frame`, `pp.FrameFromName`, `pp.FramesFromNames`, `pp.SampleTypeCpu`, `pp.SampleAggregated` from `github.com/dpsoft/perf-agent/pprof`; and `ProfileSample.Labels`, which is the prerequisite at the top of this plan. Also the test helpers `launch(...)` (Task 2) and `execFor(...)` (Task 4) — in scope already, do not redefine.
- Produces: `ProjectExecutions(Snapshot) []pp.ProfileSample`

- [ ] **Step 1: Write the failing tests**

Create `gpu/projection_test.go`:

```go
package gpu

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pp "github.com/dpsoft/perf-agent/pprof"
)

func frameNames(frames []pp.Frame) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Name)
	}
	return out
}

func TestProjectionPutsStackInFramesAndDetailInLabels(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 10)
	l.Launch.CPUStack = pp.FramesFromNames([]string{"train_step", "cudaLaunchKernel"})
	l.Launch.Tags = map[string]string{"pod_uid": "pod-a"}
	require.NoError(t, tl.EmitLaunch(l))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))
	require.NoError(t, tl.EmitPCSample(GPUPCSample{
		Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
		PCOffset:    0x1a40, StallReason: "long_scoreboard", Count: 5, TimeNs: 25,
	}))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)

	assert.Equal(t,
		[]string{"train_step", "cudaLaunchKernel", "[gpu:launch]", "[gpu:kernel:k_a]"},
		frameNames(samples[0].Stack),
		"frames carry the CPU stack, the boundary marker and the kernel - nothing else")

	assert.Equal(t, "long_scoreboard", samples[0].Labels["gpu_stall"])
	assert.Equal(t, "0x1a40", samples[0].Labels["gpu_pc"])
	assert.Equal(t, "pod-a", samples[0].Labels["pod_uid"])
}

func TestProjectionKeepsPCOutOfStackIdentity(t *testing.T) {
	// Two PC samples from one kernel must share a stack and differ only by
	// label. Putting the PC in a frame would make every sampled instruction a
	// distinct flame-graph leaf.
	tl := NewTimeline(TimelineConfig{})
	l := launch("a", 10)
	l.Launch.CPUStack = pp.FramesFromNames([]string{"train_step"})
	require.NoError(t, tl.EmitLaunch(l))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 30)))
	for _, pc := range []uint64{0x100, 0x200} {
		require.NoError(t, tl.EmitPCSample(GPUPCSample{
			Correlation: CorrelationID{Backend: BackendCUPTI, Value: "a"},
			PCOffset:    pc, StallReason: "barrier", Count: 1, TimeNs: 25,
		}))
	}

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 2)
	assert.Equal(t, frameNames(samples[0].Stack), frameNames(samples[1].Stack),
		"PC must not appear in frames")
	assert.NotEqual(t, samples[0].Labels["gpu_pc"], samples[1].Labels["gpu_pc"])
}

func TestProjectionFallsBackToExecutionDuration(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitLaunch(launch("a", 10)))
	require.NoError(t, tl.EmitExec(execFor("a", 20, 90)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, uint64(70), samples[0].Value,
		"with no PC samples the execution interval is the weight")
}

func TestProjectionHandlesUnmatchedExecution(t *testing.T) {
	tl := NewTimeline(TimelineConfig{})
	require.NoError(t, tl.EmitExec(execFor("ghost", 20, 30)))

	samples := ProjectExecutions(tl.Snapshot())
	require.Len(t, samples, 1)
	assert.Equal(t, []string{"[gpu:launch]", "[gpu:kernel:k_ghost]"}, frameNames(samples[0].Stack),
		"an execution with no launch still projects, without a fabricated CPU stack")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
go test ./gpu/ -count=1 -run TestProjection -v
```

Expected: build failure — `undefined: ProjectExecutions`.

- [ ] **Step 3: Write `gpu/projection.go`**

Rules, from spec §8:

- **Frames**, in order: the launch's `CPUStack` (omitted when there is no launch), then `[gpu:launch]`, then `[gpu:kernel:<name>]` when the kernel name is non-empty. Nothing else goes in frames.
- **Labels**: `gpu_stall`, `gpu_pc` (formatted `%#x`), `gpu_queue`, `gpu_device`, `gpu_correlation`, plus every key from the launch's `Tags`.
- One `pp.ProfileSample` per PC sample; when an execution has none, one sample weighted by `max(1, EndNs-StartNs)`.
- PC-sample weight is `max(1, Count)`.
- Set `Pid` from the launch's `LaunchContext.PID`, `SampleType: pp.SampleTypeCpu`, `Aggregation: pp.SampleAggregated`.

- [ ] **Step 4: Run the tests to verify they pass**

```bash
go test ./gpu/ -count=1 -run TestProjection -v
```

Expected: PASS, 4/4.

- [ ] **Step 5: Commit**

```bash
git add gpu/projection.go gpu/projection_test.go
git commit -m "feat(gpu): project executions into pprof frames and labels

The earlier prototype encoded every piece of GPU context as a synthetic stack
frame, including the PC. Frames are stack identity, so one flame-graph leaf per
sampled instruction - which destroys aggregation exactly when PC sampling makes
the profile interesting.

Frames now carry only what should nest: the CPU stack, the [gpu:launch]
boundary, and the kernel. Stall reason, PC, queue, device, correlation and the
pod/container tags move to per-sample labels, so two samples from one kernel
share a stack and differ only by label."
```

---

### Task 6: Conformance suite and the phase gate

Replaces PR #10's 53 checked-in goldens with invariants any producer must satisfy.

**Files:**
- Create: `gpu/conformance_test.go`
- Test: itself

**Interfaces:**
- Consumes: everything above.
- Produces: `runConformance(t *testing.T, name string, drive func(EventSink) error)` — a helper later vendor backends call.

- [ ] **Step 1: Write the suite**

Create `gpu/conformance_test.go` with a table of producer scenarios (`drive` functions emitting into a sink) and, for each, assert:

1. **Exact joins are never reported heuristic** — every `ExecutionView` with `Join == JoinExact` has `Heuristic == false`.
2. **Heuristic joins are always marked** — `Join == JoinHeuristic` implies `Heuristic == true`.
3. **No launch is fabricated** — every non-nil `view.Launch` has a `Correlation` that some emitted launch actually carried.
4. **Timestamps are monotonic within the declared domain** — every emitted `*_ns` is non-decreasing per correlation, and every accepted event has `ClockDomainCPUMonotonic` after normalization.
5. **Losses are accounted** — `SinkStats.DroppedFull + DroppedInvalid` plus `LaunchCacheStats.EvictedCapacity + EvictedHorizon` equals emitted minus retained.
6. **Bounded memory** — after driving 100k events through a cache of capacity 64, `Snapshot().LaunchCache.Live <= 64`.

Include at least three scenarios: an all-exact producer, a producer with deliberate correlation gaps, and a high-rate producer that overflows both the sink and the cache.

- [ ] **Step 2: Run the suite**

```bash
go test ./gpu/ -count=1 -run TestConformance -v
```

Expected: PASS for every scenario.

- [ ] **Step 3: Write the phase-gate benchmark**

Add to `gpu/conformance_test.go`:

```go
func BenchmarkSnapshotAtScale(b *testing.B) {
	tl := NewTimeline(TimelineConfig{LaunchCache: LaunchCacheConfig{Capacity: 65536}})
	for i := 0; i < 1_000_000; i++ {
		// Every launch gets its own correlation ID. Reusing one would mean the
		// cache holds a single entry for the whole run, and this benchmark
		// would report a fast snapshot precisely because the structure under
		// test was empty - the phase gate would pass while measuring nothing.
		id := strconv.Itoa(i)
		_ = tl.EmitLaunch(launch(id, uint64(i)))
		if i%4 == 0 {
			_ = tl.EmitExec(execFor(id, uint64(i), uint64(i+10)))
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = tl.Snapshot()
	}
}
```

- [ ] **Step 4: Run the gate**

```bash
go test ./gpu/ -count=1 -run XXX -bench BenchmarkSnapshotAtScale -benchmem -benchtime 3x
```

Expected: snapshot completes in well under a second, and `B/op` reflects the bounded cache rather than a million retained launches. **Record the numbers in the report** — this is the Phase 2 gate and the evidence has to exist, not be asserted.

- [ ] **Step 5: Full suite and race**

```bash
go test ./gpu/ -count=1 && go test ./gpu/ -count=1 -race && go build ./...
```

Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add gpu/conformance_test.go
git commit -m "test(gpu): backend-agnostic conformance suite and scale gate

Replaces the 53 checked-in replay goldens the earlier prototype relied on. Each
golden encoded the JSON shape of the normalized model, so adding a field
rewrote all of them and produced an unreviewable diff - pressure on the model to
stop evolving, which is exactly what this work needs it to do. Two of them also
failed host-dependently by resolving a cgroup ID against the real machine,
capturing environment as contract.

The suite asserts invariants instead: exact joins are never reported heuristic,
no launch is fabricated, timestamps are monotonic in the declared domain, every
loss is accounted, and memory stays bounded. Replay, HIP and CUPTI producers
will all run the same suite.

BenchmarkSnapshotAtScale is the phase gate: a million launches through a bounded
cache must snapshot in well under a second with flat memory."
```

---

## Phase gate

Phase 2 is complete when:

1. `go test ./gpu/ -count=1` passes and `go test ./gpu/ -count=1 -race` is clean.
2. `BenchmarkSnapshotAtScale` snapshots 1M emitted launches in well under a second, with allocation reflecting the bounded cache. Numbers recorded, not asserted.
3. The conformance suite passes for all three producer scenarios.
4. `go build ./...` succeeds.

Nothing in this phase touches eBPF, CGO-linked GPU code, or the CLI. The `gpu/` package is reachable only from tests until Phase 3 wires a consumer to it — deliberately, so the core is proven before a fast producer exists to stress it.
