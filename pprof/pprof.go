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

	"github.com/dpsoft/perf-agent/unwind/procmap"
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

// Frame is a single symbolized stack frame. Name is always populated;
// other fields are filled when the symbolizer (DWARF for native
// binaries, perf-map decoding for Python/Node runtimes) or the
// Resolver can provide them. Address carries the absolute PC from
// the BPF stack so Locations stay distinguishable across samples
// that symbolize to the same (file,line,func).
type Frame struct {
	Name   string
	File   string
	Line   uint32
	Module string

	Address  uint64
	BuildID  string
	MapStart uint64
	MapLimit uint64
	MapOff   uint64
	IsKernel bool
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
	Resolver      *procmap.Resolver // nil → fallback to name-based Location dedup
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
		resolver:           b.opt.Resolver,
		mappings:           make(map[mappingKey]*profile.Mapping),
		locations:          make(map[any]*profile.Location),
		functions:          make(map[any]*profile.Function),
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

// mappingKey interns per-binary pprof.Mapping entries. Two mappings
// with the same backing file but different load addresses (e.g., the
// same libc mapped into two PIDs with different ASLR slides) intern
// separately — pprof.Mapping's Start/Limit are absolute VAs.
type mappingKey struct {
	Path    string
	Start   uint64
	Limit   uint64
	Off     uint64
	BuildID string
}

// locationKey is the primary Location intern key. MappingID scopes
// Address to one binary so the same offset in two loaded copies
// dedups independently.
type locationKey struct {
	MappingID uint64
	Address   uint64 // binary-relative file offset (Address - MapStart + MapOff)
}

// locationFallbackKey is used when Address==0 (JIT runtime frames) or
// when the Resolver can't attribute the PC to any mapping. Falls back
// to the pre-S9 name/file/line dedup scheme.
type locationFallbackKey struct {
	Name, File, Module string
	Line               uint32
}

// functionKey interns pprof.Function entries per (binary, name).
// Adding MappingID means the same symbol name in two binaries (e.g.,
// main.main in a tool binary and a subprocess) produces two separate
// Functions — pprof-correct, but changes existing output fidelity.
type functionKey struct {
	MappingID uint64
	Name      string
}

var (
	kernelSentinel = procmap.Mapping{Path: "[kernel]"}
	jitSentinel    = procmap.Mapping{Path: "[jit]"}
)

// looksJIT returns true when frame.Name matches one of the perf-map
// runtime prefixes (Python, Node). decodePerfMapFrame zeros Address
// for these in Task 8; before then, the Address field may still be
// nonzero but we keep these on the [jit] sentinel because anonymous
// JIT mappings have no file-offset identity.
func looksJIT(f Frame) bool {
	return strings.HasPrefix(f.Name, "py::") ||
		strings.HasPrefix(f.Name, "JS:") ||
		strings.HasPrefix(f.Name, "LazyCompile:") ||
		strings.HasPrefix(f.Name, "Function:") ||
		strings.HasPrefix(f.Name, "Builtin:") ||
		strings.HasPrefix(f.Name, "Code:") ||
		strings.HasPrefix(f.Name, "Script:")
}

type ProfileBuilder struct {
	resolver           *procmap.Resolver
	mappings           map[mappingKey]*profile.Mapping
	locations          map[any]*profile.Location
	functions          map[any]*profile.Function
	sampleHashToSample map[uint64]*profile.Sample
	Profile            *profile.Profile
	tmpLocations       []*profile.Location
	tmpLocationIDs     []uint64
}

func (p *ProfileBuilder) CreateSample(inputSample *ProfileSample) {
	sample := p.newSample(inputSample)
	p.addValue(inputSample, sample)
	for i, f := range inputSample.Stack {
		sample.Location[i] = p.addLocation(f, inputSample.Pid)
	}
	p.Profile.Sample = append(p.Profile.Sample, sample)
}

func (p *ProfileBuilder) CreateSampleOrAddValue(inputSample *ProfileSample) {
	p.tmpLocations = p.tmpLocations[:0]
	p.tmpLocationIDs = p.tmpLocationIDs[:0]
	for _, f := range inputSample.Stack {
		loc := p.addLocation(f, inputSample.Pid)
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

func (p *ProfileBuilder) addLocation(frame Frame, pid uint32) *profile.Location {
	frame = decodePerfMapFrame(frame)

	// 1. Kernel frames use a shared sentinel mapping regardless of PID.
	if frame.IsKernel {
		mapping := p.addMapping(kernelSentinel, frame)
		return p.addLocationByAddr(mapping, frame)
	}

	// 2. Perf-map runtime frames (Python/Node JIT) live in anonymous
	// mappings with meaningless file offsets. After Task 8,
	// decodePerfMapFrame zeros Address for these; until then, the
	// Address may be nonzero but we still group them under [jit].
	if frame.Address == 0 && looksJIT(frame) {
		mapping := p.addMapping(jitSentinel, frame)
		return p.addLocationByFallback(mapping, frame)
	}

	// 3. Resolver-driven primary path.
	if frame.Address != 0 && p.resolver != nil {
		if m, ok := p.resolver.Lookup(pid, frame.Address); ok {
			frame.MapStart = m.Start
			frame.MapLimit = m.Limit
			frame.MapOff = m.Offset
			frame.BuildID = m.BuildID
			mapping := p.addMapping(m, frame)
			return p.addLocationByAddr(mapping, frame)
		}
	}

	// 4. Fallback: name-based dedup on the default single mapping.
	return p.addLocationByFallback(p.Profile.Mapping[0], frame)
}

func (p *ProfileBuilder) addMapping(m procmap.Mapping, frame Frame) *profile.Mapping {
	key := mappingKey{
		Path: m.Path, Start: m.Start, Limit: m.Limit, Off: m.Offset, BuildID: m.BuildID,
	}
	if existing, ok := p.mappings[key]; ok {
		p.updateMappingFlags(existing, frame)
		return existing
	}
	id := uint64(len(p.Profile.Mapping) + 1)
	mapping := &profile.Mapping{
		ID:      id,
		Start:   m.Start,
		Limit:   m.Limit,
		Offset:  m.Offset,
		File:    m.Path,
		BuildID: m.BuildID,
	}
	p.updateMappingFlags(mapping, frame)
	p.Profile.Mapping = append(p.Profile.Mapping, mapping)
	p.mappings[key] = mapping
	return mapping
}

func (p *ProfileBuilder) updateMappingFlags(m *profile.Mapping, f Frame) {
	if f.Name != "" {
		m.HasFunctions = true
	}
	if f.File != "" {
		m.HasFilenames = true
	}
	if f.Line > 0 {
		m.HasLineNumbers = true
	}
}

func (p *ProfileBuilder) addLocationByAddr(mapping *profile.Mapping, frame Frame) *profile.Location {
	var offset uint64
	if frame.MapStart != 0 {
		offset = frame.Address - frame.MapStart + frame.MapOff
	} else {
		offset = frame.Address
	}
	key := locationKey{MappingID: mapping.ID, Address: offset}
	if loc, ok := p.locations[key]; ok {
		return loc
	}
	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: mapping,
		Address: offset,
		Line: []profile.Line{{
			Function: p.addFunction(frame, mapping.ID),
			Line:     int64(frame.Line),
		}},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}

func (p *ProfileBuilder) addLocationByFallback(mapping *profile.Mapping, frame Frame) *profile.Location {
	key := locationFallbackKey{
		Name: frame.Name, File: frame.File, Module: frame.Module, Line: frame.Line,
	}
	if loc, ok := p.locations[key]; ok {
		return loc
	}
	id := uint64(len(p.Profile.Location) + 1)
	loc := &profile.Location{
		ID:      id,
		Mapping: mapping,
		Line: []profile.Line{{
			Function: p.addFunction(frame, mapping.ID),
			Line:     int64(frame.Line),
		}},
	}
	p.Profile.Location = append(p.Profile.Location, loc)
	p.locations[key] = loc
	return loc
}

func (p *ProfileBuilder) addFunction(frame Frame, mappingID uint64) *profile.Function {
	key := functionKey{MappingID: mappingID, Name: frame.Name}
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
		f = mergeFrame(f, dec)
		f.Address = 0 // JIT code — file-offset is meaningless
		return f
	}
	if dec, ok := decodeNode(f.Name); ok {
		f = mergeFrame(f, dec)
		f.Address = 0
		return f
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
