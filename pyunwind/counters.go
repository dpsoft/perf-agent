package pyunwind

import (
	"fmt"
)

// The py_walk_counters slots, mirroring PY_CNT_* in bpf/python_walk.h.
//
// UNITS. Every slot below names its own unit, and they are not all the same.
// PyCntFramesPushed counts FRAMES; every other slot counts SAMPLES. That is
// structural rather than conventional for slots 1..8: each of those paths
// leaves walk_ctx.py_state == PY_CHAIN_DONE, and both py_push_frames and
// walk_step's interpreter arm are no-ops once it is, so a sample whose native
// stack crosses the eval loop twenty times still contributes at most one to
// any of them. PyCntChainAbandoned reaches the same unit differently -- the
// drivers ask once, after bpf_loop returns.
//
// Comparing slot 0 against the rest as though they shared a unit makes a deep
// Python stack look like a failure storm. The unit is restated on each
// constant deliberately: a set where only the surprising ones are labelled
// reads as if the unlabelled ones are all alike.
//
// A dedicated per-CPU counter array rather than more sample_header.walker_flags
// bits: all eight of those are allocated (see the flag block in
// bpf/unwind_common.h), and a bit could only say "something went wrong" once
// per sample where these count every occurrence and name it. The pair is not
// free to drift -- a Go index larger than the C array reads a slot the map
// does not have and every lookup fails, reporting zero forever, which is the
// exact silence the counters exist to break. TestWalkCounterSlotsMirrorTheBPFHeader
// pins them against the header text.
const (
	// PyCntFramesPushed is the SUCCESS count: one Python frame pair reached
	// the sample record. Slot 0 deliberately, because a counter set that can
	// only move when something breaks cannot tell "the interpreter arm never
	// fired" from "it fired and worked", and that is the first thing an
	// operator with no Python frames needs to know.
	//
	// Unit: FRAMES. The only slot in this set that is not per sample.
	PyCntFramesPushed = 0
	// PyCntTSSMiss: the thread's TSD slot held no PyThreadState. Normal for a
	// thread that has never held the GIL; a storm of it against zero
	// PyCntFramesPushed means the TSS key or the glibc offsets are wrong.
	//
	// Unit: SAMPLES.
	PyCntTSSMiss = 1
	// PyCntNoProcInfo: an eval-loop PC in a process with no validated,
	// enabled py_procs entry. This is what separates "no Python frames
	// because attach refused this interpreter" (see Result.Reason) from "no
	// Python frames because the walk failed".
	//
	// Unit: SAMPLES -- not one per eval-loop frame; walk_step marks the
	// sample done after the first.
	PyCntNoProcInfo = 2
	// PyCntTStateReadFail: current_frame -- or, on 3.12, the extra cframe
	// hop -- could not be read out of the PyThreadState.
	//
	// Unit: SAMPLES.
	PyCntTStateReadFail = 3
	// PyCntFrameReadFail: an _PyInterpreterFrame field faulted mid-chain.
	//
	// Unit: SAMPLES.
	PyCntFrameReadFail = 4
	// PyCntOwnerImplausible: an owner byte above this version's enum ceiling,
	// the same plausibility screen Offsets.Validate applies at attach,
	// re-applied per frame. Means the offsets are being read against a layout
	// they do not describe; every frame after it would have been invented.
	//
	// Unit: SAMPLES.
	PyCntOwnerImplausible = 5
	// PyCntChainTruncated: PY_MAX_FRAMES_PER_ENTRY frames pushed and the
	// chain's C boundary still not reached, so the rest of that Python
	// segment is missing from the sample.
	//
	// Unit: SAMPLES.
	PyCntChainTruncated = 6
	// PyCntPushRefused: the sample record had no room for another two-slot
	// Python frame. Accompanied by WALKER_FLAG_FRAME_PUSH_REFUSED on the
	// sample itself; this says which of the two pushers hit it.
	//
	// Unit: SAMPLES.
	PyCntPushRefused = 7
	// PyCntNoneExecutable: f_executable was NULL or Py_None on a frame the
	// owner test did NOT already stop at. Expect zero.
	//
	// CPython >= 3.13 puts Py_None in the entry frame's f_executable, but
	// that is the same frame whose owner marks the C boundary (3.13.15
	// ceval.c:716, 3.14.3 ceval.c:1159), and the walk returns on the owner
	// before it ever reads the executable. So this is a torn-read /
	// wrong-offset backstop -- the screen is CPython's own
	// (_remote_debugging_module.c:2142-2144) -- and a zero here means it
	// never had to fire, not that it is absent.
	//
	// Unit: SAMPLES.
	PyCntNoneExecutable = 8

	// PyCntChainAbandoned: the NATIVE walk stopped while a Python cursor was
	// still live, so an outer Python segment is absent from the sample.
	//
	// THIS IS A NATIVE-WALK OUTCOME WITH A PYTHON COST, not a Python-side
	// failure. Nothing in the Python walk went wrong; it never got another
	// turn. The cursor goes live when the walk reaches an interpreter entry
	// frame with an outer segment behind it, and is consumed the next time
	// the native walk lands on an eval-loop PC -- so this counts the times
	// the native walk ended first. Expect it to be non-zero on any real
	// workload with deep stacks: read it against PyCntFramesPushed as a
	// fraction of the Python stack lost, not as an error rate.
	//
	// It never arrives alone, and the record's WALKER_FLAG_* bits separate
	// two different causes (see the same constant in bpf/python_walk.h):
	// with an early-stop bit (FP_EXHAUSTED 0x10, FP_NONMONOTONIC 0x20,
	// CFI_MISS 0x04, FRAME_PUSH_REFUSED 0x80) the native unwinder gave out
	// and the fix is on the native side; with a clean ending instead
	// (FP_TERMINATED 0x01 and/or RA_UNDEFINED 0x08 and no early-stop bit)
	// the native walk reached the root and never recognised the outer
	// segment's eval-loop PC, which points at a missing or wrong
	// py_eval_ranges entry. That second reading is the one that matters,
	// because everything else about such a sample looks perfect.
	//
	// Unit: SAMPLES -- the drivers ask once, after bpf_loop returns.
	PyCntChainAbandoned = 9

	// PyCntMax mirrors PY_CNT_MAX: the number of slots in py_walk_counters.
	PyCntMax = 10
)

