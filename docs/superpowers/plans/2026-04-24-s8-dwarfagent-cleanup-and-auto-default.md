# S8: dwarfagent Cleanup + Flip `--unwind auto` Default Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** (a) eliminate the ~70% code duplication between `dwarfagent.Profiler` (on-CPU) and `dwarfagent.OffCPUProfiler` (off-CPU) via a shared `session` abstraction; (b) flip `--unwind auto` default from FP to DWARF so users get symbolized native stacks out of the box.

**Architecture:** extract a `session` struct in a new `common.go` that owns every resource shared by both profilers — BPF handle, TableStore, PIDTracker, MmapWatcher, ringbuf reader, blazesym symbolizer, shutdown channels, and the sample-aggregation maps. Expose three shared methods: `newSession(...)` for construction, `consumeRingbuf(aggregate)` for the per-sample reader loop (aggregate is a callback so CPU sums counts and off-CPU sums blocking-ns), and `close()` for teardown. Each profiler type becomes a thin wrapper that embeds `*session` and adds its own attach shape (per-CPU `perf_event_open` + `AttachRawLink` for on-CPU; single `link.AttachTracing` for off-CPU tp_btf). Separately, change main.go's `--unwind` default from `"fp"` to `"auto"` and extend the perfagent dispatch so `"auto"` routes to the DWARF profilers.

**Tech Stack:** Go 1.26, cilium/ebpf, existing dwarfagent + ehmaps + profile packages. No new external dependencies.

---

## Scope

**S8 delivers:**
1. `unwind/dwarfagent/` file layout: one `common.go` with shared state/helpers; `agent.go` and `offcpu.go` shrink to ~120 lines each, focused on their distinctive setup (BPF load, attach, consume callback).
2. `--unwind auto` (new default) → DWARF path. Existing explicit `--unwind fp` → FP path (unchanged). Explicit `--unwind dwarf` → DWARF path.
3. All previously-green tests continue to pass. No behavioral change for anyone on `--unwind fp`.

**Explicitly NOT in S8:**
- Map sharing between perf_dwarf and offcpu_dwarf BPF programs (2× CFI compile still happens when both profilers run simultaneously). That's a separate optimization with its own plan — documented as open risk.
- ehcompile result caching across dwarfagent.Profiler instances. System-wide startup time stays at ~40s on a 500-process host.
- Any new features. This is pure cleanup + one user-facing default flip.

## Background for implementers

**Reading the current duplication:**
`unwind/dwarfagent/agent.go:38-60` and `unwind/dwarfagent/offcpu.go:20-45` — the two struct definitions — are nearly identical except for: CPU has `sampleRate int`, `perfFDs []int`, `perfLinks []link.Link`; off-CPU has just `link link.Link`. Every other field (`objs`, `store`, `tracker`, `watcher`, `ringReader`, `symbolizer`, `stop`, `trackerWG`, `readerWG`, `mu`, `samples`, `stacks`) is common.

The `NewProfiler` and `NewOffCPUProfiler` constructor bodies share ~80 lines of ehmaps-wiring that are byte-identical apart from the BPF type (PerfDwarf vs OffCPUDwarf) and the log prefix. `Collect`, `CollectAndWrite`, `Close`, and the tracker goroutine lifecycle are also structurally identical.

**The attach step differs fundamentally:**
- CPU: open one `perf_event_open` per CPU, `link.AttachRawLink` each to the BPF program, `PERF_EVENT_IOC_ENABLE`.
- Off-CPU: `link.AttachTracing` once — one link for the whole tp_btf program.

That's the only part of the constructor that shouldn't be shared.

**The consume goroutine also differs:**
- CPU: `samples[key]++` (count of PC chains sampled).
- Off-CPU: `samples[key] += s.Value` (sum of blocking-ns).

Parameterize via a callback.

**Why the `pprof.SampleType` argument:**
CPU uses `pprof.SampleTypeCpu`, off-CPU uses `pprof.SampleTypeOffCpu`. Only difference in the Collect output. Pass it as an argument.

## File Structure

