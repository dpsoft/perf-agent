package foldedstacks

import (
	"path/filepath"
	"regexp"
	"strings"
)

// A run of frames that each say nothing is worth less than one frame that says
// where they were.
//
// # What this collapses
//
// After the profiler's own delivery path is elided (gpuprobe's
// instrumentationBand), a measured PyTorch capture still carries fixed runs of
// vendor frames between the application and the kernel:
//
//	libcublas.so.13     run of 3   in 16,658 stacks
//	libcublasLt.so.13   run of 4   in 11,222 stacks
//	libcublasLt.so.13   run of 6   in  5,436 stacks
//
// and only THIRTEEN distinct addresses produce all of them. So this is not
// horizontal fragmentation, it is vertical depth: three to six rows of
// `libcublas.so.13+0x2cc690` in every stack, pushing everything beneath them
// down and telling the reader nothing about any of it.
//
// # Why not in the profile, the way the instrumentation band was
//
// Because these frames are REAL. The instrumentation band existed only because
// something was watching; removing it from the profile removed a call path the
// process would not have had unprofiled. cuBLAS frames are work the
// application actually did, and deleting them would discard something true.
//
// So this happens at the folding step, and the pprof file is untouched. The
// picture becomes readable; anyone who wants the individual addresses still
// has them.
//
// # "Uninformative" is two exact spellings, not a judgement
//
// A frame is collapsible only when its name is one of the two ways a name can
// be present and still say nothing:
//
//	module+0xHEX               pprof's own rendering of an address that no
//	                           symbol table could name (pprof.Frame.Unresolved,
//	                           applied by addLocationByAddr)
//	libfoo_<40 hex>            NVIDIA's CUDA Toolkit Symbol Server serves
//	                           OBFUSCATED names for internal functions. They
//	                           are stable and distinguish one function from
//	                           another, which is why they are worth fetching --
//	                           but `libcublasLt_c44dd15e5b30f7ac…` is no more
//	                           legible than the offset it replaced.
//
// The second is the reason this cannot key on "unsymbolized", as issue #122
// originally proposed. With the symbol server attached -- the configuration we
// recommend -- these frames ARE symbolized, and a rule testing for a missing
// name would never fire in the setup it was written for.
//
// Everything else survives. A run of five named frames from libtorch_cuda.so is
// five things the reader can look up, and merging them would destroy exactly
// the information a flame graph exists to show.
//
// # The frame count is deliberately NOT in the result
//
// A collapsed frame is the bare module name. Writing `libcublasLt.so.13 (4
// frames)` would put the run length into the merge key, so the run-of-4 and
// run-of-6 paths above would become two nodes instead of one -- which is the
// whole thing this is trying to undo. The count is a property of one stack;
// the node has to be a property of the module.
//
// Square brackets are avoided for the same class of reason: stackcollapse-perf
// reads a bracketed name as a kernel frame, and these are anything but.

// obfuscatedVendorName matches the symbol server's internal names:
// a library-ish prefix, an underscore, and a 40-character hex digest.
var obfuscatedVendorName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*_[0-9a-f]{40}$`)

// uninformative reports whether a frame name carries nothing a reader can act
// on, given the module it came from.
func uninformative(frame, module string) bool {
	if frame == "" || module == "" {
		return false
	}
	base := filepath.Base(module)
	// pprof's module+0xHEX spelling. Anchored on the module's own basename so
	// a genuine symbol that merely contains "+0x" cannot match.
	if rest, ok := strings.CutPrefix(frame, base+"+0x"); ok && rest != "" {
		return isHex(rest)
	}
	return obfuscatedVendorName.MatchString(frame)
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// collapseVendorRuns rewrites one stack, replacing each maximal run of
// consecutive uninformative frames from the SAME module with a single frame
// named after that module. Returns the stack and how many frames were removed.
//
// Same module, because two adjacent vendor libraries are two different places
// and merging them would invent a call path through neither.
func collapseVendorRuns(frames, modules []string) ([]string, []string, int) {
	if len(frames) == 0 {
		return frames, modules, 0
	}
	outF := make([]string, 0, len(frames))
	outM := make([]string, 0, len(frames))
	removed := 0
	for i := 0; i < len(frames); {
		m := ""
		if i < len(modules) {
			m = modules[i]
		}
		if !uninformative(frames[i], m) {
			outF = append(outF, frames[i])
			outM = append(outM, m)
			i++
			continue
		}
		j := i + 1
		for j < len(frames) && j < len(modules) && modules[j] == m && uninformative(frames[j], m) {
			j++
		}
		// A run of one is left exactly as it is. Renaming a lone
		// `libcublas.so.13+0x2cc690` to `libcublas.so.13` would discard the
		// address without collapsing anything -- strictly less information for
		// no gain in readability.
		if j-i == 1 {
			outF = append(outF, frames[i])
			outM = append(outM, m)
			i++
			continue
		}
		outF = append(outF, filepath.Base(m))
		outM = append(outM, m)
		removed += j - i - 1
		i = j
	}
	return outF, outM, removed
}
