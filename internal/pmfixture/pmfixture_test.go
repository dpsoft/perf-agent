package pmfixture

import (
	"bytes"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
)

func TestNamedFixturesParseWithTheirPinnedTotals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   []byte
		total int64
	}{
		{"CPUBasic", CPUBasic(), 1000},
		{"CPUBasicNew", CPUBasicNew(), 1000},
		{"GPUBasic", GPUBasic(), 500},
		{"HugeValue", HugeValue(), 9007199254740993},
		{"Recursive", Recursive(), 15},
		{"Deep", Deep(600), 1},
		{"DeepNew", DeepNew(600), 2},
		{"SameNameTwoModules", SameNameTwoModules(), 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := profile.ParseData(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.CheckValid(); err != nil {
				t.Fatal(err)
			}
			var sum int64
			for _, s := range p.Sample {
				sum += s.Value[0]
			}
			if sum != tc.total {
				t.Fatalf("total = %d, want %d", sum, tc.total)
			}
		})
	}
	var rows int
	_, tr, err := execlog.Read(bytes.NewReader(GPUBasicExeclog(t)), func(execlog.Row) error { rows++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if rows != 5 || tr.Executions != 5 || tr.AggregatedSamples != 4 {
		t.Fatalf("rows=%d trailer=%+v", rows, tr)
	}
}

func TestBuildKeepsFramesThatDifferOnlyAcrossSeparators(t *testing.T) {
	frames := []Frame{
		In("/m", "b", 1, L("a b", "c", 10)),
		In("/m", "b", 1, L("a", "b c", 10)),
		In("/m", "b", 1, L("x", "f", 1), L("y", "g", 2)),
		In("/m", "b", 1, L("x", "f", 1), L("y", "g", 3)),
		In("/m", "b", 1, L("x", "f", 1)),
	}
	var samples []Sample
	for _, f := range frames {
		samples = append(samples, Sample{Frames: []Frame{f}, Value: 1})
	}
	p, err := profile.ParseData(Build("cpu", "nanoseconds", nil, samples...))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Location) != len(frames) {
		t.Fatalf("locations = %d, want %d", len(p.Location), len(frames))
	}
	for i, s := range p.Sample {
		got, want := s.Location[0].Line, frames[i].Lines
		if len(got) != len(want) {
			t.Fatalf("sample %d: %d lines, want %d", i, len(got), len(want))
		}
		for j := range got {
			if got[j].Function.Name != want[j].Name || got[j].Function.Filename != want[j].Filename || got[j].Line != want[j].Line {
				t.Errorf("sample %d line %d = %+v, want %+v", i, j, got[j], want[j])
			}
		}
	}
}
