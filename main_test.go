package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

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

	*flagPID, *flagAll = 999999, false
	perPID := flamegraphPath("auto", "off-cpu")
	assert.True(t, strings.HasPrefix(perPID, "pid999999-"), "got %q", perPID)
	assert.True(t, strings.HasSuffix(perPID, "-off-cpu.html"), "got %q", perPID)
}

func TestFlamegraphPathPassesAnExplicitPathThrough(t *testing.T) {
	assert.Equal(t, "/tmp/x.html", flamegraphPath("/tmp/x.html", "on-cpu"))
	assert.Equal(t, "", flamegraphPath("", "on-cpu"), "unset means no flame graph")
}

func TestGenerateOutputNameShapes(t *testing.T) {
	assert.Regexp(t, `^\d{12}-on-cpu\.html$`, generateOutputName(0, true, "on-cpu", "html"))
	assert.Regexp(t, `^pid1234-\d{12}-off-cpu\.pb\.gz$`, generateOutputName(1234, false, "off-cpu", "pb.gz"))
}
