package gpuprobe

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dpsoft/perf-agent/gpu"
	"github.com/dpsoft/perf-agent/internal/cubin"
	pp "github.com/dpsoft/perf-agent/pprof"
)

// The hop issue #93 was about: a cubin that crosses the transport must end up
// in the gpu.ModuleStore the join and the projection read, and it must end up
// in THAT store rather than in a copy of it.
//
// These run without capabilities and without a GPU. The producer here is this
// test process, offering a real checked-in cubin over the real abstract socket
// as a real sealed memfd - which is byte-for-byte what shim/core/cubin.cc
// does, pinned from the other side by
// TestTheCppProducerAndTheGoCubinListenerAgreeOnTheWire. What is NOT exercised
// here is the BPF decode of gpu_pc_sample_batch_v1; the privileged gate covers
// that with 64 real records off a real uprobe_multi link.

// storeFixturePath locates a Task 1 cubin fixture. Read from
// internal/cubin/testdata rather than copied into this package: two copies of
// a binary fixture drift, and the point is that this store answers about the
// same bytes internal/cubin asserts its own claims against.
func storeFixturePath(name string) string {
	return filepath.Join("..", "internal", "cubin", "testdata", name)
}

func storeFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(storeFixturePath(name))
	require.NoError(t, err, "cubin fixture %s", name)
	require.NotEmpty(t, b)
	return b
}

// storeFixtureSymIndex reads the .symtab index the module store keys its
// functionIndex table on out of the fixture itself. Whether CUPTI's
// functionIndex IS that index is the design's premise, measured on hardware;
// nothing here depends on the answer, only on the store and the sample
// agreeing.
func storeFixtureSymIndex(t *testing.T, b []byte, fn string) uint32 {
	t.Helper()
	c, err := cubin.Parse(b)
	require.NoError(t, err)
	for _, f := range c.Functions() {
		if f.Name == fn {
			require.GreaterOrEqual(t, f.SymIndex, 0)
			return uint32(f.SymIndex)
		}
	}
	t.Fatalf("fixture has no function %q", fn)
	return 0
}

// offerFixture puts one cubin on the wire the way the shim does and returns
// the listener's reply byte.
func offerFixture(t *testing.T, l *cubinListener, body []byte, crc uint64) byte {
	t.Helper()
	fd := sealedCubinFD(t, body, cubinRequiredSeals)
	return offerCubin(t, l.address(), offerHeader(uint64(len(body)), crc), fd)
}

// The CRCs these tests offer under. Arbitrary, exactly as cubin_crc's value is
// arbitrary to this end: what matters is that the number the bytes arrived
// under is the number a later PC sample resolves against.
const (
	wiredCRCLineInfo   uint64 = 0x9317E0001
	wiredCRCNoLineInfo uint64 = 0x9317E0002
	wiredCRCAbsent     uint64 = 0x9317E0003
)

