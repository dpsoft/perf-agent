package gpuprobe

import (
	"path/filepath"
	"strings"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// The profiler's own delivery path is not the workload's call path.
//
// # What this removes, and why it is not cosmetic
//
// Every sampled launch's stack passes through the machinery that told us
// about the launch. Measured on a PyTorch capture of 47,204 stacks:
//
//	avg frames per stack                       37.5
//	avg UNNAMED vendor frames per stack        10.1   (28.6% of all frames)
//	longest consecutive unnamed run             7 frames in 40,646 stacks
//	                                           13 frames in  6,378 stacks
//
// and the band looks like this, every time:
//
//	libcublasLt.so.13+0x3279bb4        <- real work
//	cuLaunchKernel                     <- named, and the meaningful boundary
//	  libcuda.so.610.57.04+0x3d8947    ┐
//	  libcupti.so.13+0x11667f          │  CUPTI's callback machinery
//	  libcupti.so.13+0x116485          │  delivering the event to us
//	  ... four more ...                ┘
//	  (anonymous namespace)::on_launch <- the shim
//
// Across a PyTorch capture, libcupti accounts for 5.98 frames per stack and
// the driver's dispatch frame for 1.00 -- 69% of all unnamed vendor frames,
// about 19% of EVERY frame in EVERY stack. The 7 frames are the stable part:
// the depth is CUPTI's. The 19% is not, since it also counts the stacks that
// carry no CPU caller at all -- the same band is 23.5% of the microbenchmark
// under shim/nvidia/testdata. None of it is the application. It
// exists because we subscribed a callback, and it would not be in the profile
// if the profiler were not there.
//
// Leaving it in is not neutral. It pushes the application's own frames a
// quarter of the way down every flame graph, it makes the deepest common
// prefix of every stack a stretch of the profiler, and it does so with
// addresses no local symbol table can name -- so the reader cannot even tell
// what they are looking at without knowing CUPTI's internals.
//
// # Collapsed, not deleted
//
// One marker frame replaces the run. The instrumentation WAS on the stack and
// a profile that silently omitted it would be claiming a call path the process
// never had; `[gpu:instrumentation]` says a band was elided and how many
// frames it held. It is the same shape as [gpu:launch] -- a synthetic frame
// that names something real about how the measurement was made.
//
// # The rule, and why it is anchored rather than pattern-matched
//
// A maximal contiguous run whose every frame is either the SHIM's own module
// or libcupti collapses. The run may then extend across immediately adjacent
// UNNAMED libcuda frames, but only if it already contains a shim or libcupti
// frame.
//
// That last condition is the whole of the care here. libcuda is the CUDA
// driver: most of its frames are the application's work and eliding them would
// be deleting the measurement's subject. The single dispatch frame that hands
// control to a CUPTI callback is different, and the only thing that
// distinguishes it is that it sits directly against the callback machinery. So
// the run is anchored on frames that are provably ours, and libcuda is only
// ever absorbed inward from such an anchor -- never matched on its own.
//
// A NAMED libcuda frame is never absorbed either: cuLaunchKernel is the
// boundary a reader needs, it is the last thing the application actually
// called, and it must survive.
type instrumentationBand struct {
	// shim is the same predicate shimScope polices whole stacks with; the
	// shim's own frames are the innermost end of every band.
	shim shimScope
	// enabled is false when there is no shim to anchor on. Without an anchor
	// the rule would have to match libcupti alone, which is a guess about
	// somebody else's process rather than a fact about ours.
	enabled bool
}

// newInstrumentationBand builds the collapser. keep=true disables it, leaving
// every delivery-path frame in the profile.
//
// The elision happens BEFORE the profile is written, so unlike the flame
// graph's vendor-run collapse there is no way to get these frames back from a
// pb.gz that was captured without them -- the only recourse is another
// capture. That asymmetry is why the switch exists at all: eliding by default
// is right, because the frames are the measurement apparatus rather than the
// workload, but "not the workload's call path" is not the same as "nobody may
// ever want them". Anyone measuring the profiler's own overhead wants exactly
// these frames, and the default made that impossible rather than merely
// inconvenient.
func newInstrumentationBand(s shimScope, keep bool) instrumentationBand {
	return instrumentationBand{shim: s, enabled: s.guarded && !keep}
}

// InstrumentationFrameName is the marker left in place of an elided band.
// Bracketed like [gpu:launch] so it cannot collide with a real symbol, and
// deliberately not shaped like a module name: stackcollapse-perf.pl reads
// bracketed .so names as kernel frames.
const InstrumentationFrameName = "[gpu:instrumentation]"

// isInstrumentation reports whether a frame both belongs to the delivery path
// -- the shim itself, or CUPTI -- and carries no name.
//
// A NAMED frame is never elided, whoever owns it. The shim's own callback
// arrives as "(anonymous namespace)::on_launch(CUpti_CallbackData const*)",
// which tells a reader exactly what it is and where the boundary lies;
// replacing that with a generic marker would trade information for tidiness.
// What is worth removing is the run of addresses no symbol table can name, and
// that is precisely the population this predicate selects.
func (b instrumentationBand) isInstrumentation(f pp.Frame) bool {
	if !f.Unresolved {
		return false
	}
	if b.shim.isShimModule(f.Module) {
		return true
	}
	return isCUPTIModule(f.Module)
}

// absorbable frames join a band that already has an anchor, but can never
// start one.
//
// An UNNAMED driver frame sitting against the callback machinery is the
// driver's dispatch into it -- in the measured band it is the single
// libcuda frame between CUPTI and cuLaunchKernel. The same frame with no
// anchor beside it is the application's own work and is left alone, which is
// what `absorbable` never starting a band guarantees.
//
// Named-ness is what protects the boundary, not position. An earlier version
// tried to keep driver frames at the "outer edge" of a band on the theory that
// those were the application calling in; the measured stacks refute it -- the
// dispatch frame IS at the outer edge, immediately before cuLaunchKernel. What
// must survive is every frame that carries a NAME, and cuLaunchKernel does.
func absorbable(f pp.Frame) bool {
	return f.Unresolved && isDriverModule(f.Module)
}

func isCUPTIModule(module string) bool {
	return strings.HasPrefix(filepath.Base(module), "libcupti.so")
}

func isDriverModule(module string) bool {
	return strings.HasPrefix(filepath.Base(module), "libcuda.so")
}

// collapse rewrites frames in place, replacing each instrumentation band with
// a single marker, and reports how many frames were removed.
//
// Returns the (possibly shorter) slice. A stack that is ENTIRELY
// instrumentation is left untouched: shimScope has already refused those as
// unattributable, and collapsing one to a lone marker would turn a stack the
// consumer withheld into one that looks like an attribution.
func (b instrumentationBand) collapse(frames []pp.Frame) ([]pp.Frame, int) {
	if !b.enabled || len(frames) == 0 {
		return frames, 0
	}
	// A mask first, and the reason is a bug this replaced: a single forward
	// scan that only ever extended a band FORWARD was order-dependent. Frames
	// arrive outermost-first, so the driver's dispatch frame is reached before
	// the CUPTI frames that anchor it -- and since an absorbable frame may
	// never start a band, it was passed over and kept. It stayed in every
	// stack (1.00 frames per stack, measured) while the rule's comment claimed
	// it did not.
	//
	// Marking anchors first and then growing over absorbable frames in BOTH
	// directions makes the result the same whichever end of the stack the
	// slice starts at, which is the property the rule was supposed to have.
	mark := make([]bool, len(frames))
	anchored := false
	for i, f := range frames {
		if b.isInstrumentation(f) {
			mark[i] = true
			anchored = true
		}
	}
	if !anchored {
		return frames, 0
	}
	for i := 0; i < len(frames); i++ {
		if !mark[i] {
			continue
		}
		for j := i - 1; j >= 0 && !mark[j] && absorbable(frames[j]); j-- {
			mark[j] = true
		}
		for j := i + 1; j < len(frames) && !mark[j] && absorbable(frames[j]); j++ {
			mark[j] = true
		}
	}

	out := frames[:0]
	removed := 0
	for i := 0; i < len(frames); {
		if !mark[i] {
			out = append(out, frames[i])
			i++
			continue
		}
		j := i
		for j < len(frames) && mark[j] {
			j++
		}
		out = append(out, pp.Frame{Name: InstrumentationFrameName})
		removed += j - i - 1
		i = j
	}
	if len(out) == 1 && out[0].Name == InstrumentationFrameName {
		// Every frame was ours. Not this function's call to make.
		return frames, 0
	}
	return out, removed
}