```
unwind/dwarfagent/common.go           CREATE — session struct + newSession + runTracker + consumeRingbuf + close + collect. ~180 lines.
unwind/dwarfagent/common_test.go      CREATE — unit tests for the parts of session that are pure Go (e.g. hashPCs is currently defined in agent.go — move it here with a test).

unwind/dwarfagent/agent.go            REWRITE — Profiler now embeds *session and adds sampleRate + perfFDs + perfLinks. NewProfiler loads PerfDwarf, calls newSession, attaches perf events, starts goroutines. ~130 lines (down from 394).
unwind/dwarfagent/offcpu.go           REWRITE — OffCPUProfiler embeds *session and adds link.Link. NewOffCPUProfiler loads OffCPUDwarf, calls newSession, attaches tp_btf, starts goroutines. ~110 lines (down from 291).

unwind/dwarfagent/agent_test.go       UNCHANGED — call shape preserved.
unwind/dwarfagent/offcpu_test.go      UNCHANGED.
unwind/dwarfagent/sample.go           UNCHANGED.
unwind/dwarfagent/symbolize.go        UNCHANGED.

main.go                               MODIFY — flag default from "fp" to "auto".
perfagent/agent.go                    MODIFY — both dispatch switches add "auto" case that routes to DWARF.

docs/dwarf-unwinding-design.md        MODIFY — mark S8 ✅.
```

---

## Task 1 — Introduce `session` in `common.go`

**Goal:** create the shared abstraction. Nothing in `agent.go` / `offcpu.go` changes yet — that's Tasks 2 + 3. This task only compiles if the package still compiles — we take care to not shadow existing symbols.

**Files:**
- Create: `unwind/dwarfagent/common.go`
- Create: `unwind/dwarfagent/common_test.go`

- [ ] **Step 1.1: Write the session struct + helpers**

Create `unwind/dwarfagent/common.go`:

```go
package dwarfagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	blazesym "github.com/libbpf/blazesym/go"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// sessionObjs is the narrow shape both profile.PerfDwarf and
// profile.OffCPUDwarf satisfy. Lets session work generically.
type sessionObjs interface {
	Close() error
	RingbufMap() *ebpf.Map
	CFIRulesMap() *ebpf.Map
	CFILengthsMap() *ebpf.Map
	CFIClassificationMap() *ebpf.Map
	CFIClassificationLengthsMap() *ebpf.Map
	PIDMappingsMap() *ebpf.Map
	PIDMappingLengthsMap() *ebpf.Map
}

// session owns everything both dwarfagent.Profiler and
// dwarfagent.OffCPUProfiler share: BPF handle, ehmaps lifecycle
// (TableStore/PIDTracker/MmapWatcher), ringbuf reader, symbolizer,
// shutdown coordination, and the sample aggregation maps.
//
// Each profiler type embeds *session and adds its own attach-specific
// state (perf_event fds + links for CPU, one tp_btf link for off-CPU)
// plus a consume-callback that controls how Sample.Value aggregates
// (count++ vs sum+=).
type session struct {
	pid  int
	tags []string

	objs       sessionObjs
	store      *ehmaps.TableStore
	tracker    *ehmaps.PIDTracker
	watcher    mmapEventSourceCloser
	ringReader *ringbuf.Reader
	symbolizer *blazesym.Symbolizer

	stop      chan struct{}
	trackerWG sync.WaitGroup
	readerWG  sync.WaitGroup

	mu      sync.Mutex
	samples map[sampleKey]uint64
	stacks  map[sampleKey][]uint64
}

// newSession wires up everything shared after BPF is loaded: ehmaps,
// initial attach, MmapWatcher, ringbuf reader, blazesym symbolizer.
// Does NOT start any goroutines, attach the BPF program, or call
// AddPID — those are caller-specific (CPU vs off-CPU differ).
//
// On error, every resource newSession allocated is closed. Caller's
// BPF-handle `objs` is NOT closed on error — caller remains responsible
// for it (so caller's defer-close pattern still works).
func newSession(objs sessionObjs, pid int, systemWide bool, cpus []uint, tags []string, logPrefix string) (*session, error) {
	store := ehmaps.NewTableStore(
		objs.CFIRulesMap(), objs.CFILengthsMap(),
		objs.CFIClassificationMap(), objs.CFIClassificationLengthsMap(),
	)
	tracker := ehmaps.NewPIDTracker(store, objs.PIDMappingsMap(), objs.PIDMappingLengthsMap())

	if systemWide {
		nPIDs, nTables, err := ehmaps.AttachAllProcesses(tracker)
		if err != nil {
			return nil, fmt.Errorf("attach all processes: %w", err)
		}
		log.Printf("%s: attached %d distinct binaries across %d PIDs", logPrefix, nTables, nPIDs)
	} else {
		n, err := ehmaps.AttachAllMappings(tracker, uint32(pid))
		if err != nil {
			return nil, fmt.Errorf("attach initial mappings: %w", err)
		}
		log.Printf("%s: attached %d binaries from /proc/%d/maps", logPrefix, n, pid)
	}

	var watcher mmapEventSourceCloser
	if systemWide {
		cpuInts := make([]int, 0, len(cpus))
		for _, c := range cpus {
			cpuInts = append(cpuInts, int(c))
		}
		mw, err := ehmaps.NewMultiCPUMmapWatcher(cpuInts)
		if err != nil {
			return nil, fmt.Errorf("multi-cpu mmap watcher: %w", err)
		}
		watcher = mw
	} else {
		w, err := ehmaps.NewMmapWatcher(uint32(pid))
		if err != nil {
			return nil, fmt.Errorf("mmap watcher: %w", err)
		}
		watcher = w
	}

	rd, err := ringbuf.NewReader(objs.RingbufMap())
	if err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("ringbuf reader: %w", err)
	}

	symbolizer, err := blazesym.NewSymbolizer(
		blazesym.SymbolizerWithCodeInfo(true),
		blazesym.SymbolizerWithInlinedFns(true),
	)
	if err != nil {
		_ = rd.Close()
		_ = watcher.Close()
		return nil, fmt.Errorf("create symbolizer: %w", err)
	}

	return &session{
		pid:        pid,
		tags:       tags,
		objs:       objs,
		store:      store,
		tracker:    tracker,
		watcher:    watcher,
		ringReader: rd,
		symbolizer: symbolizer,
		stop:       make(chan struct{}),
		samples:    map[sampleKey]uint64{},
	}, nil
}

// runTracker starts the background goroutine that consumes MmapEvent /
// ExitEvent / ForkEvent records from the watcher and feeds them to the
// PIDTracker. Call exactly once after newSession returns.
func (s *session) runTracker() {
	s.trackerWG.Add(1)
	go func() {
		defer s.trackerWG.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			<-s.stop
			cancel()
		}()
		s.tracker.Run(ctx, s.watcher)
	}()
}

// aggregator is the per-sample callback used by consumeRingbuf.
// CPU passes a function that does `s.samples[key]++`; off-CPU passes
// a function that does `s.samples[key] += sample.Value`.
type aggregator func(s *session, sample Sample)

// consumeRingbuf runs the ringbuf reader loop until s.stop fires or
// the reader returns ErrClosed. Must be called exactly once in a
// goroutine after newSession + runTracker.
func (s *session) consumeRingbuf(agg aggregator) {
	defer s.readerWG.Done()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.ringReader.SetDeadline(time.Now().Add(200 * time.Millisecond))
		rec, err := s.ringReader.Read()
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, ringbuf.ErrClosed) {
				return
			}
			log.Printf("dwarfagent: ringbuf read: %v", err)
			return
		}
		sample, err := parseSample(rec.RawSample)
		if err != nil {
			log.Printf("dwarfagent: parseSample: %v", err)
			continue
		}
		if len(sample.PCs) == 0 {
			continue
		}
		agg(s, sample)
	}
}

// stashStack stores the PC chain for key if not already stashed.
// Must be called under s.mu.
func (s *session) stashStack(key sampleKey, pcs []uint64) {
	if s.stacks == nil {
		s.stacks = map[sampleKey][]uint64{}
	}
	if _, have := s.stacks[key]; !have {
		s.stacks[key] = append([]uint64(nil), pcs...)
	}
}

// collect drains accumulated samples, symbolizes them, and writes a
// gzipped pprof to w. sampleType distinguishes CPU vs off-CPU in the
// output. sampleRate is passed through to pprof builders (off-CPU can
// pass 0).
func (s *session) collect(w io.Writer, sampleType pprof.SampleType, sampleRate int) error {
	s.mu.Lock()
	samples := make(map[sampleKey]uint64, len(s.samples))
	stacks := make(map[sampleKey][]uint64, len(s.stacks))
	for k, v := range s.samples {
		samples[k] = v
	}
	for k, v := range s.stacks {
		stacks[k] = v
	}
	s.mu.Unlock()

	if len(samples) == 0 {
		log.Println("dwarfagent: no samples collected")
		return nil
	}

	builders := pprof.NewProfileBuilders(pprof.BuildersOptions{
		SampleRate:    int64(sampleRate),
		PerPIDProfile: false,
		Comments:      s.tags,
	})
	for key, val := range samples {
		frames := symbolizePID(s.symbolizer, key.pid, stacks[key])
		sample := pprof.ProfileSample{
			Pid:         key.pid,
			SampleType:  sampleType,
			Aggregation: pprof.SampleAggregated,
			Stack:       frames,
			Value:       val,
		}
		builders.AddSample(&sample)
	}
	for _, b := range builders.Builders {
		if _, err := b.Write(w); err != nil {
			return fmt.Errorf("write profile: %w", err)
		}
		break // single non-per-PID profile
	}
	return nil
}

// close tears down shared resources in the correct order: signal stop,
// wait reader goroutine, close reader, close watcher (which closes the
// tracker's event feed), wait tracker goroutine, close symbolizer, close
// BPF objects. Each profiler type wraps this with its own attach-link
// cleanup before delegating here.
//
// Idempotent at the stop-channel level. Returns the first non-nil error
// encountered.
func (s *session) close() error {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	s.readerWG.Wait()
	_ = s.ringReader.Close()
	_ = s.watcher.Close()
	s.trackerWG.Wait()
	if s.symbolizer != nil {
		s.symbolizer.Close()
	}
	return s.objs.Close()
}

// hashPCs is a stable, fast, collision-rare hash over a PC chain —
// FNV-1a byte-wise over all u64s. Used to dedupe identical stacks
// userspace-side so we don't re-symbolize the same chain on every
// sample. Collisions conflate counts but don't miss samples.
func hashPCs(pcs []uint64) uint64 {
	const (
		offset uint64 = 0xcbf29ce484222325
		prime  uint64 = 0x100000001b3
	)
	h := offset
	for _, pc := range pcs {
		for shift := uint(0); shift < 64; shift += 8 {
			h ^= (pc >> shift) & 0xff
			h *= prime
		}
	}
	return h
}
```

