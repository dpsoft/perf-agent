package pprof

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/cespare/xxhash/v2"
	"github.com/google/pprof/profile"

	"github.com/klauspost/compress/gzip"
)

var (
	gzipWriterPool = sync.Pool{
		New: func() any {
			res, err := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
			if err != nil {
				panic(err)
			}
			return res
		},
	}
)

type SampleType uint32

var SampleTypeCpu = SampleType(0)
var SampleTypeMem = SampleType(1)
var SampleTypeOffCpu = SampleType(2)

type SampleAggregation bool

var (
	// SampleAggregated mean samples are accumulated in ebpf, no need to dedup these
	SampleAggregated = SampleAggregation(true)
)

type CollectProfilesCallback func(sample ProfileSample)

type SamplesCollector interface {
	CollectProfiles(callback CollectProfilesCallback) error
}

// Frame is a single symbolized stack frame. Only Name is required; the other
// fields are populated when the symbolizer can provide them (DWARF for native
// binaries, perf-map decoding for Python/Node runtimes).
type Frame struct {
	Name   string
	File   string
	Line   uint32
	Module string
}

// FrameFromName is a convenience constructor for callers that only know the
// function name (synthetic "[unknown]" frames, tests).
func FrameFromName(name string) Frame { return Frame{Name: name} }

// FramesFromNames converts a []string of symbol names into []Frame. Intended
// for tests and for callers migrating incrementally.
func FramesFromNames(names []string) []Frame {
	out := make([]Frame, len(names))
	for i, n := range names {
		out[i] = Frame{Name: n}
	}
	return out
}

type ProfileSample struct {
	Pid         uint32
	SampleType  SampleType
	Aggregation SampleAggregation
	Stack       []Frame
	Value       uint64
	Value2      uint64
}

type BuildersOptions struct {
	SampleRate    int64
	PerPIDProfile bool
	Comments      []string // Profile-level comments/tags
}

type builderHashKey struct {
	pid        uint32
	sampleType SampleType
}

type ProfileBuilders struct {
	Builders map[builderHashKey]*ProfileBuilder
	opt      BuildersOptions
}

func NewProfileBuilders(options BuildersOptions) *ProfileBuilders {
	return &ProfileBuilders{Builders: make(map[builderHashKey]*ProfileBuilder), opt: options}
}

func Collect(builders *ProfileBuilders, collector SamplesCollector) error {
	return collector.CollectProfiles(func(sample ProfileSample) {
		builders.AddSample(&sample)
	})
}

func (b *ProfileBuilders) AddSample(sample *ProfileSample) {
	bb := b.BuilderForSample(sample)
	if sample.Aggregation == SampleAggregated {
		bb.CreateSample(sample)
	} else {
		bb.CreateSampleOrAddValue(sample)
	}
}

func (b *ProfileBuilders) BuilderForSample(sample *ProfileSample) *ProfileBuilder {
	k := builderHashKey{sampleType: sample.SampleType}
	if b.opt.PerPIDProfile {
		k.pid = sample.Pid
	}
	res := b.Builders[k]
	if res != nil {
		return res
	}

	var sampleType []*profile.ValueType
	var periodType *profile.ValueType
	var period int64
	switch sample.SampleType {
	case SampleTypeCpu:
		sampleType = []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}}
		periodType = &profile.ValueType{Type: "cpu", Unit: "nanoseconds"}
		period = time.Second.Nanoseconds() / b.opt.SampleRate
	case SampleTypeOffCpu:
		sampleType = []*profile.ValueType{{Type: "offcpu", Unit: "nanoseconds"}}
		periodType = &profile.ValueType{Type: "offcpu", Unit: "nanoseconds"}
		period = 1 // Direct nanosecond values, not sampled
	default:
		sampleType = []*profile.ValueType{{Type: "alloc_objects", Unit: "count"}, {Type: "alloc_space", Unit: "bytes"}}
		periodType = &profile.ValueType{Type: "space", Unit: "bytes"}
		period = 512 * 1024 // todo
	}
	builder := &ProfileBuilder{
		locations:          make(map[frameKey]*profile.Location),
		functions:          make(map[frameKey]*profile.Function),
		sampleHashToSample: make(map[uint64]*profile.Sample),
		Profile: &profile.Profile{
			Mapping: []*profile.Mapping{
				{
					ID: 1,
				},
			},
			SampleType: sampleType,
			Period:     period,
			PeriodType: periodType,
			TimeNanos:  time.Now().UnixNano(),
			Comments:   b.opt.Comments,
		},
		tmpLocationIDs: make([]uint64, 0, 128),
		tmpLocations:   make([]*profile.Location, 0, 128),
	}
	res = builder
	b.Builders[k] = res
	return res
}

// frameKey dedupes locations and functions. Line is included so two PCs at
// different source lines within the same function produce distinct locations
// (otherwise the first-seen line would silently win for all samples).
type frameKey struct {
	Name   string
	File   string
	Module string
	Line   uint32
}

