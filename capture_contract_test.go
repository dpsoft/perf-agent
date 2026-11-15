package main

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/dpsoft/perf-agent/internal/capture"
)

// The capture builder emits only these flags; each must still exist in
// perf-agent with the same arity.
func TestCaptureContractFlagsExist(t *testing.T) {
	for _, f := range capture.Flags {
		fl := flag.Lookup(f.Name)
		if fl == nil {
			t.Errorf("flag --%s is no longer registered", f.Name)
			continue
		}
		bf, isBool := fl.Value.(interface{ IsBoolFlag() bool })
		if got := isBool && bf.IsBoolFlag(); got != f.Bool {
			t.Errorf("flag --%s: bool = %v, builder expects %v", f.Name, got, f.Bool)
		}
	}
}

// Readiness is detected from this stdout line, so it must stay a literal in
// both capture paths.
func TestCaptureContractReadinessLine(t *testing.T) {
	want := `"` + capture.ReadinessFormat + `\n", *flagDuration`
	for _, file := range []string{"main.go", "telemetry.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), want) {
			t.Errorf("%s no longer prints %s", file, want)
		}
	}
	if !strings.HasPrefix(capture.ReadinessFormat, capture.ReadinessPrefix) {
		t.Error("ReadinessPrefix is not a prefix of ReadinessFormat")
	}
}