- [ ] **Step 1.2: Delete duplicate symbols from agent.go**

`hashPCs` currently lives in `agent.go`. Delete it from there — the `common.go` version takes over. Also delete `sampleKey` from `agent.go` and instead add it to `common.go` at top of the file (just before the session struct). Reason: both profilers use it; pulling it into common.go is the logical home.

Add to `common.go` just before `type sessionObjs`:

```go
// sampleKey is "(pid, stack hash)" — we dedupe identical stacks
// userspace-side to avoid re-symbolizing the same N-PC chain N times.
// The hash collides at the theoretical FNV rate (not cryptographic);
// collisions conflate counts but don't miss samples.
type sampleKey struct {
	pid  uint32
	hash uint64
}
```

Remove the duplicate declarations from `agent.go`.

- [ ] **Step 1.3: Write a unit test for hashPCs**

Create `unwind/dwarfagent/common_test.go`:

```go
package dwarfagent

import "testing"

func TestHashPCsStable(t *testing.T) {
	a := hashPCs([]uint64{0x1000, 0x2000, 0x3000})
	b := hashPCs([]uint64{0x1000, 0x2000, 0x3000})
	if a != b {
		t.Fatalf("same input → different hash: %#x vs %#x", a, b)
	}
}

func TestHashPCsDiffersByContent(t *testing.T) {
	a := hashPCs([]uint64{0x1000, 0x2000, 0x3000})
	b := hashPCs([]uint64{0x1000, 0x2000, 0x3001})
	if a == b {
		t.Fatalf("different input → same hash: %#x", a)
	}
}

func TestHashPCsEmpty(t *testing.T) {
	const want uint64 = 0xcbf29ce484222325 // FNV-1a offset basis
	if got := hashPCs(nil); got != want {
		t.Fatalf("empty chain hash = %#x, want %#x", got, want)
	}
}
```

