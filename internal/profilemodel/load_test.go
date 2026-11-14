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
