package perfagent

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/pprof/profile"
)

func writeTestProfile(t *testing.T, samples ...*profile.Sample) string {
	t.Helper()
	m := &profile.Mapping{ID: 1}
	f1 := &profile.Function{ID: 1, Name: "main"}
	f2 := &profile.Function{ID: 2, Name: "hot"}
	p := &profile.Profile{
		SampleType: []*profile.ValueType{{Type: "cpu", Unit: "nanoseconds"}},
		PeriodType: &profile.ValueType{Type: "cpu", Unit: "nanoseconds"},
		Period:     10101010,
		Mapping:    []*profile.Mapping{m},
		Function:   []*profile.Function{f1, f2},
		Location: []*profile.Location{
			{ID: 1, Mapping: m, Line: []profile.Line{{Function: f1}}},
			{ID: 2, Mapping: m, Line: []profile.Line{{Function: f2}}},
		},
		Sample: samples,
	}
	for _, s := range p.Sample {
		s.Location = []*profile.Location{p.Location[0], p.Location[1]}
	}
	path := filepath.Join(t.TempDir(), "prof.pb.gz")
	fh, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Write(fh); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

func TestWriteFlamegraphRendersFromTheProfileTheUserWasGiven(t *testing.T) {
	src := writeTestProfile(t, &profile.Sample{Value: []int64{7000000}})
	dst := filepath.Join(t.TempDir(), "graph.html")
	a := &Agent{config: &Config{PID: 4321, CPUFlamegraphPath: dst}}
	logs := captureLog(t)

	if err := a.writeFlamegraph(src, dst, "on-CPU"); err != nil {
		t.Fatalf("writeFlamegraph: %v", err)
	}
	page, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<!DOCTYPE html>", "data-name=\"main\"", "data-name=\"hot\"", "pid 4321", "on-CPU"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("page missing %q", want)
		}
	}
	if !strings.Contains(logs.String(), "Flame graph written to "+dst) {
		t.Errorf("the run must say where the graph went; got %q", logs.String())
	}
}

func TestWriteFlamegraphIsANoOpWhenNotRequested(t *testing.T) {
	a := &Agent{config: &Config{}}
	if err := a.writeFlamegraph("nonexistent.pb.gz", "", "on-CPU"); err != nil {
		t.Fatalf("an unset flame graph path must not be an error: %v", err)
	}
}

func TestWriteFlamegraphEchoesTheProfilesOwnWarnings(t *testing.T) {
	// A profile that folds to nothing must still produce a file, and the
	// run must say why it is blank rather than looking like a success.
	src := writeTestProfile(t)
	dst := filepath.Join(t.TempDir(), "graph.html")
	a := &Agent{config: &Config{SystemWide: true}}
	logs := captureLog(t)

	if err := a.writeFlamegraph(src, dst, "on-CPU"); err != nil {
		t.Fatalf("an empty profile is not a rendering failure: %v", err)
	}
	if !strings.Contains(logs.String(), "Flame graph note:") ||
		!strings.Contains(logs.String(), "no samples at all") {
		t.Errorf("the empty profile must be reported, got %q", logs.String())
	}
	page, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "No flame graph was drawn") {
		t.Error("the page must say it drew nothing")
	}
	if strings.Contains(string(page), "<svg class=\"flame\"") {
		t.Error("an empty profile must not produce a graph")
	}
}

func TestWriteFlamegraphReportsAnUnreadableProfile(t *testing.T) {
	a := &Agent{config: &Config{}}
	err := a.writeFlamegraph(filepath.Join(t.TempDir(), "gone.pb.gz"), filepath.Join(t.TempDir(), "o.html"), "on-CPU")
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestWarnFlamegraphNeedsPathExplainsTheWriterCase(t *testing.T) {
	// The writer-based options stream the profile somewhere the agent
	// cannot read back. Silence would be indistinguishable from the flag
	// having worked.
	a := &Agent{config: &Config{}}
	logs := captureLog(t)
	a.warnFlamegraphNeedsPath("wanted.html", "CPU")
	if !strings.Contains(logs.String(), "No CPU flame graph written to wanted.html") {
		t.Errorf("got %q", logs.String())
	}

	quiet := captureLog(t)
	a.warnFlamegraphNeedsPath("", "CPU")
	if quiet.String() != "" {
		t.Errorf("no warning when no flame graph was asked for; got %q", quiet.String())
	}
}

func TestTargetDescription(t *testing.T) {
	if got := (&Agent{config: &Config{SystemWide: true}}).targetDescription(); got != "system-wide" {
		t.Errorf("got %q", got)
	}
	if got := (&Agent{config: &Config{PID: 99}}).targetDescription(); got != "pid 99" {
		t.Errorf("got %q", got)
	}
}