- [ ] **Step 1.4: Verify the package still compiles + tests pass**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test ./unwind/dwarfagent/
```

Expected: all existing tests still pass; three new `TestHashPCs*` tests pass.

- [ ] **Step 1.5: Commit**

```
git add unwind/dwarfagent/common.go unwind/dwarfagent/common_test.go unwind/dwarfagent/agent.go
git commit -m "S8: extract session + hashPCs into common.go"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 2 — Refactor `Profiler` to use `session`

**Goal:** shrink `agent.go` by delegating shared lifecycle to `session`. The Profiler struct keeps only CPU-specific state; the constructor shrinks to BPF load + newSession + perf_event attach + goroutine start.

**Files:**
- Modify (rewrite): `unwind/dwarfagent/agent.go`

- [ ] **Step 2.1: Rewrite agent.go**

Replace the current contents of `unwind/dwarfagent/agent.go` with:

```go
package dwarfagent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
	"github.com/dpsoft/perf-agent/unwind/ehmaps"
)

// mmapEventSourceCloser is the local-to-dwarfagent interface that both
// *ehmaps.MmapWatcher and *ehmaps.MultiCPUMmapWatcher satisfy. Defined
// here for convenience; used by session.watcher as well.
type mmapEventSourceCloser interface {
	Events() <-chan ehmaps.MmapEventRecord
	Close() error
}

// Profiler is the DWARF-capable CPU profiler. Same public shape as
// profile.Profiler (Collect / CollectAndWrite / Close) so
// perfagent.Agent can swap on --unwind. Most of the heavy lifting
// lives in the embedded *session — Profiler only adds the per-CPU
// perf_event + RawLink slices.
type Profiler struct {
	*session
	sampleRate int
	perfFDs    []int
	perfLinks  []link.Link
}

// NewProfiler loads the perf_dwarf BPF program, wires ehmaps via
// newSession, opens per-CPU perf events at sampleRate Hz, attaches
// the BPF program to each, and starts the ringbuf reader + tracker
// goroutines. On error, every resource created is closed before
// returning.
func NewProfiler(pid int, systemWide bool, cpus []uint, tags []string, sampleRate int) (*Profiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadPerfDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load perf_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent")
	if err != nil {
		objs.Close()
		return nil, err
	}

	p := &Profiler{session: sess, sampleRate: sampleRate}
	if err := p.attachPerfEvents(objs.Program(), cpus, sampleRate); err != nil {
		_ = p.session.close()
		return nil, err
	}

	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateCPUSample)

	return p, nil
}

// attachPerfEvents opens one perf_event per CPU (pid=-1, cpu=N + BPF
// pids-map filter) and AttachRawLinks the BPF program to each. Populates
// p.perfFDs + p.perfLinks for Close to tear down later. On error, every
// fd and link opened so far is closed before returning.
func (p *Profiler) attachPerfEvents(prog *ebpf.Program, cpus []uint, sampleRate int) error {
	attr := &unix.PerfEventAttr{
		Type:   unix.PERF_TYPE_SOFTWARE,
		Config: unix.PERF_COUNT_SW_CPU_CLOCK,
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Sample: uint64(sampleRate),
		Bits:   unix.PerfBitFreq | unix.PerfBitDisabled,
	}
	cleanup := func() {
		for _, l := range p.perfLinks {
			_ = l.Close()
		}
		for _, fd := range p.perfFDs {
			_ = unix.Close(fd)
		}
		p.perfLinks = nil
		p.perfFDs = nil
	}
	for _, cpu := range cpus {
		fd, err := unix.PerfEventOpen(attr, -1, int(cpu), -1, unix.PERF_FLAG_FD_CLOEXEC)
		if err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			cleanup()
			return fmt.Errorf("perf_event_open cpu=%d: %w", cpu, err)
		}
		p.perfFDs = append(p.perfFDs, fd)
		rl, err := link.AttachRawLink(link.RawLinkOptions{
			Target:  fd,
			Program: prog,
			Attach:  ebpf.AttachPerfEvent,
		})
		if err != nil {
			cleanup()
			return fmt.Errorf("attach perf event cpu=%d: %w", cpu, err)
		}
		p.perfLinks = append(p.perfLinks, rl)
		if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
			cleanup()
			return fmt.Errorf("enable perf event cpu=%d: %w", cpu, err)
		}
	}
	if len(p.perfFDs) == 0 {
		return fmt.Errorf("no perf events attached — pid %d may have exited", p.pid)
	}
	return nil
}

// aggregateCPUSample is the CPU-specific ringbuf aggregator: each
// sample counts once; blocking-ns isn't meaningful here.
func aggregateCPUSample(s *session, sample Sample) {
	key := sampleKey{pid: sample.PID, hash: hashPCs(sample.PCs)}
	s.mu.Lock()
	s.samples[key]++
	s.stashStack(key, sample.PCs)
	s.mu.Unlock()
}

// Collect writes a gzipped pprof to w. Output is SampleTypeCpu with
// count-weighted samples.
func (p *Profiler) Collect(w io.Writer) error {
	return p.session.collect(w, pprof.SampleTypeCpu, p.sampleRate)
}

// CollectAndWrite is a file-path convenience wrapper.
func (p *Profiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close closes perf-event links + fds, then delegates the rest to
// session.close (ringbuf reader, watcher, symbolizer, BPF handle).
// Idempotent at the stop-channel level.
func (p *Profiler) Close() error {
	for _, l := range p.perfLinks {
		_ = l.Close()
	}
	for _, fd := range p.perfFDs {
		_ = unix.Close(fd)
	}
	p.perfLinks = nil
	p.perfFDs = nil
	return p.session.close()
}
```

