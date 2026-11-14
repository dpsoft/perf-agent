// Package pmfixture builds small synthetic pprof profiles and execution logs
// for tests. It does not import profilemodel, so profilemodel's own tests can
// use it.
package pmfixture

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
)

// Line is one inline level of a frame.
type Line struct {
	Name, Filename  string
	StartLine, Line int64
}

// Frame is one stack entry: a location on the mapping named by File and
// BuildID ("" File puts it on Mapping[0], which has no file), at Addr, with
// Lines innermost first as pprof stores them. No lines means address only.
type Frame struct {
	File, BuildID string
	Addr          uint64
	Lines         []Line
}

// Sample is one sample: Frames root first, a value and labels.
type Sample struct {
	Frames []Frame
	Value  int64
	Labels map[string][]string
}

// L is a Line.
func L(name, filename string, line int64) Line {
	return Line{Name: name, Filename: filename, StartLine: line, Line: line}
}

// Fn is a frame on Mapping[0] with one line naming name.
func Fn(name string) Frame { return Frame{Lines: []Line{{Name: name}}} }

// In is a frame in the module file with build id buildID, at addr.
func In(file, buildID string, addr uint64, lines ...Line) Frame {
	return Frame{File: file, BuildID: buildID, Addr: addr, Lines: lines}
}

// locationKey identifies a location by its mapping, address and every field
// of every line. Strings are quoted and numbers delimited, so no two
// different frames share a key.
func locationKey(mapping uint64, fr Frame) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(mapping, 10))
	b.WriteByte(',')
	b.WriteString(strconv.FormatUint(fr.Addr, 10))
	for _, l := range fr.Lines {
		b.WriteByte(',')
		b.WriteString(strconv.Quote(l.Name))
		b.WriteByte(',')
		b.WriteString(strconv.Quote(l.Filename))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(l.StartLine, 10))
		b.WriteByte(',')
		b.WriteString(strconv.FormatInt(l.Line, 10))
	}
	return b.String()
}

// Build serializes a single-sample-type profile (leaf first, gzipped by
// profile.Write). Mappings, functions and locations are shared between
// samples whenever their fields are equal.
func Build(typ, unit string, comments []string, samples ...Sample) []byte {
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: typ, Unit: unit}}, Comments: comments}
	empty := &profile.Mapping{ID: 1}
	p.Mapping = []*profile.Mapping{empty}
	type mkey struct{ file, buildID string }
	type fkey struct {
		name, filename string
		start          int64
	}
	maps := map[mkey]*profile.Mapping{}
	funcs := map[fkey]*profile.Function{}
	locs := map[string]*profile.Location{}
	mapping := func(fr Frame) *profile.Mapping {
		if fr.File == "" {
			return empty
		}
		k := mkey{fr.File, fr.BuildID}
		if m, ok := maps[k]; ok {
			return m
		}
		m := &profile.Mapping{ID: uint64(len(p.Mapping) + 1), File: fr.File, BuildID: fr.BuildID}
		p.Mapping = append(p.Mapping, m)
		maps[k] = m
		return m
	}
	function := func(l Line) *profile.Function {
		k := fkey{l.Name, l.Filename, l.StartLine}
		if f, ok := funcs[k]; ok {
			return f
		}
		f := &profile.Function{ID: uint64(len(p.Function) + 1), Name: l.Name, Filename: l.Filename, StartLine: l.StartLine}
		p.Function = append(p.Function, f)
		funcs[k] = f
		return f
	}
	location := func(fr Frame) *profile.Location {
		m := mapping(fr)
		k := locationKey(m.ID, fr)
		if l, ok := locs[k]; ok {
			return l
		}
		l := &profile.Location{ID: uint64(len(p.Location) + 1), Mapping: m, Address: fr.Addr}
		for _, x := range fr.Lines {
			l.Line = append(l.Line, profile.Line{Function: function(x), Line: x.Line})
		}
		p.Location = append(p.Location, l)
		locs[k] = l
		return l
	}
	for _, s := range samples {
		smp := &profile.Sample{Value: []int64{s.Value}, Label: s.Labels}
		for i := len(s.Frames) - 1; i >= 0; i-- {
			smp.Location = append(smp.Location, location(s.Frames[i]))
		}
		p.Sample = append(p.Sample, smp)
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Execlog writes an executions log bound to profile and returns its bytes.
// The profile is parsed to count its samples.
func Execlog(tb testing.TB, raw []byte, hdr execlog.Header, rows []execlog.Row) []byte {
	tb.Helper()
	p, err := profile.ParseData(raw)
	if err != nil {
		tb.Fatal(err)
	}
	path := filepath.Join(tb.TempDir(), "x.execlog")
	w, err := execlog.Create(path, hdr)
	if err != nil {
		tb.Fatal(err)
	}
	for _, r := range rows {
		w.Add(r)
	}
	if err := w.Finish(uint64(len(p.Sample)), execlog.ProfileSHA256(raw)); err != nil {
		tb.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}
