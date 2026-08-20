package gpuprobe

import (
	"debug/elf"
	"path/filepath"

	pp "github.com/dpsoft/perf-agent/pprof"
)

// A stack that never left the profiler is not an attribution.
//
// The launch sampler captures the launching thread's CPU stack with
// bpf_get_stackid, which is a frame-pointer walk: it stops at the first
// frame it cannot follow. When the shim is an adapter injected into someone
// else's process, the call chain from the probe outwards is
//
//	probe -> our callback -> libcupti -> libcudart -> the application
//
// and the walk dies in the vendor libraries, which are built without frame
// pointers. What comes back is one frame - the adapter's own callback - and
// projecting it hands every GPU kernel's measured time to a call path inside
// the profiler itself. That is not a weak attribution, it is a false one:
// the observed CUPTI run on an RTX 3090 reported 100% of the attributed GPU
// time under "(anonymous namespace)::on_callback", a function that has never
// launched a kernel in its life. A DWARF unwinder that can walk NVIDIA's
// libraries is the real fix (Phase 4b); until then the honest answer is to
// say nothing, and to say how often we had to.
//
// A stack rejected here is withheld from the launch, so the launch reaches
// the sink stackless and projects as [gpu:launch unsampled] - the same
// unattributed population as a launch the sampler never picked. No duration
// is scaled, no execution borrows another's call path, and the attributed
// and unattributed populations still sum to the measured total.
//
// # Why "every frame is inside the shim" is not the test
//
// There are two deployment shapes, and the naive test would be wrong for one
// of them:
//
//   - Injected adapter (the CUDA case). The shim is a shared object loaded
//     into a process it did not create. Application code lives in other
//     modules by construction, so a stack confined to the shim provably
//     never reached the application.
//   - Self-contained producer (shim/stub, which the phase gate runs). The
//     shim IS the program. Its whole legitimate stack - main ->
//     perfagent_stub_run -> the probe - is "inside the shim", and it is a
//     perfectly good attribution, because here the profiler and the
//     application are the same binary. There is no boundary to cross.
//
// shimScope tells the two apart from the shim's ELF alone, once, when the
// consumer is built: a file that carries a PT_INTERP segment (or is ET_EXEC)
// is a program the kernel can exec, i.e. shape two; anything else is a
// shared object, i.e. shape one. That is a static, deterministic property of
// the file the consumer attached to. The obvious alternative - readlink or
// stat /proc/<pid>/exe per launching pid and compare it to the shim - was
// rejected: it answers the same question later, per pid, on the hot path,
// needs PTRACE_MODE_READ on a process that may already have exited, and
// needs bounded per-pid memoization to stay cheap. Its one advantage is
// covering a static-PIE producer (ET_DYN with no PT_INTERP), which this
// classifier calls a library - see the failure modes below.
//
// # Failure modes, and which way they fail
//
// The guard is built to fail towards silence rather than towards invention.
// Rejecting a genuine stack loses information; accepting a profiler-only one
// is the bug this exists to prevent.
//
//   - A static-PIE self-contained producer (ET_DYN, no PT_INTERP) is
//     classified as an injected library, so its legitimate inside-only
//     stacks are rejected. Information loss, counted, honest.
//   - A frame whose module the symbolizer could not name proves nothing, so
//     it is never read as "outside". A stack of nothing but unnamed modules
//     is therefore rejected too - and counted separately, in
//     Stats.StacksProfilerOnlyUncertain, because that is a rejection without
//     proof.
//   - Module paths are compared by spelling (as configured, absolute, and
//     symlink-resolved) plus basename. Basename matching is the deliberate
//     loose end: a target in another mount namespace reports its own
//     rootfs's path for the shim, which matches none of the three spellings,
//     and without the basename fallback the shim's own frames would read as
//     "outside" - a false accept, the direction that must not happen. The
//     cost is that an application module that happens to share the shim's
//     basename reads as "inside", which can only cause a rejection.
//   - An empty ShimPath disables the guard entirely: with no idea which
//     module is the profiler's, there is no evidence either way, and
//     rejecting everything on no evidence is destruction, not honesty.
//     Attach always sets it (it parses that file's USDT notes and opens it
//     as an executable), so this is a unit-test shape, not a live one.
//   - A shim whose ELF cannot be read leaves the shape unknown; the guard
//     stays ON and treats it as injected, which is the lossy direction.
//
// BuildID would be a stronger module identity than a path, but pprof.Frame's
// BuildID is filled in by the pprof builder from /proc/<pid>/maps long after
// this check runs - the frames the consumer holds carry only what the
// symbolizer set, which is Name/Module/File/Line/Address. When Phase 4b's
// map-snapshot work puts a build ID on the frame, this comparison should
// move to it.
type shimScope struct {
	// guarded is false when there is nothing to police: no shim path
	// configured, or the shim is the program itself.
	guarded bool
	// paths holds the accepted spellings of the shim's own module.
	paths map[string]struct{}
	// base is the shim's file name, the cross-mount-namespace fallback.
	base string
}

