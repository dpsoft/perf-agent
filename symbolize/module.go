package symbolize

import "github.com/dpsoft/perf-agent/unwind/procmap"

// ModuleIndex answers "which file is this address in, in this process".
// *procmap.Resolver satisfies it; the interface exists so symbolize does not
// have to own a maps cache of its own, and so tests can supply a fixture.
//
// A ModuleIndex must return ok=false rather than a nearby or stale mapping:
// everything downstream treats a hit as a fact about the profile.
type ModuleIndex interface {
	Lookup(pid uint32, addr uint64) (procmap.Mapping, bool)
}

// attachModules fills Module/BuildID/MapStart/MapLimit/MapOff on the frames a
// symbolizer could not name, so that an unresolved frame can at least say
// which library it is in.
//
// It runs only over frames with Reason != FailureNone and Module == "", so a
// resolved frame is never touched and a symbolizer that already knew the
// module keeps its own answer. That also keeps the cost proportional to the
// failures, not to the stack: a fully symbolized stack does no lookups.
//
// The lookup must happen while the target process is alive - /proc/<pid>/maps
// vanishes the moment it exits - which is why this lives in the symbolizer
// and not in the pprof builder. The GPU tools build their profile after the
// workload has exited; by then there is nothing left to ask.
//
// Returns how many frames gained a module and how many were left bare. Both
// numbers matter: "unresolved" and "unresolved, and we cannot even say where"
// are different failures and must not be reported as one.
func attachModules(idx ModuleIndex, pid uint32, frames []Frame) (attached, bare int) {
	if idx == nil {
		for i := range frames {
			if frames[i].Reason != FailureNone && frames[i].Module == "" {
				bare++
			}
		}
		return 0, bare
	}
	for i := range frames {
		f := &frames[i]
		if f.Reason == FailureNone || f.Module != "" {
			continue
		}
		m, ok := idx.Lookup(pid, f.Address)
		if !ok || m.Path == "" {
			bare++
			continue
		}
		f.Module = m.Path
		f.MapStart = m.Start
		f.MapLimit = m.Limit
		f.MapOff = m.Offset
		if f.BuildID == "" {
			f.BuildID = m.BuildID
		}
		attached++
	}
	return attached, bare
}
