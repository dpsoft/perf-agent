//go:build linux

package capture

import (
	"slices"
	"syscall"
	"testing"
	"time"
)

// clock is a fake now that advances by step on every call.
func clock(step time.Duration) func() time.Time {
	t := time.Unix(0, 0)
	return func() time.Time {
		t = t.Add(step)
		return t
	}
}

func TestSweepReportsSurvivors(t *testing.T) {
	var killed []int
	kill := func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGKILL {
			t.Errorf("signal %v, want SIGKILL", sig)
		}
		killed = append(killed, pid)
		return nil
	}
	got := sweep(func() []int { return []int{42} }, kill, func(int) bool { return true }, time.Second, clock(100*time.Millisecond))
	if !slices.Equal(got, []int{42}) {
		t.Errorf("survivors = %v, want [42]", got)
	}
	if len(killed) == 0 || slices.ContainsFunc(killed, func(p int) bool { return p != 42 }) {
		t.Errorf("killed %v, want only 42", killed)
	}
}

func TestSweepKillsDescendantsAsTheyAreAdopted(t *testing.T) {
	// 7 is a child; once it dies its own child 8 is adopted.
	dead := map[int]bool{}
	children := func() []int {
		if dead[7] {
			return []int{7, 8}
		}
		return []int{7}
	}
	kill := func(pid int, _ syscall.Signal) error {
		dead[pid] = true
		return nil
	}
	alive := func(pid int) bool { return !dead[pid] }
	if got := sweep(children, kill, alive, time.Second, clock(time.Millisecond)); got != nil {
		t.Errorf("survivors = %v, want none", got)
	}
	if !dead[7] || !dead[8] {
		t.Errorf("killed %v, want 7 and the adopted 8", dead)
	}
}

func TestSweepWithNothingLeftKillsNothing(t *testing.T) {
	kill := func(pid int, _ syscall.Signal) error {
		t.Errorf("killed %d", pid)
		return nil
	}
	alive := func(pid int) bool { return pid != 5 }
	if got := sweep(func() []int { return []int{5} }, kill, alive, 0, time.Now); got != nil {
		t.Errorf("survivors = %v", got)
	}
}