// functionKey is the subset of frameKey that identifies a pprof Function
// (Line is per-location, not per-function).
type functionKey struct {
	Name   string
	File   string
	Module string
}

func (f Frame) locationKey() frameKey {
	return frameKey{Name: f.Name, File: f.File, Module: f.Module, Line: f.Line}
}

func (f Frame) functionKey() frameKey {
	return frameKey{Name: f.Name, File: f.File, Module: f.Module}
}

type ProfileBuilder struct {
	locations          map[frameKey]*profile.Location
	functions          map[frameKey]*profile.Function
	sampleHashToSample map[uint64]*profile.Sample
	Profile            *profile.Profile
	tmpLocations       []*profile.Location
	tmpLocationIDs     []uint64
}

func (p *ProfileBuilder) CreateSample(inputSample *ProfileSample) {
	sample := p.newSample(inputSample)
	p.addValue(inputSample, sample)
	for i, f := range inputSample.Stack {
		sample.Location[i] = p.addLocation(f)
	}
	p.Profile.Sample = append(p.Profile.Sample, sample)
}

func (p *ProfileBuilder) CreateSampleOrAddValue(inputSample *ProfileSample) {
	p.tmpLocations = p.tmpLocations[:0]
	p.tmpLocationIDs = p.tmpLocationIDs[:0]
	for _, f := range inputSample.Stack {
		loc := p.addLocation(f)
		p.tmpLocations = append(p.tmpLocations, loc)
		p.tmpLocationIDs = append(p.tmpLocationIDs, loc.ID)
	}
	h := xxhash.Sum64(uint64Bytes(p.tmpLocationIDs))
	sample := p.sampleHashToSample[h]
	if sample != nil {
		p.addValue(inputSample, sample)
		return
	}
	sample = p.newSample(inputSample)
	p.addValue(inputSample, sample)
	copy(sample.Location, p.tmpLocations)
	p.sampleHashToSample[h] = sample
	p.Profile.Sample = append(p.Profile.Sample, sample)
}

func (p *ProfileBuilder) addLocation(frame Frame) *profile.Location {
	// Runtimes that symbolize via perf-maps pack name+file into a single
	// string in Frame.Name. Decode here so pprof's Function.Filename and
	// Line fields populate for any known runtime.
	frame = decodePerfMapFrame(frame)

	key := frame.locationKey()
	if loc, ok := p.locations[key]; ok {
		return loc
	}

	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: p.Profile.Mapping[0],
		Line: []profile.Line{
			{
				Function: p.addFunction(frame),
				Line:     int64(frame.Line),
			},
		},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}

func (p *ProfileBuilder) addFunction(frame Frame) *profile.Function {
	key := frame.functionKey()
	if f, ok := p.functions[key]; ok {
		return f
	}

	id := uint64(len(p.Profile.Function) + 1)
	f := &profile.Function{
		ID:       id,
		Name:     frame.Name,
		Filename: frame.File,
	}
	p.Profile.Function = append(p.Profile.Function, f)
	p.functions[key] = f
	return f
}

func (p *ProfileBuilder) Write(dst io.Writer) (int64, error) {
	gzipWriter := gzipWriterPool.Get().(*gzip.Writer)
	gzipWriter.Reset(dst)
	defer func() {
		gzipWriter.Reset(io.Discard)
		gzipWriterPool.Put(gzipWriter)
	}()
	err := p.Profile.WriteUncompressed(gzipWriter)
	if err != nil {
		return 0, fmt.Errorf("ebpf profile encode %w", err)
	}
	err = gzipWriter.Close()
	if err != nil {
		return 0, fmt.Errorf("ebpf profile encode %w", err)
	}
	return 0, nil
}

func uint64Bytes(s []uint64) []byte {
	if len(s) == 0 {
		return nil
	}
	// Use unsafe.SliceData instead of deprecated reflect.SliceHeader
	return unsafe.Slice((*byte)(unsafe.Pointer(&s[0])), len(s)*8)
}
func (p *ProfileBuilder) newSample(inputSample *ProfileSample) *profile.Sample {
	sample := new(profile.Sample)
	if inputSample.SampleType == SampleTypeCpu || inputSample.SampleType == SampleTypeOffCpu {
		sample.Value = []int64{0}
	} else {
		sample.Value = []int64{0, 0}
	}
	sample.Location = make([]*profile.Location, len(inputSample.Stack))
	return sample
}

func (p *ProfileBuilder) addValue(inputSample *ProfileSample, sample *profile.Sample) {
	switch inputSample.SampleType {
	case SampleTypeCpu:
		sample.Value[0] += int64(inputSample.Value) * p.Profile.Period
	case SampleTypeOffCpu:
		// Off-CPU values are already in nanoseconds, no scaling needed
		sample.Value[0] += int64(inputSample.Value)
	default:
		sample.Value[0] += int64(inputSample.Value)
		sample.Value[1] += int64(inputSample.Value2)
	}
}

