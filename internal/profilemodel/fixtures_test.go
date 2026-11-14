package profilemodel

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
	"github.com/dpsoft/perf-agent/internal/capturemeta"
)

// fx is one synthetic sample: frames root first, a value, and labels.
type fx struct {
	stack  []string
	value  int64
	labels map[string][]string
}

// build serializes a single-sample-type profile whose frames all sit on
// Mapping[0] with an empty file, as the GPU and fallback frames do.
func build(typ, unit, captureID string, samples ...fx) []byte {
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: typ, Unit: unit}},
	}
	m := &profile.Mapping{ID: 1}
	p.Mapping = []*profile.Mapping{m}
	if captureID != "" {
		p.Comments = []string{capturemeta.CaptureIDComment + captureID}
	}
	locs := map[string]*profile.Location{}
	loc := func(name string) *profile.Location {
		if l, ok := locs[name]; ok {
			return l
		}
		f := &profile.Function{ID: uint64(len(p.Function) + 1), Name: name}
		p.Function = append(p.Function, f)
		l := &profile.Location{ID: uint64(len(p.Location) + 1), Mapping: m, Line: []profile.Line{{Function: f}}}
		p.Location = append(p.Location, l)
		locs[name] = l
		return l
	}
	for _, s := range samples {
		smp := &profile.Sample{Value: []int64{s.value}, Label: s.labels}
		// pprof stacks are leaf first.
		for i := len(s.stack) - 1; i >= 0; i-- {
			smp.Location = append(smp.Location, loc(s.stack[i]))
		}
		p.Sample = append(p.Sample, smp)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func gpuProfile(samples ...fx) []byte { return build("gpu", "nanoseconds", "", samples...) }

func gpuProfileWithID(id string, samples ...fx) []byte {
	return build("gpu", "nanoseconds", id, samples...)
}

func cpuProfile(samples ...fx) []byte { return build("cpu", "nanoseconds", "", samples...) }

// execlogFor writes a log bound to raw and returns its bytes.
func execlogFor(t *testing.T, raw []byte, hdr execlog.Header, rows []execlog.Row, nSamples int) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.execlog")
	w, err := execlog.Create(path, hdr)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		w.Add(r)
	}
	if err := w.Finish(uint64(nSamples), execlog.ProfileSHA256(raw)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// allFixtures is the registry of named fixture builders; later tasks append.
func allFixtures() map[string]func() []byte {
	return map[string]func() []byte{
		"gpu-two-samples": func() []byte {
			return gpuProfile(
				fx{stack: []string{"main", "launch", "[gpu:kernel_a]"}, value: 300},
				fx{stack: []string{"main", "launch", "[gpu:kernel_b]"}, value: 0},
			)
		},
		"cpu-one-sample": func() []byte {
			return cpuProfile(fx{stack: []string{"main", "work"}, value: 10})
		},
		"recursion": func() []byte {
			return cpuProfile(fx{stack: []string{"main", "a", "b", "a", "b"}, value: 10})
		},
		"same-name-two-modules": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libx.so", "x", 0x10, ln("foo"))}, value: 5},
				rs{stack: []frame{fn("main"), in("/usr/lib/liby.so", "y", 0x10, ln("foo"))}, value: 7},
			)
		},
		"one-function-two-pcs": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x10, ln("foo"))}, value: 30},
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x20, ln("foo"))}, value: 70},
			)
		},
		"sentinel-jit": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("[jit]", "", 0x10, ln("foo"))}, value: 3},
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "", 0x10, ln("foo"))}, value: 4},
				rs{stack: []frame{fn("main"), in("[kernel]", "", 0x30, ln("do_syscall_64"))}, value: 2},
			)
		},
		"unmapped-init-with-filename": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), {lines: []line{{name: "__init__", filename: "a.py", startLine: 1}}}}, value: 2},
				rs{stack: []frame{fn("main"), {lines: []line{{name: "__init__", filename: "b.py", startLine: 1}}}}, value: 3},
			)
		},
		"unmapped-init-bare": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), {addr: 1, lines: []line{{name: "__init__"}}}}, value: 2},
				rs{stack: []frame{fn("main"), {addr: 2, lines: []line{{name: "__init__"}}}}, value: 3},
			)
		},
		"inline": func() []byte {
			// Line[0] is the innermost function: inner was inlined into outer.
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/bin/app", "app", 0x40, ln("inner"), ln("outer"))}, value: 9},
			)
		},
		"address-only-and-empty-stack": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x123)}, value: 6},
				rs{stack: []frame{fn("main"), {addr: 0x7f00}}, value: 1},
				rs{stack: nil, value: 4},
				rs{stack: []frame{fn("main"), fn("idle")}, value: 0},
			)
		},
		"inexact-labels": func() []byte {
			return gpuProfile(
				fx{stack: []string{"main", "launch", "[gpu:k]"}, value: 65, labels: map[string][]string{"gpu_join": {"exact"}}},
				fx{stack: []string{"main", "launch", "[gpu:k]"}, value: 20, labels: map[string][]string{"gpu_join": {"heuristic"}}},
				fx{stack: []string{"main", "other", "[gpu:k]"}, value: 15, labels: map[string][]string{"gpu_ambiguous": {"true"}}},
			)
		},
		"gpu-three-executions": gpuThreeExecutions,
		"gpu-pc-sampling":      gpuPCSampling,
		"gpu-sampled-and-unsampled": func() []byte {
			return gpuProfile(
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 70, labels: map[string][]string{"gpu_sample_period": {"8"}}},
				fx{stack: []string{"[gpu:launch unsampled]", "[gpu:kernel:k]"}, value: 30},
			)
		},
		"inexact-joins": func() []byte {
			join := func(v string) map[string][]string { return map[string][]string{"gpu_join": {v}} }
			return gpuProfile(
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 65, labels: join("exact")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 10, labels: join("heuristic")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 20, labels: join("unmatched")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 5, labels: map[string][]string{"gpu_ambiguous": {"true"}}},
			)
		},
		"gpu-serialized": func() []byte {
			ser := func(v string) map[string][]string { return map[string][]string{"gpu_serialized": {v}} }
			return gpuProfile(
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 10, labels: ser("true")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 50, labels: ser("false")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 30, labels: ser("unknown")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 10},
			)
		},
		"cpu-all-zero": func() []byte {
			return cpuProfile(fx{stack: []string{"main", "work"}, value: 0})
		},
		"gpu-kernel-names": func() []byte {
			return gpuProfile(
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k1]"}, value: 10, labels: map[string][]string{"stall": {"a"}}},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k1]"}, value: 20, labels: map[string][]string{"stall": {"b"}}},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k2]"}, value: 0},
			)
		},
		"gpu-unresolved-ancestor": func() []byte {
			// The kernel leaf is named on both stacks; only the CPU caller
			// beneath it is unsymbolized.
			return rich("gpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f00", 0x123), fn("[gpu:kernel:k]")}, value: 40},
				rs{stack: []frame{fn("main"), fn("launch"), fn("[gpu:kernel:k]")}, value: 60},
			)
		},
		"unresolved-twice-in-one-module": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f00", 0x10), in("/usr/lib/libfoo.so", "f00", 0x20), fn("leaf")}, value: 50},
				rs{stack: []frame{fn("main"), fn("work")}, value: 50},
				rs{stack: []frame{fn("main"), in("/usr/lib/libbar.so", "ba5", 0x30)}, value: 0},
			)
		},
		"unresolved-ancestor-base": func() []byte {
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f00", 0x1), fn("foo"), fn("bar")}, value: 100},
			)
		},
		"unresolved-ancestor-new": func() []byte {
			// As the base, but half of foo now ends in an unsymbolized
			// libfoo frame.
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f00", 0x1), fn("foo"), fn("bar")}, value: 50},
				rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f00", 0x1), fn("foo"), in("/usr/lib/libfoo.so", "f00", 0x9)}, value: 50},
			)
		},
		"same-base-name-two-modules": func() []byte {
			// Both frames display as "libfoo.so+0x10".
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{fn("main"), in("/a/libfoo.so", "1", 0x10)}, value: 30},
				rs{stack: []frame{fn("main"), in("/b/libfoo.so", "2", 0x10)}, value: 70},
			)
		},
		"mapped-with-collisions": func() []byte {
			// x and y are bare unmapped functions, so both are
			// collision_possible; the second sample passes through both.
			app := func(addr uint64, name string) frame { return in("/usr/bin/app", "app", addr, ln(name)) }
			return rich("cpu", "nanoseconds",
				rs{stack: []frame{app(0x1, "main"), app(0x2, "work")}, value: 70},
				rs{stack: []frame{app(0x1, "main"), fn("x"), fn("y")}, value: 30},
			)
		},
		"gpu-graph-refused": func() []byte {
			refused := func(v string) map[string][]string {
				return map[string][]string{"gpu_join": {"exact"}, "gpu_graph_refused": {v}}
			}
			return gpuProfile(
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 25, labels: refused("true")},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 70, labels: map[string][]string{"gpu_join": {"exact"}}},
				fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 5, labels: refused("false")},
			)
		},
		"gpu-zero-total": func() []byte {
			return gpuProfile(fx{
				stack:  []string{"main", "[gpu:launch]", "[gpu:kernel:k]"},
				value:  0,
				labels: map[string][]string{"gpu_serialized": {"true"}, "gpu_join": {"exact"}, "gpu_sample_period": {"8"}},
			})
		},
	}
}

