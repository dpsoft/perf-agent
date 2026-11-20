package perfmcp

import (
	"strings"
	"testing"
	"time"
)

// The hub's transport limits have their defaults, are filled in only when
// all of them are zero, and are checked only for a server with the page
// (the hub): stdio cannot set them, so it never refuses them. The engine
// wait is every server's.
func TestHubLimitsDefaultsAndValidation(t *testing.T) {
	want := HubLimits{
		MaxSessions: 32, MaxIngressReaders: 16, MaxIngressBytes: 8 << 20, IngressBodyTimeout: 2 * time.Second,
		MaxControlInFlight: 32, MaxPageStateInFlight: 4, MaxCalls: 16, MaxCallsPerSession: 4,
		MaxRequestBytes: 16 << 20, MaxInboxEntries: 32,
	}
	d := DefaultLimits()
	if d.Hub != want {
		t.Errorf("DefaultLimits().Hub = %+v, want %+v", d.Hub, want)
	}
	if d.MaxEngineWait != 45*time.Second {
		t.Errorf("DefaultLimits().MaxEngineWait = %v, want 45s", d.MaxEngineWait)
	}
	// Zero parts are filled in; a Hub with any field set is kept whole.
	l := d
	l.Hub, l.MaxEngineWait = HubLimits{}, 0
	if got := l.withDefaults(); got.Hub != want || got.MaxEngineWait != 45*time.Second {
		t.Errorf("withDefaults: Hub %+v, MaxEngineWait %v", got.Hub, got.MaxEngineWait)
	}
	l.Hub = HubLimits{MaxCalls: 3}
	if got := l.withDefaults(); got.Hub != (HubLimits{MaxCalls: 3}) {
		t.Errorf("a partly set Hub was filled in: %+v", got.Hub)
	}

	for _, tc := range []struct {
		name  string
		edit  func(*Limits)
		names []string
	}{
		{"calls per session over calls", func(l *Limits) { l.Hub.MaxCallsPerSession, l.Hub.MaxCalls = 5, 4 }, []string{"Hub.MaxCallsPerSession", "Hub.MaxCalls"}},
		{"ingress under a message", func(l *Limits) { l.Hub.MaxIngressBytes = int64(l.MaxMessageBytes) - 1 }, []string{"Hub.MaxIngressBytes", "MaxMessageBytes"}},
		{"request under a message", func(l *Limits) { l.Hub.MaxRequestBytes = int64(l.MaxMessageBytes) - 1 }, []string{"Hub.MaxRequestBytes", "MaxMessageBytes"}},
		{"stdio's message over the hub's ingress", func(l *Limits) { l.MaxMessageBytes = 9 << 20 }, []string{"Hub.MaxIngressBytes", "MaxMessageBytes"}},
		{"body timeout", func(l *Limits) { l.Hub.IngressBodyTimeout = 31 * time.Second }, []string{"Hub.IngressBodyTimeout", "30s"}},
		{"a zero hub limit", func(l *Limits) { l.Hub.MaxSessions = 0; l.Hub.MaxCalls = 16 }, []string{"Hub.MaxSessions"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := DefaultLimits()
			tc.edit(&l)
			err := l.validate(true)
			if err == nil {
				t.Fatal("accepted with the page")
			}
			for _, n := range tc.names {
				if !strings.Contains(err.Error(), n) {
					t.Errorf("%v does not name %s", err, n)
				}
			}
			if err := l.validate(false); err != nil {
				t.Errorf("refused without the page: %v", err)
			}
		})
	}
	if err := DefaultLimits().validate(true); err != nil {
		t.Errorf("the defaults with the page: %v", err)
	}
	if err := (Limits{}).withDefaults().validate(true); err != nil {
		t.Errorf("the zero limits with the page: %v", err)
	}

	for _, page := range []bool{false, true} {
		l := DefaultLimits()
		l.MaxEngineWait = 56 * time.Second
		if err := l.validate(page); err == nil || !strings.Contains(err.Error(), "MaxEngineWait") || !strings.Contains(err.Error(), "55s") {
			t.Errorf("MaxEngineWait 56s, page %v: %v", page, err)
		}
		l.MaxEngineWait = 55 * time.Second
		if err := l.validate(page); err != nil {
			t.Errorf("MaxEngineWait 55s, page %v: %v", page, err)
		}
		l.MaxEngineWait = -time.Second
		if err := l.validate(page); err == nil || !strings.Contains(err.Error(), "MaxEngineWait") {
			t.Errorf("MaxEngineWait -1s, page %v: %v", page, err)
		}
	}

	// New refuses them as validate does.
	cfg := testConfig(t)
	cfg.Page = livePage()
	cfg.Limits.Hub.MaxCallsPerSession = 17
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "Hub.MaxCallsPerSession") {
		t.Errorf("New with the page and MaxCallsPerSession over MaxCalls: %v", err)
	}
	cfg.Page = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New without the page: %v", err)
	}
	_ = s.Close()
}