// Reverse reverses a slice in place
func Reverse[S ~[]E, E any](s S) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// decodePerfMapFrame expands a Frame whose Name was produced by a runtime
// perf-map (where the symbolizer returns the raw map line as a single
// string) into a richer Frame with File/Line populated.
//
// Applied only when File is still empty (DWARF-symbolized frames already
// have File/Line set). Runtimes without a matching case fall through
// unchanged — strictly more information than before, never less.
//
// TODO(blazesym): once the Go binding exposes structured perf-map symbol
// info, remove this and pull fields directly from blazesym.
//
// Recognized formats:
//
//	Python 3.12+ (PYTHONPERFSUPPORT):
//	    py::<qualname>:<absolute_file.py>
//
//	Node.js (--perf-basic-prof):
//	    LazyCompile:~<name> <file.js>:<line>:<col>
//	    Function:<name>     <file.js>:<line>:<col>
//	    Builtin:<name>                (no file/line)
func decodePerfMapFrame(f Frame) Frame {
	if f.File != "" || f.Name == "" {
		return f
	}
	if dec, ok := decodePython(f.Name); ok {
		return mergeFrame(f, dec)
	}
	if dec, ok := decodeNode(f.Name); ok {
		return mergeFrame(f, dec)
	}
	return f
}

func mergeFrame(base, dec Frame) Frame {
	if dec.Name != "" {
		base.Name = dec.Name
	}
	if dec.File != "" {
		base.File = dec.File
	}
	if dec.Line != 0 {
		base.Line = dec.Line
	}
	return base
}

// decodePython parses "py::<qual>:<file.py>" emitted by CPython's
// PYTHONPERFSUPPORT. The qualname itself can contain "::" (e.g.
// "py::Class.method"), so we anchor on ":/" — Linux absolute paths always
// start with "/".
func decodePython(raw string) (Frame, bool) {
	if !strings.HasPrefix(raw, "py::") {
		return Frame{}, false
	}
	if idx := strings.LastIndex(raw, ":/"); idx != -1 {
		return Frame{Name: raw[:idx], File: raw[idx+1:]}, true
	}
	// Prefix matched but no path — keep name as-is.
	return Frame{Name: raw}, true
}

// decodeNode parses Node.js --perf-basic-prof lines. Returns ok=true when the
// shape is recognized, even if file/line are absent (Builtin:, Code:, Script:).
//
// Modern Node (v16+) emits "JS:<tier><name> <file>:<line>:<col>" where <tier>
// is a single marker character (~ lazy, ^ sparkplug, + baseline/other,
// * optimized). Older versions emit "LazyCompile:~<name>" or "Function:<name>".
// Builtin:/Code:/Script: carry no file info.
func decodeNode(raw string) (Frame, bool) {
	var prefix string
	switch {
	case strings.HasPrefix(raw, "JS:"):
		prefix = "JS:"
	case strings.HasPrefix(raw, "LazyCompile:"):
		prefix = "LazyCompile:"
	case strings.HasPrefix(raw, "Function:"):
		prefix = "Function:"
	case strings.HasPrefix(raw, "Builtin:"), strings.HasPrefix(raw, "Code:"), strings.HasPrefix(raw, "Script:"):
		return Frame{Name: raw}, true
	default:
		return Frame{}, false
	}
	tail := raw[len(prefix):]
	// Format: "<name> <file>:<line>:<col>" or "<name> <file>:<line>".
	sp := strings.IndexByte(tail, ' ')
	if sp == -1 {
		return Frame{Name: raw}, true
	}
	name := prefix + tail[:sp]
	loc := tail[sp+1:]

	// Rightmost ":<num>" is either line (no col) or col (with line preceding).
	// Parse both numeric suffixes; the leftmost of the two is the line.
	file, line := parseTrailingFileLine(loc)
	return Frame{Name: name, File: file, Line: line}, true
}

// parseTrailingFileLine splits "<file>:<line>" or "<file>:<line>:<col>" into
// file and line. Returns line=0 if no numeric suffix is found.
func parseTrailingFileLine(s string) (file string, line uint32) {
	file = s
	var nums [2]uint32
	var found int
	for found < 2 {
		colon := strings.LastIndexByte(file, ':')
		if colon == -1 || colon == len(file)-1 {
			break
		}
		n, err := strconv.ParseUint(file[colon+1:], 10, 32)
		if err != nil {
			break
		}
		nums[found] = uint32(n)
		found++
		file = file[:colon]
	}
	switch found {
	case 0:
		return s, 0
	case 1:
		// "<file>:<line>" — the single parsed number is the line.
		return file, nums[0]
	default:
		// "<file>:<line>:<col>" — nums[1] was parsed second (leftmost), so it's the line.
		return file, nums[1]
	}
}
