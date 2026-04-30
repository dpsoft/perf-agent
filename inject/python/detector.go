package python

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/inject/elfsym"
)

// Target describes a Python 3.12+ process that detection has confirmed is
// suitable for trampoline injection. All address fields are absolute remote
// addresses (load_base + symbol_offset) ready to be passed to ptraceop.
type Target struct {
	PID              uint32
	LibPythonPath    string // on-disk path used for ELF parsing
	LoadBase         uint64 // address from /proc/<pid>/maps for libpython
	PyGILEnsureAddr  uint64
	PyGILReleaseAddr uint64
	PyRunStringAddr  uint64
	Major, Minor     int // detected libpython version
}

// Detector inspects a process and reports whether it is a Python 3.12+
// candidate suitable for trampoline injection.
type Detector interface {
	Detect(pid uint32) (*Target, error)
}

// requiredSymbols are resolved by the detector; addresses are recorded on Target.
var requiredSymbols = []string{
	"PyGILState_Ensure",
	"PyGILState_Release",
	"PyRun_SimpleString",
}

// We previously used `_PyPerf_Callbacks` as a presence-only "is the trampoline
// compiled in?" pre-flight check. That was wrong: distributors (Fedora,
// Ubuntu, etc.) strip internal symbols from production libpython builds, so
// the symbol is absent from the binary's symbol table even when
// `--enable-perf-trampoline` was set at compile time. We therefore rely on
// the runtime return value of `PyRun_SimpleString` to tell us whether the
// activation actually succeeded — at the cost of one extra ptrace round trip
// for genuinely-unsupported targets.

// NewDetector builds a /proc-based Detector. procRoot is configurable for
// testing; production callers pass "/proc". log may be nil (uses
// slog.Default()).
func NewDetector(procRoot string, log *slog.Logger) Detector {
	if log == nil {
		log = slog.Default()
	}
	return &procDetector{procRoot: procRoot, log: log}
}

type procDetector struct {
	procRoot string
	log      *slog.Logger
}

// Detect implements Detector. Errors are sentinel-wrapped (errors.Is friendly):
// ErrNotPython, ErrPythonTooOld, ErrStaticallyLinkedNoSymbols. Any other
// error (e.g. EACCES on /proc) is returned as a hard error for the caller to
// decide whether to abort. ErrNoPerfTrampoline is now surfaced at activation
// time only (when PyRun_SimpleString returns non-zero), not at detection.
func (d *procDetector) Detect(pid uint32) (*Target, error) {
	mapsPath := filepath.Join(d.procRoot, strconv.FormatUint(uint64(pid), 10), "maps")
	f, err := os.Open(mapsPath)
	if err != nil {
		// /proc/<pid>/maps disappeared — process exited between scan and detect.
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: process gone", ErrNotPython)
		}
		return nil, fmt.Errorf("open %s: %w", mapsPath, err)
	}
	defer func() { _ = f.Close() }()

	libpythonPath, libpythonBase, ok := scanForLibpython(f)
	if ok {
		return d.resolveDynamic(pid, libpythonPath, libpythonBase)
	}

	// Fall back to /proc/<pid>/exe (statically-linked CPython).
	return d.resolveStatic(pid)
}

// scanForLibpython walks the maps file looking for the libpython load base —
// the mapping that backs the file at offset 0 (the first PT_LOAD segment).
// Returns the on-disk path and the load address, or ("", 0, false).
//
// We filter on offset==0 rather than executable perms because modern toolchains
// (glibc with -Wl,-z,separate-code, default on Ubuntu 24.04+) produce an ELF
// with a leading r-- LOAD segment for the header + RELRO data and a separate
// r-x segment for .text at a non-zero file offset. Picking the first
// executable mapping in that layout returns load_base + first_segment_size,
// which is wrong: every absolute symbol address derived from it is off by
// that delta, RIP jumps to garbage on remote call, and the target SEGVs with
// RAX still holding whatever value the kernel left in it (e.g.
// -ERESTARTNOHAND if attached mid-syscall).
//
// Returns the first match — multiple libpython mappings can exist when
// extension modules embed a second interpreter (rare); see spec §5 edge cases.
func scanForLibpython(maps *os.File) (string, uint64, bool) {
	sc := bufio.NewScanner(maps)
	for sc.Scan() {
		line := sc.Text()
		// Format: "addrStart-addrEnd perms offset dev inode pathname"
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		offset, err := strconv.ParseUint(fields[2], 16, 64)
		if err != nil || offset != 0 {
			continue
		}
		path := fields[5]
		if _, _, isPy := elfsym.ParseLibpythonSONAME(path); !isPy {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		startHex := fields[0][:dash]
		start, err := strconv.ParseUint(startHex, 16, 64)
		if err != nil {
			continue
		}
		return path, start, true
	}
	return "", 0, false
}

// resolveDynamic handles the case where libpython is mapped as a shared lib.
func (d *procDetector) resolveDynamic(pid uint32, libpath string, loadBase uint64) (*Target, error) {
	major, minor, _ := elfsym.ParseLibpythonSONAME(libpath)
	if !elfsym.IsPython312Plus(major, minor) {
		return nil, fmt.Errorf("%w: detected %d.%d", ErrPythonTooOld, major, minor)
	}
	resolved, err := elfsym.ResolveSymbols(libpath, requiredSymbols)
	if err != nil {
		return nil, fmt.Errorf("resolve symbols in %s: %w", libpath, err)
	}
	d.log.Debug("python detector: dynamic match",
		"pid", pid, "libpython", libpath, "loadbase", loadBase,
		"version", fmt.Sprintf("%d.%d", major, minor))
	for _, sym := range requiredSymbols {
		if _, ok := resolved[sym]; !ok {
			return nil, fmt.Errorf("%w: %s missing in %s", ErrStaticallyLinkedNoSymbols, sym, libpath)
		}
	}
	return &Target{
		PID:              pid,
		LibPythonPath:    libpath,
		LoadBase:         loadBase,
		PyGILEnsureAddr:  loadBase + resolved["PyGILState_Ensure"],
		PyGILReleaseAddr: loadBase + resolved["PyGILState_Release"],
		PyRunStringAddr:  loadBase + resolved["PyRun_SimpleString"],
		Major:            major,
		Minor:            minor,
	}, nil
}

// resolveStatic handles statically-linked CPython, where libpython symbols are
// in the executable itself rather than a shared library.
func (d *procDetector) resolveStatic(pid uint32) (*Target, error) {
	exePath := filepath.Join(d.procRoot, strconv.FormatUint(uint64(pid), 10), "exe")
	realExe, err := os.Readlink(exePath)
	if err != nil {
		return nil, fmt.Errorf("%w: readlink %s: %v", ErrNotPython, exePath, err)
	}
	resolved, err := elfsym.ResolveSymbols(realExe, requiredSymbols)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotPython, err)
	}
	d.log.Debug("python detector: static match",
		"pid", pid, "exe", realExe)
	for _, sym := range requiredSymbols {
		if _, ok := resolved[sym]; !ok {
			return nil, fmt.Errorf("%w: %s missing in %s", ErrNotPython, sym, realExe)
		}
	}
	mapsPath := filepath.Join(d.procRoot, strconv.FormatUint(uint64(pid), 10), "maps")
	f, err := os.Open(mapsPath)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", mapsPath, err)
	}
	defer func() { _ = f.Close() }()
	loadBase := scanForExeBase(f, realExe)
	if loadBase == 0 {
		return nil, fmt.Errorf("%w: cannot find exe load base in %s", ErrNotPython, mapsPath)
	}
	return &Target{
		PID:              pid,
		LibPythonPath:    realExe,
		LoadBase:         loadBase,
		PyGILEnsureAddr:  loadBase + resolved["PyGILState_Ensure"],
		PyGILReleaseAddr: loadBase + resolved["PyGILState_Release"],
		PyRunStringAddr:  loadBase + resolved["PyRun_SimpleString"],
		Major:            0, // static path; SONAME version unknown
		Minor:            0,
	}, nil
}

// scanForExeBase finds the load base of the statically-linked CPython exe.
// Same rationale as scanForLibpython: we filter on offset==0 to pick the
// first PT_LOAD segment's mapping, which gives the correct load_base even
// when modern -Wl,-z,separate-code lays out a leading r-- segment.
func scanForExeBase(maps *os.File, realExe string) uint64 {
	var lowest uint64
	sc := bufio.NewScanner(maps)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 {
			continue
		}
		if fields[5] != realExe {
			continue
		}
		offset, err := strconv.ParseUint(fields[2], 16, 64)
		if err != nil || offset != 0 {
			continue
		}
		dash := strings.IndexByte(fields[0], '-')
		if dash < 0 {
			continue
		}
		start, err := strconv.ParseUint(fields[0][:dash], 16, 64)
		if err != nil {
			continue
		}
		if lowest == 0 || start < lowest {
			lowest = start
		}
	}
	return lowest
}
