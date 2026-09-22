package foldedstacks

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// FuseInput is one folded profile and the name of the synthetic root every
// one of its stacks will hang under.
type FuseInput struct {
	// Result is the folded profile. Fold has already normalized its
	// Stack.Frames to root-first, whatever order the file on disk used,
	// which is the whole reason fusing happens here and not over pprof:
	// there is no Mapping to rebuild and no stack order left to guess.
	Result *Result
	// Label names the synthetic root, e.g. "[cpu] perf-agent 99 Hz". It is
	// required: an unlabelled root is indistinguishable from a real frame.
	Label string
}

// Fuse draws several folded profiles as one tree, each hanging under its own
// labelled root.
//
// It exists because the two things a reader most wants side by side —
// CPU and GPU, or on-CPU and off-CPU — are measured by different collectors
// into different files, and the alternative is merging the pprof profiles by
// hand. That alternative is worse than it looks: a merger that rebuilds
// frames as names rather than carrying Mapping through silently disables
// vendor-run collapsing in the renderer, because isVendorModule keys on the
// module. Fusing after the fold cannot make that mistake, because the fold
// has already produced exactly the fields the renderer consumes.
//
// What Fuse refuses to do is add values that are not the same kind of thing.
// The inputs must agree on Unit; the sample type NAMES may differ, and the
// fused axis says so, because "cpu+gpu nanoseconds" is honest where a single
// borrowed name would not be.
func Fuse(inputs []FuseInput) (*Result, error) {
	// One input is a legitimate degenerate case -- it relabels a single
	// profile under a root -- so the "two or more is the point" minimum
	// belongs to the caller that has a user to talk to, not here.
	if len(inputs) == 0 {
		return nil, fmt.Errorf("foldedstacks: fuse got no inputs")
	}

	unit := ""
	for i, in := range inputs {
		if in.Result == nil {
			return nil, fmt.Errorf("foldedstacks: fuse input %d has no result", i)
		}
		if strings.TrimSpace(in.Label) == "" {
			return nil, fmt.Errorf("foldedstacks: fuse input %d (%s) has no label; an unlabelled root cannot be told from a real frame",
				i, in.Result.SampleTypeName)
		}
		switch {
		case unit == "":
			unit = in.Result.Unit
		case in.Result.Unit != unit:
			return nil, fmt.Errorf("foldedstacks: fuse refuses to add %q to %q: the inputs disagree on unit, and one axis cannot carry both",
				in.Result.Unit, unit)
		}
	}

	out := &Result{
		Unit: unit,
		// Root-first by construction: every Stack.Frames Fold produces is
		// root-first, and the label goes in front of it.
		StackOrder: RootFirst,
	}

	var names []string
	for _, in := range inputs {
		r := in.Result

		if !slices.Contains(names, r.SampleTypeName) {
			names = append(names, r.SampleTypeName)
		}
		for _, st := range r.SampleTypes {
			if !slices.Contains(out.SampleTypes, st) {
				out.SampleTypes = append(out.SampleTypes, st)
			}
		}

		for _, s := range r.Stacks {
			fused := Stack{
				Frames:  append([]string{in.Label}, s.Frames...),
				Modules: append([]string{""}, s.Modules...),
				Value:   s.Value,
				Inexact: s.Inexact,
			}
			if s.Collapsed != nil {
				// Parallel to Frames or not at all. A root is never the
				// product of a merge.
				fused.Collapsed = append([]bool{false}, s.Collapsed...)
			}
			out.Stacks = append(out.Stacks, fused)
		}

		out.Total += r.Total
		out.InexactTotal += r.InexactTotal
		out.UnsampledLaunchTotal += r.UnsampledLaunchTotal
		out.Samples += r.Samples
		out.ZeroValueSamples += r.ZeroValueSamples
		out.EmptyStackSamples += r.EmptyStackSamples
		// One added root frame per stack, so the frame count the page
		// reports still matches the picture it draws.
		out.Frames += r.Frames + len(r.Stacks)
		out.VendorFramesCollapsed += r.VendorFramesCollapsed
		out.AddressOnlyFrames += r.AddressOnlyFrames
		out.InlinedFrames += r.InlinedFrames
		out.MaxDepth = max(out.MaxDepth, r.MaxDepth+1)

		for _, w := range r.Warnings {
			out.Warnings = append(out.Warnings, in.Label+": "+w)
		}
		mergeLabels(out, r)
	}

	out.SampleTypeName = strings.Join(names, "+")

	if out.Total <= 0 {
		out.Warnings = append(out.Warnings,
			"every fused input folded to zero; there is nothing to draw")
	}
	return out, nil
}

// mergeLabels folds one input's label summaries into the fused result,
// summing by key. Labels are provenance, not structure — see Fold — so two
// inputs observing the same key are describing the same processes and their
// totals add.
func mergeLabels(out *Result, r *Result) {
	for _, ls := range r.Labels {
		i := slices.IndexFunc(out.Labels, func(e LabelSummary) bool { return e.Key == ls.Key })
		if i < 0 {
			out.Labels = append(out.Labels, ls)
			continue
		}
		dst := &out.Labels[i]
		dst.Total += ls.Total
		dst.Count += ls.Count
		for _, v := range ls.Top {
			j := slices.IndexFunc(dst.Top, func(e LabelValue) bool { return e.Value == v.Value })
			if j < 0 {
				dst.Top = append(dst.Top, v)
				dst.Distinct++
				continue
			}
			dst.Top[j].Total += v.Total
			dst.Top[j].Count += v.Count
		}
		slices.SortFunc(dst.Top, func(a, b LabelValue) int { return cmp.Compare(b.Total, a.Total) })
	}
}
