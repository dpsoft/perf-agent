package python

// activatePayload is the C string written into the target process's address
// space. PyRun_SimpleString reads it as a NUL-terminated cstring.
//
// We import sys explicitly each time rather than relying on it already being
// imported, because PyRun_SimpleString runs in a fresh "main" namespace.
var activatePayload = []byte("import sys; sys.activate_stack_trampoline('perf')\x00")

var deactivatePayload = []byte("import sys; sys.deactivate_stack_trampoline()\x00")

// ActivatePayload returns the byte slice (NUL-terminated) to write into the
// target's address space before calling PyRun_SimpleString. Caller must NOT
// mutate the returned slice.
func ActivatePayload() []byte { return activatePayload }

// DeactivatePayload returns the byte slice (NUL-terminated) for the
// shutdown deactivation call. Caller must NOT mutate the returned slice.
func DeactivatePayload() []byte { return deactivatePayload }
