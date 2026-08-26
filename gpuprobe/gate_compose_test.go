package gpuprobe

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// TestPhase6GateConsumerHalf is the consumer-side quarter of the Phase 6 phase
// gate: assertions 10, 10a, 11 and 12 of Task 13.
//
// It is a SECOND gate entry point, in package gpuprobe rather than
// gpuprobe_test, and both halves of that are load-bearing:
//
//   - second, because TestStubDrivesThePipelineToPprofWithoutAGPU and the
//     end-to-end PC-sampling gate beside it need CAP_BPF, CAP_PERFMON and
//     CAP_CHECKPOINT_RESTORE and SKIP without them. Assertions 10, 10a, 11 and
//     12 need no capability at all - the cubin channel is an AF_UNIX socket and
//     a memfd, the decode path is pure Go - so putting them behind the
//     privileged skip would be putting four assertions behind a gate nobody
//     without a GPU box can run;
//   - in-package, because the tests these compose are in-package. Calling them
//     is the point: the plan asks the gate to COMPOSE the assertions the tasks
//     already made rather than to write second copies of them, and a second
//     copy of an assertion is two assertions of one fact that drift apart.
//
// See gpu/gate_test.go for assertions 1-9 and 10b, and
// gpuprobe/gate_test.go for the privileged end-to-end run.
func TestPhase6GateConsumerHalf(t *testing.T) {
	// 10. The cubin channel cannot touch enrolment, in BOTH orders -
	//     including offers flooded AHEAD of an enrolment, which is the
	//     direction a shared socket fails and the easy direction is not.
	//     CubinsThrottled non-zero while UnwindEnrollThrottled is unchanged,
	//     and the enrolment still succeeds.
	//
	//     Catches: moving cubin offers onto the enrolment listener, its
	//     goroutine, its Accept loop or its admission bucket. Any of those
	//     restores issue #49's ~38% stack loss on a module-heavy workload,
	//     silently, with only UnwindEnrollThrottled moving.
	t.Run("assertion-10-cubins-cannot-starve-or-throttle-enrolment",
		TestFloodingTheCubinChannelCannotStarveOrThrottleAnEnrolment)
	t.Run("assertion-10-the-buckets-are-separate-objects", TestTheCubinAdmissionBucketIsItsOwn)
	t.Run("assertion-10-they-are-not-the-same-socket",
		TestTheCubinAddressIsASiblingOfTheRendezvousAndNotTheSameSocket)
	// The regression that keeps them apart forever. The enrolment handler must
	// never read from its connection: a read would block until the producer's
	// 2s budget expired, turning every rendezvous into a 2s stall ending in
	// kEnrollError. Discriminating an offer from an enrolment on one socket
	// REQUIRES such a read, so this is the assertion that makes the shared
	// socket unbuildable rather than merely unbuilt.
	t.Run("assertion-10-enrolment-performs-no-read", TestAnEnrolmentCompletesWithNoReadOnThatConnection)

	// 10a. Seals are enforced: a memfd missing ANY required seal is rejected,
	//      counted in CubinsRejectedUnsealed, and never mapped.
	//
	//      Catches: dropping a seal from the required set, or - worse - a
	//      fallback that maps it anyway. Without F_SEAL_SHRINK a peer can
	//      ftruncate under our mmap and SIGBUS the agent; without F_SEAL_WRITE
	//      the ELF mutates under the parser mid-parse. Falling back is how a
	//      defended path becomes an undefended one.
	t.Run("assertion-10a-each-missing-seal-is-rejected-and-never-mapped",
		TestEachRequiredSealMissingInTurnIsRejectedAndNeverMapped)
	t.Run("assertion-10a-a-plain-fd-is-not-a-sealed-memfd", TestADescriptorThatIsNotASealedMemfdIsRefused)
	t.Run("assertion-10a-the-required-seals-are-spelt-out", TestSealNamesAreSpeltOut)

	// 11. The capability set does not grow: no cap_sys_admin, anywhere.
	t.Run("assertion-11-no-cap-sys-admin", TestPhase6GateBinaryDoesNotAskForCapSysAdmin)

	// 12. Stats.Undecoded is zero for every kind this phase decodes.
	//
	//     Catches: a KIND_* added on one side of the wire and not the other,
	//     which is silent loss unless counted - and, in the other direction, a
	//     decode arm removed, which would put a kind back into the default arm
	//     while every other counter still read healthy.
	t.Run("assertion-12-the-five-pc-kinds-are-decoded",
		TestTheFivePCSamplingKindsAreDecodedNotCountedUndecoded)
	t.Run("assertion-12-a-healthy-tier-b-run-loses-nothing",
		TestHealthyTierBRunLeavesEveryNewLossCounterAtZero)
	t.Run("assertion-12-unknown-kinds-are-still-counted", TestUndecodedKindsAreCountedNotDropped)
	t.Run("assertion-12-kindmax-matches-the-bpf-object", TestEmbeddedProgramIsUprobeMulti)
	t.Run("assertion-12-every-cookie-has-a-sized-kind", TestBPFSizesEveryKindCookieForInstalls)

	// Not one of the twelve: the wiring gap that stops assertion 2 from being
	// reachable by the shipping product. Pinned here so it fails the moment it
	// is closed, rather than being remembered only in a report.
	t.Run("outstanding-cubin-transport-does-not-feed-the-module-store",
		TestGateTheCubinTransportDoesNotYetFeedTheModuleStore)
}

