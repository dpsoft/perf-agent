// Package python implements injection of CPython 3.12+'s perf trampoline
// (sys.activate_stack_trampoline) into running processes via ptrace, so that
// perf-agent can resolve Python JIT frames to qualnames without requiring the
// target to be launched with `python -X perf`.
package python

import "errors"

// Detection-result sentinels. All are non-fatal: detection returns one of
// these (wrapped) when a process should be skipped without aborting the run.
var (
	// ErrNotPython is returned when the target is not a CPython process.
	// Examples: a Go binary, a non-Python executable, or a process whose
	// libpython/exe lacks the interpreter symbols we need.
	ErrNotPython = errors.New("not a python process")

	// ErrPythonTooOld is returned when a Python process is detected but its
	// libpython SONAME indicates a version older than 3.12. The
	// sys.activate_stack_trampoline primitive does not exist on older versions.
	ErrPythonTooOld = errors.New("python version too old (need 3.12+)")

	// ErrNoPerfTrampoline is returned when libpython is 3.12+ but was compiled
	// without --enable-perf-trampoline, detected by absence of the
	// _PyPerf_Callbacks symbol in .dynsym/.symtab.
	ErrNoPerfTrampoline = errors.New("python built without --enable-perf-trampoline")

	// ErrStaticallyLinkedNoSymbols is returned when neither libpython nor
	// /proc/<pid>/exe exposes the libpython internal symbols needed for
	// remote calls (PyRun_SimpleString, PyGILState_Ensure, PyGILState_Release).
	ErrStaticallyLinkedNoSymbols = errors.New("python interpreter symbols not resolvable")

	// ErrPreexisting is recorded (not returned to callers) when /tmp/perf-<pid>.map
	// already exists at activation time, indicating the trampoline was activated
	// by a prior perf-agent run or by user code. We skip both activation and
	// deactivation in this case to avoid stomping on prior state.
	ErrPreexisting = errors.New("perf trampoline already active (preexisting marker)")
)