// TestACubinOfferedOverTheChannelBecomesASourceLine is gate assertion 2's
// transport half, driven end to end through the SHIPPING path: a cubin crosses
// the real socket, the listener writes it into the store the consumer's Config
// named, and a PC sample carrying that CRC comes out of the projection as a
// named line of the CUDA source the cubin was built from - on a sample whose
// frames are the CPU call path that launched the kernel.
//
// Before issue #93 was closed this could not be written at all: the listener's
// sink was a placeholder map with no line table and no Resolve, and
// gpuprobe.Config had no field by which a caller could supply anything else.
// The pin that recorded that gap - TestGateTheCubinTransportDoesNotYetFeedThe
// ModuleStore - is deleted, and this is what replaced it.
//
// Mutations this catches: a sink that stores into a store of its own rather
// than the caller's (the projection would then resolve nothing); a listener
// that ignores Config.Modules; a Put that stores the header rather than the
// payload; a resolution naming a line the fixture's table does not contain.
func TestACubinOfferedOverTheChannelBecomesASourceLine(t *testing.T) {
	const pid = 4242
	lineInfo := storeFixture(t, "single_lineinfo.cubin")
	noLineInfo := storeFixture(t, "single_nolineinfo.cubin")
	fnIndex := storeFixtureSymIndex(t, lineInfo, "addOne")

	// The store the DRIVER builds, exactly as cmd/gpu-cuda-profile does, and
	// the one all three readers are handed.
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{})
	l := testCubinListener(t, Config{ShimPath: selfExe(t), Modules: store}, nil)

	// Structurally the caller's store, not one this package made: a listener
	// writing into a private store would satisfy every counter below and
	// resolve nothing in the profile.
	sink, ok := l.sink.(moduleStoreSink)
	require.True(t, ok, "Config.Modules did not become the listener's sink (it is %T)", l.sink)
	require.Same(t, store, sink.store, "the listener writes into a DIFFERENT store from the one the caller supplied")

	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, lineInfo, wiredCRCLineInfo))
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, noLineInfo, wiredCRCNoLineInfo))

	st := l.snapshot()
	assertNoCubinRejections(t, st)
	require.Equal(t, uint64(2), st.received)
	require.Equal(t, uint64(len(lineInfo)+len(noLineInfo)), st.bytes)

	// The store holds them, and holds them as MODULES rather than as bytes:
	// one parsed with a line table, one parsed without.
	ms := store.Stats()
	assert.Equal(t, 2, store.Len())
	assert.Equal(t, uint64(2), ms.ModulesStored)
	assert.Equal(t, uint64(1), ms.ModulesWithLineInfo)
	assert.Equal(t, uint64(1), ms.ModulesWithoutLineInfo)
	assert.Zero(t, ms.ModulesUnparseable, "a cubin arrived that the store could not parse")
	assert.Zero(t, ms.ModulesEvicted)

	// --- and now the profile, against the same store instance.
	tl := gpu.NewTimeline(gpu.TimelineConfig{Modules: store})
	corr := gpu.CorrelationID{Backend: gpu.BackendCUPTI, PID: pid, Value: "17"}
	require.NoError(t, tl.EmitLaunch(gpu.GPUKernelLaunch{
		Correlation: corr,
		KernelName:  "addOne",
		TimeNs:      10,
		Launch: gpu.LaunchContext{
			PID:          pid,
			TimeNs:       10,
			CPUStack:     pp.FramesFromNames([]string{"main", "run_training_step", "cudaLaunchKernel"}),
			SamplePeriod: 8,
		},
	}))
	// Tier B: no correlation value. The sample reaches its execution through
	// cubin_crc -> module -> function name, which is the store's other reader.
	require.NoError(t, tl.EmitPCSample(gpu.GPUPCSample{
		Correlation:   gpu.CorrelationID{Backend: gpu.BackendCUPTI, PID: pid},
		Module:        gpu.ModuleRef{Backend: gpu.BackendCUPTI, CRC: wiredCRCLineInfo},
		FunctionIndex: fnIndex,
		TimeNs:        20,
		PCOffset:      0x10,
		StallReason:   "long_scoreboard",
		Count:         1,
	}))
	require.NoError(t, tl.EmitExec(gpu.GPUKernelExec{
		Correlation: corr,
		KernelName:  "addOne",
		StartNs:     30,
		EndNs:       130,
	}))

	snap := tl.Snapshot()
	require.Len(t, snap.Executions, 1)
	require.Len(t, snap.Executions[0].PCSamples, 1,
		"the sample never reached its execution through the store the transport filled")

	samples, _ := gpu.ProjectExecutionsWith(snap, gpu.ProjectionConfig{Modules: store})
	require.Len(t, samples, 1)
	s := samples[0]

	var names []string
	for _, f := range s.Stack {
		names = append(names, f.Name)
	}
	assert.Equal(t,
		[]string{"main", "run_training_step", "cudaLaunchKernel", gpu.FrameLaunch, "[gpu:kernel:addOne]"},
		names, "the frames must still be the launching CPU call path, the boundary marker and the kernel")

	require.Equal(t, "resolved", s.Labels["gpu_src_status"],
		"the cubin crossed the transport and the store still cannot resolve against it")
	assert.Equal(t, "single.cu", s.Labels["gpu_src_file"], "the basename, never the build-host path")
	assert.Equal(t, "addOne", s.Labels["gpu_src_func"])
	require.Contains(t, s.Labels, "gpu_src_line")

	// The line names a real, non-blank line of the fixture's own source, read
	// from the .cu rather than pinned as a constant: a fixture rebuilt from
	// edited source would otherwise keep this green while the label pointed
	// somewhere else.
	line, err := strconv.Atoi(s.Labels["gpu_src_line"])
	require.NoError(t, err)
	src := strings.Split(strings.TrimRight(string(storeFixture(t, "single.cu")), "\n"), "\n")
	require.Positive(t, line)
	require.LessOrEqual(t, line, len(src),
		"gpu_src_line=%d is past the end of single.cu (%d lines)", line, len(src))
	body := strings.TrimSpace(src[line-1])
	assert.NotEmpty(t, body, "gpu_src_line names a blank line of single.cu")
	t.Logf("wire -> store -> profile: %s -> %s:%s  %q",
		strings.Join(names, " -> "), s.Labels["gpu_src_file"], s.Labels["gpu_src_line"], body)

	// The other fixture, through the store's own reader, so that "the
	// transport fed the store" is not one lucky module: the no-lineinfo cubin
	// arrived too, and it answers the status that names its build rather than
	// the status that names a missing module.
	assert.Equal(t, gpu.SrcNoLineInfo,
		store.Resolve(wiredCRCNoLineInfo, storeFixtureSymIndex(t, noLineInfo, "addOne"), 0x10).Status())
	// And a CRC nothing was offered under still answers no-module, which is
	// what keeps that status reachable after this wiring (gate assertion 4).
	assert.Equal(t, gpu.SrcNoModule, store.Resolve(wiredCRCAbsent, fnIndex, 0x10).Status())
}