// gpuThreeExecutions has two samples carrying three executions: 0 and 1
// on sample 0, 2 on sample 1 (see threeExecutionRows).
func gpuThreeExecutions() []byte {
	return gpuProfile(
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k1]"}, value: 300},
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k2]"}, value: 50},
	)
}

func threeExecutionRows() []execlog.Row {
	return []execlog.Row{
		{Sample: 0, Execution: 0, Value: 100},
		{Sample: 0, Execution: 1, Value: 200},
		{Sample: 1, Execution: 2, Value: 50},
	}
}

// gpuPCSampling is one execution's PC samples split by stall reason into
// three samples; the last received no weight (see pcSamplingRows).
func gpuPCSampling() []byte {
	stall := func(v string) map[string][]string { return map[string][]string{"stall": {v}} }
	return gpuProfile(
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 4, labels: stall("a")},
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 6, labels: stall("b")},
		fx{stack: []string{"main", "[gpu:launch]", "[gpu:kernel:k]"}, value: 0, labels: stall("c")},
	)
}

func pcSamplingRows() []execlog.Row {
	return []execlog.Row{
		{Sample: 0, Execution: 7, Value: 4},
		{Sample: 1, Execution: 7, Value: 6},
		{Sample: 2, Execution: 7, Value: 0},
	}
}

