package flamegraph

import (
	"path"
	"strings"

	"github.com/dpsoft/perf-agent/internal/foldedstacks"
)

// Domain is what a frame *is*, not how deep it sits or what its name hashes
// to. A hashed palette carries no information; these profiles have real
// structure worth showing — application code, the CPU path that initiates
// GPU work, an explicit boundary marker, and the accelerator side — so the
// palette carries that instead. (The idea of colouring a mixed CPU/GPU flame
// graph by domain is Brendan Gregg's.)
//
// Two domains have no counterpart in that scheme and exist here because they
// are honesty features rather than decoration:
//
//   - DomainUnsymbolized. These frames were unwound correctly; no symbol
//     table could name them. That is a symbolization gap, not a hole in the
//     stack, and the reader must be able to tell the difference at a glance.
//   - DomainProfilerShim. perf-agent's own injected callback appears in
//     perf-agent's own profile. Colouring it as ordinary CPU work would bill
//     the profiler's overhead to the program being profiled.
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
	// Overlay names an SVG pattern painted over the fill, or "" for none.
	// Patterns encode uncertainty: a hatched frame is one the profile
	// could not fully account for.
	Overlay string
	// Stroke is an explicit outline colour, or "" for the default.
	Stroke string
}

var domainInfo = [numDomains]DomainInfo{
	DomainRoot: {
		Key: "root", Label: "all", Fill: "hsl(30 8% 78%)",
		Desc: "Synthetic root. Its width is the whole profile.",
	},
	DomainApplication: {
		Key: "app", Label: "application", Fill: "hsl(336 62% 74%)",
		Desc: "Your program's own code, and compiler-generated glue in its binary.",
	},
	DomainSystem: {
		Key: "system", Label: "process startup and libc", Fill: "hsl(4 62% 67%)",
		Desc: "The C runtime, dynamic loader and thread entry that get to main.",
	},
	DomainKernel: {
		Key: "kernel", Label: "kernel", Fill: "hsl(48 82% 62%)",
		Desc: "Kernel-mode frames, identified by the profile's [kernel] mapping.",
	},
	DomainVendorRuntime: {
		Key: "vendor", Label: "GPU runtime, CPU side", Fill: "hsl(26 88% 64%)",
		Desc: "CUDA/HIP/HSA runtime and driver frames: the CPU path that initiates GPU work.",
	},
	DomainUnsymbolized: {
		Key: "unsym", Label: "unsymbolized", Fill: "hsl(214 10% 68%)", Overlay: "url(#p-gap)",
		Desc: "Unwound correctly; no symbol table could name it. The depth here is real — the names are missing, usually because a stripped vendor library has no exported symbols.",
	},
	DomainProfilerShim: {
		Key: "shim", Label: "perf-agent's own shim", Fill: "hsl(272 32% 70%)",
		Desc: "The profiler observing itself: perf-agent's injected callback, not work your program asked for.",
	},
	DomainBoundary: {
		Key: "boundary", Label: "CPU→GPU boundary", Fill: "hsl(0 0% 82%)", Stroke: "hsl(0 0% 42%)",
		Desc: "[gpu:launch] — the launch this execution was joined to. Everything below it ran on the CPU; everything above it ran on the accelerator.",
	},
	DomainBoundaryUnattributed: {
		Key: "boundary-unattributed", Label: "GPU work with no CPU stack", Fill: "hsl(0 0% 88%)", Overlay: "url(#p-gap)", Stroke: "hsl(0 0% 55%)",
		Desc: "[gpu:launch unsampled] — measured GPU time whose launch was not one of the sampled ones, so no CPU call path exists for it. Its duration is measured; its caller is unknown, and is not borrowed from a sampled sibling.",
	},
	DomainGPUKernel: {
		Key: "gpu-kernel", Label: "GPU kernel execution", Fill: "hsl(142 44% 55%)",
		Desc: "The kernel that ran on the accelerator, and how long it ran for.",
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
	case isShimModule(module):
		return DomainProfilerShim
	case isVendorModule(module):
		return DomainVendorRuntime
	case isSystemModule(module):
		return DomainSystem

	// --- name-derived ---
	case isUnsymbolized(name):
		return DomainUnsymbolized
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
// address, or the placeholder the folder writes for a location with neither.
func isUnsymbolized(name string) bool {
	return name == foldedstacks.UnknownFrame || (strings.HasPrefix(name, "0x") && isHex(name[2:]))
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
