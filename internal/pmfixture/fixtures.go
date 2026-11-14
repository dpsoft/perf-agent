package pmfixture

import (
	"fmt"
	"testing"

	"github.com/dpsoft/perf-agent/gpu/execlog"
)

func cpu(samples ...Sample) []byte { return Build("cpu", "nanoseconds", nil, samples...) }

// frames builds a root-first stack of plain named frames.
func frames(names ...string) []Frame {
	out := make([]Frame, len(names))
	for i, n := range names {
		out[i] = Fn(n)
	}
	return out
}

const appModule = "/opt/app/bin/app"

func appFrames(buildID string, names ...string) []Frame {
	lines := map[string]Line{
		"main": L("main", "/build/app/src/main.c", 10),
		"a":    L("a", "/build/app/src/app.c", 20),
		"b":    L("b", "/build/app/src/app.c", 30),
		"c":    L("c", "/build/app/src/util.c", 40),
		"d":    L("d", "/build/app/src/util.c", 50),
	}
	addr := map[string]uint64{"main": 0x10, "a": 0x20, "b": 0x30, "c": 0x40, "d": 0x50}
	out := make([]Frame, len(names))
	for i, n := range names {
		out[i] = In(appModule, buildID, addr[n], lines[n])
	}
	return out
}

// CPUBasic is cpu/nanoseconds in module /opt/app/bin/app, build id b1:
// main;a;b = 411, main;a = 100, main;c = 489. The total is 1000; a's
// cumulative is 511, b's 411 (0.411 of the total, 411/511 = "0.804" of its
// parent). Lines: main main.c:10, a app.c:20, b app.c:30, c util.c:40, all
// under /build/app/src.
func CPUBasic() []byte {
	return cpu(
		Sample{Frames: appFrames("b1", "main", "a", "b"), Value: 411},
		Sample{Frames: appFrames("b1", "main", "a"), Value: 100},
		Sample{Frames: appFrames("b1", "main", "c"), Value: 489},
	)
}

// CPUBasicNew is CPUBasic's next build (build id b2): main;a;b = 300,
// main;a = 100, main;c = 489, main;d = 111. The total is 1000.
func CPUBasicNew() []byte {
	return cpu(
		Sample{Frames: appFrames("b2", "main", "a", "b"), Value: 300},
		Sample{Frames: appFrames("b2", "main", "a"), Value: 100},
		Sample{Frames: appFrames("b2", "main", "c"), Value: 489},
		Sample{Frames: appFrames("b2", "main", "d"), Value: 111},
	)
}

const trainModule = "/opt/app/lib/libtrain.so"

// GPUBasic is gpu/nanoseconds with comment perf-agent.capture_id=cap-1 and
// gpu_sample_period=8 on every sample. train and conv are in
// /opt/app/lib/libtrain.so (build id t1); the synthetic frames sit on
// Mapping[0]. Samples in order:
//
//	train;conv;[gpu:launch];[gpu:kernel:k1] = 300 (gpu_join=exact, gpu_serialized=false,
//	  gpu_src_status=resolved, gpu_src_file=conv.cu, gpu_src_line=42, gpu_src_func=k1)
//	train;conv;[gpu:launch];[gpu:kernel:k2] = 100 (gpu_join=heuristic, gpu_serialized=true)
//	[gpu:launch unsampled];[gpu:kernel:k1] = 100 (gpu_join=exact, gpu_serialized=false)
//	train;conv;[gpu:launch];[gpu:kernel:k3] = 0 (gpu_serialized=false)
//
// The total is 500; conv's cumulative is 400 and the unsampled portion 100.
func GPUBasic() []byte {
	host := []Frame{
		In(trainModule, "t1", 0x100, L("train", "", 0)),
		In(trainModule, "t1", 0x200, L("conv", "", 0)),
	}
	stack := func(tail ...string) []Frame {
		return append(append([]Frame(nil), host...), frames(tail...)...)
	}
	lab := func(kv ...string) map[string][]string {
		m := map[string][]string{"gpu_sample_period": {"8"}}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = []string{kv[i+1]}
		}
		return m
	}
	return Build("gpu", "nanoseconds", []string{"perf-agent.capture_id=cap-1"},
		Sample{Frames: stack("[gpu:launch]", "[gpu:kernel:k1]"), Value: 300,
			Labels: lab("gpu_join", "exact", "gpu_serialized", "false", "gpu_src_status", "resolved",
				"gpu_src_file", "conv.cu", "gpu_src_line", "42", "gpu_src_func", "k1")},
		Sample{Frames: stack("[gpu:launch]", "[gpu:kernel:k2]"), Value: 100,
			Labels: lab("gpu_join", "heuristic", "gpu_serialized", "true")},
		Sample{Frames: frames("[gpu:launch unsampled]", "[gpu:kernel:k1]"), Value: 100,
			Labels: lab("gpu_join", "exact", "gpu_serialized", "false")},
		Sample{Frames: stack("[gpu:launch]", "[gpu:kernel:k3]"), Value: 0,
			Labels: lab("gpu_serialized", "false")},
	)
}