// frame is one stack entry for rich: a location on the mapping named by
// file and buildID ("" file = Mapping[0], which has no file), at addr, with
// its lines innermost first as pprof stores them. No lines = address only.
type frame struct {
	file, buildID string
	addr          uint64
	lines         []line
}

type line struct {
	name, filename string
	startLine      int64
}

// rs is one sample for rich: frames root first, a value, and labels.
type rs struct {
	stack  []frame
	value  int64
	labels map[string][]string
}

func ln(name string) line { return line{name: name} }

// fn is a frame on Mapping[0] with one line naming name.
func fn(name string) frame { return frame{lines: []line{ln(name)}} }

func in(file, buildID string, addr uint64, lines ...line) frame {
	return frame{file: file, buildID: buildID, addr: addr, lines: lines}
}

// rich serializes a single-sample-type profile with real mappings, build
// ids, addresses and inline chains. Mappings, functions and locations are
// shared between samples whenever their fields are equal.
func rich(typ, unit string, samples ...rs) []byte {
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: typ, Unit: unit}}}
	empty := &profile.Mapping{ID: 1}
	p.Mapping = []*profile.Mapping{empty}
	type mkey struct{ file, buildID string }
	maps := map[mkey]*profile.Mapping{}
	funcs := map[line]*profile.Function{}
	locs := map[string]*profile.Location{}
	mapping := func(fr frame) *profile.Mapping {
		if fr.file == "" {
			return empty
		}
		k := mkey{fr.file, fr.buildID}
		if m, ok := maps[k]; ok {
			return m
		}
		m := &profile.Mapping{ID: uint64(len(p.Mapping) + 1), File: fr.file, BuildID: fr.buildID}
		p.Mapping = append(p.Mapping, m)
		maps[k] = m
		return m
	}
	function := func(l line) *profile.Function {
		if f, ok := funcs[l]; ok {
			return f
		}
		f := &profile.Function{ID: uint64(len(p.Function) + 1), Name: l.name, Filename: l.filename, StartLine: l.startLine}
		p.Function = append(p.Function, f)
		funcs[l] = f
		return f
	}
	location := func(fr frame) *profile.Location {
		m := mapping(fr)
		k := fmt.Sprint(m.ID, fr.addr, fr.lines)
		if l, ok := locs[k]; ok {
			return l
		}
		l := &profile.Location{ID: uint64(len(p.Location) + 1), Mapping: m, Address: fr.addr}
		for _, x := range fr.lines {
			l.Line = append(l.Line, profile.Line{Function: function(x)})
		}
		p.Location = append(p.Location, l)
		locs[k] = l
		return l
	}
	for _, s := range samples {
		smp := &profile.Sample{Value: []int64{s.value}, Label: s.labels}
		for i := len(s.stack) - 1; i >= 0; i-- {
			smp.Location = append(smp.Location, location(s.stack[i]))
		}
		p.Sample = append(p.Sample, smp)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

var gpuHdr = execlog.Header{Backend: "cupti", SampleType: "gpu/nanoseconds"}

// diffPair is a base and a new profile for the diff tests.
type diffPair struct{ base, new func() []byte }

// diffPairs is the registry of named base/new pairs; every one is also
// checked against the union invariants.
func diffPairs() map[string]diffPair {
	app := func(buildID string, addr uint64, name string) frame {
		return in("/usr/bin/app", buildID, addr, ln(name))
	}
	ser := func(v string) map[string][]string { return map[string][]string{"gpu_serialized": {v}} }
	gpuK := func(kernel string, value int64, labels map[string][]string) fx {
		return fx{stack: []string{"main", "[gpu:launch]", kernel}, value: value, labels: labels}
	}
	period := func(p, join string) map[string][]string {
		return map[string][]string{"gpu_sample_period": {p}, "gpu_join": {join}}
	}
	initAt := func(file string, start int64) frame {
		return frame{lines: []line{{name: "__init__", filename: file, startLine: start}}}
	}
	// degraded builds a pair whose base calls foo in libfoo and whose new
	// profile has the same call unsymbolized in the module given.
	degraded := func(file, buildID string) diffPair {
		return diffPair{
			base: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x123, ln("foo"))}, value: 50},
					rs{stack: []frame{fn("main"), fn("work")}, value: 50},
				)
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in(file, buildID, 0x123)}, value: 50},
					rs{stack: []frame{fn("main"), fn("work")}, value: 50},
				)
			},
		}
	}
	oneFoo := func() []byte {
		return rich("cpu", "nanoseconds",
			rs{stack: []frame{fn("main"), in("/a/libfoo.so", "x1", 0x10, ln("foo"))}, value: 10},
		)
	}
	twoFoos := func() []byte {
		return rich("cpu", "nanoseconds",
			rs{stack: []frame{fn("main"), in("/a/libfoo.so", "y1", 0x10, ln("foo"))}, value: 5},
			rs{stack: []frame{fn("main"), in("/b/libfoo.so", "y2", 0x10, ln("foo"))}, value: 6},
		)
	}
	return map[string]diffPair{
		// The application was rebuilt (build id a → b); libz was not.
		"rebuilt-app-unchanged-lib": {
			base: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{app("a", 1, "main"), app("a", 2, "work")}, value: 60},
					rs{stack: []frame{app("a", 1, "main"), in("/usr/lib/libz.so", "z", 0x10, ln("compress"))}, value: 40},
				)
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{app("b", 1, "main"), app("b", 2, "work")}, value: 30},
					rs{stack: []frame{app("b", 1, "main"), in("/usr/lib/libz.so", "z", 0x10, ln("compress"))}, value: 50},
				)
			},
		},
		"no-build-id": {
			base: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{app("", 1, "main"), app("", 2, "foo")}, value: 10})
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{app("", 1, "main"), app("", 2, "foo")}, value: 20})
			},
		},
		"moved-module": {
			base: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{fn("main"), in("/opt/a/libfoo.so", "1", 0x10, ln("foo"))}, value: 10})
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{fn("main"), in("/opt/b/libfoo.so", "2", 0x10, ln("foo"))}, value: 12})
			},
		},
		"one-vs-two": {base: oneFoo, new: twoFoos},
		"two-vs-one": {base: twoFoos, new: oneFoo},
		"removed-kernel": {
			base: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), fn("work")}, value: 63},
					rs{stack: []frame{fn("main"), in("[kernel]", "", 0x30, ln("do_syscall_64"))}, value: 37},
				)
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{fn("main"), fn("work")}, value: 63})
			},
		},
		// The unsymbolized libfoo frame is found on the new side by build
		// id, by full path, and by base name; libbar is another module.
		"degraded-same-module":   degraded("/usr/lib/libfoo.so", "f"),
		"degraded-same-path":     degraded("/usr/lib/libfoo.so", "g"),
		"degraded-same-basename": degraded("/opt/lib/libfoo.so", "h"),
		"degraded-other-module":  degraded("/usr/lib/libbar.so", "b"),
		// foo's module is /usr/lib/libfoo.so with build id f. The new side
		// has two unsymbolized libfoo modules: the one the ladder picks
		// first moved by 0.05, the other by 0.5.
		"ladder-build-id-before-path": {
			base: degraded("", "").base,
			new: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in("/opt/x/libfoo.so", "f", 0x1)}, value: 5},
					rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "g", 0x2)}, value: 50},
					rs{stack: []frame{fn("main"), fn("work")}, value: 45},
				)
			},
		},
		"ladder-path-before-base-name": {
			base: degraded("", "").base,
			new: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "g", 0x1)}, value: 5},
					rs{stack: []frame{fn("main"), in("/opt/lib/libfoo.so", "h", 0x2)}, value: 50},
					rs{stack: []frame{fn("main"), fn("work")}, value: 45},
				)
			},
		},
		// Half the base is unsymbolized libfoo; the new view is empty.
		"unresolved-base-zero-new": {
			base: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x1)}, value: 50},
					rs{stack: []frame{fn("main"), fn("work")}, value: 50},
				)
			},
			new: allFixtures()["cpu-all-zero"],
		},
		"perturbation-shift": {
			base: func() []byte {
				return gpuProfile(gpuK("[gpu:kernel:k1]", 50, ser("false")), gpuK("[gpu:kernel:k2]", 50, ser("false")))
			},
			new: func() []byte {
				return gpuProfile(gpuK("[gpu:kernel:k1]", 50, ser("true")), gpuK("[gpu:kernel:k2]", 50, ser("false")))
			},
		},
		"serialized-missing-to-false": {
			base: func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, nil)) },
			new:  func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, ser("false"))) },
		},
		"serialized-unknown-to-false": {
			base: func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, ser("unknown"))) },
			new:  func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, ser("false"))) },
		},
		// Every state moves by 0.05, under the threshold, but the new side
		// knows its perturbation and the base does not.
		"serialized-known-vs-unknown": {
			base: func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, ser("unknown"))) },
			new: func() []byte {
				return gpuProfile(gpuK("[gpu:kernel:k]", 5, ser("false")), gpuK("[gpu:kernel:k]", 95, ser("unknown")))
			},
		},
		// Known on both sides, every state within the threshold.
		"serialized-small-shift": {
			base: func() []byte {
				return gpuProfile(gpuK("[gpu:kernel:k]", 95, ser("false")), gpuK("[gpu:kernel:k]", 5, ser("unknown")))
			},
			new: func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, ser("false"))) },
		},
		"zero-total-new": {
			base: func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, ser("false"))) },
			new:  allFixtures()["gpu-zero-total"],
		},
		"gpu-ancestor-degradation": {
			base: func() []byte {
				return rich("gpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x123, ln("foo")), fn("[gpu:kernel:k]")}, value: 100},
				)
			},
			new: func() []byte {
				return rich("gpu", "nanoseconds",
					rs{stack: []frame{fn("main"), in("/usr/lib/libfoo.so", "f", 0x123), fn("[gpu:kernel:k]")}, value: 100},
				)
			},
		},
		"unmapped-filename-change": {
			base: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{fn("main"), initAt("a.py", 10)}, value: 4})
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds", rs{stack: []frame{fn("main"), initAt("b.py", 20)}, value: 4})
			},
		},
		"unmapped-stable-siblings": {
			base: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), initAt("a.py", 1)}, value: 2},
					rs{stack: []frame{fn("main"), initAt("b.py", 1)}, value: 3},
				)
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), initAt("a.py", 1)}, value: 5},
					rs{stack: []frame{fn("main"), initAt("b.py", 1)}, value: 6},
				)
			},
		},
		"sample-period-change": {
			base: func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, period("8", "exact"))) },
			new:  func() []byte { return gpuProfile(gpuK("[gpu:kernel:k]", 100, period("16", "exact"))) },
		},
		// Join coverage and the sample period both change; join comes first.
		"join-and-period-change": {
			base: func() []byte {
				return gpuProfile(gpuK("[gpu:kernel:k]", 90, period("8", "exact")), gpuK("[gpu:kernel:k]", 10, period("8", "heuristic")))
			},
			new: func() []byte {
				return gpuProfile(gpuK("[gpu:kernel:k]", 50, period("16", "exact")), gpuK("[gpu:kernel:k]", 50, period("16", "heuristic")))
			},
		},
		// idle and the /a foo only have zero-value samples in the base.
		"zero-value-nodes": {
			base: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), fn("work")}, value: 10},
					rs{stack: []frame{fn("main"), fn("idle")}, value: 0},
					rs{stack: []frame{fn("main"), in("/a/libfoo.so", "1", 0x10, ln("foo"))}, value: 0},
					rs{stack: []frame{fn("main"), in("/b/libfoo.so", "2", 0x10, ln("foo"))}, value: 5},
				)
			},
			new: func() []byte {
				return rich("cpu", "nanoseconds",
					rs{stack: []frame{fn("main"), fn("idle")}, value: 5},
					rs{stack: []frame{fn("main"), in("/c/libfoo.so", "3", 0x10, ln("foo"))}, value: 5},
				)
			},
		},
	}
}
