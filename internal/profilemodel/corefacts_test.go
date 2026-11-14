package profilemodel

import (
	"reflect"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func TestCoreFactsAgreeWithFacts(t *testing.T) {
	for name, raw := range map[string][]byte{"cpu": pmfixture.CPUBasic(), "gpu": pmfixture.GPUBasic()} {
		t.Run(name, func(t *testing.T) {
			p, err := FromBytes(raw, nil, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			v, err := p.View(ViewOptions{SampleIndex: -1})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			v.Walk(func(n *Node) bool {
				count++
				pf, err := v.Facts(n.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
				cf, err := v.CoreFacts(n.ID)
				if err != nil {
					t.Fatal(err)
				}
				got := []any{cf.Node, cf.ParentNode, cf.Cumulative, cf.Self, cf.ShareOfTotal, cf.ShareOfParent, cf.InexactPortion}
				want := []any{pf.Node, pf.ParentNode, pf.Cumulative, pf.Self, pf.ShareOfTotal, pf.ShareOfParent, pf.InexactPortion}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("node %s: core %+v != facts %+v", n.ID, got, want)
				}
				return true
			})
			if count < 3 {
				t.Fatalf("walked %d nodes", count)
			}
			if _, err := v.CoreFacts("nope"); err == nil {
				t.Fatal("unknown node accepted")
			}
		})
	}
}
