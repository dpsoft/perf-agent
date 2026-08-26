package flamegraph

import (
	"path"
	"strings"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
	"github.com/dpsoft/perf-agent/internal/framename"
)

// Domain is what a frame *is*, not how deep it sits or what its name hashes
// to. A hashed palette carries no information; these profiles have real
// structure worth showing — application code, the CPU path that initiates
// GPU work, an explicit boundary marker, and the accelerator side — so the
// palette carries that instead.
//
// The colours are Brendan Gregg's AI Flame Graph palette
// (https://www.brendangregg.com/blog/2024-10-29/ai-flame-graphs.html), whose
// layers map onto our domains like this:
//
//	pink    application code                DomainApplication
//	red     C: process startup and libc     DomainSystem
//	yellow  C++: the GPU runtime and driver DomainVendorRuntime
//	orange  kernel                          DomainKernel
//	grey    the CPU/accelerator boundary    DomainBoundary, …Unattributed
//	aqua    accelerator source lines        (reserved — see below)
//	green   accelerator execution           DomainGPUKernel
//
// Aqua is deliberately unused. It is Gregg's colour for the *source* of
// functions running on the accelerator, which needs GPU PC sampling with a
// SASS→source mapping; perf-agent does not emit those frames yet. Reserving
// the hue now means the layer has a home when it arrives, and means nothing
// on the page can be mistaken for it in the meantime. The token exists in
// the stylesheet (--fill-accel-source) with no domain pointing at it, and
// the legend says so.
//
// Two domains have no counterpart in Gregg's scheme and exist here because
// they are honesty features rather than decoration. Neither gets a sixth
// hue; both are drained almost to grey, which is the palette's way of
// saying "this frame is not a layer of your computation":
//
//   - DomainUnsymbolized. These frames were unwound correctly; no symbol
//     table could name them. That is a symbolization gap, not a hole in the
//     stack, and the reader must be able to tell the difference at a glance.
//     Two grades of it share the domain and are told apart by the label:
//     "libcuda.so.1+0x1b71c6" means the module is known and only the symbol
//     is missing, "0x7f2c945b2c2b" means not even the module is.
//     They keep the CPU band's warm hue drained to a pale sand, and keep the
//     hatch: the layer is known, the name is not. Warm-but-colourless, so it
//     never reads as one of the named CPU layers and never steals the pure
//     grey the boundary markers own.
//   - DomainProfilerShim. perf-agent's own injected callback appears in
//     perf-agent's own profile. Colouring it as ordinary CPU work would bill
//     the profiler's overhead to the program being profiled. It gets the
//     opposite drain — a cool slate with a cool outline — so the instrument
//     sits visibly outside the warm CPU band without claiming a hue of its
//     own.
//
// Classification is INFERRED — from the mapping file when the profile
// supplies one, otherwise from the symbol name. The legend says so. It is a
// reading aid layered on the graph, never a claim the profile itself makes;
// nothing about a frame's value, width or position depends on it.
type Domain int

// Domains, in CPU→accelerator order. The order is also the legend order.
const (
	DomainRoot Domain = iota
	DomainApplication
	DomainSystem
	DomainKernel
	DomainVendorRuntime
	DomainUnsymbolized
	DomainProfilerShim
	DomainBoundary
	DomainBoundaryUnattributed
	DomainGPUKernel
	numDomains
)

// DomainInfo is everything the page needs to draw and explain a domain.
type DomainInfo struct {
	// Key is a stable machine name, emitted as data-domain.
	Key string
	// Label is the legend heading.
	Label string
	// Desc is the legend's one-line explanation.
	Desc string
	// Fill is a CSS colour.
	Fill string
	// Overlay is a CSS background-image layered over the fill, or "" for
	// none. It is always a hatch, and a hatch always encodes uncertainty: a
	// hatched frame is one the profile could not fully account for.
	Overlay string
	// Stroke is an explicit outline colour, or "" for the default.
	Stroke string
}

