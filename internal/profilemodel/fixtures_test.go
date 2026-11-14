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
