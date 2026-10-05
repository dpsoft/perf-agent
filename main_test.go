package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// unresolvablePID returns a pid whose /proc/<pid>/comm cannot be read, so
// readProcessName must fall back to the "pid<N>" form these tests are about.
//
// It used to be the literal 1234. Whether a pid resolves to a process name is
// a property of the MACHINE, not of the code under test: on a GitHub arm64
// runner pid 1234 is dockerd, so the name came out "dockerd-202610032257-..."
// and the assertion failed the first time ./... ran the root package. It had
// never run in CI before (#183).
//
// Walking down from pid_max rather than trusting a big-looking constant: the
// highest pids are the least likely to be allocated, and a free one is
// confirmed here instead of assumed.
func unresolvablePID(t *testing.T) int {
	t.Helper()

	pidMax := 1 << 22
	if b, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v > 1 {
			pidMax = v
		}
	}
	for pid := pidMax - 1; pid > 1 && pid > pidMax-10000; pid-- {
		if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid)); err != nil {
			return pid
		}
	}
	t.Fatalf("every pid below pid_max=%d resolves to a process; cannot exercise the "+
		"unresolvable-pid fallback", pidMax)
	return 0
}

func TestFlamegraphPathFollowsTheExistingAutoNamingConvention(t *testing.T) {
	// --flamegraph-output auto must name its file exactly the way
	// --pmu-output auto and the default profile names do, so the three
	// artifacts of one run sort together.
	oldPID, oldAll := *flagPID, *flagAll
	t.Cleanup(func() { *flagPID, *flagAll = oldPID, oldAll })

	*flagPID, *flagAll = 0, true
	auto := flamegraphPath("auto", "on-cpu")
	assert.True(t, strings.HasSuffix(auto, "-on-cpu.html"), "got %q", auto)
	assert.Equal(t, generateOutputName(0, true, "on-cpu", "html"), auto)

	pid := unresolvablePID(t)
	*flagPID, *flagAll = pid, false
	perPID := flamegraphPath("auto", "off-cpu")
	assert.True(t, strings.HasPrefix(perPID, fmt.Sprintf("pid%d-", pid)), "got %q", perPID)
	assert.True(t, strings.HasSuffix(perPID, "-off-cpu.html"), "got %q", perPID)
}

func TestFlamegraphPathPassesAnExplicitPathThrough(t *testing.T) {
	assert.Equal(t, "/tmp/x.html", flamegraphPath("/tmp/x.html", "on-cpu"))
	assert.Equal(t, "", flamegraphPath("", "on-cpu"), "unset means no flame graph")
}

func TestGenerateOutputNameShapes(t *testing.T) {
	assert.Regexp(t, `^\d{12}-on-cpu\.html$`, generateOutputName(0, true, "on-cpu", "html"))
	pid := unresolvablePID(t)
	assert.Regexp(t,
		fmt.Sprintf(`^pid%d-\d{12}-off-cpu\.pb\.gz$`, pid),
		generateOutputName(pid, false, "off-cpu", "pb.gz"))
}