// stackVerdict is what shimScope makes of one resolved capture.
type stackVerdict int

const (
	// stackAttributable: a frame provably outside the shim, or no boundary
	// to police in the first place.
	stackAttributable stackVerdict = iota
	// stackProfilerOnly: every frame's module is known, and every one of
	// them is the shim. The walk never reached the application.
	stackProfilerOnly
	// stackProfilerOnlyUncertain: no frame is provably outside the shim, but
	// at least one frame's module is unknown, so "never reached the
	// application" is the honest assumption rather than a proven fact.
	stackProfilerOnlyUncertain
)

// newShimScope classifies the shim the consumer attached to. It reads the
// file once, at construction; nothing on the capture path touches the
// filesystem.
func newShimScope(shimPath string) shimScope {
	if shimPath == "" {
		return shimScope{}
	}
	if isELFProgram(shimPath) {
		// The shim is the program. Profiler and application are the same
		// binary, so "inside the shim" says nothing about attribution.
		return shimScope{}
	}
	s := shimScope{
		guarded: true,
		paths:   map[string]struct{}{shimPath: {}},
		base:    filepath.Base(shimPath),
	}
	if abs, err := filepath.Abs(shimPath); err == nil {
		s.paths[abs] = struct{}{}
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			s.paths[real] = struct{}{}
		}
	}
	return s
}

// isELFProgram reports whether path is a file the kernel can exec directly:
// ET_EXEC, or an ET_DYN carrying PT_INTERP (a position-independent
// executable). A shared object is neither. An unreadable or unparseable file
// reports false, which leaves the guard on - the lossy direction.
func isELFProgram(path string) bool {
	f, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if f.Type == elf.ET_EXEC {
		return true
	}
	if f.Type != elf.ET_DYN {
		return false
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return true
		}
	}
	return false
}

// verdict judges one resolved capture. See stackVerdict.
//
// It requires POSITIVE evidence that the walk left the shim: one frame in a
// module that is known and is not the shim's. A frame with no module is
// evidence of nothing and never counts as an escape, because reading
// "unknown" as "outside" is exactly the false accept this guard exists to
// prevent.
func (s shimScope) verdict(frames []pp.Frame) stackVerdict {
	if !s.guarded {
		return stackAttributable
	}
	unknown := false
	for _, f := range frames {
		if f.Module == "" {
			unknown = true
			continue
		}
		if !s.isShimModule(f.Module) {
			return stackAttributable
		}
	}
	if unknown {
		return stackProfilerOnlyUncertain
	}
	return stackProfilerOnly
}

// isShimModule reports whether a frame's module is the shim's own. Spelling
// first, then file name - see the failure modes on shimScope for why the
// basename fallback is there and which way it errs.
func (s shimScope) isShimModule(module string) bool {
	if _, ok := s.paths[module]; ok {
		return true
	}
	return filepath.Base(module) == s.base
}
