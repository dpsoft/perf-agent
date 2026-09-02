package test

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The end-to-end gate's teeth live in one line of YAML, and this test is
// what stops that line from being deleted quietly.
//
// PERF_AGENT_REQUIRE_PYTHON_WALK is what turns a decline into a failure.
// Without it every skip path in TestPythonFramesInterleavedWithNative goes
// back to a green skip -- AND the TestMain audit still passes, because
// declinePythonGate records an outcome. That is precisely the false green
// this branch already produced once (commit bcd4185d went green with the
// gate skipped), reachable again by removing a single workflow line that no
// Go test would notice.
//
// Everything else on this branch pins its cross-file contracts by reading
// the other file: walk_source_test.go reads bpf/*.c, counters_test.go reads
// python_walk.h. This does the same for the one contract that lives in CI
// configuration.
const integrationWorkflow = "../.github/workflows/tests.yml"

// runStepRe finds the start of the step by name; nextStepRe finds the start
// of the following step, so the body can be bounded without a YAML parser
// (this module has no YAML dependency, and adding one to read four lines
// would be its own kind of cost).
var (
	runStepRe  = regexp.MustCompile(`(?m)^\s+- name: Run integration tests\s*$`)
	nextStepRe = regexp.MustCompile(`(?m)^\s+- name: `)
	goTestRe   = regexp.MustCompile(`go\b[^\n]*\btest\b`)
	runFlagRe  = regexp.MustCompile(`(^|\s)--?run(\s|=)`)
)

// requireWalkValue extracts what the step assigns to requireWalkEnv.
//
// It reads the Go constant rather than a literal, so renaming the constant
// without updating the workflow fails here instead of silently disarming
// the gate. The second return distinguishes "not assigned at all" from
// "assigned the empty string" -- the Go side treats those identically, but
// this test must not, or a deleted line and an empty value would produce
// the same message.
func requireWalkValue(body string) (value string, assigned bool) {
	i := strings.Index(body, requireWalkEnv+"=")
	if i < 0 {
		return "", false
	}
	rest := body[i+len(requireWalkEnv)+1:]
	if rest == "" {
		return "", true
	}
	if q := rest[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(rest[1:], q); end >= 0 {
			return rest[1 : 1+end], true
		}
		return rest, true
	}
	if end := strings.IndexAny(rest, " \t\n\\"); end >= 0 {
		return rest[:end], true
	}
	return rest, true
}

// integrationTestStep returns the body of the workflow step that runs this
// module's tests.
func integrationTestStep(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(integrationWorkflow)
	if err != nil {
		// Not a skip. The workflow is checked in; if it cannot be read from
		// here, the thing this test pins is unpinned.
		t.Fatalf("cannot read %s: %v", integrationWorkflow, err)
	}
	src := string(raw)
	loc := runStepRe.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("%s has no %q step; the pin below cannot be checked", integrationWorkflow, "Run integration tests")
	}
	body := src[loc[1]:]
	if next := nextStepRe.FindStringIndex(body); next != nil {
		body = body[:next[0]]
	}
	return body
}

// TestRequireWalkValueReadsAnAssignment exercises the parser above against
// the forms a workflow can take, including the two that must NOT read as a
// live setting. Without it, a parser that returned ("", false) for
// everything would make the pin below pass on a workflow that sets nothing.
func TestRequireWalkValueReadsAnAssignment(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantValue string
		wantSet   bool
	}{
		{"absent", "sudo env FOO=1 go test ./...", "", false},
		{"bare", requireWalkEnv + "=1 go test", "1", true},
		{"double quoted", requireWalkEnv + `="1" \`, "1", true},
		{"single quoted", requireWalkEnv + `='1'`, "1", true},
		{"empty", requireWalkEnv + `="" \`, "", true},
		{"matrix expression", requireWalkEnv + `="${{ matrix.arch == 'amd64' && '1' || '' }}" \`,
			"${{ matrix.arch == 'amd64' && '1' || '' }}", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, set := requireWalkValue(tc.body)
			if set != tc.wantSet || got != tc.wantValue {
				t.Fatalf("got (%q,%v), want (%q,%v)", got, set, tc.wantValue, tc.wantSet)
			}
		})
	}
}

// TestIntegrationWorkflowRequiresThePythonWalk asserts CI sets the variable
// that makes a decline fatal, and that it is non-empty on amd64.
//
// The Go side reads os.Getenv(requireWalkEnv) != "", so an assignment to an
// empty string is the same as no assignment at all -- which is why the
// value is checked and not just the name.
func TestIntegrationWorkflowRequiresThePythonWalk(t *testing.T) {
	body := integrationTestStep(t)

	value, assigned := requireWalkValue(body)
	if !assigned {
		t.Fatalf("the integration step does not set %s.\n"+
			"Without it every decline in TestPythonFramesInterleavedWithNative is a green skip and the branch's\n"+
			"only end-to-end evidence vanishes silently. Step body:\n%s", requireWalkEnv, body)
	}
	if strings.Contains(value, "${{") {
		// A matrix expression. It must be able to yield something non-empty
		// on amd64, which means naming amd64 and carrying a non-empty
		// literal to yield.
		if !strings.Contains(value, "amd64") {
			t.Fatalf("%s is set from an expression that never mentions amd64: %q", requireWalkEnv, value)
		}
		if !regexp.MustCompile(`'[^']+'`).MatchString(value) {
			t.Fatalf("%s's expression has no non-empty literal, so it can only ever be unset: %q", requireWalkEnv, value)
		}
		return
	}
	if value == "" {
		t.Fatalf("%s is set to the empty string, which the Go side treats as unset", requireWalkEnv)
	}
}

// TestIntegrationWorkflowRunsEveryTest asserts CI's invocation carries no
// -run filter.
//
// TestMain's audit stands down when -run is narrowing the selection (a
// developer running one unrelated test has not disarmed anything). That
// exemption is safe only while CI runs unfiltered: `go test -run
// TestProfileMode ./...` would be green with the gate never executing and
// nothing saying so.
func TestIntegrationWorkflowRunsEveryTest(t *testing.T) {
	body := integrationTestStep(t)
	if !goTestRe.MatchString(body) {
		t.Fatalf("the integration step does not appear to invoke `go test`:\n%s", body)
	}
	if runFlagRe.MatchString(body) {
		t.Fatalf("the integration step passes a -run filter, which disarms TestMain's audit:\n%s", body)
	}
}
