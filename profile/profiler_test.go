package profile

import (
	"testing"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

func TestProfilerHasResolver(t *testing.T) {
	// Compile-time check: Profiler has a resolver field with the
	// expected procmap.Resolver type. Behavioral tests live in the
	// integration suite. The blazesym→Frame translation is now tested
	// in symbolize/{local,toprof}_test.go.
	var p Profiler
	_ = (*procmap.Resolver)(p.resolver)
}
