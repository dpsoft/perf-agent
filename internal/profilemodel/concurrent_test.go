package profilemodel

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

// viewReads is every read a live page's handlers make of a view, for every
// node, with errors as text.
type viewReads struct {
	Quality Quality
	Notes   []string
	NotesErr,
	Walk string
	Nodes []nodeReads
}

type nodeReads struct {
	Facts       PathFacts
	FactsErr    string
	Core        CoreFacts
	CoreErr     string
	NodeQuality Quality
	QualityErr  string
	Kernels     []KernelRow
	KernelsErr  string
	Path        []NodeID
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func readView(v *View, ids []NodeID) viewReads {
	var out viewReads
	out.Quality = v.Quality()
	notes, err := v.Notes()
	out.Notes, out.NotesErr = notes, errText(err)
	v.Walk(func(n *Node) bool {
		out.Walk += string(n.ID) + ","
		return true
	})
	for _, id := range ids {
		var r nodeReads
		var err error
		r.Facts, err = v.Facts(id, nil)
		r.FactsErr = errText(err)
		r.Core, err = v.CoreFacts(id)
		r.CoreErr = errText(err)
		r.NodeQuality, err = v.NodeQuality(id)
		r.QualityErr = errText(err)
		r.Kernels, err = v.KernelsBelow(id)
		r.KernelsErr = errText(err)
		n, _ := v.Node(id)
		for _, q := range v.Path(n) {
			r.Path = append(r.Path, q.ID)
		}
		out.Nodes = append(out.Nodes, r)
	}
	return out
}

// TestViewReadsAreSafeConcurrently is the guarantee the live page's HTTP
// handlers rely on: a built view is only read, so any number of
// goroutines may read it at once and each sees what one goroutine alone
// would.
func TestViewReadsAreSafeConcurrently(t *testing.T) {
	p, err := FromBytes(pmfixture.GPUBasic(), pmfixture.GPUBasicExeclog(t), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Executions.OK {
		t.Fatalf("executions not used: %+v", p.Executions)
	}
	v, err := p.View(ViewOptions{SampleIndex: -1})
	if err != nil {
		t.Fatal(err)
	}
	var ids []NodeID
	v.Walk(func(n *Node) bool {
		ids = append(ids, n.ID)
		return true
	})
	want := readView(v, ids)

	const goroutines, iterations = 16, 200
	var wg sync.WaitGroup
	errs := make(chan string, goroutines)
	for g := range goroutines {
		wg.Go(func() {
			for i := range iterations {
				if got := readView(v, ids); !reflect.DeepEqual(got, want) {
					errs <- fmt.Sprintf("goroutine %d, iteration %d: reads differ from a sequential run", g, i)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
