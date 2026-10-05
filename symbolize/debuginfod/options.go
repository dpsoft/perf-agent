package debuginfod

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/dpsoft/perf-agent/unwind/procmap"
)

// DialTimeout bounds establishing the TCP connection, as distinct from
// FetchTimeout which bounds the whole request.
//
// The two measure different things and only one of them is about an air-gapped
// host. Measured at the fetch layer against the default 5s FetchTimeout:
//
//	blackhole (SYN dropped)   5.004s   the full timeout
//	connection refused            0s
//	unresolvable host           2ms
//
// Only the first stalls, and it is the shape the environments that matter
// actually produce: a Kubernetes pod with no egress route, or a host firewall
// set to DROP rather than REJECT, silently discards the SYN. Without a dial
// bound that costs FetchTimeout per distinct build-id with no local
// debuginfo -- up to ~80s on the first capture of a 16-binary host -- while
// the profile waits. Issue #111.
//
// Shortening FetchTimeout instead would be the wrong lever: it exists to bound
// the TRANSFER, so lowering it trades away real fetches from a slow-but-working
// server to fix a case that is not about throughput at all. Bounding the dial
// separately collapses the blackhole row into the fast-fail rows and leaves a
// reachable server its full budget.
//
// 300ms: a TCP handshake to a reachable server is one round trip, so this is
// generous for anything on a LAN or in the same region, and still two orders of
// magnitude below the fetch budget. A debuginfod server more than 300ms away by
// RTT alone would be a poor choice for a synchronous symbolization path anyway.
const DialTimeout = 300 * time.Millisecond

// defaultTransport clones http.DefaultTransport with DialTimeout applied.
// Cloned rather than constructed from scratch so proxy settings, HTTP/2 and the
// connection-pool defaults keep behaving as the standard library intends -- the
// only deliberate difference is the dial bound.
func defaultTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	return t
}

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
		// 5s, not 30s, because this bounds a SYNCHRONOUS stall on the
		// symbolization path rather than a background download.
		//
		// Every distinct build-id the servers cannot serve costs this once —
		// negFetch stops it recurring, but the first attempt is paid in full,
		// while the profile waits. Measured on a workstation with ~16 large
		// Go binaries mapped: a 5s system-wide capture spent 1m12s in
		// symbolization, the slowest single call taking 31.937s, which is the
		// old default plus scheduling. Two or three such misses accounted for
		// the whole of it. Issue #109.
		//
		// The cost of being wrong in each direction is asymmetric: too short
		// and a slow server yields a profile with less source detail, which
		// degrades gracefully; too long and the profile does not arrive.
		// A capture is a foreground operation with someone waiting on it.
		o.FetchTimeout = 5 * time.Second
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{
			Timeout:   o.FetchTimeout,
			Transport: defaultTransport(),
		}
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
