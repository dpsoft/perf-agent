package profilemodel

import "unsafe"

// Weights of the retained-memory estimates: the heap bytes one item keeps
// reachable, size-class rounding and map overhead included. They were
// calibrated against the measured heap of a 100k-sample profile and of a
// 5000-frame chain, and their views (TestRetainedEstimatesTrackTheHeap
// holds every estimate within [0.5x, 2x] of it).
const (
	// A profile: the parsed pprof structure.
	retainedProfileBase    = 4 << 10 // the Profile, its pprof header and sample types
	retainedPerSample      = 136     // a *profile.Sample, its pointer and its value slice
	retainedPerLocationRef = 8       // one entry of a sample's location slice
	retainedPerLocation    = 80      // a *profile.Location and its pointer
	retainedPerLine        = 32      // a profile.Line
	retainedPerFunction    = 104     // a *profile.Function and its pointer
	retainedPerMapping     = 160     // a *profile.Mapping and its pointer
	// A sample with labels holds a map of them; each label is a map slot,
	// a value slice and the decoder's copy of it.
	retainedPerLabelledSample = 352
	retainedPerLabel          = 64
	// Executions: a per-sample slice header and 8 bytes per ordinal.
	retainedPerExecSample = 24
	retainedPerExecution  = 8

	// A view: its nodes and their index.
	retainedViewBase      = 1 << 10
	retainedPerNode       = 344 // a *Node, its 16-byte ID and its slot in the node index
	retainedPerChild      = 8   // one slot of a children slice
	retainedPerCallSite   = 64  // a LocationID
	retainedPerSampleSlot = 8   // one slot of a node's sample-index slice
)

// RetainedBytes estimates the heap p keeps reachable: its parsed profile,
// every string in it (names, labels, comments) counted once, and its
// execution ordinals. It is computed once, at load, from
// counts, so it costs nothing to ask for and never changes.
func (p *Profile) RetainedBytes() int64 { return p.retained }

// RetainedBytes estimates the heap v keeps reachable beyond its profile:
// its nodes, their slices and the node index. It is computed once, when
// the view is built, from counts.
func (v *View) RetainedBytes() int64 { return v.retained }

// profileRetained is p's estimate, from its counts and its strings.
func profileRetained(p *Profile) int64 {
	src := p.Source
	strs := stringBytes{}
	n := int64(retainedProfileBase)
	for _, st := range src.SampleType {
		strs.add(st.Type, st.Unit)
	}
	if pt := src.PeriodType; pt != nil {
		strs.add(pt.Type, pt.Unit)
	}
	strs.add(src.DropFrames, src.KeepFrames, src.DefaultSampleType, src.DocURL)
	strs.add(src.Comments...)
	for _, s := range src.Sample {
		n += retainedPerSample + retainedPerLocationRef*int64(len(s.Location)) + 8*int64(len(s.Value))
		labels := 0
		for k, vs := range s.Label {
			labels += len(vs)
			strs.add(k)
			strs.add(vs...)
		}
		for k, vs := range s.NumLabel {
			labels += len(vs)
			strs.add(k)
		}
		for k, us := range s.NumUnit {
			strs.add(k)
			strs.add(us...)
		}
		if labels > 0 {
			n += retainedPerLabelledSample + retainedPerLabel*int64(labels)
		}
	}
	for _, l := range src.Location {
		n += retainedPerLocation + retainedPerLine*int64(len(l.Line))
	}
	for _, f := range src.Function {
		n += retainedPerFunction
		strs.add(f.Name, f.SystemName, f.Filename)
	}
	for _, m := range src.Mapping {
		n += retainedPerMapping
		strs.add(m.File, m.BuildID, m.KernelRelocationSymbol)
	}
	if p.execOf != nil {
		n += retainedPerExecSample * int64(len(p.execOf))
		for _, ex := range p.execOf {
			n += retainedPerExecution * int64(cap(ex))
		}
	}
	return n + strs.total
}

// stringBytes sums the bytes of distinct string storage. A parsed profile
// interns its strings in a table, so one string a million samples label
// with is one allocation: strings are told apart by where their bytes
// are, not by their text, and each is counted once at its longest.
type stringBytes struct {
	seen  map[*byte]int
	total int64
}

func (b *stringBytes) add(ss ...string) {
	for _, s := range ss {
		if s == "" {
			continue
		}
		if b.seen == nil {
			b.seen = map[*byte]int{}
		}
		d := unsafe.StringData(s)
		if had := b.seen[d]; len(s) > had {
			b.seen[d] = len(s)
			b.total += int64(len(s) - had)
		}
	}
}

// viewRetained is v's estimate, from its counts. Slices count at their
// capacity, which is what they hold.
func viewRetained(v *View) int64 {
	n := int64(retainedViewBase)
	for _, nd := range v.nodes {
		n += retainedPerNode +
			retainedPerChild*int64(cap(nd.Children)) +
			retainedPerCallSite*int64(cap(nd.CallSites)) +
			retainedPerSampleSlot*int64(cap(nd.samples)+cap(nd.zeroSamples))
	}
	return n
}