- [ ] **Step 2.2: Verify tests still pass**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test ./unwind/dwarfagent/
```

Expected: all 3 sample-parser tests + 3 hashPCs tests + CAP-gated TestProfilerEndToEnd (skipped unprivileged) all build; non-gated tests PASS.

- [ ] **Step 2.3: DO NOT run capped binary**

Controller will rebuild dwarfagent.test and verify TestProfilerEndToEnd still PASSes under caps.

- [ ] **Step 2.4: Commit**

```
git add unwind/dwarfagent/agent.go
git commit -m "S8: Profiler embeds *session; delegates shared lifecycle"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 3 — Refactor `OffCPUProfiler` to use `session`

**Goal:** same treatment for the off-CPU sibling. Reduces `offcpu.go` from ~291 lines to ~110.

**Files:**
- Modify (rewrite): `unwind/dwarfagent/offcpu.go`

- [ ] **Step 3.1: Rewrite offcpu.go**

Replace the current contents of `unwind/dwarfagent/offcpu.go` with:

```go
package dwarfagent

import (
	"fmt"
	"io"
	"os"

	"github.com/cilium/ebpf/link"

	"github.com/dpsoft/perf-agent/pprof"
	"github.com/dpsoft/perf-agent/profile"
)

// OffCPUProfiler is the DWARF-capable off-CPU profiler. Same public
// shape as offcpu.Profiler. Most lifecycle lives in the embedded
// *session; OffCPUProfiler adds only the single tp_btf link.
type OffCPUProfiler struct {
	*session
	link link.Link
}

// NewOffCPUProfiler loads the offcpu_dwarf BPF program, wires ehmaps
// via newSession, attaches the tp_btf program via link.AttachTracing,
// and starts the ringbuf reader + tracker goroutines.
func NewOffCPUProfiler(pid int, systemWide bool, cpus []uint, tags []string) (*OffCPUProfiler, error) {
	if !systemWide && pid <= 0 {
		return nil, fmt.Errorf("dwarfagent: pid must be > 0 when systemWide=false")
	}
	objs, err := profile.LoadOffCPUDwarf(systemWide)
	if err != nil {
		return nil, fmt.Errorf("load offcpu_dwarf: %w", err)
	}
	if !systemWide {
		if err := objs.AddPID(uint32(pid)); err != nil {
			objs.Close()
			return nil, fmt.Errorf("add pid to filter: %w", err)
		}
	}

	sess, err := newSession(objs, pid, systemWide, cpus, tags, "dwarfagent (offcpu)")
	if err != nil {
		objs.Close()
		return nil, err
	}

	tpLink, err := link.AttachTracing(link.TracingOptions{Program: objs.Program()})
	if err != nil {
		_ = sess.close()
		return nil, fmt.Errorf("attach tp_btf: %w", err)
	}

	p := &OffCPUProfiler{session: sess, link: tpLink}
	sess.runTracker()
	sess.readerWG.Add(1)
	go sess.consumeRingbuf(aggregateOffCPUSample)

	return p, nil
}

// aggregateOffCPUSample is the off-CPU-specific ringbuf aggregator:
// samples that never saw a switch-IN (Value == 0) are skipped; valid
// samples add their blocking-ns into the accumulator.
func aggregateOffCPUSample(s *session, sample Sample) {
	if sample.Value == 0 {
		return
	}
	key := sampleKey{pid: sample.PID, hash: hashPCs(sample.PCs)}
	s.mu.Lock()
	s.samples[key] += sample.Value
	s.stashStack(key, sample.PCs)
	s.mu.Unlock()
}

// Collect writes a gzipped pprof to w. SampleType is off-CPU;
// sample values are accumulated blocking-ns.
func (p *OffCPUProfiler) Collect(w io.Writer) error {
	return p.session.collect(w, pprof.SampleTypeOffCpu, 0)
}

// CollectAndWrite is a file-path convenience wrapper.
func (p *OffCPUProfiler) CollectAndWrite(outputPath string) error {
	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create profile file: %w", err)
	}
	defer f.Close()
	return p.Collect(f)
}

// Close closes the tp_btf link, then delegates the rest to
// session.close. Idempotent.
func (p *OffCPUProfiler) Close() error {
	if p.link != nil {
		_ = p.link.Close()
		p.link = nil
	}
	return p.session.close()
}
```

