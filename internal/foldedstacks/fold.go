// Package foldedstacks turns a pprof profile into folded stacks — the
// `root;mid;leaf <value>` form a flame graph consumes.
//
// It exists because perf-agent writes pprof and nothing else, so every
// flame graph previously required an out-of-tree shell pipeline
// (`pprof -traces | awk ...`) that no test could cover and that silently
// mangled C++ symbol names containing spaces.
//
// Two things in here are deliberate and easy to get wrong:
//
// Stack order. The pprof proto says Sample.Location[0] is the leaf.
// perf-agent does not follow that: profile/profiler.go and
// offcpu/profiler.go both call pprof.Reverse before handing the stack to
// the builder, and gpu/projection.go builds its frames root-first, so in
// every profile this repo writes Location[0] is the ROOT. Folding with the
// wrong assumption does not fail — it draws a perfectly plausible flame
// graph upside down. So the order is an explicit option, defaulting to
// what this repo writes, and Result.StackOrder records the choice so the
// renderer can state it on the page instead of leaving the reader to guess.
//
// Degeneracy. A profile with no samples, or whose samples all carry zero
// at the chosen value index, folds to nothing. Fold returns that as a
// Result with Total == 0 and a Warning, not as an error and never as a
// silently empty success: the caller is expected to render the warning.
package foldedstacks

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	"github.com/google/pprof/profile"
)

// StackOrder describes how a profile stores frames within Sample.Location.
type StackOrder int

const (
	// RootFirst means Location[0] is the outermost frame. Every profile
	// perf-agent writes is RootFirst; see the package comment.
	RootFirst StackOrder = iota
	// LeafFirst means Location[0] is the innermost frame, which is what the
	// pprof proto specifies and what foreign profiles (Go runtime, perf)
	// use.
	LeafFirst
)

func (o StackOrder) String() string {
	if o == LeafFirst {
		return "leaf-first"
	}
	return "root-first"
}

// UnknownFrame is the placeholder for a location that carries neither a
// function name nor an address.
const UnknownFrame = "[unknown]"

// Options configures Fold.
type Options struct {
	// SampleIndex selects which of Profile.SampleType to fold. Negative
	// means "choose automatically" — see chooseSampleIndex.
	SampleIndex int

	// StackOrder is how the profile stores Sample.Location. Defaults to
	// RootFirst, which is what perf-agent writes.
	StackOrder StackOrder

	// InexactLabels marks samples whose stack attribution is not a
	// measurement. Each entry is a label key and the set of values that
	// mean "inexact"; an empty value set means "any value present".
	// Fold sums the value of matching samples into Stack.Inexact so the
	// renderer can say which part of the picture is inferred. Defaults to
	// DefaultInexactLabels.
	InexactLabels []InexactRule

	// MaxLabelValues caps how many distinct values are retained per label
	// key in the summary. Zero means DefaultMaxLabelValues.
	MaxLabelValues int
}

// InexactRule flags samples whose stack is attributed by inference.
type InexactRule struct {
	Key string
	// Values that mean "inexact". Empty means the key's mere presence does.
	Values []string
}

// DefaultMaxLabelValues bounds the per-key value list in a label summary.
const DefaultMaxLabelValues = 8

// UnsampledLaunchFrame is the root frame gpu/projection.go emits for an
// execution whose launch did not carry a sampled CPU stack. Its GPU time is
// measured; it simply has no caller, and none is borrowed from a sampled
// sibling. Fold reports the share of the profile sitting under it because a
// reader looking at the graph would otherwise read "6.6% of GPU time comes
// from this call path" when the truth is "this call path is the 1-in-N of
// launches whose stack we kept".
const UnsampledLaunchFrame = "[gpu:launch unsampled]"

// SamplePeriodLabel is the launch sampling denominator gpu/projection.go
// attaches to the sampled population only.
const SamplePeriodLabel = "gpu_sample_period"

