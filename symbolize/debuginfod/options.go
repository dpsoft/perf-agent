package debuginfod

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// Options configures a Symbolizer. Zero value is invalid; at minimum URLs
// must be set.
type Options struct {
	URLs          []string
	CacheDir      string
	CacheMaxBytes int64
	FetchTimeout  time.Duration
	FailClosed    bool
	Resolver      *procmap.Resolver
	HTTPClient    *http.Client
	Logger        *slog.Logger
	// Demangle is always true in v1. The field exists so we can switch to a
	// *bool tristate later without API breakage.
	Demangle bool
	// InlinedFns is always true in v1. The field exists so we can switch to a
	// *bool tristate later without API breakage.
	InlinedFns bool
	// CodeInfo is always true in v1. The field exists so we can switch to a
	// *bool tristate later without API breakage.
	CodeInfo bool
}

// validate fills in defaults and returns ErrNoURLs / ErrInvalidOpts when
// something is wrong.
func (o *Options) validate() error {
	if len(o.URLs) == 0 {
		return ErrNoURLs
	}
	if o.CacheDir == "" {
		o.CacheDir = "/tmp/perf-agent-debuginfod"
	}
	if o.CacheMaxBytes == 0 {
		o.CacheMaxBytes = 2 << 30 // 2 GiB
	}
	if o.FetchTimeout == 0 {
		o.FetchTimeout = 30 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: o.FetchTimeout}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(devNull{}, nil))
	}
	// Defaults: matches LocalSymbolizer + the spec's documented behavior. We
	// don't honor user-provided false here because Go bool defaults to false;
	// distinguishing unset from explicit-false would require *bool, and the
	// fields don't have a good reason to be turned off in v1.
	o.Demangle = true
	o.InlinedFns = true
	o.CodeInfo = true
	return nil
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }
