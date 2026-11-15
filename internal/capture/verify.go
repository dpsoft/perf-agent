package capture

import (
	"debug/buildinfo"
	"debug/elf"
	"fmt"
	"io"
	"runtime/debug"
	"strings"
	"unicode/utf8"
)

// PerfAgentModule is perf-agent's Go module path. Its main package is the
// module root.
const PerfAgentModule = "github.com/dpsoft/perf-agent"

// checkBuildInfo identifies the file as perf-agent without running it: a
// Go binary whose main package is the root of PerfAgentModule. This is a
// marker, not authentication; the capability xattr is the trust anchor.
// It returns the module version and the VCS revision it was built from
// (empty when it was built without VCS information). name is only for
// messages.
func checkBuildInfo(r io.ReaderAt, name string) (version, revision string, err error) {
	bi, err := buildinfo.Read(r)
	if err != nil {
		return "", "", Errorf(CodeDisabled, "pass the perf-agent binary built from "+PerfAgentModule,
			"%s is not perf-agent: no Go build information (%v)", name, err)
	}
	version, revision, err = checkModule(bi)
	if err != nil {
		return "", "", Errorf(CodeDisabled, "pass the perf-agent binary built from "+PerfAgentModule,
			"%s is not perf-agent: %v", name, err)
	}
	return version, revision, nil
}

// maxEchoBytes bounds each string from the file's build information that a
// message or a result repeats.
const maxEchoBytes = 200

// clip cuts s to at most maxEchoBytes on a rune boundary, marking the cut.
func clip(s string) string {
	if len(s) <= maxEchoBytes {
		return s
	}
	i := maxEchoBytes
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i] + "…"
}

// checkModule checks the build information of a binary.
func checkModule(bi *debug.BuildInfo) (version, revision string, err error) {
	if bi.Main.Path != PerfAgentModule {
		return "", "", fmt.Errorf("its main module is %q, want %q", clip(bi.Main.Path), PerfAgentModule)
	}
	if bi.Path != PerfAgentModule {
		return "", "", fmt.Errorf("its main package is %q, want the module root %q", clip(bi.Path), PerfAgentModule)
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
	}
	return clip(bi.Main.Version), clip(revision), nil
}

// checkELF warns when perf-agent loads blazesym as a shared library.
// Under secure execution (file capabilities) the loader ignores
// LD_LIBRARY_PATH, so the library must be in a system path. name is only
// for messages.
func checkELF(r io.ReaderAt, name string) []string {
	f, err := elf.NewFile(r)
	if err != nil {
		return []string{fmt.Sprintf("cannot read %s as ELF to check its shared libraries: %v", name, err)}
	}
	libs, err := f.ImportedLibraries()
	if err != nil {
		return []string{fmt.Sprintf("cannot read the shared libraries of %s: %v", name, err)}
	}
	return blazesymWarnings(libs)
}

// blazesymWarnings returns a warning per DT_NEEDED entry naming blazesym.
func blazesymWarnings(libs []string) []string {
	var w []string
	for _, l := range libs {
		if strings.Contains(l, "blazesym") {
			w = append(w, fmt.Sprintf("perf-agent loads %q as a shared library; with file capabilities LD_LIBRARY_PATH is ignored, so it must be in a system library path, or rebuild perf-agent with `make build` to link it statically", clip(l)))
		}
	}
	return w
}

// checkIdentity compares the identity taken at verification with the
// current one. Changed names the fields that differ.
func checkIdentity(was, now Identity) *Error {
	if was == now {
		return nil
	}
	var changed []string
	add := func(differs bool, name string) {
		if differs {
			changed = append(changed, name)
		}
	}
	add(was.Dev != now.Dev, "dev")
	add(was.Ino != now.Ino, "ino")
	add(was.Size != now.Size, "size")
	add(was.Mtime != now.Mtime, "mtime")
	add(was.Ctime != now.Ctime, "ctime")
	add(was.Xattr != now.Xattr, "xattr")
	e := Errorf(CodeBinaryChanged, "restart perf-agent-mcp", "perf-agent changed since start-up")
	e.Changed = strings.Join(changed, ",")
	return e
}
