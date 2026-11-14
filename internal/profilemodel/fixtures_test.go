package profilemodel

import (
	"bytes"
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
	}
}

var gpuHdr = execlog.Header{Backend: "cupti", SampleType: "gpu/nanoseconds"}
