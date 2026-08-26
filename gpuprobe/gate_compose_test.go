package gpuprobe

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"kernel.org/pub/linux/libs/security/libcap/cap"
)

// TestPhase6GateConsumerHalf is the consumer-side quarter of the Phase 6 phase
// gate: assertions 10, 10a, 11 and 12 of Task 13, plus the half of assertion 2
// this package owns - the hop from the cubin transport into gpu.ModuleStore
// (issue #93), which used to be a pin here and is now an assertion.
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

	// 2, at the level this package owns: the hop from the cubin transport to
	// gpu.ModuleStore. Until issue #93 was closed this slot held a PIN - a
	// test that passed BECAUSE the hop was missing - because assertion 2 was
	// not reachable through the shipping path at all: Attach installed a
	// placeholder store with no line table, and gpuprobe.Config had no field
	// by which a caller could supply the real one. It is now the real thing: a
	// cubin crosses the socket and comes out of the projection as a named line
	// of the CUDA source it was built from.
	//
	// Catches: a sink that writes into a store the projection cannot see (the
	// bug #93 filed, in its subtler form); a wiring that remembers CRCs
	// outside the store, which would make one eviction permanent while every
	// counter read healthy; a Put error translated into a transport rejection,
	// which would have the transport report a refusal for a cubin the agent is
	// holding.
	t.Run("assertion-02-an-offered-cubin-becomes-a-source-line",
		TestACubinOfferedOverTheChannelBecomesASourceLine)
	t.Run("assertion-02-an-evicted-module-is-no-module-and-can-return",
		TestAnEvictedModuleAnswersNoModuleAndIsOfferedAgainNotSuppressed)
	t.Run("assertion-02-an-unreadable-cubin-still-lands-and-is-counted-apart",
		TestACubinTheStoreCannotParseStillLandsAndIsCountedApart)
	// And assertion 4's floor: no-module stays reachable after the wiring,
	// both from a consumer that configured no store at all and from a CRC
	// nothing was ever offered under.
	t.Run("assertion-04-no-store-is-a-supported-no-module-state",
		TestAConsumerWithNoModuleStoreKeepsTheBoundedPlaceholder)
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