// DefaultInexactLabels encodes gpu/projection.go's honesty contract:
// gpu_join is written unconditionally as exactly "exact", "heuristic" or
// "unmatched", and gpu_ambiguous appears only when the join picked one of
// several candidates. A heuristic or unmatched join means the CPU call path
// under [gpu:launch] was guessed or is absent — which is precisely the part
// of a flame graph a reader would otherwise read as measured fact.
func DefaultInexactLabels() []InexactRule {
	return []InexactRule{
		{Key: "gpu_join", Values: []string{"heuristic", "unmatched"}},
		{Key: "gpu_ambiguous", Values: []string{"true"}},
	}
}

// Stack is one folded stack.
type Stack struct {
	// Frames run root-first regardless of the input profile's order.
	Frames []string
	// Modules is parallel to Frames: the mapping file each frame came
	// from, or "" when the profile does not say. perf-agent routes kernel
	// frames through a "[kernel]" sentinel mapping and JIT frames through
	// "[jit]", so this is the only channel that identifies those two
	// classes from the profile itself rather than by guessing at a symbol
	// name. Consumers must tolerate "" everywhere: the GPU builder emits
	// a single empty mapping for every location.
	Modules []string
	// Value is the summed value at the chosen sample index.
	Value int64
	// Inexact is the part of Value contributed by samples matched by
	// Options.InexactLabels. Always <= Value.
	Inexact int64
}

// LabelValue is one observed value of a pprof label.
type LabelValue struct {
	Value string
	Total int64
	Count int
}

// LabelSummary aggregates one pprof label key across the whole profile.
// Labels never enter the folded frames — see the Fold doc comment — so this
// is the only place they survive into the rendered page.
type LabelSummary struct {
	Key string
	// Distinct is the number of distinct values observed, which may exceed
	// len(Top).
	Distinct int
	// Total is the summed sample value carrying this key.
	Total int64
	// Count is the number of profile samples carrying this key.
	Count int
	// Top holds the highest-Total values, capped by Options.MaxLabelValues
	// and sorted descending.
	Top []LabelValue
}

// Result is everything the renderer needs, including the reasons a picture
// might be misleading.
type Result struct {
	Stacks []Stack

	// Total is the sum of Stack.Value.
	Total int64
	// InexactTotal is the sum of Stack.Inexact.
	InexactTotal int64
	// UnsampledLaunchTotal is the part of Total whose stack begins at
	// UnsampledLaunchFrame: measured GPU time with no CPU caller.
	UnsampledLaunchTotal int64

	// SampleTypeName and Unit describe the folded value axis, e.g.
	// ("gpu", "nanoseconds"). The renderer must print these: labelling
	// 5,987,854 nanoseconds as "samples" is a lie a flame graph makes very
	// easy to tell.
	SampleTypeName string
	Unit           string
	SampleIndex    int
	// SampleTypes lists every type the profile offered, so a page can say
	// which one it is not showing.
	SampleTypes []string

	StackOrder StackOrder

	// Samples is the number of profile.Sample records read.
	Samples int
	// ZeroValueSamples were skipped: they carry 0 at the chosen index.
	ZeroValueSamples int
	// EmptyStackSamples carried a non-zero value but no locations; they are
	// folded under UnknownFrame rather than dropped.
	EmptyStackSamples int
	// Frames is the number of frame slots emitted across all folded stacks.
	Frames int
	// AddressOnlyFrames is how many of those carry no symbol — a bare
	// address, or UnknownFrame. A flame graph that quietly dropped these
	// would look cleaner precisely when symbolization worked worst.
	AddressOnlyFrames int
	// InlinedFrames is how many frames came from a Location's second and
	// later Line entries.
	InlinedFrames int
	// MaxDepth is the deepest folded stack.
	MaxDepth int

	Labels []LabelSummary

	// Warnings are reader-facing statements about why the picture may be
	// empty, partial or misleading. Never empty when Total == 0.
	Warnings []string
}

