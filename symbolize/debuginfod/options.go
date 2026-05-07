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
	Demangle      bool
	InlinedFns    bool
	CodeInfo      bool
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
	return nil
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }
