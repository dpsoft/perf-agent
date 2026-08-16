package perfagent

import (
	"testing"
)

func TestWithLabels_AddsToConfig(t *testing.T) {
	cfg := DefaultConfig()
	WithLabels(map[string]string{"service": "api"})(cfg)
	if cfg.Labels["service"] != "api" {
		t.Errorf("service label = %q", cfg.Labels["service"])
	}
}

func TestWithLabels_MergesAcrossCalls(t *testing.T) {
	cfg := DefaultConfig()
	WithLabels(map[string]string{"service": "api"})(cfg)
	WithLabels(map[string]string{"version": "1.2.3"})(cfg)
	if cfg.Labels["service"] != "api" || cfg.Labels["version"] != "1.2.3" {
		t.Errorf("merged labels = %v", cfg.Labels)
	}
}

func TestWithLabelEnricher_StoresAndMarksSet(t *testing.T) {
	cfg := DefaultConfig()
	called := false
	WithLabelEnricher(func(int) map[string]string {
		called = true
		return map[string]string{"x": "y"}
	})(cfg)
	if !cfg.LabelEnricherSet {
		t.Fatal("LabelEnricherSet should be true after WithLabelEnricher")
	}
	got := cfg.LabelEnricher(0)
	if !called || got["x"] != "y" {
		t.Errorf("enricher not stored correctly")
	}
}

func TestWithLabelEnricher_NilDisables(t *testing.T) {
	cfg := DefaultConfig()
	WithLabelEnricher(nil)(cfg)
	if !cfg.LabelEnricherSet {
		t.Fatal("LabelEnricherSet should be true even when fn is nil")
	}
	if cfg.LabelEnricher != nil {
		t.Errorf("LabelEnricher should be nil")
	}
}

func TestWithPerfDataOutput_SetsConfig(t *testing.T) {
	cfg := DefaultConfig()
	WithPerfDataOutput("app.perf.data")(cfg)
	if cfg.PerfDataOutput != "app.perf.data" {
		t.Errorf("PerfDataOutput = %q, want %q", cfg.PerfDataOutput, "app.perf.data")
	}
}