// Degenerate reports whether there is nothing worth drawing.
func (r *Result) Degenerate() bool { return r == nil || r.Total <= 0 || len(r.Stacks) == 0 }

// Fold converts a pprof profile into folded stacks.
//
// Labels are deliberately NOT folded into frames. §8 of the GPU profiling
// v2 design ("Output representation") rules that frames carry the real CPU
// stack, then [gpu:launch], then the kernel name, and that everything which
// would fragment that identity — stall reason, PC, queue, device,
// correlation ID, cgroup, pod UID, container ID — is a label. Folding
// gpu_correlation into the path would give this profile 4000 distinct
// leaves instead of 3 and destroy the aggregation the flame graph exists to
// show. They are summarised in Result.Labels instead, so they are visible
// without being structural.
func Fold(p *profile.Profile, opts Options) (*Result, error) {
	if p == nil {
		return nil, fmt.Errorf("foldedstacks: nil profile")
	}
	if len(p.SampleType) == 0 {
		return nil, fmt.Errorf("foldedstacks: profile declares no sample types")
	}

	idx := opts.SampleIndex
	if idx < 0 {
		idx = chooseSampleIndex(p)
	}
	if idx >= len(p.SampleType) {
		return nil, fmt.Errorf("foldedstacks: sample index %d out of range (profile has %d sample types)", idx, len(p.SampleType))
	}
	maxLabelValues := opts.MaxLabelValues
	if maxLabelValues <= 0 {
		maxLabelValues = DefaultMaxLabelValues
	}
	rules := opts.InexactLabels
	if rules == nil {
		rules = DefaultInexactLabels()
	}

	res := &Result{
		SampleTypeName: p.SampleType[idx].Type,
		Unit:           p.SampleType[idx].Unit,
		SampleIndex:    idx,
		StackOrder:     opts.StackOrder,
		Samples:        len(p.Sample),
	}
	for _, st := range p.SampleType {
		res.SampleTypes = append(res.SampleTypes, st.Type+"/"+st.Unit)
	}

	agg := make(map[string]*Stack, len(p.Sample))
	var order []string
	labels := make(map[string]map[string]*LabelValue)
	labelTotals := make(map[string]*LabelSummary)

	for _, s := range p.Sample {
		if idx >= len(s.Value) {
			return nil, fmt.Errorf("foldedstacks: sample carries %d values, need index %d", len(s.Value), idx)
		}
		v := s.Value[idx]
		if v < 0 {
			// A negative value is a diff profile ("pprof -diff_base").
			// A flame graph has no way to draw a negative rectangle, and
			// scaling or clamping it would present a subtraction as a
			// measurement. Refuse rather than draw a confident lie.
			return nil, fmt.Errorf("foldedstacks: sample has negative value %d at index %d; a flame graph cannot represent a differential profile", v, idx)
		}

		accumulateLabels(labels, labelTotals, s.Label, v)

		if v == 0 {
			res.ZeroValueSamples++
			continue
		}

		frames, mods, stats := framesOf(s, opts.StackOrder)
		if len(frames) == 0 {
			res.EmptyStackSamples++
			frames = []string{UnknownFrame}
			mods = []string{""}
			stats.addressOnly = 1
		}
		res.Frames += len(frames)
		res.AddressOnlyFrames += stats.addressOnly
		res.InlinedFrames += stats.inlined
		if len(frames) > res.MaxDepth {
			res.MaxDepth = len(frames)
		}

		key := strings.Join(frames, "\x00")
		st := agg[key]
		if st == nil {
			st = &Stack{Frames: frames, Modules: mods}
			agg[key] = st
			order = append(order, key)
		}
		st.Value += v
		if matchesInexact(s.Label, rules) {
			st.Inexact += v
		}
	}

	res.Stacks = make([]Stack, 0, len(order))
	for _, k := range order {
		st := agg[k]
		res.Stacks = append(res.Stacks, *st)
		res.Total += st.Value
		res.InexactTotal += st.Inexact
		if len(st.Frames) > 0 && st.Frames[0] == UnsampledLaunchFrame {
			res.UnsampledLaunchTotal += st.Value
		}
	}
	// Sort for a byte-stable rendering: two runs over the same profile must
	// produce the same HTML, or nobody can diff two flame graphs.
	slices.SortFunc(res.Stacks, func(a, b Stack) int {
		return cmp.Compare(strings.Join(a.Frames, "\x00"), strings.Join(b.Frames, "\x00"))
	})

	res.Labels = summariseLabels(labels, labelTotals, maxLabelValues)
	res.Warnings = buildWarnings(res, p)
	return res, nil
}

