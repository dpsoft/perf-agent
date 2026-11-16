package perfmcp

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// The defaults are the §6.1 limits table's.
func TestCaptureLimitsDefaults(t *testing.T) {
	c := DefaultLimits().Capture
	model := profilemodel.DefaultLimits()
	for _, f := range []struct {
		name      string
		got, want any
	}{
		{"MaxDuration", c.MaxDuration, 30 * time.Second},
		{"StartTimeout", c.StartTimeout, 15 * time.Second},
		{"FinishTimeout", c.FinishTimeout, 30 * time.Second},
		{"TTL", c.TTL, 7 * 24 * time.Hour},
		{"StopGrace", c.StopGrace, 5 * time.Second},
		{"WindowDuration", c.WindowDuration, 30 * time.Minute},
		{"MaxSampleRate", c.MaxSampleRate, 1000},
		{"KeepPerSeries", c.KeepPerSeries, 10},
		{"MaxKeptPerSeries", c.MaxKeptPerSeries, 4},
		{"CompareRows", c.CompareRows, 20},
		{"MaxJobs", c.MaxJobs, 32},
		{"MaxJobBytes", c.MaxJobBytes, 4 << 20},
		{"WindowCaptures", c.WindowCaptures, 60},
		{"MaxOutputTail", c.MaxOutputTail, 2 << 10},
		{"MaxProfileBytes", c.MaxProfileBytes, model.MaxRawBytes},
		{"MaxExeclogBytes", c.MaxExeclogBytes, model.MaxSidecarBytes},
		{"MaxStoredBytes", c.MaxStoredBytes, int64(4 << 30)},
		{"MinFreeBytes", c.MinFreeBytes, int64(1 << 30)},
	} {
		if f.got != f.want {
			t.Errorf("Capture.%s = %v, want %v", f.name, f.got, f.want)
		}
	}
	if model.MaxRawBytes != 256<<20 {
		t.Errorf("Model.MaxRawBytes = %d: the table's 256 MiB is stale", model.MaxRawBytes)
	}

	// A zero Capture is filled in whole, alongside limits that set others.
	cfg := testConfig(t)
	cfg.Limits = Limits{MaxMessageBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxOpenHandles: 4}
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	if s.lim.Capture != c {
		t.Errorf("a zero Capture became %+v, want the defaults", s.lim.Capture)
	}
}

func TestCaptureLimitsCeilings(t *testing.T) {
	set := func(f func(*CaptureLimits)) Limits {
		l := DefaultLimits()
		f(&l.Capture)
		return l
	}
	for _, tc := range []struct {
		name string
		lim  Limits
		want []string
	}{
		{"MaxDuration", set(func(c *CaptureLimits) { c.MaxDuration = 6 * time.Minute }), []string{"Capture.MaxDuration", "5m0s"}},
		{"MaxSampleRate", set(func(c *CaptureLimits) { c.MaxSampleRate = 10001 }), []string{"Capture.MaxSampleRate", "10000"}},
		{"StartTimeout", set(func(c *CaptureLimits) { c.StartTimeout = 5*time.Minute + 1 }), []string{"Capture.StartTimeout", "5m0s"}},
		{"FinishTimeout", set(func(c *CaptureLimits) { c.FinishTimeout = 11 * time.Minute }), []string{"Capture.FinishTimeout", "10m0s"}},
		{"StopGrace", set(func(c *CaptureLimits) { c.StopGrace = 31 * time.Second }), []string{"Capture.StopGrace", "30s"}},
		{"MaxOutputTail", set(func(c *CaptureLimits) { c.MaxOutputTail = 64<<10 + 1 }), []string{"Capture.MaxOutputTail", "65536"}},
		{"CompareRows", set(func(c *CaptureLimits) { c.CompareRows = defaultMaxDiffRows + 1 }), []string{"Capture.CompareRows", "MaxDiffRows", "500"}},
		// StopGrace 0 with the rest set is refused, not filled in.
		{"StopGrace zero", set(func(c *CaptureLimits) { c.StopGrace = 0 }), []string{"Capture.StopGrace", "positive"}},
		{"MaxStoredBytes negative", set(func(c *CaptureLimits) { c.MaxStoredBytes = -1 }), []string{"Capture.MaxStoredBytes", "positive"}},
		{"TTL negative", set(func(c *CaptureLimits) { c.TTL = -time.Second }), []string{"Capture.TTL"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.Limits = tc.lim
			s, err := New(cfg)
			if err == nil {
				_ = s.Close()
				t.Fatal("New accepted it")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%v does not say %q", err, w)
				}
			}
		})
	}
	// At the ceilings, and with TTL 0 (no age limit), the limits are fine.
	at := set(func(c *CaptureLimits) {
		*c = CaptureLimits{
			MaxDuration: 5 * time.Minute, StartTimeout: 5 * time.Minute, FinishTimeout: 10 * time.Minute,
			StopGrace: 30 * time.Second, WindowDuration: 5 * time.Minute,
			MaxSampleRate: 10000, KeepPerSeries: 1, MaxKeptPerSeries: 1, CompareRows: defaultMaxDiffRows,
			MaxJobs: 1, MaxJobBytes: 1, WindowCaptures: 1, MaxOutputTail: 64 << 10,
			MaxProfileBytes: 1, MaxExeclogBytes: 1, MaxStoredBytes: 4 + storeHeadroom, MinFreeBytes: 1,
		}
	})
	cfg := testConfig(t)
	cfg.Limits = at
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	if s.lim.Capture != at.Capture {
		t.Errorf("Capture = %+v, want %+v", s.lim.Capture, at.Capture)
	}
}