// GPUBasicExeclog is the executions log of GPUBasic: sample 0 carries
// executions 0 (200) and 1 (100), sample 1 execution 2 (100), sample 2
// execution 3 (100) and sample 3 execution 4 (0). conv has 4 executions
// below it and 3 kernel names observed.
func GPUBasicExeclog(tb testing.TB) []byte {
	tb.Helper()
	return Execlog(tb, GPUBasic(), execlog.Header{CaptureID: "cap-1", Backend: "cupti", SampleType: "gpu/nanoseconds"}, []execlog.Row{
		{Sample: 0, Execution: 0, Value: 200},
		{Sample: 0, Execution: 1, Value: 100},
		{Sample: 1, Execution: 2, Value: 100},
		{Sample: 2, Execution: 3, Value: 100},
		{Sample: 3, Execution: 4, Value: 0},
	})
}

// HugeValue is cpu/nanoseconds with main;big = 9007199254740993 (2^53+1).
func HugeValue() []byte {
	return cpu(Sample{Frames: frames("main", "big"), Value: 9007199254740993})
}

// Recursive is cpu/nanoseconds with a;b;a;b = 10 and a = 5.
func Recursive() []byte {
	return cpu(
		Sample{Frames: frames("a", "b", "a", "b"), Value: 10},
		Sample{Frames: frames("a"), Value: 5},
	)
}

func deepFrames(n int) []Frame {
	out := make([]Frame, n)
	for i := range n {
		out[i] = In("/opt/deep", "", uint64(i+1), L(fmt.Sprintf("f%d", i), "", 0))
	}
	return out
}

func deepWith(buildID string, n int, extra bool) []byte {
	chain := deepFrames(n)
	for i := range chain {
		chain[i].BuildID = buildID
	}
	s := []Sample{{Frames: chain, Value: 1}}
	if extra {
		leaf := In("/opt/deep", buildID, uint64(n+1), L("g", "", 0))
		s = append(s, Sample{Frames: append(append([]Frame(nil), chain...), leaf), Value: 1})
	}
	return cpu(s...)
}

// Deep is one chain f0;...;f(n-1) of value 1 in module /opt/deep, build id d1.
func Deep(n int) []byte { return deepWith("d1", n, false) }

// DeepNew is Deep(n) at build id d2 plus a leaf g of value 1 under f(n-1).
func DeepNew(n int) []byte { return deepWith("d2", n, true) }

// SameNameTwoModules is main;foo in /lib/libx.so (= 7) and main;foo in
// /lib/liby.so (= 3).
func SameNameTwoModules() []byte {
	return cpu(
		Sample{Frames: []Frame{Fn("main"), In("/lib/libx.so", "x", 0x10, L("foo", "", 0))}, Value: 7},
		Sample{Frames: []Frame{Fn("main"), In("/lib/liby.so", "y", 0x10, L("foo", "", 0))}, Value: 3},
	)
}