var domainInfo = [numDomains]DomainInfo{
	DomainRoot: {
		Key: "root", Label: "all", Fill: "var(--fill-root)",
		Desc: "Synthetic root. Its width is the whole profile.",
	},
	DomainApplication: {
		Key: "app", Label: "application", Fill: "var(--fill-app)",
		Desc: "Your program's own code, and compiler-generated glue in its binary. Pink, Gregg's application layer.",
	},
	DomainSystem: {
		Key: "system", Label: "CPU: process startup and libc", Fill: "var(--fill-system)",
		Desc: "The C runtime, dynamic loader and thread entry that get to main. Red, Gregg's C layer.",
	},
	DomainKernel: {
		Key: "kernel", Label: "CPU: kernel", Fill: "var(--fill-kernel)",
		Desc: "Kernel-mode frames, identified by the profile's [kernel] mapping. Orange, Gregg's kernel layer.",
	},
	DomainVendorRuntime: {
		Key: "vendor", Label: "CPU: GPU runtime and driver", Fill: "var(--fill-vendor)",
		Desc: "CUDA/HIP/HSA runtime and driver frames: the CPU path that initiates GPU work. Yellow, Gregg's C++ layer.",
	},
	DomainUnsymbolized: {
		Key: "unsym", Label: "vendor, no symbols", Fill: "var(--fill-unsym)", Overlay: "var(--hatch-gap)",
		Desc: "Unwound correctly; no symbol table could name it. The depth is real, the names are missing — usually a stripped vendor library with no exported symbols. Labelled module+offset (libcuda.so.1+0x1b71c6) where the profile knows which file the address fell in, and as a bare address where it does not. The CPU band's hue, drained: right layer, no name.",
	},
	DomainProfilerShim: {
		Key: "shim", Label: "perf-agent", Fill: "var(--fill-shim)", Stroke: "var(--edge-shim)",
		Desc: "The profiler observing itself: perf-agent's injected callback, not work your program asked for. Cool slate, outside the warm CPU band, because it is not your computation.",
	},
	DomainBoundary: {
		Key: "boundary", Label: "CPU→GPU boundary", Fill: "var(--fill-boundary)", Stroke: "var(--edge-boundary)",
		Desc: "[gpu:launch] — the launch this execution was joined to. Everything below it ran on the CPU; everything above it ran on the accelerator. Grey, Gregg's boundary marker.",
	},
	DomainBoundaryUnattributed: {
		Key: "boundary-unattributed", Label: "GPU work with no CPU stack", Fill: "var(--fill-boundary-unattributed)", Overlay: "var(--hatch-gap)", Stroke: "var(--edge-unattributed)",
		Desc: "[gpu:launch unsampled] — measured GPU time whose launch was not one of the sampled ones: its duration is measured, its caller unknown and not borrowed from a sampled sibling. The same boundary grey, but paler, hatched and dashed: a boundary with nothing behind it.",
	},
	DomainGPUKernel: {
		Key: "gpu-kernel", Label: "GPU kernel execution", Fill: "var(--fill-gpu-kernel)",
		Desc: "The kernel that ran on the accelerator, and how long it ran for. Green, Gregg's accelerator-execution layer.",
	},
}

// Info returns the domain's presentation.
func (d Domain) Info() DomainInfo {
	if d < 0 || d >= numDomains {
		return domainInfo[DomainApplication]
	}
	return domainInfo[d]
}

