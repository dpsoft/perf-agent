//go:build fixtures

package pyunwind

// TestOffsetsMatchRealInterpreters compares each table in offsets.go against
// offsetof() compiled fresh inside a container running that exact micro
// version's own interpreter, via podman. Run with:
//
//	go test -tags fixtures ./pyunwind/
//
// This is the test that makes the tables mean anything. Asserting a Go
// constant equals the literal it was written from proves only that nobody
// mistyped it twice; this test instead recompiles a tiny C probe against
// python:X.Y.Z-slim's own internal/pycore_frame.h (or, on 3.14,
// internal/pycore_interpframe_structs.h) and internal/pycore_frame.h's
// _frameowner enum, and fails if the header ever disagrees with offsets.go.
//
// Skips (does not fail) when podman is unavailable, so CI without
// containers stays green.
import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// probeSource is the per-minor-version C source. Field names diverge across
// versions (f_code vs f_executable, prev_instr vs instr_ptr, direct vs
// indirect current_frame), so each version gets its own tailored probe
// rather than one probe scanning for names that don't all exist everywhere.
type fixtureCase struct {
	version Version
	image   string // exact micro-version image, matching version precisely
	include string // -I directory suffix under /usr/local/include
	source  string
}

const probe312 = `
#define Py_BUILD_CORE 1
#include <Python.h>
#include <internal/pycore_frame.h>
#include <stddef.h>
#include <stdio.h>

int main(void) {
    printf("FramePrevious=%zu\n", offsetof(struct _PyInterpreterFrame, previous));
    printf("FrameExecutable=%zu\n", offsetof(struct _PyInterpreterFrame, f_code));
    printf("FrameInstrPtr=%zu\n", offsetof(struct _PyInterpreterFrame, prev_instr));
    printf("FrameOwner=%zu\n", offsetof(struct _PyInterpreterFrame, owner));
    printf("FrameOwnerMax=%d\n", (int)FRAME_OWNED_BY_CSTACK);
    printf("FrameOwnerCStack=%d\n", (int)FRAME_OWNED_BY_CSTACK);
    printf("ThreadStateFrame=%zu\n", offsetof(PyThreadState, cframe));
    printf("CFrameCurrentFrame=%zu\n", offsetof(_PyCFrame, current_frame));
    printf("CodeArgCount=%zu\n", offsetof(PyCodeObject, co_argcount));
    printf("CodeKwOnlyArgCount=%zu\n", offsetof(PyCodeObject, co_kwonlyargcount));
    printf("CodeFlags=%zu\n", offsetof(PyCodeObject, co_flags));
    printf("CodeFirstLineNo=%zu\n", offsetof(PyCodeObject, co_firstlineno));
    return 0;
}
`

const probe313 = `
#define Py_BUILD_CORE 1
#include <Python.h>
#include <internal/pycore_frame.h>
#include <stddef.h>
#include <stdio.h>

int main(void) {
    printf("FramePrevious=%zu\n", offsetof(struct _PyInterpreterFrame, previous));
    printf("FrameExecutable=%zu\n", offsetof(struct _PyInterpreterFrame, f_executable));
    printf("FrameInstrPtr=%zu\n", offsetof(struct _PyInterpreterFrame, instr_ptr));
    printf("FrameOwner=%zu\n", offsetof(struct _PyInterpreterFrame, owner));
    printf("FrameOwnerMax=%d\n", (int)FRAME_OWNED_BY_CSTACK);
    printf("FrameOwnerCStack=%d\n", (int)FRAME_OWNED_BY_CSTACK);
    printf("ThreadStateFrame=%zu\n", offsetof(PyThreadState, current_frame));
    printf("CodeArgCount=%zu\n", offsetof(PyCodeObject, co_argcount));
    printf("CodeKwOnlyArgCount=%zu\n", offsetof(PyCodeObject, co_kwonlyargcount));
    printf("CodeFlags=%zu\n", offsetof(PyCodeObject, co_flags));
    printf("CodeFirstLineNo=%zu\n", offsetof(PyCodeObject, co_firstlineno));
    return 0;
}
`

