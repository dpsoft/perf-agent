package main

import (
	"flag"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// The guard this whole classification exists for.
//
// Attach mode's contract is that a flag which cannot take effect is refused
// rather than ignored, and that contract is only as good as the coverage of
// the table behind it. A flag added to defineFlags and classified nowhere
// would fall through refusedLaunchFlags and be accepted in attach mode while
// doing nothing at all -- which is precisely the failure the refusal exists
// to prevent, reintroduced by omission rather than by decision.
//
// So the test is over the REGISTERED set, not over a list written here: it
// enumerates what defineFlags actually installed and fails until every name
// has been put on one side or the other. Adding a flag without thinking about
// attach mode is not possible; adding one and deciding it is attach-safe
// takes one line.
func TestEveryFlagIsClassifiedForAttachMode(t *testing.T) {
	fs := flag.NewFlagSet("gpu-cuda-profile", flag.ContinueOnError)
	defineFlags(fs)

	var unclassified, both []string
	registered := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		registered[f.Name] = true
		_, launch := launchOnlyInAttachMode[f.Name]
		safe := attachSafeFlags[f.Name]
		switch {
		case launch && safe:
			both = append(both, f.Name)
		case !launch && !safe:
			unclassified = append(unclassified, f.Name)
		}
	})
	if len(unclassified) > 0 {
		t.Errorf("these flags are registered but classified neither launch-only nor "+
			"attach-safe, so -pid would accept them and they would do nothing: %s",
			strings.Join(unclassified, ", "))
	}
	if len(both) > 0 {
		t.Errorf("these flags are in both tables, which cannot be true of any flag: %s",
			strings.Join(both, ", "))
	}
	// The tables must not outlive the flags they describe either: a name left
	// behind after a flag is removed makes the classification look more
	// complete than it is.
	for name := range launchOnlyInAttachMode {
		if !registered[name] {
			t.Errorf("launchOnlyInAttachMode names %q, which defineFlags does not register", name)
		}
	}
	for name := range attachSafeFlags {
		if !registered[name] {
			t.Errorf("attachSafeFlags names %q, which defineFlags does not register", name)
		}
	}
}

// A reason that does not say why is a refusal the operator cannot act on.
func TestEveryRefusalExplainsItself(t *testing.T) {
	for name, why := range launchOnlyInAttachMode {
		if len(why) < 20 {
			t.Errorf("-%s is refused with %q, which does not tell the operator what to do "+
				"instead", name, why)
		}
	}
}

func TestOnlyTheFlagsActuallySetAreRefused(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	defineFlags(fs)
	if err := fs.Parse([]string{"-pid", "1234", "-period", "4", "-out", "x.pb.gz"}); err != nil {
		t.Fatal(err)
	}
	got := refusedLaunchFlags(fs)
	if len(got) != 1 || !strings.HasPrefix(got[0], "-period:") {
		t.Fatalf("want exactly -period refused, got %v", got)
	}
	// -iters and -linger-ms are launch-only and have non-zero DEFAULTS. If
	// refusal keyed on the value rather than on the flag having been set,
	// every attach run would be refused for flags nobody typed.
	for _, r := range got {
		if strings.HasPrefix(r, "-iters") || strings.HasPrefix(r, "-linger-ms") {
			t.Fatalf("a launch-only flag was refused on its default value: %q", r)
		}
	}
}

// waitForShimIn must distinguish "I looked and it is not there" from "I could
// not look", because they send the reader to completely different places: the
// first to how the target was started, the second to their own command line
// or to their capabilities. A single generic failure would send everyone to
// the driver.
func TestWaitForShimInSeparatesNotMappedFromCannotLook(t *testing.T) {
	t.Run("a process that maps the file", func(t *testing.T) {
		// Whatever libc this test binary is linked against is mapped into
		// this very process by definition, so this is the positive case with
		// no GPU, no shim and no capabilities involved.
		libc := findMappedLibrary(t)
		if err := waitForShimIn(os.Getpid(), libc, time.Second); err != nil {
			t.Fatalf("self maps %s but waitForShimIn says: %v", libc, err)
		}
	})

	t.Run("a process that does not map the file", func(t *testing.T) {
		err := waitForShimIn(os.Getpid(), "/bin/true", 50*time.Millisecond)
		if err == nil {
			t.Fatal("want an error: this process does not map /bin/true")
		}
		// The message must name the mechanism, or the operator has no way to
		// know that restarting the target is the fix and re-running this
		// command is not.
		if !strings.Contains(err.Error(), "cuInit") {
			t.Errorf("the refusal does not explain that injection happens at cuInit "+
				"and cannot be added later: %v", err)
		}
	})

	t.Run("a pid that cannot be read", func(t *testing.T) {
		err := waitForShimIn(1<<21, "/bin/true", 50*time.Millisecond)
		if err == nil {
			t.Fatal("want an error for a pid that does not exist")
		}
		if !strings.Contains(err.Error(), "could not read") {
			t.Errorf("a pid that cannot be inspected is reported as though it had been "+
				"inspected and found wanting: %v", err)
		}
	})
}

func findMappedLibrary(t *testing.T) string {
	t.Helper()
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Skipf("no /proc/self/maps: %v", err)
	}
	for _, line := range strings.Split(string(maps), "\n") {
		i := strings.Index(line, " /")
		if i < 0 {
			continue
		}
		path := strings.TrimSpace(line[i+1:])
		if strings.Contains(path, ".so") && !strings.Contains(path, "(deleted)") {
			return path
		}
	}
	t.Skip("this binary maps no shared library to test against")
	return ""
}

// The launch cache's counters are cumulative for the life of the run --
// Snapshot drains the timeline's rings but not the cache -- so an eviction
// total is re-reported, larger, in every subsequent snapshot. Collapsing a
// line to its kind is what stops one ongoing condition from being announced
// as a fresh anomaly on every drain interval.
func TestAnOngoingConditionIsOneAnomalyAndNotOnePerInterval(t *testing.T) {
	kind := func(s string) string { return anomalyDigits.ReplaceAllString(s, "#") }

	// The exact lines a 20s attach produced on the 3090, four intervals apart.
	a := "gpu join ANOMALY: launch cache evicted 10198 launches at capacity (65536 live) — too small for the launch rate"
	b := "gpu join ANOMALY: launch cache evicted 45034 launches at capacity (65536 live) — too small for the launch rate"
	if kind(a) != kind(b) {
		t.Errorf("the same growing condition reads as two different anomalies:\n  %q\n  %q",
			kind(a), kind(b))
	}

	// And it must not over-collapse: two genuinely different findings that
	// happen to differ only in their numbers are still different findings,
	// but two different SENTENCES must stay apart.
	c := "gpu join ANOMALY: 353 of 65536 executions unmatched — GPU time arrived with no launch"
	if kind(a) == kind(c) {
		t.Errorf("two unrelated anomalies collapsed to one kind: %q", kind(a))
	}
}
