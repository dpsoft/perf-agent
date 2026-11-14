package profilemodel

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
)

// Source reference kinds.
const (
	// SourceCallSite is the file and line of the node's own frame.
	SourceCallSite = "call_site"
	// SourceGPUPC is the source line PC sampling attributed a kernel's
	// stall to, from the gpu_src_* labels of a sample whose status is
	// "resolved".
	SourceGPUPC = "gpu_pc"
)

// Labels perf-agent writes on a GPU sample whose PC was mapped to source.
const (
	labelGPUSrcStatus = "gpu_src_status"
	labelGPUSrcFile   = "gpu_src_file" // a basename
	labelGPUSrcLine   = "gpu_src_line"
	labelGPUSrcFunc   = "gpu_src_func"
	gpuSrcResolved    = "resolved"
)

// SourceRef is one source line a node's samples point at, as the profile
// names it; nothing here says the file exists or matches the binary.
type SourceRef struct {
	Kind     string    `json:"kind"` // SourceCallSite or SourceGPUPC
	Function string    `json:"function"`
	Filename string    `json:"filename"`
	Line     int64     `json:"line"`
	Module   ModuleRef `json:"module"` // zero for SourceGPUPC: the labels name no module
	// Value is the sum of the values of the samples through the node that
	// give this reference, once per sample.
	Value int64 `json:"value"`
}

type sourceKey struct {
	kind, file, fn, module string
	line                   int64
}

// SourceRefs lists the source lines of id's samples: for each sample with a
// non-zero value through the node, the node's own frame (when the profile
// gives it a file) and, on a GPU sample whose PC source is resolved, the
// labelled line. Rows are distinct by kind, file, line, function and module;
// they are ordered by value descending, then kind, file and line.
func (v *View) SourceRefs(id NodeID) ([]SourceRef, error) {
	n, ok := v.nodes[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownNode, id)
	}
	b := newBuilder()
	byKey := map[sourceKey]*SourceRef{}
	var order []*SourceRef
	add := func(r SourceRef, seen map[sourceKey]bool) error {
		k := sourceKey{r.Kind, r.Filename, r.Function, r.Module.Key(), r.Line}
		if seen[k] {
			return nil
		}
		seen[k] = true
		row := byKey[k]
		if row == nil {
			row = new(r)
			row.Value = 0
			byKey[k] = row
			order = append(order, row)
		}
		var err error
		row.Value, err = addChecked(row.Value, r.Value, "source reference value")
		return err
	}
	for _, i := range n.samples {
		s := v.profile.Source.Sample[i]
		val := s.Value[v.Index]
		seen := map[sourceKey]bool{}
		if n.Depth > 0 {
			frames := b.framesOf(s)
			if n.Depth <= len(frames) {
				if fr := frames[n.Depth-1]; fr.file != "" {
					ref := SourceRef{Kind: SourceCallSite, Function: fr.fn.Name, Filename: fr.file, Line: fr.line, Module: fr.fn.Module, Value: val}
					if err := add(ref, seen); err != nil {
						return nil, err
					}
				}
			}
		}
		if ref, ok := gpuPCRef(s.Label); ok {
			ref.Value = val
			if err := add(ref, seen); err != nil {
				return nil, err
			}
		}
	}
	out := make([]SourceRef, len(order))
	for i, r := range order {
		out[i] = *r
	}
	sortSourceRefs(out)
	return out, nil
}

// gpuPCRef is the PC sampling source line a sample's labels give, when its
// status is resolved and the file and a positive line are present.
func gpuPCRef(labels map[string][]string) (SourceRef, bool) {
	one := func(k string) string {
		if vs := labels[k]; len(vs) == 1 {
			return vs[0]
		}
		return ""
	}
	if one(labelGPUSrcStatus) != gpuSrcResolved {
		return SourceRef{}, false
	}
	file := one(labelGPUSrcFile)
	line, err := strconv.ParseInt(one(labelGPUSrcLine), 10, 64)
	if file == "" || err != nil || line <= 0 {
		return SourceRef{}, false
	}
	return SourceRef{Kind: SourceGPUPC, Function: one(labelGPUSrcFunc), Filename: file, Line: line}, true
}

func sortSourceRefs(refs []SourceRef) {
	slices.SortFunc(refs, func(a, b SourceRef) int {
		return cmp.Or(
			cmp.Compare(b.Value, a.Value),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Filename, b.Filename),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Function, b.Function),
			cmp.Compare(a.Module.Key(), b.Module.Key()),
		)
	})
}