const probe314 = `
#define Py_BUILD_CORE 1
#include <Python.h>
#include <internal/pycore_frame.h>
#include <internal/pycore_interpframe_structs.h>
#include <stddef.h>
#include <stdio.h>

int main(void) {
    printf("FramePrevious=%zu\n", offsetof(struct _PyInterpreterFrame, previous));
    printf("FrameExecutable=%zu\n", offsetof(struct _PyInterpreterFrame, f_executable));
    printf("FrameInstrPtr=%zu\n", offsetof(struct _PyInterpreterFrame, instr_ptr));
    printf("FrameOwner=%zu\n", offsetof(struct _PyInterpreterFrame, owner));
    printf("FrameOwnerMax=%d\n", (int)FRAME_OWNED_BY_CSTACK);
    printf("FrameOwnerCStack=%d\n", (int)FRAME_OWNED_BY_CSTACK);
    printf("ThreadStateFrame=%zu\n", offsetof(PyThreadState, current_frame));
    printf("CodeArgCount=%zu\n", offsetof(PyCodeObject, co_argcount));
    printf("CodeKwOnlyArgCount=%zu\n", offsetof(PyCodeObject, co_kwonlyargcount));
    printf("CodeFlags=%zu\n", offsetof(PyCodeObject, co_flags));
    printf("CodeFirstLineNo=%zu\n", offsetof(PyCodeObject, co_firstlineno));
    return 0;
}
`

var fixtureCases = []fixtureCase{
	{Version{3, 12, 14}, "docker.io/library/python:3.12.14-slim", "python3.12", probe312},
	{Version{3, 13, 15}, "docker.io/library/python:3.13.15-slim", "python3.13", probe313},
	{Version{3, 14, 3}, "docker.io/library/python:3.14.3-slim", "python3.14", probe314},
}

func TestOffsetsMatchRealInterpreters(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available; skipping fixture verification against real interpreters")
	}

	for _, tc := range fixtureCases {
		t.Run(tc.version.String(), func(t *testing.T) {
			measured := runProbe(t, tc)
			want, err := TableFor(tc.version)
			if err != nil {
				t.Fatalf("TableFor(%v): %v", tc.version, err)
			}

			check := func(field string, got, wantVal uint64) {
				t.Helper()
				if got != wantVal {
					t.Errorf("%s: measured %d, table has %d", field, got, wantVal)
				}
			}
			check("FramePrevious", measured["FramePrevious"], uint64(want.FramePrevious))
			check("FrameExecutable", measured["FrameExecutable"], uint64(want.FrameExecutable))
			check("FrameInstrPtr", measured["FrameInstrPtr"], uint64(want.FrameInstrPtr))
			check("FrameOwner", measured["FrameOwner"], uint64(want.FrameOwner))
			check("FrameOwnerMax", measured["FrameOwnerMax"], uint64(want.FrameOwnerMax))
			check("FrameOwnerCStack", measured["FrameOwnerCStack"], uint64(want.FrameOwnerCStack))
			check("CodeArgCount", measured["CodeArgCount"], uint64(want.CodeArgCount))
			check("CodeKwOnlyArgCount", measured["CodeKwOnlyArgCount"], uint64(want.CodeKwOnlyArgCount))
			check("CodeFlags", measured["CodeFlags"], uint64(want.CodeFlags))
			check("CodeFirstLineNo", measured["CodeFirstLineNo"], uint64(want.CodeFirstLineNo))

			// ThreadStateFrame: on 3.12 the probe measures the offset of
			// `cframe` (the indirect case); on 3.13/3.14 it measures
			// current_frame directly. Both compare against
			// want.ThreadStateFrame, since that's what TableFor stores
			// either way -- see ThreadStateFrameIndirect.
			check("ThreadStateFrame", measured["ThreadStateFrame"], uint64(want.ThreadStateFrame))
			if tc.version.Minor == 12 {
				if got := measured["CFrameCurrentFrame"]; got != 0 {
					t.Errorf("3.12: _PyCFrame.current_frame must be at offset 0 (first field), got %d", got)
				}
				if !want.ThreadStateFrameIndirect {
					t.Error("3.12: table must mark ThreadStateFrameIndirect")
				}
			} else if want.ThreadStateFrameIndirect {
				t.Errorf("%v: table must not mark ThreadStateFrameIndirect", tc.version)
			}
		})
	}
}

func runProbe(t *testing.T, tc fixtureCase) map[string]uint64 {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "p.c"), []byte(tc.source), 0o644); err != nil {
		t.Fatalf("write probe source: %v", err)
	}

	script := fmt.Sprintf(`set -e
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq gcc >/dev/null 2>&1
gcc -I/usr/local/include/%s -o /tmp/p /probe/p.c
/tmp/p`, tc.include)

	cmd := exec.Command("podman", "run", "--rm", "-v", dir+":/probe:Z", tc.image, "bash", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("podman probe failed for %v (%s): %v\n%s", tc.version, tc.image, err, out)
	}

	result := make(map[string]uint64)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		result[k] = n
	}
	return result
}