// TestAnEvictedModuleAnswersNoModuleAndIsOfferedAgainNotSuppressed is the
// bound half of the same wiring, and it is the one that decides whether the
// hop can lie.
//
// gpu.ModuleStore is bounded and evicts least-recently-used; Task 4's
// TestResolveAfterEvictionIsNoModuleNotStale pins that an evicted module
// answers no-module rather than a stale line. The transport asks HasCubin
// BEFORE it maps a payload and treats "yes" as a counted no-op, so the wiring
// has exactly one way to break that guarantee: remember CRCs somewhere other
// than the store. It would look like an optimisation - the same bytes, why map
// them twice - and it would make one eviction permanent, with CubinsDuplicate
// climbing and every store counter reading healthy while every PC sample for
// that module said no-module forever.
//
// So: evict, and require that the module comes BACK when it is offered again.
func TestAnEvictedModuleAnswersNoModuleAndIsOfferedAgainNotSuppressed(t *testing.T) {
	lineInfo := storeFixture(t, "single_lineinfo.cubin")
	noLineInfo := storeFixture(t, "single_nolineinfo.cubin")
	fnIndex := storeFixtureSymIndex(t, lineInfo, "addOne")

	// One module at a time, so the second offer evicts the first.
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{Capacity: 1})
	l := testCubinListener(t, Config{ShimPath: selfExe(t), Modules: store}, nil)

	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, lineInfo, wiredCRCLineInfo))
	before := store.Resolve(wiredCRCLineInfo, fnIndex, 0x10)
	require.Equal(t, gpu.SrcResolved, before.Status())
	_, _, wantLine, ok := before.Source()
	require.True(t, ok)

	// A second offer while the first is still held is the counted no-op, and
	// it costs no mapping: that is the property HasCubin exists for.
	mappedBefore := l.snapshot().mapped
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, lineInfo, wiredCRCLineInfo))
	st := l.snapshot()
	assert.Equal(t, uint64(1), st.duplicate, "a re-offer of a HELD crc must be the counted no-op")
	assert.Equal(t, mappedBefore, st.mapped, "a duplicate offer mapped the payload anyway")
	assert.Equal(t, uint64(1), st.received)

	// Now push it out.
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, noLineInfo, wiredCRCNoLineInfo))
	require.Equal(t, uint64(1), store.Stats().ModulesEvicted, "the bound did not bite, so this proves nothing")

	assert.Equal(t, gpu.SrcNoModule, store.Resolve(wiredCRCLineInfo, fnIndex, 0x10).Status(),
		"an evicted module answered something other than no-module: the wiring is serving a stale answer")

	// And the recovery: the store no longer holds it, so the transport must
	// admit it again rather than suppressing it as a duplicate.
	require.False(t, l.sink.HasCubin(wiredCRCLineInfo),
		"the wiring remembers a CRC the store has evicted; one eviction would then be permanent")
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, lineInfo, wiredCRCLineInfo))
	st = l.snapshot()
	assert.Equal(t, uint64(3), st.received,
		"the re-offer of an evicted module was not stored (offers that stored: the first, the no-lineinfo one, and this)")
	assert.Equal(t, uint64(1), st.duplicate, "the re-offer of an EVICTED module was miscounted as a duplicate")

	after := store.Resolve(wiredCRCLineInfo, fnIndex, 0x10)
	require.Equal(t, gpu.SrcResolved, after.Status(), "a re-offered module did not become resolvable again")
	_, _, gotLine, ok := after.Source()
	require.True(t, ok)
	assert.Equal(t, wantLine, gotLine, "the same PC resolved to a different line after a round trip through eviction")
}