// TestPhase6GateBinaryDoesNotAskForCapSysAdmin is gate assertion 11, the
// standing Phase 1 assertion: the capability set this pipeline runs under is
// cap_bpf,cap_perfmon,cap_checkpoint_restore and it does not grow.
//
// # Why this is worth a test rather than a README line
//
// CAP_SYS_ADMIN is the difference between an agent that can run as one pod
// among many and an agent that is effectively root on the node. Nothing about
// losing it is loud: the two ways to acquire the requirement are both quiet
// one-liners a reviewer would wave through -
//
//   - attaching with link.Uprobe instead of link.UprobeMulti, which routes
//     through the perf_uprobe PMU (measured: needs CAP_SYS_ADMIN, while the BPF
//     link does not - see Attach's doc comment and
//     TestEmbeddedProgramIsUprobeMulti, composed above);
//   - reading the producer's address space to follow gpu_module_load_v1's
//     bytes_ptr, which needs /proc/<pid>/mem or process_vm_readv and therefore
//     CAP_SYS_PTRACE, and which the whole cubin channel exists to avoid.
//
// Both would be found on a developer box that runs the tests as root, where
// everything works and nothing says why. So this asserts the negative from two
// independent directions, because either one alone is escapable:
//
//  1. the FILE capabilities of the test binary itself, read with getcap(8) -
//     which is the plan's own wording, and which is the thing a CI job or a
//     developer following the plan's setcap line actually installs;
//  2. this process's own Permitted set, which is where a capability acquired
//     any other way (a setuid wrapper, an inherited ambient set) would show up.
//
// Running as root makes (2) vacuous - root has the full set - so it is skipped
// there rather than asserted falsely, and (1) still runs, because a file
// capability set naming cap_sys_admin is wrong whoever executes it.
func TestPhase6GateBinaryDoesNotAskForCapSysAdmin(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	// --- 1. the file capabilities of this very binary.
	//
	// cap.GetFile rather than shelling out to getcap(8): it reads the same
	// security.capability xattr getcap prints, needs no external tool, and
	// cannot be defeated by getcap being absent from a container image - which
	// would otherwise turn this assertion into a skip on exactly the machines
	// that most need it.
	set, err := cap.GetFile(self)
	switch {
	case err != nil:
		// No file capabilities at all. That is the ordinary state for an
		// unprivileged `go test` run and for a sudo run, and it is the
		// strongest possible form of "no cap_sys_admin".
		t.Logf("no file capabilities on %s (%v): nothing to grant cap_sys_admin", self, err)
	default:
		for _, flag := range []cap.Flag{cap.Effective, cap.Permitted, cap.Inheritable} {
			have, gerr := set.GetFlag(flag, cap.SYS_ADMIN)
			require.NoError(t, gerr)
			assert.False(t, have,
				"the gate binary's file capabilities name cap_sys_admin in the %v set (%s): "+
					"this pipeline runs on cap_bpf,cap_perfmon,cap_checkpoint_restore and the "+
					"capability set does not grow. The two ways to acquire this requirement are "+
					"attaching through the perf_uprobe PMU instead of a uprobe_multi BPF link, "+
					"and reading the producer's address space to follow bytes_ptr",
				flag, set)
		}
		t.Logf("file capabilities on %s: %s", self, set)
	}

	// --- 2. this process's own set.
	if os.Geteuid() == 0 {
		t.Log("running as root: the process capability half of this assertion is vacuous and is skipped; " +
			"the file-capability half above still ran")
		return
	}
	proc := cap.GetProc()
	require.NotNil(t, proc)
	for _, flag := range []cap.Flag{cap.Effective, cap.Permitted} {
		have, gerr := proc.GetFlag(flag, cap.SYS_ADMIN)
		require.NoError(t, gerr)
		assert.False(t, have,
			"this process holds cap_sys_admin in the %v set (%s); the gate is supposed to prove "+
				"the pipeline works WITHOUT it, and a run that holds it cannot",
			flag, proc)
	}
	t.Logf("process capabilities: %s", proc)
}

