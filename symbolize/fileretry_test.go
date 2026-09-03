package symbolize

import (
	"testing"

	blazesym "github.com/libbpf/blazesym/go"
)

// newRetryTestSymbolizer builds a LocalSymbolizer WITHOUT NewLocalSymbolizer's
// map_files capability check.
//
// That check guards the PROCESS source, which needs CAP_CHECKPOINT_RESTORE to
// dereference /proc/<pid>/map_files symlinks. The retry under test uses only
// the ELF source and needs no capability, so going through the constructor
// would make this skip on every unprivileged machine including CI, and a test
// that always skips asserts nothing.
func newRetryTestSymbolizer(t *testing.T) *LocalSymbolizer {
	t.Helper()
	bz, err := blazesym.NewSymbolizer()
	if err != nil {
		t.Fatalf("blazesym.NewSymbolizer: %v", err)
	}
	return &LocalSymbolizer{bz: bz}
}

// The retry must not invent work it cannot do. Each case lacks something that
// makes a virtual address computable, and a frame it cannot ask about has to
// come back untouched rather than mangled or renamed.
//
// NOTE ON COVERAGE: the positive path -- a frame the process source could not
// name being named from its module file -- is NOT unit-tested here. It needs a
// real ELF with ordinary .dynsym/.symtab plus a live mapping; a Go test binary
// keeps its symbols in .gopclntab, which the ELF source does not read, and
// reaching a C library's symbols needs dlsym, which this package's tests
// cannot use (cgo is not supported in _test.go here). Building the mapping
// arithmetic into the test instead would duplicate procmap.AddressMapper --
// the very code the retry depends on -- so a shared mistake would pass.
// That path is covered by measurement on a real capture: PyTorch libtorch
// frames went from 156 named / 306 module+offset to 421 named / 0, and the
// whole profile from 59% module+offset to 25%. See issue #125.
func TestRetrySkipsFramesItCannotAskAbout(t *testing.T) {
	s := newRetryTestSymbolizer(t)
	defer func() { _ = s.Close() }()

	cases := map[string]Frame{
		"already resolved":  {Address: 0x1000, Reason: FailureNone, Name: "known", Module: "/bin/sh", MapStart: 0x1000},
		"no module":         {Address: 0x1000, Reason: FailureMissingSymbols, MapStart: 0x1000},
		"no mapping":        {Address: 0x1000, Reason: FailureMissingSymbols, Module: "/bin/sh"},
		"sentinel [kernel]": {Address: 0x1000, Reason: FailureMissingSymbols, Module: "[kernel]", MapStart: 0x1000},
		"unreadable module": {Address: 0x1000, Reason: FailureMissingSymbols, Module: "/nonexistent/x.so", MapStart: 0x800},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			before := f
			frames := []Frame{f}
			s.retryAgainstModuleFiles(frames)
			got := frames[0]
			// Field by field: Frame carries an Inlined slice and is not
			// comparable with ==.
			if got.Name != before.Name || got.Reason != before.Reason ||
				got.Module != before.Module || got.Address != before.Address ||
				got.Offset != before.Offset {
				t.Fatalf("frame must be untouched, was %+v now %+v", before, got)
			}
		})
	}
}

// A module the retry cannot open must be counted as "could not look", not
// silently ignored: a run that recovers nothing because every file was
// unreadable must not read like one where the two sources simply agreed.
func TestUnreadableModuleIsCountedAsAFailedLook(t *testing.T) {
	s := newRetryTestSymbolizer(t)
	defer func() { _ = s.Close() }()

	frames := []Frame{{
		Address: 0x1000, Reason: FailureMissingSymbols,
		Module: "/nonexistent/does-not-exist.so", MapStart: 0x800,
	}}
	s.retryAgainstModuleFiles(frames)

	if got := s.Stats().FileRetryFailed; got != 1 {
		t.Fatalf("FileRetryFailed = %d, want 1", got)
	}
	if got := s.Stats().FileRetryNamed; got != 0 {
		t.Fatalf("FileRetryNamed = %d, want 0", got)
	}
}
