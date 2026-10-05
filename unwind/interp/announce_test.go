package interp

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cilium/ebpf"
)

// refusingModule recognises every process and refuses it permanently, with a
// cause worth reading. The shape pyunwind has on an interpreter whose eval
// loop it cannot locate.
type refusingModule struct {
	err      error
	rng      Range
	ok       bool
	mu       sync.Mutex
	enrolled int
}

func (f *refusingModule) ID() uint32                  { return 2 }
func (f *refusingModule) Name() string                { return "python" }
func (f *refusingModule) ProgramName(Flavour) string  { return "" }
func (f *refusingModule) Bind(*ebpf.Collection) error { return nil }
func (f *refusingModule) Detach(uint32) error         { return nil }
func (f *refusingModule) Counters(bool) string        { return "" }
func (f *refusingModule) Close() error                { return nil }

func (f *refusingModule) Spec() (*ebpf.CollectionSpec, error) {
	return nil, errors.New("unused")
}

func (f *refusingModule) Enroll(uint32) (Range, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enrolled++
	return f.rng, f.ok, f.err
}

// An interpreter the agent cannot walk must be ANNOUNCED, not merely skipped.
//
// This is the half of #170 that outlived its first half. The symptom there was
// Fedora's stock python3 being declined, and the fix was to find the eval loop
// in .gnu_debugdata -- but the reason it took an issue to notice at all is
// that a decline costs the operator a profile with no Python frames and no
// statement about why. "Not a Python process", "Python but a build this agent
// refuses" and "attached, and the walk found nothing" are three very different
// things that look identical from the outside.
//
// Set.Enroll does emit that line. NOTHING PINNED IT: the only test that
// mentioned REFUSED asserted its ABSENCE on a healthy run
// (test/python_walk_test.go), so deleting the logf call left every test in the
// tree green. That is the same defect the rest of #183 was about, one level up.
func TestAPermanentRefusalIsAnnouncedWithItsCause(t *testing.T) {
	const pid = 4242
	cause := errors.New("cannot locate the interpreter's eval loop: totals 994 bytes across 1 fragments")
	f := &refusingModule{err: fmt.Errorf("/usr/lib64/libpython3.14.so.1.0: %w", cause), ok: true}
	s := &Set{stop: make(chan struct{}), entries: []*entry{{mod: f}}}
	defer func() { _ = s.Close() }()

	var lines []string
	found := s.Enroll(pid, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	if !found {
		t.Fatal("a recognised-but-refused process reported as unrecognised: the caller cannot tell it from a non-Python one")
	}
	if len(lines) == 0 {
		t.Fatal("a permanent refusal was silent; an operator sees a profile with no Python frames and no reason")
	}
	line := strings.Join(lines, "\n")

	// Each of these is something the operator needs in order to act, and the
	// line is useless if any is missing: which language, which process, and
	// what the actual obstacle was.
	for _, want := range []string{
		"python",   // which module declined
		"4242",     // which process
		"REFUSED",  // that it is a refusal, greppable
		"libpython3.14.so.1.0",
		"cannot locate the interpreter's eval loop",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the refusal line does not carry %q, so it cannot be acted on:\n%s", want, line)
		}
	}
}

// Recognised, claimed nothing, and the module already said why: the Set must
// NOT add a second line. Two lines for one refusal reads as two problems.
func TestAModuleThatReportedItsOwnRefusalIsNotLoggedTwice(t *testing.T) {
	f := &refusingModule{ok: true} // ok, no error, zero Range
	s := &Set{stop: make(chan struct{}), entries: []*entry{{mod: f}}}
	defer func() { _ = s.Close() }()

	var lines []string
	found := s.Enroll(99, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	})

	if !found {
		t.Fatal("a recognised process reported as unrecognised")
	}
	if len(lines) != 0 {
		t.Fatalf("the Set restated a refusal the module had already reported:\n%s", strings.Join(lines, "\n"))
	}
}

// Not an interpreter at all: no line, and not "found". The overwhelmingly
// common case -- every non-Python process on the machine -- and a line here
// would drown the ones that matter.
func TestAnUnrecognisedProcessIsSilent(t *testing.T) {
	f := &refusingModule{ok: false}
	s := &Set{stop: make(chan struct{}), entries: []*entry{{mod: f}}}
	defer func() { _ = s.Close() }()

	var lines []string
	if s.Enroll(7, func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}) {
		t.Fatal("an unrecognised process was reported as found")
	}
	if len(lines) != 0 {
		t.Fatalf("an unrecognised process produced output:\n%s", strings.Join(lines, "\n"))
	}
}