type frameStats struct {
	addressOnly int
	inlined     int
}

// framesOf renders one sample's locations as root-first frame names, with a
// parallel slice of the mapping file each frame came from.
func framesOf(s *profile.Sample, order StackOrder) ([]string, []string, frameStats) {
	var stats frameStats
	if len(s.Location) == 0 {
		return nil, nil, stats
	}
	out := make([]string, 0, len(s.Location))
	mods := make([]string, 0, len(s.Location))

	emit := func(loc *profile.Location) {
		mod := ""
		if loc != nil && loc.Mapping != nil {
			mod = loc.Mapping.File
		}
		if loc == nil {
			out = append(out, UnknownFrame)
			mods = append(mods, mod)
			stats.addressOnly++
			return
		}
		if len(loc.Line) == 0 {
			name := addressName(loc)
			out = append(out, name)
			mods = append(mods, mod)
			stats.addressOnly++
			return
		}
		// pprof: "the last entry represents the caller into which the
		// preceding entries were inlined", so Line[0] is innermost. Walking
		// backwards emits the inlined chain root-first, matching the frame
		// order we produce for locations.
		for i := len(loc.Line) - 1; i >= 0; i-- {
			name := lineName(loc, loc.Line[i])
			if isAddressOnly(name) {
				stats.addressOnly++
			}
			if i != len(loc.Line)-1 {
				stats.inlined++
			}
			out = append(out, name)
			mods = append(mods, mod)
		}
	}

	if order == LeafFirst {
		for i := len(s.Location) - 1; i >= 0; i-- {
			emit(s.Location[i])
		}
	} else {
		for _, loc := range s.Location {
			emit(loc)
		}
	}
	return out, mods, stats
}

func lineName(loc *profile.Location, ln profile.Line) string {
	if ln.Function != nil {
		if n := strings.TrimSpace(ln.Function.Name); n != "" {
			return n
		}
		if n := strings.TrimSpace(ln.Function.SystemName); n != "" {
			return n
		}
		if f := strings.TrimSpace(ln.Function.Filename); f != "" {
			if ln.Line > 0 {
				return fmt.Sprintf("%s:%d", f, ln.Line)
			}
			return f
		}
	}
	return addressName(loc)
}

func addressName(loc *profile.Location) string {
	if loc != nil && loc.Address != 0 {
		return fmt.Sprintf("0x%x", loc.Address)
	}
	if loc != nil && loc.Mapping != nil && loc.Mapping.File != "" {
		return fmt.Sprintf("%s+0x%x", loc.Mapping.File, loc.Address)
	}
	return UnknownFrame
}