// TestACubinTheStoreCannotParseStillLandsAndIsCountedApart pins the one place
// the two contracts the adapter joins disagree.
//
// gpu.ModuleStore.Put returns the parse error as a DIAGNOSTIC: unparseable
// bytes are still stored (so a re-offer is not re-parsed), counted in
// ModulesUnparseable, and resolve as no-module. cubinSink.PutCubin's error
// means "the offer did not land", and the listener answers it with a refusal
// counted in CubinsRejectedMalformed.
//
// Propagating one as the other would make the transport report a rejection for
// a cubin the agent is holding: CubinsReceived and CubinBytesReceived would
// understate what is held (and the total-bytes ceiling with them), a healthy
// run would show a non-zero rejection counter, and the producer would be told
// 'X' for an offer that landed. The two facts belong in different counters
// because they are different facts - what arrived, and what could be read.
func TestACubinTheStoreCannotParseStillLandsAndIsCountedApart(t *testing.T) {
	store := gpu.NewModuleStore(gpu.ModuleStoreConfig{})
	l := testCubinListener(t, Config{ShimPath: selfExe(t), Modules: store}, nil)

	// Well-formed on the wire, not a cubin underneath: exactly what a
	// truncated or garbled module would look like to this end, since the
	// transport deliberately does not parse.
	body := cubinFixture(4096)
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, body, wiredCRCLineInfo),
		"the offer was refused; the bytes crossed and are held, so the transport must say so")

	st := l.snapshot()
	assertNoCubinRejections(t, st)
	assert.Equal(t, uint64(1), st.received)
	assert.Equal(t, uint64(len(body)), st.bytes)

	ms := store.Stats()
	assert.Equal(t, uint64(1), ms.ModulesStored)
	assert.Equal(t, uint64(1), ms.ModulesUnparseable,
		"the store did not count the module it cannot read; the loss would then be invisible")
	assert.Zero(t, ms.ModulesWithLineInfo)

	assert.Equal(t, gpu.SrcNoModule, store.Resolve(wiredCRCLineInfo, 0, 0x10).Status(),
		"bytes we hold and cannot read are no-module - we have nothing to resolve against")
	// And it is not re-parsed on every offer: the store holds it, so the
	// transport's duplicate path takes over.
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, body, wiredCRCLineInfo))
	assert.Equal(t, uint64(1), l.snapshot().duplicate)
}

// TestAConsumerWithNoModuleStoreKeepsTheBoundedPlaceholder pins the nil case
// as a supported configuration rather than an accident.
//
// A backend that does not resolve source lines passes no store. The channel
// still binds, offers are still authenticated, admitted and counted, and the
// bytes land in a bounded map nothing reads - so every PC sample carries
// gpu_src_status="no-module", which is the truth for it: there is no store to
// resolve against, and for the reader that is the same fact as a cubin that
// never arrived.
//
// Nothing here constructs a store on the caller's behalf, deliberately. A
// store gpuprobe owned would be one gpu.ProjectionConfig cannot see, which is
// the bug issue #93 filed - a store with bytes in it and no reader.
func TestAConsumerWithNoModuleStoreKeepsTheBoundedPlaceholder(t *testing.T) {
	sink := cubinSinkFor(Config{})
	mem, ok := sink.(*memCubinStore)
	require.True(t, ok, "a consumer with no Config.Modules got a %T", sink)
	assert.Equal(t, defaultCubinStoreCapacity, mem.cap, "the placeholder must still be bounded")

	l := testCubinListener(t, Config{ShimPath: selfExe(t)}, nil)
	_, ok = l.sink.(*memCubinStore)
	assert.True(t, ok, "Attach's default sink for a store-less consumer is %T", l.sink)

	body := storeFixture(t, "single_lineinfo.cubin")
	require.Equal(t, byte(cubinReplyOK), offerFixture(t, l, body, wiredCRCLineInfo),
		"a store-less consumer must still accept and account for an offer")
	assert.Equal(t, uint64(1), l.snapshot().received)

	// The reader's side of the same fact: with no store the projection
	// resolves an empty one, and says no-module rather than saying nothing.
	assert.Equal(t, gpu.SrcNoModule,
		gpu.NewModuleStore(gpu.ModuleStoreConfig{}).Resolve(wiredCRCLineInfo, 0, 0x10).Status())
}