// TestGetcapAgreesWithTheLibraryReading cross-checks the assertion above
// against the tool the plan's wording actually names.
//
// Assertion 11 is written as "getcap on the gate binary shows no
// cap_sys_admin". The test above reads the same xattr through libcap rather
// than through getcap(8), which is stricter in one direction (it cannot be
// skipped by the tool being missing) and weaker in another: if libcap's
// reading and getcap's ever disagreed, the assertion would be about something
// other than what an operator sees. So when getcap IS available, run it and
// require the two to agree.
//
// Skipped, not failed, when getcap is absent: the assertion it backs has
// already run through libcap, and a missing binutils-ish tool is not a defect
// in this pipeline.
func TestGetcapAgreesWithTheLibraryReading(t *testing.T) {
	path, err := exec.LookPath("getcap")
	if err != nil {
		t.Skip("getcap(8) unavailable; TestPhase6GateBinaryDoesNotAskForCapSysAdmin " +
			"reads the same xattr through libcap and has already run")
	}
	self, err := os.Executable()
	require.NoError(t, err)

	out, err := exec.Command(path, self).CombinedOutput()
	require.NoError(t, err, "getcap %s: %s", self, out)
	text := string(out)
	t.Logf("getcap %s -> %q", self, strings.TrimSpace(text))
	assert.NotContains(t, strings.ToLower(text), "cap_sys_admin",
		"getcap names cap_sys_admin on the gate binary")
}

// TestGateTheCubinTransportDoesNotYetFeedTheModuleStore pins the gap that
// stops the Phase 6 exit condition from being reachable by the shipping
// product, so that it is a named, self-invalidating fact rather than prose in
// a report nobody re-reads.
//
// # The gap
//
// Task 3 built the cubin channel and Task 4 built gpu.ModuleStore - the store
// that turns (cubin_crc, functionIndex, pcOffset) into a source line and is
// "the single place gpu_src_status is decided". Nothing connects them:
//
//   - Attach calls newCubinListener(cfg, nil), and a nil sink becomes
//     memCubinStore - a bounded map of CRC to bytes with no line table, no LRU
//     and no Resolve. Its own comment says "Task 4 replaces it";
//   - gpuprobe.Config has no field by which a caller could supply one, and
//     gpu.ModuleStore does not implement cubinSink (Put/HasCubin versus
//     PutCubin/HasCubin) even if it had;
//   - cmd/gpu-cuda-profile builds neither: it calls ProjectExecutionsWith with
//     a zero ProjectionConfig and NewTimeline without Modules.
//
// So on hardware today, every cubin is received, sealed, verified, size- and
// identity-checked, stored - and then never read. Every PC sample in a real
// profile reads gpu_src_status="no-module", and gate assertion 2 (a source
// line reached from a CPU stack) cannot be satisfied end to end by the product
// as shipped. TestStubDrivesPCSamplingToPprofWithoutAGPU therefore builds the
// store itself, from the CRCs the producer declared, and says so.
//
// This is the ONE hop between the transport and the labels, and both ends of
// it are built and tested. It is a wiring task, not a design gap.
//
// # Why a passing test rather than a failing one
//
// Same reason as gpu's TestGateGraphExecutionRefusalIsNotAssertableYet: the
// gate must not ship red, and it must not ship silently short of an assertion
// either. When the hop is wired, this test fails - by name, in the gate's own
// file - and the person wiring it deletes it and drops the injection from the
// end-to-end gate.
func TestGateTheCubinTransportDoesNotYetFeedTheModuleStore(t *testing.T) {
	// The default sink a real Attach installs.
	l, err := newCubinListener(Config{ShimPath: selfExe(t)}, nil)
	require.NoError(t, err)
	defer func() { _ = l.close() }()

	_, isPlaceholder := l.sink.(*memCubinStore)
	assert.True(t, isPlaceholder,
		"the cubin listener's default sink is no longer the placeholder memCubinStore (it is %T). "+
			"If that is gpu.ModuleStore, gate assertion 2 is now reachable end to end: delete this "+
			"test and drop the PC-sample injection from TestStubDrivesPCSamplingToPprofWithoutAGPU.",
		l.sink)

	// And no caller can supply one, which is why the default is the whole
	// story rather than merely a default.
	typ := reflect.TypeOf(Config{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		assert.NotContains(t, name, "module",
			"gpuprobe.Config grew a field naming a module store (%s); the hop may now be wired - see this test's doc comment",
			typ.Field(i).Name)
		assert.NotContains(t, name, "store",
			"gpuprobe.Config grew a store field (%s); the hop may now be wired - see this test's doc comment",
			typ.Field(i).Name)
	}
	t.Log("cubin transport -> gpu.ModuleStore: OUTSTANDING - the bytes arrive and stop at " +
		"memCubinStore; see .superpowers/sdd/task-13-gate-report.md")
}
