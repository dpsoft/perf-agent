//go:build linux && (amd64 || arm64) && cgo

package pyunwind

import (
	"runtime"
	"testing"

	"github.com/dpsoft/perf-agent/pyunwind/internal/tsdprobe"
)

// TestGlibcTSDOffsetsMatchThisMachine measures glibc's pthread TSD layout on
// the machine the test is running on and compares it to the table
// glibcTSDOffsets serves.
//
// This exists because a wrong offset in that table does not fail loudly: it
// reads a pointer out of the wrong byte of `struct pthread` and returns a
// plausible, wrong PyThreadState, which becomes confident, fabricated Python
// frames. The table is measured data, so what guards it has to be a
// measurement, not a restatement of its own constants.
//
// It runs on both supported architectures, which is the point: the same
// probe that confirms amd64 is the one that produced arm64's row, so
// neither is trusted on authority.
func TestGlibcTSDOffsetsMatchThisMachine(t *testing.T) {
	// The probe and the offsets must describe the same thread, and a Go
	// goroutine may migrate between OS threads at any call.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	got, ok := tsdprobe.Measure()
	if !ok {
		t.Skipf("TSD probe did not complete on this libc (not glibc, or an unexpected layout); GOARCH=%s",
			runtime.GOARCH)
	}

	want, err := glibcTSDOffsets(runtime.GOARCH)
	if err != nil {
		t.Fatalf("glibcTSDOffsets(%q): %v -- the probe measured this machine fine, so the table is "+
			"missing a row it could have: Specific1stBlock=%#x KeyDataSize=%d KeyDataOff=%d PthreadSize=%d",
			runtime.GOARCH, err, got.Specific1stBlock, got.KeyDataSize, got.KeyDataOff, got.PthreadSize)
	}

	for _, c := range []struct {
		name string
		got  uint64
		want uint32
	}{
		{"Specific1stBlock", got.Specific1stBlock, want.Specific1stBlock},
		{"KeyDataSize", got.KeyDataSize, want.KeyDataSize},
		{"KeyDataOff", got.KeyDataOff, want.KeyDataOff},
		{"PthreadSize", got.PthreadSize, want.PthreadSize},
	} {
		if c.got != uint64(c.want) {
			t.Errorf("%s: measured %#x (%d) on this machine, table says %#x (%d). "+
				"One of them is wrong, and the table is what the walk trusts.",
				c.name, c.got, c.got, c.want, c.want)
		}
	}
}
