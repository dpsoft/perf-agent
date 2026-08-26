package symbolize

import "github.com/dpsoft/perf-agent/pprof"

// ToProfFrames flattens a []Frame (each with optional Inlined chain) into
// a leaf-first []pprof.Frame for direct insertion into pprof builders.
//
// blazesym reports Inlined caller-most-to-callee; pprof wants leaf-first.
// Each frame in a chain shares the outer Frame's Address so pprof's
// Locations stay distinguishable when two PCs symbolize identically.
func ToProfFrames(frames []Frame) []pprof.Frame {
	if len(frames) == 0 {
		return nil
	}
	out := make([]pprof.Frame, 0, len(frames))
	for _, f := range frames {
		for i := len(f.Inlined) - 1; i >= 0; i-- {
			in := f.Inlined[i]
			out = append(out, pprof.Frame{
				Name:    in.Name,
				Module:  f.Module,
				File:    in.File,
				Line:    uint32(in.Line),
				Address: f.Address,
			})
		}
		out = append(out, pprof.Frame{
			Name:    f.Name,
			Module:  f.Module,
			File:    f.File,
			Line:    uint32(f.Line),
			Address: f.Address,
			// Carried, not re-derived. The mapping was read while the
			// target process was alive; the pprof builder runs later - for
			// the GPU tools, after the workload has exited - and its own
			// Resolver would find nothing left in /proc to look at.
			BuildID:  f.BuildID,
			MapStart: f.MapStart,
			MapLimit: f.MapLimit,
			MapOff:   f.MapOff,
			// The one bit pprof.Frame cannot recover for itself. After this
			// conversion an address-shaped Name is indistinguishable from a
			// function genuinely called "0x4017c2", so the failure has to be
			// carried explicitly rather than sniffed out of the string.
			// Inlined frames are never marked: an inline chain only exists
			// where resolution succeeded.
			Unresolved: f.Reason != FailureNone,
		})
	}
	return out
}
