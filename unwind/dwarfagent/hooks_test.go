package dwarfagent

import (
	"testing"
	"time"
)

func TestHooksNilSafe(t *testing.T) {
	var h *Hooks
	cb := h.onCompileFunc()
	cb("p", "b", 0, 0) // must not panic on nil receiver
}

func TestHooksNilFieldSafe(t *testing.T) {
	h := &Hooks{}
	cb := h.onCompileFunc()
	cb("p", "b", 0, 0) // must not panic when OnCompile is nil
}

func TestHooksCallbackFires(t *testing.T) {
	var got struct {
		path    string
		buildID string
		bytes   int
		dur     time.Duration
		fired   bool
	}
	h := &Hooks{
		OnCompile: func(path, buildID string, ehFrameBytes int, dur time.Duration) {
			got.path, got.buildID, got.bytes, got.dur, got.fired =
				path, buildID, ehFrameBytes, dur, true
		},
	}
	h.onCompileFunc()("/bin/foo", "abc", 1234, 5*time.Millisecond)
	if !got.fired {
		t.Fatal("OnCompile did not fire")
	}
	if got.path != "/bin/foo" || got.buildID != "abc" || got.bytes != 1234 || got.dur != 5*time.Millisecond {
		t.Errorf("got = %+v", got)
	}
}

func TestHooksRecoversFromPanic(t *testing.T) {
	h := &Hooks{
		OnCompile: func(string, string, int, time.Duration) {
			panic("boom")
		},
	}
	// Must not propagate the panic.
	h.onCompileFunc()("p", "b", 0, 0)
}
