package profilemodel

import (
	"runtime"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// heapGrowth is how much the live heap grew while build ran, with what
// build returned still reachable.
func heapGrowth[T any](build func() T) (T, int64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	v := build()
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(v)
	return v, int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

func TestRetainedEstimatesTrackTheHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("loads a 100k-sample profile")
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"wide 100k samples", pmfixture.Wide(100_000)},
		{"Deep(5000)", pmfixture.Deep(5000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, heldP := heapGrowth(func() *Profile {
				p, err := FromBytes(tc.raw, nil, DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				return p
			})
			v, heldV := heapGrowth(func() *View {
				v, err := p.View(ViewOptions{SampleIndex: -1})
				if err != nil {
					t.Fatal(err)
				}
				return v
			})
			runtime.KeepAlive(p)
			for _, c := range []struct {
				what           string
				estimate, held int64
			}{
				{"profile", p.RetainedBytes(), heldP},
				{"view", v.RetainedBytes(), heldV},
			} {
				ratio := float64(c.estimate) / float64(c.held)
				t.Logf("%s: estimate %d, heap %d, ratio %.3f", c.what, c.estimate, c.held, ratio)
				if c.held <= 0 || ratio < 0.5 || ratio > 2 {
					t.Errorf("%s: RetainedBytes %d is not within [0.5x, 2x] of the %d bytes it holds on the heap", c.what, c.estimate, c.held)
				}
			}
		})
	}
}

func TestRetainedEstimatesAreFixedAtConstruction(t *testing.T) {
	p, err := FromBytes(pmfixture.CPUBasic(), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	pb, vb := p.RetainedBytes(), v.RetainedBytes()
	if pb <= 0 || vb <= 0 {
		t.Fatalf("estimates %d and %d, want both positive", pb, vb)
	}
	if _, err := v.Facts(v.Root.Children[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	if p.RetainedBytes() != pb || v.RetainedBytes() != vb {
		t.Errorf("estimates moved after a read: %d→%d, %d→%d", pb, p.RetainedBytes(), vb, v.RetainedBytes())
	}
}

func TestExecutionsCountInTheProfileEstimate(t *testing.T) {
	without, err := FromBytes(pmfixture.GPUBasic(), nil, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	with, err := FromBytes(pmfixture.GPUBasic(), pmfixture.GPUBasicExeclog(t), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !with.Executions.OK {
		t.Fatalf("executions not used: %+v", with.Executions)
	}
	if with.RetainedBytes() <= without.RetainedBytes() {
		t.Errorf("with executions %d, without %d: the ordinals are not counted", with.RetainedBytes(), without.RetainedBytes())
	}
}
