package test

import "testing"

// TestCanRunBPFIgnoresProcessCapsForExecdBinaries pins the rule that a child
// process does not inherit the test process's capabilities.
//
// The row that matters is "capped test process, uncapped target": the gate
// used to answer true there, so tests that exec an uncapped binary ran and
// failed instead of skipping, and the failure pointed at the feature rather
// than at the missing setcap.
func TestCanRunBPFIgnoresProcessCapsForExecdBinaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    bpfEvidence
		want bool
	}{
		{"root can do anything", bpfEvidence{root: true}, true},
		{"root, exec'd binary uncapped", bpfEvidence{root: true, execsBinary: true}, true},

		{"in-process, this process capped", bpfEvidence{procHasBPF: true}, true},
		{"in-process, this process uncapped", bpfEvidence{}, false},

		{"exec'd binary capped", bpfEvidence{execsBinary: true, targetHasBPF: true}, true},
		{"exec'd binary uncapped", bpfEvidence{execsBinary: true}, false},

		// The regression.
		{"capped process, uncapped exec'd binary",
			bpfEvidence{procHasBPF: true, execsBinary: true}, false},
		// And its mirror: file caps do not help an in-process load.
		{"uncapped process, capped binary but loading in-process",
			bpfEvidence{targetHasBPF: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canRunBPF(tc.e); got != tc.want {
				t.Fatalf("canRunBPF(%+v) = %v, want %v", tc.e, got, tc.want)
			}
		})
	}
}
