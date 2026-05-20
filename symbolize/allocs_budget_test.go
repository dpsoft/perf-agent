package symbolize

import (
	"testing"
)

// TestAllocsBudget_ParseKallsymsLine asserts the per-line parser
// remains allocation-free. Catches PRs that accidentally
// reintroduce strings.Fields / strconv.ParseUint or other
// allocating helpers into the hot path (the regression that
// dogfood iter 5 surfaced via mallocgc / sweepone in user-side
// profiles).
//
// Budget: 0 allocs per call. Anything > 0 indicates a regression.
func TestAllocsBudget_ParseKallsymsLine(t *testing.T) {
	const budget = 0
	line := []byte("ffffffffc0001234 T kvm_vcpu_ioctl	[kvm]")
	got := testing.AllocsPerRun(1000, func() {
		_, _, _, _, _ = parseKallsymsLine(line)
	})
	if got > float64(budget) {
		t.Errorf("parseKallsymsLine allocs/op = %.2f, want <= %d", got, budget)
	}
}

// TestAllocsBudget_ResolveKernelIPs caps the per-Resolve-call
// allocation count. The current implementation allocates exactly
// one []Frame slice per call (the return value), so the budget
// is 1. Going above means someone added per-IP allocation —
// likely a [Name string conversion inside the hot loop instead
// of using pre-interned strings.
func TestAllocsBudget_ResolveKernelIPs(t *testing.T) {
	const budget = 1
	k := &kallsymsSymbolizer{
		addrs:   []uint64{0xffffffff80001000, 0xffffffff80002000, 0xffffffff80003000},
		names:   []string{"sym_a", "sym_b", "sym_c"},
		modules: []string{"", "", ""},
	}
	ips := []uint64{0xffffffff80001042, 0xffffffff80002042, 0xffffffff80003042}
	got := testing.AllocsPerRun(1000, func() {
		_ = k.Resolve(ips)
	})
	if got > float64(budget) {
		t.Errorf("Resolve allocs/op = %.2f, want <= %d", got, budget)
	}
}

// TestAllocsBudget_LatencyHistRecord caps the per-Record cost
// of the histogram on the hot path. Record runs under a mutex
// but should never allocate — the ring buffer is fixed-size.
func TestAllocsBudget_LatencyHistRecord(t *testing.T) {
	const budget = 0
	var h LatencyHist
	got := testing.AllocsPerRun(1000, func() {
		h.Record(42)
	})
	if got > float64(budget) {
		t.Errorf("LatencyHist.Record allocs/op = %.2f, want <= %d", got, budget)
	}
}