// WalkCounters is a snapshot of py_walk_counters, summed across CPUs.
//
// Field order matches the slot order above; ReadWalkCounters is the only
// thing that maps between them.
// See the UNITS note above the slot constants: FramesPushed is a frame count,
// every other field is a sample count.
type WalkCounters struct {
	FramesPushed     uint64
	TSSMiss          uint64
	NoProcInfo       uint64
	TStateReadFail   uint64
	FrameReadFail    uint64
	OwnerImplausible uint64
	ChainTruncated   uint64
	PushRefused      uint64
	NoneExecutable   uint64
	ChainAbandoned   uint64
}

// perCPUArray is the slice of *ebpf.Map ReadWalkCounters needs. Narrowed to
// an interface so the summing and slot mapping can be tested without a
// kernel: this package's tests run without capabilities, and a counter
// decoder that is only exercised on a privileged machine is a decoder whose
// slot mapping is checked nowhere.
type perCPUArray interface {
	Lookup(key, valueOut any) error
}

// ReadWalkCounters sums each py_walk_counters slot across CPUs and names the
// results. m must be the BPF_MAP_TYPE_PERCPU_ARRAY declared in
// bpf/python_walk.h.
func ReadWalkCounters(m perCPUArray) (WalkCounters, error) {
	var out WalkCounters
	dst := [PyCntMax]*uint64{
		PyCntFramesPushed:     &out.FramesPushed,
		PyCntTSSMiss:          &out.TSSMiss,
		PyCntNoProcInfo:       &out.NoProcInfo,
		PyCntTStateReadFail:   &out.TStateReadFail,
		PyCntFrameReadFail:    &out.FrameReadFail,
		PyCntOwnerImplausible: &out.OwnerImplausible,
		PyCntChainTruncated:   &out.ChainTruncated,
		PyCntPushRefused:      &out.PushRefused,
		PyCntNoneExecutable:   &out.NoneExecutable,
		PyCntChainAbandoned:   &out.ChainAbandoned,
	}
	for slot := range uint32(PyCntMax) {
		var perCPU []uint64
		if err := m.Lookup(&slot, &perCPU); err != nil {
			return WalkCounters{}, fmt.Errorf("pyunwind: read py_walk_counters slot %d: %w", slot, err)
		}
		var sum uint64
		for _, v := range perCPU {
			sum += v
		}
		*dst[slot] = sum
	}
	return out, nil
}