// isAddressOnly reports whether a frame name carries no symbol. perf-agent's
// symbolizer already formats unresolved PCs as "0x7f2c945ace62" and missing
// frames as "[unknown]", so a name-shaped test is the only way to count them
// once the profile is written; there is no separate "unsymbolized" bit in
// pprof to consult.
func isAddressOnly(name string) bool {
	if name == UnknownFrame || name == "" {
		return true
	}
	if !strings.HasPrefix(name, "0x") || len(name) < 3 {
		return false
	}
	for _, r := range name[2:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func matchesInexact(labels map[string][]string, rules []InexactRule) bool {
	for _, rule := range rules {
		vals, ok := labels[rule.Key]
		if !ok {
			continue
		}
		if len(rule.Values) == 0 {
			return true
		}
		for _, v := range vals {
			if slices.Contains(rule.Values, v) {
				return true
			}
		}
	}
	return false
}

func accumulateLabels(byKey map[string]map[string]*LabelValue, totals map[string]*LabelSummary, labels map[string][]string, v int64) {
	for k, vals := range labels {
		sum := totals[k]
		if sum == nil {
			sum = &LabelSummary{Key: k}
			totals[k] = sum
			byKey[k] = make(map[string]*LabelValue)
		}
		sum.Total += v
		sum.Count++
		for _, val := range vals {
			lv := byKey[k][val]
			if lv == nil {
				lv = &LabelValue{Value: val}
				byKey[k][val] = lv
			}
			lv.Total += v
			lv.Count++
		}
	}
}

func summariseLabels(byKey map[string]map[string]*LabelValue, totals map[string]*LabelSummary, maxValues int) []LabelSummary {
	out := make([]LabelSummary, 0, len(totals))
	for k, sum := range totals {
		vals := make([]LabelValue, 0, len(byKey[k]))
		for _, lv := range byKey[k] {
			vals = append(vals, *lv)
		}
		slices.SortFunc(vals, func(a, b LabelValue) int {
			if c := cmp.Compare(b.Total, a.Total); c != 0 {
				return c
			}
			return cmp.Compare(a.Value, b.Value)
		})
		s := *sum
		s.Distinct = len(vals)
		if len(vals) > maxValues {
			vals = vals[:maxValues]
		}
		s.Top = vals
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func buildWarnings(res *Result, p *profile.Profile) []string {
	var w []string
	switch {
	case res.Samples == 0:
		w = append(w, "This profile contains no samples at all. Nothing was collected, so there is nothing to draw. That is a fact about the profile, not a statement that the target was idle.")
	case res.Total == 0:
		w = append(w, fmt.Sprintf("Every one of the %d samples carries the value 0 for %s/%s. There is nothing to draw.",
			res.Samples, res.SampleTypeName, res.Unit))
	}
	if res.ZeroValueSamples > 0 && res.Total > 0 {
		w = append(w, fmt.Sprintf("%d of %d samples carry the value 0 for %s/%s and are not drawn.",
			res.ZeroValueSamples, res.Samples, res.SampleTypeName, res.Unit))
	}
	if res.EmptyStackSamples > 0 {
		w = append(w, fmt.Sprintf("%d samples carry a value but no stack; they are folded under %s rather than dropped.",
			res.EmptyStackSamples, UnknownFrame))
	}
	if res.AddressOnlyFrames > 0 {
		w = append(w, fmt.Sprintf("%d of %d frame slots (%.1f%%) have no symbol and are drawn as a raw address or %s.",
			res.AddressOnlyFrames, res.Frames, pct(int64(res.AddressOnlyFrames), int64(res.Frames)), UnknownFrame))
	}
	if res.InexactTotal > 0 {
		w = append(w, fmt.Sprintf("%.1f%% of the total is attributed to its CPU call path by inference, not measurement (gpu_join is heuristic or unmatched, or the join was ambiguous). Those frames name a plausible caller, not an observed one.",
			pct(res.InexactTotal, res.Total)))
	}
	if res.Total > 0 {
		w = append(w, gpuSamplingWarnings(res)...)
	}
	if len(p.SampleType) > 1 {
		w = append(w, fmt.Sprintf("This profile carries %d value axes (%s); only %s/%s is drawn.",
			len(p.SampleType), strings.Join(res.SampleTypes, ", "), res.SampleTypeName, res.Unit))
	}
	return w
}

// gpuSamplingWarnings states what launch sampling does to a GPU flame
// graph's shape, because the shape alone is actively misleading without it.
//
// gpu/projection.go samples launch STACKS (one launch in SamplePeriod carries
// one) but records every execution. So the CPU call path under [gpu:launch]
// is a sample of launches, while the width beside it under
// [gpu:launch unsampled] is the rest of the measured GPU time with no caller.
// A reader who takes the attributed width as "the share of GPU time this call
// path is responsible for" is wrong by roughly the sampling period. And
// neither population is scaled: multiplying by the period would turn a
// measurement into an estimate and present it as fact, which is precisely
// what that package refuses to do on the reader's behalf.
func gpuSamplingWarnings(res *Result) []string {
	var period string
	var periodSamples int
	for _, l := range res.Labels {
		if l.Key == SamplePeriodLabel && len(l.Top) > 0 {
			period = l.Top[0].Value
			periodSamples = l.Count
		}
	}
	if res.UnsampledLaunchTotal == 0 && period == "" {
		return nil
	}

	var w []string
	if res.UnsampledLaunchTotal > 0 {
		w = append(w, fmt.Sprintf(
			"%.1f%% of the total sits under %s: measured GPU time whose launch was not one of the sampled ones, so it has no CPU caller and none is borrowed from a sampled sibling.",
			pct(res.UnsampledLaunchTotal, res.Total), UnsampledLaunchFrame))
	}
	if period != "" {
		w = append(w, fmt.Sprintf(
			"%d samples carry %s=%s: one launch in %s contributed a CPU stack. The call path beneath [gpu:launch] is therefore a sample of launches, not a sample of GPU time - do not read its width as the share of GPU work that call path is responsible for. Nothing here is scaled by %s; every value is a measured duration.",
			periodSamples, SamplePeriodLabel, period, period, period))
	}
	return w
}

func pct(part, whole int64) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

// chooseSampleIndex picks the value axis to fold.
//
// The rule, in order: honour Profile.DefaultSampleType when it names a real
// type; otherwise take the LAST sample type, which is what google/pprof's
// own driver does and which picks the meaningful axis for the two-axis
// profiles this repo can emit (alloc_objects/count, alloc_space/bytes →
// bytes). Every profiling mode perf-agent ships today — cpu, offcpu, gpu —
// declares exactly one type, so this only matters for the memory builder
// and for foreign profiles.
func chooseSampleIndex(p *profile.Profile) int {
	if p.DefaultSampleType != "" {
		for i, st := range p.SampleType {
			if st.Type == p.DefaultSampleType {
				return i
			}
		}
	}
	return len(p.SampleType) - 1
}

// WriteFolded emits the classic `a;b;c 123` text form, one stack per line,
// in Result order (which Fold has already sorted).
//
// The text form cannot carry Stack.Inexact, so a folded file loses the
// distinction between a measured and an inferred call path. Anything that
// needs that must read the Result. Frame names containing ';' or a newline
// would corrupt the format — the folded format has no escape — so they are
// substituted and reported in the returned count.
func (r *Result) WriteFolded(w io.Writer) (substituted int, err error) {
	bw := &countingWriter{w: w}
	for _, st := range r.Stacks {
		for i, f := range st.Frames {
			clean := sanitizeFolded(f)
			if clean != f {
				substituted++
			}
			if i > 0 {
				bw.writeString(";")
			}
			bw.writeString(clean)
		}
		bw.writeString(fmt.Sprintf(" %d\n", st.Value))
	}
	return substituted, bw.err
}

func sanitizeFolded(s string) string {
	if !strings.ContainsAny(s, ";\n\r") {
		return s
	}
	return foldedReplacer.Replace(s)
}

// foldedSemicolon (U+FF1B FULLWIDTH SEMICOLON) stands in for a literal ";"
// inside a frame name, which would otherwise read as a frame separator.
const foldedSemicolon = "\uFF1B"

var foldedReplacer = strings.NewReplacer(";", foldedSemicolon, "\n", " ", "\r", " ")

type countingWriter struct {
	w   io.Writer
	err error
}

func (c *countingWriter) writeString(s string) {
	if c.err != nil {
		return
	}
	_, c.err = io.WriteString(c.w, s)
}