- [ ] **Step 3.2: Verify tests still pass**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go test ./unwind/dwarfagent/
```

Expected: all tests compile + pre-existing non-gated tests PASS + CAP-gated tests SKIP.

- [ ] **Step 3.3: Commit**

```
git add unwind/dwarfagent/offcpu.go
git commit -m "S8: OffCPUProfiler embeds *session; delegates shared lifecycle"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 4 — Flip `--unwind auto` default to DWARF

**Goal:** user-facing change — `perf-agent --profile --pid N` (no explicit `--unwind`) now gets DWARF unwinding. Explicit `--unwind fp` continues to use FP.

**Files:**
- Modify: `main.go`
- Modify: `perfagent/agent.go`

- [ ] **Step 4.1: Change flag default**

In `main.go`, find:

```go
flagUnwind = flag.String("unwind", "fp", "Stack unwinding strategy: fp | dwarf | auto")
```

Change to:

```go
flagUnwind = flag.String("unwind", "auto", "Stack unwinding strategy: fp | dwarf | auto (default auto = dwarf)")
```

- [ ] **Step 4.2: Route "auto" to DWARF in perfagent dispatch**

In `perfagent/agent.go`, the CPU dispatch is:

```go
switch a.config.Unwind {
case "dwarf":
    // dwarf path
default:
    // fp path
}
```

Change to:

```go
switch a.config.Unwind {
case "dwarf", "auto":
    // dwarf path
default:
    // fp path
}
```

Do the same for the off-CPU dispatch block directly below.

- [ ] **Step 4.3: Smoke-test the new default**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS="-L/home/diego/github/blazesym/target/release -Wl,-Bstatic -lblazesym_c -Wl,-Bdynamic" go build -o /tmp/perf-agent .
/tmp/perf-agent --help 2>&1 | grep unwind
```

Expected output includes:
```
  -unwind string
    	Stack unwinding strategy: fp | dwarf | auto (default auto = dwarf) (default "auto")