// Classify assigns a frame to a domain. name is the symbol; module is the
// mapping file, or "" when the profile does not say.
//
// Rule order matters: the accelerator-side names gpu/projection.go emits are
// matched before anything name-shaped, then whatever the profile's mapping
// tells us, then symbol-name rules, and application is the fallback.
func Classify(name, module string) Domain {
	switch {
	// --- accelerator side ---
	case name == "[gpu:launch]":
		return DomainBoundary
	case name == "[gpu:launch unsampled]":
		return DomainBoundaryUnattributed
	case strings.HasPrefix(name, "[gpu:"):
		// [gpu:kernel:<name>], plus any other accelerator-side frame:
		// green is the claim being made, and it is the right one.
		return DomainGPUKernel

	// --- module-derived, i.e. taken from the profile rather than guessed ---
	case module == "[kernel]":
		return DomainKernel

	// --- no symbol, whether or not the module is known ---
	//
	// This sits ABOVE the remaining module rules on purpose. Once frames
	// carry their mapping, an unnamed frame inside libcuda.so.1 matches
	// isVendorModule, and colouring it as ordinary vendor code would erase
	// the only signal saying no symbol table could name it - which is what
	// DomainUnsymbolized exists to show ("vendor, no symbols"). Its label
	// now carries the module too ("libcuda.so.1+0x1b71c6"), so the layer is
	// still legible; the hatch is what says the name is not.
	//
	// Kernel stays above this, so a hex-named kernel frame is still orange.
	case isUnsymbolized(name):
		return DomainUnsymbolized

	case isShimModule(module):
		return DomainProfilerShim
	case isVendorModule(module):
		return DomainVendorRuntime
	case isSystemModule(module):
		return DomainSystem

	// --- name-derived ---
	case isShimSymbol(name):
		return DomainProfilerShim
	case isVendorSymbol(name):
		return DomainVendorRuntime
	case isSystemSymbol(name):
		return DomainSystem
	default:
		return DomainApplication
	}
}

func base(module string) string {
	if module == "" {
		return ""
	}
	return path.Base(module)
}

// isShimModule matches the adapter perf-agent injects into the target
// (shim/Makefile builds libperfagent-gpu-nvidia.so and -amd.so).
func isShimModule(module string) bool {
	return strings.HasPrefix(base(module), "libperfagent")
}

// isShimSymbol is the fallback for a shim frame in a profile whose mappings
// are empty, which is every profile the GPU builder writes today. The CUPTI
// and rocprofiler callback entry points are the only shim symbols that can
// appear on a target's stack; requiring both halves keeps an application
// function innocently named on_callback from being billed to the profiler.
func isShimSymbol(name string) bool {
	return strings.Contains(name, "on_callback") &&
		(strings.Contains(name, "CallbackDomain") || strings.Contains(name, "rocprofiler"))
}

var vendorModulePrefixes = []string{
	"libcuda", "libcudart", "libcupti", "libnvidia", "libnvperf",
	"libamdhip", "libhsa-runtime", "librocprofiler", "libroctracer", "libhsakmt",
}

func isVendorModule(module string) bool {
	b := base(module)
	if b == "" {
		return false
	}
	for _, p := range vendorModulePrefixes {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	return false
}

var vendorSymbolPrefixes = []string{
	"cuda", "cuda_", "cuLaunch", "cuMem", "cuStream", "cuModule", "cuCtx", "cuDevice",
	"cupti", "cuptiActivity", "nvidia", "nvrtc",
	"hip", "hsa_", "roctracer", "rocprofiler", "amd_",
}

func isVendorSymbol(name string) bool {
	for _, p := range vendorSymbolPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	// cuFoo / cuBar: the CUDA driver API is "cu" + an uppercase letter.
	if len(name) > 2 && strings.HasPrefix(name, "cu") && name[2] >= 'A' && name[2] <= 'Z' {
		return true
	}
	return false
}

var systemModulePrefixes = []string{"ld-linux", "ld-musl", "libc.so", "libc-", "libpthread", "libdl.so", "libm.so", "librt.so"}

func isSystemModule(module string) bool {
	b := base(module)
	if b == "" {
		return false
	}
	for _, p := range systemModulePrefixes {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	return false
}

var systemSymbols = map[string]bool{
	"_start": true, "_start_c": true, "start_thread": true, "clone": true,
	"clone3": true, "__clone": true, "__clone3": true, "abort": true,
	"_exit": true, "exit": true, "syscall": true,
}

var systemSymbolPrefixes = []string{"__libc_", "__GI_", "_dl_", "__pthread_", "__nptl_", "_IO_"}

func isSystemSymbol(name string) bool {
	if systemSymbols[name] {
		return true
	}
	for _, p := range systemSymbolPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// isUnsymbolized reports whether a frame name carries no symbol: a bare
// address, the module-relative "libcuda.so.1+0x1b71c6" form, or the
// placeholder the folder writes for a location with neither.
func isUnsymbolized(name string) bool {
	return name == foldedstacks.UnknownFrame || framename.IsAddressOnly(name)
}
