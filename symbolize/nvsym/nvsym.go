// Package nvsym fetches symbol-only ELF files from NVIDIA's CUDA Toolkit
// Symbol Server.
//
// Why this exists
// ---------------
// NVIDIA ships libcuda, libcupti, libcublas and libcublasLt stripped, and no
// distribution publishes debuginfo for them: NVIDIA's own Fedora 44 repo has
// 150 packages and only DCGM among them, RPM Fusion has none, and the public
// debuginfod federation 404s on all three build-ids with a libc control
// returning 200. Measured coverage of the exported symbols alone is 0.27% of
// .text for libcuda, 3.2% for libcupti and 0.16% for libcublasLt, so almost
// every address in them arrives unnameable.
//
// NVIDIA does, however, run a symbol server that serves a symbols-only ELF per
// build-id. Verified against this machine's driver: the local
// /lib64/libcuda.so.1 has zero .symtab entries, while the file fetched for its
// build-id carries 59,954 defined symbols, and 13 of 13 frames from a real
// PyTorch capture resolve with it in place.
//
// The URL shape is not the debuginfod one and not guessable
// ---------------------------------------------------------
// It is <bare soname>/<build-id>/index.html, where "bare" means the soname
// with every version suffix removed. Measured, for one build-id:
//
//	libcuda.so/<id>/index.html            -> 200
//	libcuda.so.1/<id>/index.html          -> 403
//	libcuda.so.610.57.04/<id>/index.html  -> 403
//
// An implementation that passes the name as it appears in /proc/<pid>/maps
// therefore gets 403 on everything and looks like the server is down. An
// all-zero build-id also returns 403, so a 200 is a real hit rather than a
// catch-all.
//
// Coverage is partial and a caller must degrade rather than depend on it: on
// this machine the pip-wheel cuBLASLt returns 200 while the CUDA-toolkit build
// of the same library, and libcupti.so.13.3, both 403.
package nvsym

import (
	"path"
	"regexp"
	"strings"
)

// DefaultBaseURL is NVIDIA's CUDA Toolkit Symbol Server.
const DefaultBaseURL = "https://cudatoolkit-symbols.nvidia.com"

// versionSuffix matches the trailing ".N" groups of a versioned soname:
// ".1" in libcuda.so.1, ".610.57.04" in libcuda.so.610.57.04.
var versionSuffix = regexp.MustCompile(`(\.[0-9]+)+$`)

// SonameKey reduces a module path to the key the symbol server expects: the
// base name with every trailing version component removed.
//
//	/usr/lib64/libcuda.so.610.57.04           -> libcuda.so
//	.../nvidia/cu13/lib/libcublasLt.so.13     -> libcublasLt.so
//	/usr/lib64/libcuda.so                     -> libcuda.so
//
// Returns "" for a path with no usable base name, and for one whose base does
// not contain ".so" at all -- there is nothing to ask the server about, and a
// malformed key would be a 403 indistinguishable from a real miss.
func SonameKey(modulePath string) string {
	base := path.Base(strings.TrimSpace(modulePath))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	if !strings.Contains(base, ".so") {
		return ""
	}
	return versionSuffix.ReplaceAllString(base, "")
}

// IsNVIDIAModule reports whether a module is one this server might serve.
//
// Used to avoid spending a network round trip, and a build-id disclosure, on
// every unrelated library in the process. The list is the CUDA runtime and
// driver family; it is deliberately narrower than flamegraph's vendor list,
// which also classifies AMD libraries this server knows nothing about.
func IsNVIDIAModule(modulePath string) bool {
	base := path.Base(modulePath)
	for _, p := range []string{
		"libcuda", "libcudart", "libcupti", "libcublas", "libcudnn",
		"libcufft", "libcurand", "libcusparse", "libcusolver",
		"libnccl", "libnvrtc", "libnvjitlink", "libnvidia", "libnvperf",
	} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// symHref matches the listing's link to the symbol file. The name is NOT
// derivable from the soname -- the listing for libcuda.so serves
// libcuda.so.1.1.sym -- so it has to be read rather than constructed.
var symHref = regexp.MustCompile(`href="([^"]+\.sym)"`)

// SymFileName extracts the symbol file's name from a directory listing.
//
// The server's <soname>/<build-id>/index.html is an HTML listing, not the
// symbols: fetching that URL and caching the body yields a 740-byte web page
// that every later reader will treat as a corrupt ELF. The real artifact is
// one [FILE] link inside it.
//
// Returns "" when the listing contains no .sym link, which is how an error
// page or a changed format arrives.
func SymFileName(listing []byte) string {
	m := symHref.FindSubmatch(listing)
	if m == nil {
		return ""
	}
	name := string(m[1])
	// Refuse anything that would escape the build-id directory. The listing
	// is third-party input and this name becomes part of a URL and, on the
	// caller's side, nothing else -- but a "../" here is a signal the format
	// is not what we think it is.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return ""
	}
	return name
}

// ListingURL builds the URL of the directory listing for a module's build-id.
// Returns "" when either input is unusable, so a caller cannot accidentally
// request a malformed path.
func ListingURL(baseURL, modulePath, buildID string) string {
	soname := SonameKey(modulePath)
	if soname == "" || buildID == "" {
		return ""
	}
	// An all-zero build-id is what an ELF without a real note looks like
	// after hex-encoding, and the server answers 403 for it. Refuse locally
	// rather than spend the round trip.
	if strings.Trim(buildID, "0") == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/" + soname + "/" + buildID + "/index.html"
}

// FileURL builds the URL of the symbol file itself, given the name read from
// the listing.
func FileURL(baseURL, modulePath, buildID, symName string) string {
	soname := SonameKey(modulePath)
	if soname == "" || buildID == "" || symName == "" {
		return ""
	}
	return strings.TrimRight(baseURL, "/") + "/" + soname + "/" + buildID + "/" + symName
}
