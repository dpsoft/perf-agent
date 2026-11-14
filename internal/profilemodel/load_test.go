package profilemodel

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/pprof/profile"

	"github.com/dpsoft/perf-agent/gpu/execlog"
)

func twoSamples() []byte {
	return gpuProfile(
		fx{stack: []string{"main", "[gpu:a]"}, value: 300},
		fx{stack: []string{"main", "[gpu:b]"}, value: 0},
	)
}

func goodRows() []execlog.Row {
	return []execlog.Row{
		{Sample: 0, Execution: 0, Value: 100},
		{Sample: 0, Execution: 1, Value: 200},
		{Sample: 1, Execution: 1, Value: 0},
	}
}

func mustLoad(t *testing.T, raw, log []byte, lim Limits) *Profile {
	t.Helper()
	p, err := FromBytes(raw, log, lim)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDigestsTheBytesRead(t *testing.T) {
	raw := twoSamples()
	path := filepath.Join(t.TempDir(), "p.pb.gz")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path, "", DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if p.Digest != sha256.Sum256(raw) {
		t.Fatal("digest differs from sha256 of the bytes read")
	}
	if p.Executions != (Availability{false, ReasonNoSidecar}) {
		t.Fatalf("executions = %+v", p.Executions)
	}
	if string(p.Handle) != p.Digest.String() {
		t.Fatalf("handle = %q", p.Handle)
	}
}

func TestLoadAcceptsACheckedExeclog(t *testing.T) {
	raw := twoSamples()
	log := execlogFor(t, raw, gpuHdr, goodRows(), 2)
	p := mustLoad(t, raw, log, DefaultLimits())
	if !p.Executions.OK {
		t.Fatalf("executions = %+v", p.Executions)
	}
	if !slices.Equal(p.execOf[0], []uint64{0, 1}) || !slices.Equal(p.execOf[1], []uint64{1}) {
		t.Fatalf("execOf = %v", p.execOf)
	}
	if !strings.Contains(string(p.Handle), "+") {
		t.Fatalf("handle = %q", p.Handle)
	}
}

func reason(t *testing.T, raw, log []byte) Availability {
	t.Helper()
	return mustLoad(t, raw, log, DefaultLimits()).Executions
}

func TestLoadRejectsAnotherProfilesLog(t *testing.T) {
	other := gpuProfile(fx{stack: []string{"x"}, value: 5})
	log := execlogFor(t, other, gpuHdr, []execlog.Row{{Value: 5}}, 1)
	if a := reason(t, twoSamples(), log); a.Reason != ReasonDigestMismatch {
		t.Fatalf("got %+v", a)
	}
}

func TestLoadRejectsSampleCountMismatch(t *testing.T) {
	raw := twoSamples()
	log := execlogFor(t, raw, gpuHdr, goodRows(), 3)
	if a := reason(t, raw, log); a.Reason != ReasonSampleCount {
		t.Fatalf("got %+v", a)
	}
}

func TestLoadRejectsTruncatedLog(t *testing.T) {
	raw := twoSamples()
	log := execlogFor(t, raw, gpuHdr, goodRows(), 2)
	if a := reason(t, raw, log[:len(log)-10]); a.Reason != ReasonCorrupt {
		t.Fatalf("got %+v", a)
	}
}

func TestLoadRejectsMetadataMismatch(t *testing.T) {
	raw := gpuProfileWithID("7f3a", fx{stack: []string{"a"}, value: 300})
	rows := []execlog.Row{{Value: 300}}
	for name, hdr := range map[string]execlog.Header{
		"sample type": {SampleType: "cpu"},
		"capture id":  {CaptureID: "other", SampleType: "gpu/nanoseconds"},
	} {
		log := execlogFor(t, raw, hdr, rows, 1)
		if a := reason(t, raw, log); a.Reason != ReasonMetadataMismatch {
			t.Fatalf("%s: got %+v", name, a)
		}
	}
	ok := execlogFor(t, raw, execlog.Header{CaptureID: "7f3a", SampleType: "gpu/nanoseconds"}, rows, 1)
	if a := reason(t, raw, ok); !a.OK {
		t.Fatalf("matching id: got %+v", a)
	}
	cpu := cpuProfile(fx{stack: []string{"a"}, value: 300})
	log := execlogFor(t, cpu, gpuHdr, rows, 1)
	if a := reason(t, cpu, log); a.Reason != ReasonMetadataMismatch {
		t.Fatalf("no gpu type: got %+v", a)
	}
}

func TestLoadRejectsValueMismatch(t *testing.T) {
	raw := twoSamples()
	rows := []execlog.Row{{Sample: 0, Execution: 0, Value: 100}, {Sample: 0, Execution: 1, Value: 150}, {Sample: 1, Execution: 1}}
	p := mustLoad(t, raw, execlogFor(t, raw, gpuHdr, rows, 2), DefaultLimits())
	if p.Executions.Reason != ReasonValueMismatch || p.execOf != nil {
		t.Fatalf("got %+v execOf=%v", p.Executions, p.execOf)
	}
}

func TestLoadEnforcesLimits(t *testing.T) {
	raw := twoSamples()
	lim := DefaultLimits()
	lim.MaxSamples = 1
	_, err := FromBytes(raw, nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxSamples") {
		t.Fatalf("samples: %v", err)
	}
	lim = DefaultLimits()
	lim.MaxDepth = 1
	_, err = FromBytes(raw, nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxDepth") {
		t.Fatalf("depth: %v", err)
	}
	lim = DefaultLimits()
	lim.MaxLocations = 1
	_, err = FromBytes(raw, nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxLocations") {
		t.Fatalf("locations: %v", err)
	}
	lim = DefaultLimits()
	lim.MaxRawBytes = 4
	_, err = FromBytes(raw, nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxRawBytes") {
		t.Fatalf("raw: %v", err)
	}

	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	_, _ = zw.Write(make([]byte, 4<<20))
	_ = zw.Close()
	lim = DefaultLimits()
	lim.MaxDecompressedBytes = 1 << 10
	_, err = FromBytes(bomb.Bytes(), nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxDecompressedBytes") {
		t.Fatalf("decompressed: %v", err)
	}
}

func TestLoadAcceptsTheProducersJoinedSampleType(t *testing.T) {
	raw := twoSamples()
	log := execlogFor(t, raw, execlog.Header{SampleType: "gpu/nanoseconds"}, goodRows(), 2)
	if a := reason(t, raw, log); !a.OK {
		t.Fatalf("got %+v", a)
	}
	log = execlogFor(t, raw, execlog.Header{SampleType: "gpu"}, goodRows(), 2)
	if a := reason(t, raw, log); a.Reason != ReasonMetadataMismatch {
		t.Fatalf("bare type: got %+v", a)
	}
}

func TestLoadDistinguishesNoSidecarFromEmptyFile(t *testing.T) {
	raw := twoSamples()
	if a := reason(t, raw, nil); a.Reason != ReasonNoSidecar {
		t.Fatalf("nil: %+v", a)
	}
	if a := reason(t, raw, []byte{}); a.Reason != ReasonEmptySidecar {
		t.Fatalf("empty: %+v", a)
	}
	dir := t.TempDir()
	pp, lp := filepath.Join(dir, "p"), filepath.Join(dir, "l")
	if err := os.WriteFile(pp, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(pp, lp, DefaultLimits())
	if err != nil || p.Executions.Reason != ReasonEmptySidecar {
		t.Fatalf("file: %v %+v", err, p)
	}
}

func TestLoadBoundsTheSidecarWhileReading(t *testing.T) {
	raw := twoSamples()
	lim := DefaultLimits()
	lim.MaxSidecarRows = 2
	log := execlogFor(t, raw, gpuHdr, goodRows(), 2)
	if a := mustLoad(t, raw, log, lim).Executions; a.Reason != ReasonSidecarLimits {
		t.Fatalf("rows: %+v", a)
	}
	lim = DefaultLimits()
	lim.MaxSidecarExecutions = 1
	if a := mustLoad(t, raw, log, lim).Executions; a.Reason != ReasonSidecarLimits {
		t.Fatalf("executions: %+v", a)
	}
	lim = DefaultLimits()
	lim.MaxSidecarBytes = 8
	if a := mustLoad(t, raw, log, lim).Executions; a.Reason != ReasonSidecarLimits {
		t.Fatalf("bytes: %+v", a)
	}
}

func TestLoadRejectsRowOutsideTheProfile(t *testing.T) {
	raw := twoSamples()
	rows := append(goodRows(), execlog.Row{Sample: 5, Execution: 2, Value: 1})
	log := execlogFor(t, raw, gpuHdr, rows, 6)
	if a := reason(t, raw, log); a.Reason != ReasonCorrupt {
		t.Fatalf("got %+v", a)
	}
}

func TestLoadRejectsWrappedValueSums(t *testing.T) {
	raw := gpuProfile(fx{stack: []string{"a"}, value: 1})
	rows := []execlog.Row{{Value: math.MaxUint64}, {Value: 2}}
	log := execlogFor(t, raw, gpuHdr, rows, 1)
	if a := reason(t, raw, log); a.Reason != ReasonValueMismatch {
		t.Fatalf("got %+v", a)
	}
}

func TestFixturesParse(t *testing.T) {
	for name, build := range allFixtures() {
		if _, err := profile.ParseData(build()); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestLoadRefusesNestedGzipWithoutInflatingIt(t *testing.T) {
	gz := func(b []byte) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(b)
		_ = zw.Close()
		return buf.Bytes()
	}
	var plain bytes.Buffer
	if err := mustLoad(t, twoSamples(), nil, DefaultLimits()).Source.WriteUncompressed(&plain); err != nil {
		t.Fatal(err)
	}
	// A valid profile inside two gzip layers: ParseData would inflate the
	// inner layer itself and load it; it must be refused instead.
	valid := gz(gz(plain.Bytes()))
	if _, err := FromBytes(valid, nil, DefaultLimits()); err == nil {
		t.Fatal("doubly gzipped profile accepted")
	}
	nested := gz(gz(make([]byte, 8<<20)))
	lim := DefaultLimits()
	lim.MaxDecompressedBytes = 1 << 20
	if _, err := FromBytes(nested, nil, lim); err == nil {
		t.Fatal("nested gzip accepted")
	}
	// Within the cap, the inner stream is still not inflated: it is a parse
	// refusal, not a decompression.
	if _, err := FromBytes(nested, nil, DefaultLimits()); err == nil {
		t.Fatal("nested gzip accepted under the default cap")
	}
}

func TestLoadAcceptsUncompressedProtobuf(t *testing.T) {
	p := mustLoad(t, twoSamples(), nil, DefaultLimits())
	var buf bytes.Buffer
	if err := p.Source.WriteUncompressed(&buf); err != nil {
		t.Fatal(err)
	}
	q, err := FromBytes(buf.Bytes(), nil, DefaultLimits())
	if err != nil || len(q.Source.Sample) != 2 {
		t.Fatalf("err=%v", err)
	}
}

func TestHandleNamesTheSidecarSelection(t *testing.T) {
	raw := twoSamples()
	good := execlogFor(t, raw, gpuHdr, goodRows(), 2)
	wrong := execlogFor(t, gpuProfile(fx{stack: []string{"x"}, value: 5}), gpuHdr, []execlog.Row{{Value: 5}}, 1)
	badValue := execlogFor(t, raw, gpuHdr, []execlog.Row{{Sample: 0, Value: 1}, {Sample: 1, Execution: 1}}, 2)
	over := DefaultLimits()
	over.MaxSidecarBytes = 8

	type c struct {
		name string
		log  []byte
		lim  Limits
	}
	cases := []c{
		{"none", nil, DefaultLimits()},
		{"empty", []byte{}, DefaultLimits()},
		{"corrupt", good[:len(good)-3], DefaultLimits()},
		{"wrong profile", wrong, DefaultLimits()},
		{"value mismatch", badValue, DefaultLimits()},
		{"over limit", good, over},
		{"accepted", good, DefaultLimits()},
	}
	seen := map[Handle]string{}
	for _, tc := range cases {
		p := mustLoad(t, raw, tc.log, tc.lim)
		if prev, dup := seen[p.Handle]; dup {
			t.Fatalf("%s and %s share handle %q", prev, tc.name, p.Handle)
		}
		seen[p.Handle] = tc.name
	}
	// Same accepted sidecar twice: same handle.
	a, b := mustLoad(t, raw, good, DefaultLimits()), mustLoad(t, raw, good, DefaultLimits())
	if a.Handle != b.Handle || !a.Executions.OK {
		t.Fatalf("handles %q %q", a.Handle, b.Handle)
	}
	// Over-limit files are never hashed: two different ones do not collide.
	other := execlogFor(t, raw, gpuHdr, goodRows()[:2], 2)
	x, y := mustLoad(t, raw, good, over), mustLoad(t, raw, other, over)
	if x.Handle == y.Handle || !strings.Contains(string(x.Handle), "+unread:") {
		t.Fatalf("over-limit handles %q %q", x.Handle, y.Handle)
	}
	// Equal handles never carry different states.
	states := map[Handle]Availability{}
	for _, tc := range cases {
		p := mustLoad(t, raw, tc.log, tc.lim)
		if s, ok := states[p.Handle]; ok && s != p.Executions {
			t.Fatalf("handle %q has two states", p.Handle)
		}
		states[p.Handle] = p.Executions
	}
}

func TestLoadFileOverSidecarCapIsUnreadHandle(t *testing.T) {
	dir := t.TempDir()
	pp, lp := filepath.Join(dir, "p"), filepath.Join(dir, "l")
	if err := os.WriteFile(pp, twoSamples(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lp, bytes.Repeat([]byte{1}, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	lim := DefaultLimits()
	lim.MaxSidecarBytes = 10
	p, err := Load(pp, lp, lim)
	if err != nil || p.Executions.Reason != ReasonSidecarLimits || !strings.Contains(string(p.Handle), "+unread:") {
		t.Fatalf("%v %+v %q", err, p.Executions, p.Handle)
	}
	p, err = Load(pp, filepath.Join(dir, "missing"), lim)
	if err != nil || p.Executions.Reason != ReasonCorrupt || !strings.Contains(string(p.Handle), "+unread:") {
		t.Fatalf("%v %+v %q", err, p.Executions, p.Handle)
	}
}

// inlineProfile has one location with n inline Line entries, referenced
// refs times by a single sample.
func inlineProfile(t *testing.T, n, refs int) []byte {
	t.Helper()
	p := &profile.Profile{SampleType: []*profile.ValueType{{Type: "gpu", Unit: "nanoseconds"}}}
	m := &profile.Mapping{ID: 1}
	p.Mapping = []*profile.Mapping{m}
	l := &profile.Location{ID: 1, Mapping: m}
	for i := range n {
		f := &profile.Function{ID: uint64(i + 1), Name: "f" + strings.Repeat("x", i)}
		p.Function = append(p.Function, f)
		l.Line = append(l.Line, profile.Line{Function: f})
	}
	p.Location = []*profile.Location{l}
	s := &profile.Sample{Value: []int64{1}}
	for range refs {
		s.Location = append(s.Location, l)
	}
	p.Sample = []*profile.Sample{s}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMaxDepthCountsInlineFrames(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxDepth = 12
	_, err := FromBytes(inlineProfile(t, 13, 1), nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxDepth") {
		t.Fatalf("13 inline lines under 12: %v", err)
	}
	lim.MaxDepth = 13
	if _, err := FromBytes(inlineProfile(t, 13, 1), nil, lim); err != nil {
		t.Fatalf("13 at 13: %v", err)
	}
	// Repeated reference to an inline-heavy location: 5 lines x 3 = 15.
	lim.MaxDepth = 14
	_, err = FromBytes(inlineProfile(t, 5, 3), nil, lim)
	if !errors.Is(err, ErrLimit) || !strings.Contains(err.Error(), "MaxDepth") {
		t.Fatalf("repeated: %v", err)
	}
	lim.MaxDepth = 15
	if _, err := FromBytes(inlineProfile(t, 5, 3), nil, lim); err != nil {
		t.Fatalf("repeated at 15: %v", err)
	}
}
