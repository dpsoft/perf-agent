package profilemodel

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	pprof "github.com/google/pprof/profile"

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

// labelled is CPUBasic with sample 0 copied n times, every copy carrying
// the same label value of size bytes, which the profile stores once.
func labelled(t *testing.T, n, size int) []byte {
	t.Helper()
	p, err := pprof.ParseData(pmfixture.CPUBasic())
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("x", size)
	first := p.Sample[0]
	first.Label = map[string][]string{"request": {value}}
	for range n - 1 {
		c := *first
		c.Label = map[string][]string{"request": {value}}
		p.Sample = append(p.Sample, &c)
	}
	var b bytes.Buffer
	if err := p.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRetainedEstimatesCountEveryStringOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n, size int
	}{
		{"one 8 MiB label", 1, 8 << 20},
		{"one 1 MiB label on 1000 samples", 1000, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := labelled(t, tc.n, tc.size)
			p, held := heapGrowth(func() *Profile {
				p, err := FromBytes(raw, nil, DefaultLimits())
				if err != nil {
					t.Fatal(err)
				}
				return p
			})
			ratio := float64(p.RetainedBytes()) / float64(held)
			t.Logf("%d compressed bytes: estimate %d, heap %d, ratio %.3f", len(raw), p.RetainedBytes(), held, ratio)
			if held <= 0 || ratio < 0.5 || ratio > 2 {
				t.Errorf("RetainedBytes %d is not within [0.5x, 2x] of the %d bytes the profile holds", p.RetainedBytes(), held)
			}
		})
	}
}

func TestDiffRetainedEstimatesTrackTheHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("compares two 100k-sample profiles")
	}
	for _, tc := range []struct {
		name      string
		base, new []byte
	}{
		{"wide 100k samples against itself", pmfixture.Wide(100_000), pmfixture.Wide(100_000)},
		{"Deep(5000) vs DeepNew(5000)", pmfixture.Deep(5000), pmfixture.DeepNew(5000)},
		{"CPUBasic vs CPUBasicNew", pmfixture.CPUBasic(), pmfixture.CPUBasicNew()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bv, nv := viewOf(t, tc.base), viewOf(t, tc.new)
			d, heldD := heapGrowth(func() *Diff {
				d, err := Compare(bv, nv, WorkCounts{Base: 3, New: 4})
				if err != nil {
					t.Fatal(err)
				}
				return d
			})
			ix, heldI := heapGrowth(func() *DiffIndex { return NewDiffIndex(d) })
			runtime.KeepAlive(bv)
			runtime.KeepAlive(nv)
			for _, c := range []struct {
				what           string
				estimate, held int64
			}{
				{"diff", d.RetainedBytes(), heldD},
				{"index", ix.RetainedBytes(), heldI},
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