// cleanStatus is a /proc/self/status excerpt of an unprivileged server.
const cleanStatus = "Name:\tperf-agent-mcp\nUid:\t1000\t1000\t1000\t1000\n" +
	"CapPrm:\t0000000000000000\nCapEff:\t0000000000000000\nCapBnd:\t000001ffffffffff\nNoNewPrivs:\t0\n"

// withSelfStatus makes New read status (or fail with err) as its own
// /proc/self/status until the test ends.
func withSelfStatus(t *testing.T, status string, err error) {
	t.Helper()
	old := readSelfStatus
	readSelfStatus = func() ([]byte, error) { return []byte(status), err }
	t.Cleanup(func() { readSelfStatus = old })
}

// fakeVerify is a Verify seam that accepts any path as a perf-agent
// granted caps.
func fakeVerify(caps capture.CapSet) func(string) (capture.Binary, error) {
	return func(p string) (capture.Binary, error) {
		return capture.Binary{Path: p, Caps: caps, Version: "test"}, nil
	}
}

// plainBinary is a 0755 file in a directory where VerifyBinary gets as
// far as the capability check: t.TempDir() is usually under a nosuid
// /tmp, so it also tries a dot-directory beside the package and one in
// the user's cache directory, each removed at the end of the test.
func plainBinary(t *testing.T) string {
	t.Helper()
	if !capture.Supported {
		t.Skip("capture needs Linux")
	}
	var tried []string
	try := func(dir string) string {
		p := filepath.Join(dir, "perf-agent")
		if err := os.WriteFile(p, []byte("not perf-agent\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := capture.VerifyBinary(p)
		if err != nil && strings.Contains(err.Error(), "no file capabilities") {
			return p
		}
		tried = append(tried, fmt.Sprint(err))
		return ""
	}
	if p := try(t.TempDir()); p != "" {
		return p
	}
	parents := []string{"."}
	if cache, err := os.UserCacheDir(); err == nil {
		parents = append(parents, cache)
	}
	for _, parent := range parents {
		d, err := os.MkdirTemp(parent, ".verify-test-")
		if err != nil {
			tried = append(tried, err.Error())
			continue
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		if d, err = filepath.Abs(d); err != nil {
			t.Fatal(err)
		}
		if p := try(d); p != "" {
			return p
		}
	}
	t.Skipf("no directory where a plain file reaches the capability check: %s", strings.Join(tried, "; "))
	return ""
}

func TestNewVerifiesThePerfAgentBinary(t *testing.T) {
	withSelfStatus(t, cleanStatus, nil)
	plain := plainBinary(t)

	cfg := testConfig(t)
	cfg.Capture.PerfAgent = plain
	if s, err := New(cfg); err == nil {
		_ = s.Close()
		t.Fatal("New accepted a perf-agent without file capabilities")
	} else if !strings.Contains(err.Error(), "no file capabilities") || !strings.Contains(err.Error(), "setcap") {
		t.Fatalf("New: %v, want the missing capabilities and the setcap line", err)
	}

	// Through the seam a verified binary enables capture.
	cfg.Capture.Verify = fakeVerify(capture.CapSet(1) << capture.CapPerfmon)
	s := must(New(cfg))(t)
	if s.captureDisabled != "" || s.perfAgent.Path != plain {
		t.Errorf("captureDisabled %q, perf-agent %q", s.captureDisabled, s.perfAgent.Path)
	}
	_ = s.Close()

	boom := errors.New("boom: not perf-agent")
	cfg.Capture.Verify = func(string) (capture.Binary, error) { return capture.Binary{}, boom }
	if _, err := New(cfg); !errors.Is(err, boom) {
		t.Errorf("a Verify error: New = %v", err)
	}

	// The shim and every command are checked too, and their failures are
	// New errors.
	cfg.Capture.Verify = fakeVerify(0)
	for _, tc := range []struct {
		name string
		set  func(*CaptureConfig)
		want string
	}{
		{"relative shim", func(c *CaptureConfig) { c.GPUShim = "libshim.so" }, "absolute"},
		{"missing shim", func(c *CaptureConfig) { c.GPUShim = filepath.Join(t.TempDir(), "libshim.so") }, "GPU shim"},
		{"relative command", func(c *CaptureConfig) { c.Commands = []string{"bench"} }, "absolute"},
		{"missing command", func(c *CaptureConfig) { c.Commands = []string{filepath.Join(t.TempDir(), "bench")} }, "cannot stat command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			tc.set(&c.Capture)
			s, err := New(c)
			if err == nil {
				_ = s.Close()
				t.Fatal("New accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("New: %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCaptureIsDisabledForAPrivilegedOrSandboxedServer(t *testing.T) {
	if !capture.Supported {
		t.Skip("capture needs Linux")
	}
	perfCaps := capture.CapSet(1)<<capture.CapPerfmon | capture.CapSet(1)<<capture.CapBPF
	for _, tc := range []struct {
		name   string
		status string
		err    error
		want   string
	}{
		{"no_new_privs", strings.Replace(cleanStatus, "NoNewPrivs:\t0", "NoNewPrivs:\t1", 1),
			nil, "this server inherited no_new_privs (a sandbox?); perf-agent's file capabilities cannot apply"},
		{"root", strings.Replace(cleanStatus, "Uid:\t1000\t1000", "Uid:\t1000\t0", 1), nil, "euid 0"},
		{"capabilities", strings.Replace(cleanStatus, "CapEff:\t0000000000000000", "CapEff:\t0000000000000400", 1), nil, "holds capabilities"},
		{"bounding set", strings.Replace(cleanStatus, "CapBnd:\t000001ffffffffff", "CapBnd:\t0000007fffffffff", 1), nil, "cap_bpf"},
		{"unreadable", "", errors.New("no proc"), "no proc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSelfStatus(t, tc.status, tc.err)
			cfg := testConfig(t)
			cfg.Capture.PerfAgent = "/opt/perf-agent"
			cfg.Capture.Verify = fakeVerify(perfCaps)
			s := must(New(cfg))(t)
			defer func() { _ = s.Close() }()
			if !strings.Contains(s.captureDisabled, tc.want) {
				t.Errorf("captureDisabled = %q, want %q", s.captureDisabled, tc.want)
			}
		})
	}

	// Without PerfAgent the reason is the tool's DISABLED sentence, and
	// nothing is verified.
	withSelfStatus(t, "", errors.New("must not be read"))
	cfg := testConfig(t)
	cfg.Capture.Verify = func(string) (capture.Binary, error) { return capture.Binary{}, errors.New("must not run") }
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	const want = "DISABLED: start perf-agent-mcp with --perf-agent PATH (the setcap'd perf-agent binary) to enable capturing."
	if s.captureDisabled != want {
		t.Errorf("captureDisabled = %q, want %q", s.captureDisabled, want)
	}
}

// A capture limit left at its default follows the M2 limit it depends
// on, so lowering that limit alone never refuses the server.
func TestCaptureLimitsFollowTheM2Limits(t *testing.T) {
	l := DefaultLimits()
	l.MaxDiffRows = 10
	l.Model.MaxRawBytes = 1 << 20
	l.Model.MaxSidecarBytes = 2 << 20
	l.MaxResponseBytes = 1024
	cfg := testConfig(t)
	cfg.Limits = l
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	c := s.lim.Capture
	if c.CompareRows != 10 || c.MaxProfileBytes != 1<<20 || c.MaxExeclogBytes != 2<<20 || c.MaxOutputTail != 64 {
		t.Errorf("CompareRows %d, MaxProfileBytes %d, MaxExeclogBytes %d, MaxOutputTail %d; want 10, 1 MiB, 2 MiB, 64",
			c.CompareRows, c.MaxProfileBytes, c.MaxExeclogBytes, c.MaxOutputTail)
	}

	// A zero Capture is filled the same way.
	cfg = testConfig(t)
	cfg.Limits = Limits{MaxMessageBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxOpenHandles: 4, MaxDiffRows: 10}
	s2 := must(New(cfg))(t)
	defer func() { _ = s2.Close() }()
	if s2.lim.Capture.CompareRows != 10 {
		t.Errorf("a zero Capture with MaxDiffRows 10: CompareRows %d", s2.lim.Capture.CompareRows)
	}
}

// A capture limit set against the limit it depends on is refused at
// start-up, naming both.
func TestCaptureLimitsCrossFieldChecks(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Limits)
		want []string
	}{
		{"profile over the model", func(l *Limits) { l.Capture.MaxProfileBytes = l.Model.MaxRawBytes + 1 }, []string{"Capture.MaxProfileBytes", "Model.MaxRawBytes"}},
		{"execlog over the model", func(l *Limits) { l.Capture.MaxExeclogBytes = l.Model.MaxSidecarBytes + 1 }, []string{"Capture.MaxExeclogBytes", "Model.MaxSidecarBytes"}},
		{"window under one capture", func(l *Limits) { l.Capture.WindowDuration = l.Capture.MaxDuration - time.Second }, []string{"Capture.WindowDuration", "Capture.MaxDuration"}},
		{"store under one capture", func(l *Limits) { l.Capture.MaxStoredBytes = 300 << 20 }, []string{"Capture.MaxStoredBytes", "Capture.MaxProfileBytes", "Capture.MaxExeclogBytes"}},
		// The capture directory's own small files need room beside the
		// largest capture: three profiles and an executions log.
		{"store of exactly one capture", func(l *Limits) {
			l.Capture.MaxStoredBytes = 3*l.Capture.MaxProfileBytes + l.Capture.MaxExeclogBytes
		}, []string{"Capture.MaxStoredBytes", "3 × Capture.MaxProfileBytes + Capture.MaxExeclogBytes + 1 MiB"}},
		{"store of one cpu capture", func(l *Limits) {
			l.Capture.MaxStoredBytes = l.Capture.MaxProfileBytes + l.Capture.MaxExeclogBytes + storeHeadroom
		}, []string{"Capture.MaxStoredBytes"}},
		{"tails over the response", func(l *Limits) { l.Capture.MaxOutputTail = 16 << 10; l.MaxResponseBytes = 256<<10 - 1 }, []string{"Capture.MaxOutputTail", "MaxResponseBytes"}},
		{"explicit compare rows", func(l *Limits) { l.MaxDiffRows = 10; l.Capture.CompareRows = 15 }, []string{"Capture.CompareRows", "MaxDiffRows"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := DefaultLimits()
			tc.set(&l)
			cfg := testConfig(t)
			cfg.Limits = l
			s, err := New(cfg)
			if err == nil {
				_ = s.Close()
				t.Fatal("New accepted it")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%v does not name %s", err, w)
				}
			}
		})
	}
}

func TestANegativeTTLSaysNotNegative(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits = DefaultLimits()
	cfg.Limits.Capture.TTL = -time.Second
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("New = %v", err)
	}
}

// With a raised Model and a zero Capture, the store left at its default
// grows to hold the largest capture, so the defaults never trip their own
// rule.
func TestCaptureLimitsDefaultStoreHoldsTheLargestCapture(t *testing.T) {
	model := profilemodel.DefaultLimits()
	model.MaxRawBytes, model.MaxSidecarBytes = 4<<30, 1<<30
	cfg := testConfig(t)
	cfg.Limits = Limits{Model: model, MaxMessageBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxOpenHandles: 4}
	s := must(New(cfg))(t)
	defer func() { _ = s.Close() }()
	if got, want := s.lim.Capture.MaxStoredBytes, int64(3*4<<30+1<<30+storeHeadroom); got != want {
		t.Errorf("MaxStoredBytes %d, want %d", got, want)
	}
	// Under the default it stays 4 GiB.
	if got := DefaultLimits().withDefaults().Capture.MaxStoredBytes; got != 4<<30 {
		t.Errorf("default MaxStoredBytes %d", got)
	}
}

// A tail left at its default is lowered to fit any MaxResponseBytes, so
// lowering that limit alone is never refused; an explicit tail is still
// checked against it.
func TestCaptureLimitsDefaultTailFitsATinyResponse(t *testing.T) {
	for _, n := range []int{1, 8, 15, 16, 64} {
		l := DefaultLimits()
		l.MaxResponseBytes = n
		cfg := testConfig(t)
		cfg.Limits = l
		s, err := New(cfg)
		if err != nil {
			t.Errorf("MaxResponseBytes %d: %v", n, err)
			continue
		}
		if s.lim.Capture.MaxOutputTail != max(n/16, 1) {
			t.Errorf("MaxResponseBytes %d: MaxOutputTail %d", n, s.lim.Capture.MaxOutputTail)
		}
		_ = s.Close()
	}
	l := DefaultLimits()
	l.MaxResponseBytes, l.Capture.MaxOutputTail = 8, 2
	cfg := testConfig(t)
	cfg.Limits = l
	if s, err := New(cfg); err == nil {
		_ = s.Close()
		t.Error("an explicit tail of 2 with MaxResponseBytes 8 was accepted")
	}
}

// Each capture limit fit moves from its default is logged once, naming
// the field, its default and the value it now has; none is logged when
// nothing moves.
func TestCaptureLimitsFitIsLogged(t *testing.T) {
	var buf bytes.Buffer
	l := DefaultLimits()
	l.MaxDiffRows = 10
	l.MaxResponseBytes = 1024
	cfg := testConfig(t)
	cfg.Limits = l
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	s := must(New(cfg))(t)
	_ = s.Close()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var fitted []string
	for _, line := range lines {
		if strings.Contains(line, "fitted") {
			fitted = append(fitted, line)
		}
	}
	if len(fitted) != 2 ||
		!strings.Contains(fitted[0], "limit=Capture.CompareRows default=20 now=10") ||
		!strings.Contains(fitted[1], "limit=Capture.MaxOutputTail default=2048 now=64") {
		t.Errorf("log:\n%s", buf.String())
	}

	buf.Reset()
	cfg = testConfig(t)
	cfg.Log = slog.New(slog.NewTextHandler(&buf, nil))
	s = must(New(cfg))(t)
	_ = s.Close()
	if strings.Contains(buf.String(), "fitted") {
		t.Errorf("defaults logged a fit:\n%s", buf.String())
	}
}

// The most a capture writes is a profile per mode and, with gpu, an
// executions log.
func TestCaptureNeed(t *testing.T) {
	c := CaptureLimits{MaxProfileBytes: 10, MaxExeclogBytes: 3}
	for _, tc := range []struct {
		modes []capture.Mode
		want  int64
	}{
		{[]capture.Mode{capture.ModeCPU}, 10},
		{[]capture.Mode{capture.ModeCPU, capture.ModeOffCPU}, 20},
		{[]capture.Mode{capture.ModeGPU}, 13},
		{[]capture.Mode{capture.ModeCPU, capture.ModeOffCPU, capture.ModeGPU}, 33},
	} {
		if got := c.captureNeed(tc.modes); got != tc.want {
			t.Errorf("captureNeed(%v) = %d, want %d", tc.modes, got, tc.want)
		}
	}
	if got := c.largestCapture(); got != 33 {
		t.Errorf("largestCapture = %d", got)
	}
}
