package profile

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/cilium/ebpf"
)

// The interpreter handoff is gated at LOAD time, and this is the code that
// decides.
//
// bpf/unwind_common.h's `interp_enabled` is a `const volatile bool`, so the
// verifier sees a constant and prunes the whole handoff when it is false --
// the lookup, both comparisons and the early-return branch. It exists because
// asking the question at all costs real verifier budget, and on Linux 6.19.4+
// an earlier shape of it pushed perf_dwarf, offcpu_dwarf and gpu_usdt past the
// 1M processed-instruction ceiling, which took out --unwind auto, the DEFAULT
// profiling mode, on those kernels while CI (older) stayed green.
//
// The policy here is: ask for the handoff, and if the verifier refuses the
// program, load it again without it rather than leaving the user with no
// profiler at all. A degraded capture that says so is worth more than a dead
// one; a silent degraded capture is worth less than nothing, which is why
// the outcome is recorded and reported rather than only logged once at
// startup.

// interpGate records what the last load decided. Package-level because the
// decision is a property of this process's kernel, not of one profiler
// instance, and because the reporting path (dwarfagent's shutdown counter
// line) is nowhere near the loading path.
var interpGate struct {
	sync.Mutex
	attempted bool
	enabled   bool
	reason    string
}

// InterpState reports whether the interpreter arm survived the load, and
// why not if it did not. attempted is false before any DWARF program has been
// loaded in this process.
func InterpState() (attempted, enabled bool, reason string) {
	interpGate.Lock()
	defer interpGate.Unlock()
	return interpGate.attempted, interpGate.enabled, interpGate.reason
}

func setInterpGate(enabled bool, reason string) {
	interpGate.Lock()
	defer interpGate.Unlock()
	interpGate.attempted = true
	interpGate.enabled = enabled
	interpGate.reason = reason
}

// loadWithInterpGate loads a collection twice at most: once with the
// interpreter arm on, and -- only if the VERIFIER rejected it -- once with
// the arm off.
//
// newSpec returns a fresh spec each call because LoadAndAssign consumes it;
// assign receives the loaded objects.
//
// It retries on any *ebpf.VerifierError rather than string-matching the
// complexity message. A verifier rejection the handoff causes is a reason to
// drop it whatever its wording, and the wording is preserved in the
// reason string either way, so nothing is hidden by being generous here.
func loadWithInterpGate(name string, newSpec func() (*ebpf.CollectionSpec, error), assign func(*ebpf.CollectionSpec) error) error {
	spec, err := newSpec()
	if err != nil {
		return err
	}
	if err := spec.Variables["interp_enabled"].Set(true); err != nil {
		return fmt.Errorf("set interp_enabled: %w", err)
	}
	firstErr := assign(spec)
	if firstErr == nil {
		setInterpGate(true, "")
		return nil
	}

	var verr *ebpf.VerifierError
	if !errors.As(firstErr, &verr) {
		return firstErr
	}

	retry, err := newSpec()
	if err != nil {
		return firstErr
	}
	if err := retry.Variables["interp_enabled"].Set(false); err != nil {
		return firstErr
	}
	if err := assign(retry); err != nil {
		// Both attempts failed: the arm was not the problem. Report the
		// ORIGINAL error, which is the one describing the program the user
		// asked for.
		return firstErr
	}

	reason := summariseVerifierError(firstErr)
	setInterpGate(false, reason)
	log.Printf("%s: INTERPRETER FRAMES DISABLED on this kernel: the verifier rejected the program with the "+
		"handoff compiled in, so it was loaded without it. Native stacks are unaffected; frames from any "+
		"language runtime will be absent from every profile in this run. Verifier said: %s",
		name, reason)
	return nil
}

// summariseVerifierError reduces a verifier log to one line worth putting in
// a log and in the shutdown report. The full log is enormous and its last
// lines are the summary ("processed N insns ..."), which is the part that
// says whether this was the complexity ceiling or something else.
func summariseVerifierError(err error) string {
	s := err.Error()
	if i := strings.Index(s, "BPF program is too large"); i >= 0 {
		return strings.TrimSpace(s[i : strings.IndexByte(s[i:], '\n')+i+1])
	}
	const max = 200
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
