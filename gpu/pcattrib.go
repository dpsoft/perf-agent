package gpu

import "fmt"

// PCAttrib is gpu_pc_attrib: HOW a PC sample reached the execution it is
// attached to, and therefore how much the attachment can be trusted.
//
// # Why this is its own label and not ExecutionView.Ambiguous
//
// ExecutionView.Ambiguous means one thing and only one thing: the heuristic
// LAUNCH join found more than one candidate launch for a correlation-less
// execution and picked one (see findLaunchHeuristic, and
// JoinStats.AmbiguousHeuristicMatchCount, which counts exactly that). PC
// attribution is a different join, over different inputs, with a different
// failure mode. Reusing that boolean would put two unrelated facts on one bit
// and would emit gpu_join="exact" gpu_ambiguous="true" on a single sample - an
// execution whose launch was joined by vendor correlation, exactly, while its
// PC samples were inferred. A reader has no way to tell which of the two the
// flag is about, and AmbiguousHeuristicMatchCount stops counting what its name
// says. So the two stay entirely separate: Ambiguous keeps its meaning
// untouched, and PC-attribution quality lives here.
//
// # The four values
//
//	exact               the sample carried a vendor correlation and joined
//	                    through it. Kernel-serialized collection only; this is
//	                    vendor-provided truth, not an inference.
//	kernel              joined through the module: the sample's
//	                    (cubin CRC, functionIndex) named a device function,
//	                    and exactly one execution of that kernel was in the
//	                    horizon. The sample is on the right kernel; which
//	                    invocation of it is not in question because there was
//	                    only one.
//	kernel-ambiguous    the same join, with MORE than one execution of that
//	                    kernel in the horizon. The samples were attached to
//	                    one of them. This is an inference and it is marked as
//	                    one - spec §10 requires the marking rather than
//	                    guessing it away.
//	kernel-multidevice  the same join, in a process observed running kernels
//	                    on more than one device. gpu_pc_sample_batch_v1
//	                    carries no device id and two devices running one
//	                    binary produce one cubin CRC, so the samples are
//	                    indistinguishable on the wire. PC sampling is
//	                    single-GPU in this phase; this value is the refusal,
//	                    made visible rather than silent.
//
// The empty value is not one of the four and is not a fifth outcome: it is
// what an ExecutionView with no PC samples at all carries, because there is
// nothing to describe. Every ExecutionView holding at least one PC sample
// carries one of the four.
type PCAttrib string

const (
	// PCAttribExact is the correlation-keyed join, unchanged by the module
	// path. It is the only value that is not an inference.
	PCAttribExact PCAttrib = "exact"

	// PCAttribKernel is the module join with one candidate execution.
	PCAttribKernel PCAttrib = "kernel"

	// PCAttribKernelAmbiguous is the module join with several candidate
	// executions of the same kernel in the horizon.
	PCAttribKernelAmbiguous PCAttrib = "kernel-ambiguous"

	// PCAttribKernelMultiDevice is the module join in a multi-device process,
	// where cubin_crc cannot distinguish the devices.
	PCAttribKernelMultiDevice PCAttrib = "kernel-multidevice"
)

// pcAttribs lists the four in stable order, worst-caveat last. The order is
// also the precedence order used by worsePCAttrib.
var pcAttribs = []PCAttrib{
	PCAttribExact,
	PCAttribKernel,
	PCAttribKernelAmbiguous,
	PCAttribKernelMultiDevice,
}

// PCAttribs returns the four gpu_pc_attrib values in stable order.
//
// It exists for the same reason SrcStatuses does: a consumer switching on the
// value can be tested for exhaustiveness against the enum itself rather than
// against a hand-copied list that a fifth value would escape.
func PCAttribs() []PCAttrib {
	out := make([]PCAttrib, len(pcAttribs))
	copy(out, pcAttribs)
	return out
}

// pcAttribRank orders the four by how much doubt they carry, so that an
// execution served by two pending groups of differing quality reports the
// worse of the two rather than whichever happened to be processed last. A
// value not in the table ranks below everything, which is what makes the empty
// value lose to any real one.
func pcAttribRank(a PCAttrib) int {
	for i, v := range pcAttribs {
		if v == a {
			return i + 1
		}
	}
	return 0
}

// worsePCAttrib returns whichever of the two carries more doubt. Attribution
// quality can only ever be revised downward as more evidence arrives, never
// up: an execution that took ambiguous samples does not become unambiguous
// because a second, unambiguous group also landed on it.
func worsePCAttrib(a, b PCAttrib) PCAttrib {
	if pcAttribRank(b) > pcAttribRank(a) {
		return b
	}
	return a
}

