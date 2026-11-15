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
	got, quiet := sweep(func() ([]int, bool) { return []int{42}, false }, kill, func(int) bool { return true }, time.Second, clock(100*time.Millisecond))
	if !slices.Equal(got, []int{42}) || quiet {
		t.Errorf("survivors = %v, quiet %v, want [42] and not quiet", got, quiet)
	}
	if len(killed) == 0 || slices.ContainsFunc(killed, func(p int) bool { return p != 42 }) {
		t.Errorf("killed %v, want only 42", killed)
	}
}

func TestSweepKillsDescendantsAsTheyAreAdopted(t *testing.T) {
	// 7 is a child; once it dies its own child 8 is adopted.
	dead := map[int]bool{}
	// Dead children stay listed until reaped; the last round lists none.
	rounds := 0
	children := func() ([]int, bool) {
		rounds++
		switch {
		case dead[7] && dead[8]:
			return nil, true
		case dead[7]:
			return []int{7, 8}, false
		}
		return []int{7}, false
	}
	kill := func(pid int, _ syscall.Signal) error {
		dead[pid] = true
		return nil
	}
	alive := func(pid int) bool { return !dead[pid] }
	if got, quiet := sweep(children, kill, alive, time.Second, clock(time.Millisecond)); got != nil || !quiet {
		t.Errorf("survivors = %v, quiet %v, want none and quiet", got, quiet)
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
	if got, quiet := sweep(func() ([]int, bool) { return nil, true }, kill, func(int) bool { return true }, 0, time.Now); got != nil || !quiet {
		t.Errorf("survivors = %v, quiet %v", got, quiet)
	}
}

// A listed zombie is not alive but keeps the sweep going: its orphans are
// adopted only as it exits, and it must be reaped first.
func TestSweepLoopsWhileAZombieIsListed(t *testing.T) {
	rounds := 0
	children := func() ([]int, bool) {
		rounds++
		if rounds < 3 {
			return []int{9}, false
		}
		return nil, true
	}
	got, quiet := sweep(children, func(int, syscall.Signal) error { return nil }, func(int) bool { return false }, time.Second, clock(time.Millisecond))
	if got != nil || !quiet || rounds != 3 {
		t.Errorf("survivors %v, quiet %v after %d rounds, want none, quiet, 3", got, quiet, rounds)
	}
}
