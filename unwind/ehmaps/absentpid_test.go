package ehmaps

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// absentPID returns a pid with no /proc entry, so a tracker handling a
// synthetic event for it reads no mappings and touches no table.
//
// Walking down from pid_max rather than trusting a big-looking constant: the
// highest pids are the least likely to be allocated, and a free one is
// confirmed here instead of assumed. See #191 for what assuming cost.
func absentPID(t *testing.T) uint32 {
	t.Helper()

	pidMax := 1 << 22
	if b, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 1 {
			pidMax = v
		}
	}
	for pid := pidMax - 1; pid > 1 && pid > pidMax-10000; pid-- {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
			return uint32(pid)
		}
	}
	t.Fatalf("every pid below pid_max=%d exists; cannot pick one with no /proc entry", pidMax)
	return 0
}

// The panic in #191, as a test: a store-less tracker asked to attach must
// report that it cannot, not dereference nil. Driven directly rather than
// through Run, because reproducing it via Run needed a pid that happened to
// exist -- which is exactly why it hid for so long.
func TestAttachWithoutATableStoreReportsRatherThanPanics(t *testing.T) {
	tracker := NewPIDTracker(nil, nil, nil)

	err := tracker.Attach(uint32(os.Getpid()), "/proc/self/exe", "/proc/self/exe")
	if err == nil {
		t.Fatal("a tracker with no table store reported success for an attach it cannot have done")
	}
	if !strings.Contains(err.Error(), "no table store") {
		t.Fatalf("error does not name the cause, so a reader cannot tell it from an I/O failure: %v", err)
	}
}
