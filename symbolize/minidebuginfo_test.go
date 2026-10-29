package symbolize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	blazesym "github.com/libbpf/blazesym/go"
)

// A function that only .gnu_debugdata names must resolve (issue #220).
//
// Fedora and RHEL ship MiniDebugInfo: stripped binaries whose local function
// symbols live in an xz-compressed ELF in .gnu_debugdata. A libblazesym_c built
// without its xz feature answers "unsupported" for such a file -- and the
// process source fails the whole batch, not just the frame, so every address
// in python3, libpython and libm came back as module+offset. This links
// against whatever libblazesym_c the build found, so it fails if a build site
// drops the feature again.
//
// The fixture is checked from both sides: stripped and without the section,
// the function must NOT resolve, or the positive half proves nothing.
func TestMiniDebugInfoResolves(t *testing.T) {
	for _, tool := range []string{"cc", "nm", "objcopy", "xz"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	path := func(name string) string { return filepath.Join(dir, name) }
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	// static and noinline: a local symbol, so .dynsym cannot name it and the
	// only name left after stripping is the one in .gnu_debugdata.
	src := "__attribute__((noinline)) static int mdi_target(int x) { return x * 3 + 1; }\n" +
		"int main(int argc, char **argv) { (void)argv; return mdi_target(argc); }\n"
	if err := os.WriteFile(path("t.c"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	run("cc", "-O1", "-o", path("full"), path("t.c"))

	var addr uint64
	for line := range strings.Lines(run("nm", "--defined-only", path("full"))) {
		f := strings.Fields(line)
		if len(f) == 3 && f[2] == "mdi_target" {
			v, err := strconv.ParseUint(f[0], 16, 64)
			if err != nil {
				t.Fatalf("nm address %q: %v", f[0], err)
			}
			addr = v
		}
	}
	if addr == 0 {
		t.Fatal("mdi_target not in the unstripped fixture's symbol table")
	}

	// The MiniDebugInfo recipe, reduced: a symbol-table-only ELF, xz'd, added
	// to a fully stripped copy.
	run("objcopy", "--only-keep-debug", "--strip-debug", path("full"), path("mini"))
	run("xz", "-k", path("mini"))
	run("objcopy", "--strip-all", path("full"), path("stripped"))
	run("objcopy", "--add-section", ".gnu_debugdata="+path("mini.xz"), path("stripped"), path("mdi"))

	bz, err := blazesym.NewSymbolizer()
	if err != nil {
		t.Fatalf("blazesym.NewSymbolizer: %v", err)
	}
	defer bz.Close()
	name := func(file string) (string, error) {
		syms, err := bz.SymbolizeElfVirtOffsets([]uint64{addr}, file, blazesym.ElfSourceWithDebugSyms(true))
		if err != nil {
			return "", err
		}
		if len(syms) != 1 {
			t.Fatalf("%s: %d symbols for one address", file, len(syms))
		}
		return syms[0].Name, nil
	}

	if got, err := name(path("stripped")); err == nil && got == "mdi_target" {
		t.Fatal("the stripped fixture still names mdi_target; the test would pass without .gnu_debugdata")
	}
	got, err := name(path("mdi"))
	if err != nil {
		t.Fatalf("symbolizing a MiniDebugInfo binary: %v (libblazesym_c built without --features blazesym/xz?)", err)
	}
	if got != "mdi_target" {
		t.Fatalf("got %q, want mdi_target from .gnu_debugdata", got)
	}
}
