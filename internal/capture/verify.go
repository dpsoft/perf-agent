package capture

import (
	"debug/buildinfo"
	"debug/elf"
	"fmt"
	"runtime/debug"
	"strings"
)

// PerfAgentModule is perf-agent's Go module path. Its main package is the
// module root.
const PerfAgentModule = "github.com/dpsoft/perf-agent"

// checkBuildInfo proves, without running it, that path is perf-agent: a
// Go binary whose main package is the root of PerfAgentModule. It returns
// the module version and the VCS revision it was built from (empty when
// it was built without VCS information).
func checkBuildInfo(path string) (version, revision string, err error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", "", Errorf(CodeDisabled, "pass the perf-agent binary built from "+PerfAgentModule,
			"%s is not perf-agent: no Go build information (%v)", path, err)
	}
	version, revision, err = checkModule(bi)
	if err != nil {
		return "", "", Errorf(CodeDisabled, "pass the perf-agent binary built from "+PerfAgentModule,
			"%s is not perf-agent: %v", path, err)
	}
	return version, revision, nil
}

// checkModule checks the build information of a binary.
func checkModule(bi *debug.BuildInfo) (version, revision string, err error) {
	if bi.Main.Path != PerfAgentModule {
		return "", "", fmt.Errorf("its main module is %q, want %q", bi.Main.Path, PerfAgentModule)
	}
	if bi.Path != PerfAgentModule {
		return "", "", fmt.Errorf("its main package is %q, want the module root %q", bi.Path, PerfAgentModule)
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			revision = s.Value
		}
	}
	return bi.Main.Version, revision, nil
}

// checkELF warns when perf-agent loads blazesym as a shared library.
// Under secure execution (file capabilities) the loader ignores
// LD_LIBRARY_PATH, so the library must be in a system path.
func checkELF(path string) []string {
	f, err := elf.Open(path)
	if err != nil {
		return []string{fmt.Sprintf("cannot read %s as ELF to check its shared libraries: %v", path, err)}
	}
	defer func() { _ = f.Close() }()
	libs, err := f.ImportedLibraries()
	if err != nil {
		return []string{fmt.Sprintf("cannot read the shared libraries of %s: %v", path, err)}
	}
	return blazesymWarnings(libs)
}

// blazesymWarnings returns a warning per DT_NEEDED entry naming blazesym.
func blazesymWarnings(libs []string) []string {
	var w []string
	for _, l := range libs {
		if strings.Contains(l, "blazesym") {
			w = append(w, fmt.Sprintf("perf-agent loads %s as a shared library; with file capabilities LD_LIBRARY_PATH is ignored, so it must be in a system library path, or rebuild perf-agent with `make build` to link it statically", l))
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