```

- [ ] **Step 4.4: Run test-unit**

```
GOTOOLCHAIN=go1.26.0 make test-unit
```

Expected: all PASS.

- [ ] **Step 4.5: Commit**

```
git add main.go perfagent/agent.go
git commit -m "S8: flip --unwind auto default to DWARF path"
```

No `--no-verify`. No Co-Authored-By.

---

## Task 5 — Sanity matrix + doc update

**Goal:** confirm the refactor and default flip didn't regress anything. Mark S8 ✅.

- [ ] **Step 5.1: Rebuild capped test binaries**

```
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/profile.test ./profile/
GOTOOLCHAIN=go1.26.0 go test -c -o /home/diego/bin/ehmaps.test ./unwind/ehmaps/
GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/dwarfagent.test ./unwind/dwarfagent/
cd test && GOTOOLCHAIN=go1.26.0 CGO_CFLAGS="-I /usr/include/bpf -I /usr/include/pcap -I /home/diego/github/blazesym/capi/include" CGO_LDFLAGS=" -I /home/diego/github/blazesym/capi/include -L /usr/lib -L /home/diego/github/blazesym/target/release -lblazesym_c -static " go test -c -o /home/diego/bin/integration.test .
cd /home/diego/github/perf-agent && GOTOOLCHAIN=go1.26.0 make build
```

Ask the user to reapply caps on all four test binaries + the perf-agent binary.

- [ ] **Step 5.2: Run the full capped matrix**

```
/home/diego/bin/profile.test -test.v -test.run TestPerfDwarfLoads
/home/diego/bin/ehmaps.test -test.v
cd /home/diego/github/perf-agent/unwind/dwarfagent && /home/diego/bin/dwarfagent.test -test.v
cd /home/diego/github/perf-agent/test && /home/diego/bin/integration.test -test.v -test.run "TestPerfDwarf|TestPerfAgentDwarf|TestPerfAgentOffCPUDwarf|TestPerfAgentSystemWideDwarf"
```

Expected: every test PASS or SKIP cleanly.

- [ ] **Step 5.3: Update design doc**

In `docs/dwarf-unwinding-design.md`, update the S8 row:

```
| S8 ✅  | Flip `--unwind auto` default to DWARF-routed programs      | <1d     | CLI default change + release notes. Gated on real-workload validation from S5–S7. **Shipped**: see `docs/superpowers/plans/2026-04-24-s8-dwarfagent-cleanup-and-auto-default.md`. Bundled the dwarfagent.Profiler / OffCPUProfiler refactor — shared `session` struct eliminates ~70% duplication. |
```

- [ ] **Step 5.4: Commit**

```
git add docs/dwarf-unwinding-design.md docs/superpowers/plans/2026-04-24-s8-dwarfagent-cleanup-and-auto-default.md
git commit -m "S8: design doc status update + preserve implementation plan"
```

---

## Success criteria recap

From `docs/dwarf-unwinding-design.md` §Execution plan:

> **S8: Flip `--unwind auto` default to DWARF-routed programs** — CLI default change + release notes. Gated on real-workload validation from S5–S7.

Satisfied by:
- `main.go` default change + perfagent dispatch handling "auto" identically to "dwarf".
- All S5/S6/S7 integration tests continue to pass against the new default (they specify `--unwind dwarf` explicitly, so behavior is unchanged — the default flip is tested separately via help-text inspection and a smoke run).

Additionally (bundled in this plan):
- dwarfagent refactor eliminates the agent.go/offcpu.go duplication.

## Open risks

1. **Behavior change for existing users.** Anyone who upgraded past S8 and runs `perf-agent --profile --pid N` without specifying `--unwind` now gets DWARF unwinding instead of FP. For Go processes (no .eh_frame), ehcompile will fail on the main binary. AttachAllMappings handles that gracefully — libc + others still attach — but the cfi map won't cover the Go code. End result: Go stacks fall back to FP walking (which works since Go emits FP prologues). Net: no regression vs today's FP default, but the startup takes ~2s extra for the CFI compile. Document in release notes.
2. **Test hosts with few .eh_frame-bearing binaries.** On a Go-heavy host, some integration tests using `--unwind auto` (if added) could hit the same "ehcompile: no .eh_frame" path. Not an issue today since our existing integration tests specify `--unwind` explicitly.
3. **Refactor risk: embedded struct visibility.** Go's embedded-struct promotion makes `p.samples`, `p.stop`, etc. accessible on Profiler directly. If any test relied on those being on the outer struct literal, it'll still work (promoted fields compile identically). No other external visibility concerns.
4. **Map sharing between perf_dwarf and offcpu_dwarf** — still out of scope. When both are active, 2× CFI compile + 2× memory. For busy hosts this doubles startup time; document, defer to a future pinned-map plan.