// MarshalJSON refuses any value that is not one of the four, matching
// SrcStatus, ClockDomain and GPUCapability in this package. The empty value is
// refused too: an ExecutionView that holds PC samples and no attribution is a
// bug in the join, and it must fail at the serialization boundary rather than
// ship as a string a consumer would read as meaningful. ExecutionView tags the
// field omitempty, so a view with no PC samples never reaches here.
func (a PCAttrib) MarshalJSON() ([]byte, error) {
	if pcAttribRank(a) == 0 {
		return nil, fmt.Errorf("invalid gpu_pc_attrib %q", string(a))
	}
	return []byte(`"` + string(a) + `"`), nil
}

// PCJoinStats accounts for the module-keyed PC join that Snapshot performs on
// correlation-less samples - the continuous-mode path. Every field is
// per-Snapshot except MultiDeviceProcesses, which is a cumulative gauge of the
// Timeline's observations (see Timeline.devicesByPID).
//
// The four Groups* counters partition every pending group the join examined,
// which is the identity that makes this set checkable rather than merely
// reported: a group is joined, or it is left pending for exactly one stated
// reason. A group left pending is not lost - it stays in the store and is
// eligible for a later Snapshot, and if it never becomes joinable it ages out
// into Dropped.EvictedPendingModuleSamples, which is where the loss is
// finally counted.
type PCJoinStats struct {
	// AttributedExact and AttributedKernel split Snapshot.AttributedPCSamples
	// by which index served the sample. They always sum to it.
	AttributedExact  uint64 `json:"attributed_exact,omitempty"`
	AttributedKernel uint64 `json:"attributed_kernel,omitempty"`

	// GroupsJoined counts pending module groups consumed into an execution by
	// this Snapshot.
	GroupsJoined uint64 `json:"groups_joined,omitempty"`

	// GroupsUnresolvedName counts groups whose (cubin CRC, functionIndex)
	// could not be turned into a device function name: no module store is
	// configured, the cubin never reached the agent, it was evicted, its
	// bytes did not parse, or the index is not in its symbol table. Without a
	// name there is nothing to match an execution's KernelName against, and
	// the samples stay pending rather than being attached to a plausible
	// neighbour.
	GroupsUnresolvedName uint64 `json:"groups_unresolved_name,omitempty"`

	// GroupsNoExecution counts groups that DID resolve to a device function
	// but found no execution to attach to: no execution in this snapshot came
	// from the same process carrying that kernel name, or every one that did
	// had already been served by the exact-correlation index. This is the
	// ordinary state at a snapshot boundary - the execution may simply not
	// have arrived yet - and is not on its own an anomaly.
	GroupsNoExecution uint64 `json:"groups_no_execution,omitempty"`

	// GroupsNoProcess counts groups whose PID is zero: the producer named no
	// process. Such a group cannot be joined at all, because every process
	// that names no process shares the key, so attaching it to an execution
	// would be attributing one process's GPU samples to another's call stack
	// on nothing but a kernel name. Issue #52's refusal, applied to PC
	// samples.
	GroupsNoProcess uint64 `json:"groups_no_process,omitempty"`

	// AmbiguousAttributions and MultiDeviceAttributions count EXECUTIONS in
	// this snapshot whose final gpu_pc_attrib is kernel-ambiguous and
	// kernel-multidevice respectively. They are counts of executions, not of
	// groups, because that is the unit the label is carried on.
	//
	// AmbiguousAttributions is emphatically NOT
	// JoinStats.AmbiguousHeuristicMatchCount and must never be merged with
	// it. That one counts heuristic LAUNCH joins; this one counts PC
	// attributions. See PCAttrib.
	AmbiguousAttributions   uint64 `json:"ambiguous_attributions,omitempty"`
	MultiDeviceAttributions uint64 `json:"multi_device_attributions,omitempty"`

	// MultiDeviceProcesses counts distinct processes the Timeline has ever
	// seen execute kernels on more than one device. Cumulative, not
	// per-snapshot: the condition is a property of the process, and a process
	// does not stop having used two devices because this snapshot only saw
	// one.
	MultiDeviceProcesses uint64 `json:"multi_device_processes,omitempty"`

	// DeviceTrackingCapped counts processes the multi-device tracker refused
	// to admit because it was already at maxTrackedDeviceProcesses. It exists
	// so that "no multi-device process was found" and "we stopped looking"
	// are distinguishable: past the cap a new process is treated as
	// single-device, which is the right guess and still a guess. Zero on
	// anything short of thousands of concurrently profiled GPU processes.
	DeviceTrackingCapped uint64 `json:"device_tracking_capped,omitempty"`
}

// GroupsExamined returns the number of pending module groups the four Groups*
// counters account for, which must equal the number of groups the join
// actually walked.
func (s PCJoinStats) GroupsExamined() uint64 {
	return s.GroupsJoined + s.GroupsUnresolvedName + s.GroupsNoExecution + s.GroupsNoProcess
}

// AttributedTotal returns the number of PC samples the two Attributed*
// counters account for, which must equal Snapshot.AttributedPCSamples.
func (s PCJoinStats) AttributedTotal() uint64 {
	return s.AttributedExact + s.AttributedKernel
}
