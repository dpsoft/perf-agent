package profilemodel

import (
	"errors"
	"slices"
	"testing"

	"github.com/dpsoft/perf-agent/internal/pmfixture"
)

func TestSourceRefsForACallSite(t *testing.T) {
	v := viewOf(t, pmfixture.CPUBasic())
	b := nodesAt(v, "main", "a", "b")
	if len(b) != 1 {
		t.Fatalf("main;a;b: %d nodes", len(b))
	}
	got, err := v.SourceRefs(b[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []SourceRef{{
		Kind:     SourceCallSite,
		Function: "b",
		Filename: "/build/app/src/app.c",
		Line:     30,
		Module:   ModuleRef{File: "/opt/app/bin/app", BuildID: "b1"},
		Value:    411,
	}}
	if !slices.Equal(got, want) {
		t.Fatalf("SourceRefs(b) = %+v, want %+v", got, want)
	}
}

func TestSourceRefsCountASampleOncePerRef(t *testing.T) {
	v := viewOf(t, pmfixture.CPUBasic())
	a := nodesAt(v, "main", "a")
	if len(a) != 1 {
		t.Fatalf("main;a: %d nodes", len(a))
	}
	got, err := v.SourceRefs(a[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	// a's own frame in both samples through it (411 + 100); b's frame below
	// it is not a's source.
	if len(got) != 1 || got[0].Line != 20 || got[0].Value != 511 {
		t.Fatalf("SourceRefs(a) = %+v, want one app.c:20 ref of 511", got)
	}
}

func TestSourceRefsFromPCSampling(t *testing.T) {
	v := gpuView(t, false)
	k1 := nodesAt(v, "train", "conv", "[gpu:launch]", "[gpu:kernel:k1]")
	if len(k1) != 1 {
		t.Fatalf("k1 under conv: %d nodes", len(k1))
	}
	got, err := v.SourceRefs(k1[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	want := SourceRef{Kind: SourceGPUPC, Function: "k1", Filename: "conv.cu", Line: 42, Value: 300}
	if !slices.Contains(got, want) {
		t.Fatalf("SourceRefs(k1) = %+v, want it to include %+v", got, want)
	}

	// conv's samples are 300 (resolved) and 100 (no PC source): only the
	// resolved one gives a gpu_pc reference, and conv's own frame has no file.
	conv := nodesAt(v, "train", "conv")
	got, err = v.SourceRefs(conv[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []SourceRef{want}) {
		t.Fatalf("SourceRefs(conv) = %+v, want only %+v", got, want)
	}
}

func TestSourceRefsOrder(t *testing.T) {
	v := viewOf(t, pmfixture.CPUBasic())
	got, err := v.SourceRefs(nodesAt(v, "main")[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Filename != "/build/app/src/main.c" || got[0].Value != 1000 {
		t.Fatalf("SourceRefs(main) = %+v", got)
	}
	if _, err := v.SourceRefs("nope"); !errors.Is(err, ErrUnknownNode) {
		t.Fatalf("unknown node: err = %v, want ErrUnknownNode", err)
	}
}

func TestSourceRefsSortByValueThenKindFileLine(t *testing.T) {
	refs := []SourceRef{
		{Kind: SourceGPUPC, Filename: "b.cu", Line: 2, Value: 5},
		{Kind: SourceCallSite, Filename: "z.c", Line: 1, Value: 9},
		{Kind: SourceGPUPC, Filename: "a.cu", Line: 7, Value: 5},
		{Kind: SourceCallSite, Filename: "a.c", Line: 3, Value: 5},
		{Kind: SourceGPUPC, Filename: "a.cu", Line: 1, Value: 5},
	}
	sortSourceRefs(refs)
	var got []string
	for _, r := range refs {
		got = append(got, r.Kind+":"+r.Filename)
	}
	want := []string{"call_site:z.c", "call_site:a.c", "gpu_pc:a.cu", "gpu_pc:a.cu", "gpu_pc:b.cu"}
	if !slices.Equal(got, want) || refs[2].Line != 1 {
		t.Fatalf("order = %v (third line %d), want %v (line 1 third)", got, refs[2].Line, want)
	}
}
