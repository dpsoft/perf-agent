package dwarfagent

import (
	"log"
	"time"
)

// Hooks is an optional observation surface for the dwarf-mode profilers
// (Profiler and OffCPUProfiler). All fields may be nil; the profiler
// nil-checks each before invoking. Hooks must not panic — if they do,
// the call site recovers and logs at debug level. Hooks are observers,
// not gatekeepers; they cannot fail or alter the operation.
type Hooks struct {
	// OnCompile fires after each successful CFI table compile in
	// TableStore.AcquireBinary. Path is the binary or shared library
	// path; buildID may be empty if the ELF lacks a NT_GNU_BUILD_ID
	// note. ehFrameBytes is the raw .eh_frame section size in bytes.
	// dur is the wall-clock duration of the ehcompile.Compile call.
	OnCompile func(path, buildID string, ehFrameBytes int, dur time.Duration)
}

// onCompileFunc returns a non-nil callback safe to invoke from anywhere.
// If h or h.OnCompile is nil, returns a no-op. The returned function
// recovers from panics inside the user-supplied OnCompile and logs them
// (observers must not break operations).
func (h *Hooks) onCompileFunc() func(path, buildID string, ehFrameBytes int, dur time.Duration) {
	if h == nil || h.OnCompile == nil {
		return func(string, string, int, time.Duration) {}
	}
	user := h.OnCompile
	return func(path, buildID string, ehFrameBytes int, dur time.Duration) {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("dwarfagent: Hooks.OnCompile panic recovered: %v (binary=%s)", r, path)
			}
		}()
		user(path, buildID, ehFrameBytes, dur)
	}
}
