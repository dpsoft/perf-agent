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
