// Package mcpflags holds the command-line flags perf-agent-mcp and
// perf-agent-hub share, and the checks that turn them into a
// perfmcp.Config.
package mcpflags

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// Kind is the binary the flags are registered for.
type Kind string

const (
	// Stdio is perf-agent-mcp, one server per client over stdio.
	Stdio Kind = "stdio"
	// Hub is perf-agent-hub, one long-lived server shared by several clients.
	Hub Kind = "hub"
)

// hubOpenHandles is the hub's default for Limits.MaxOpenHandles: several
// agents share its handles.
const hubOpenHandles = 32

// UsageError is a mistake in the command line; the command exits 2. Msg is
// the whole line, prefixed with the program name.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// captureFlags are the flags that only matter with --perf-agent.
var captureFlags = []string{"captures", "capture-command", "capture-ttl", "gpu-shim"}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// Values are the flags registered on a FlagSet, read after it parses.
type Values struct {
	kind        Kind
	profiles    *string
	output      *string
	sourceGit   *bool
	showVersion *bool
	perfAgent   *string
	captures    *string
	gpuShim     *string
	captureTTL  *time.Duration
	sources     multiFlag
	manifests   multiFlag
	limits      multiFlag
	commands    multiFlag
}

// Register registers every flag on fs for the given kind.
func Register(fs *flag.FlagSet, kind Kind) *Values {
	v := &Values{kind: kind}
	capturesDefault := "captures"
	if kind == Hub {
		capturesDefault = "captures-hub"
	}
	v.profiles = fs.String("profiles", "", "`DIR` profiles and executions logs are read from (required)")
	v.output = fs.String("output", "", "`DIR` rendered pages are written to; without it render_flamegraph is disabled")
	v.sourceGit = fs.Bool("source-git", true, "allow read-only offline git probes in the source roots (--source-git=false disables)")
	v.showVersion = fs.Bool("version", false, "print the version and exit")
	v.perfAgent = fs.String("perf-agent", "", "absolute `PATH` of the setcap'd perf-agent binary; without it capture is disabled")
	v.captures = fs.String("captures", "", "`DIR` captures are written to, inside --profiles (relative to it) (default <profiles>/"+capturesDefault+")")
	v.gpuShim = fs.String("gpu-shim", "", "absolute `PATH` of the CUPTI adapter gpu captures inject; without it gpu captures are refused")
	fs.Var(&v.sources, "source", "source root `DIR` (repeatable)")
	fs.Var(&v.manifests, "source-manifest", "source manifest `FILE` (repeatable)")
	fs.Var(&v.limits, "limit", "override a limit, `Name=Value` (repeatable), e.g. Model.MaxRawBytes=1073741824")
	fs.Var(&v.commands, "capture-command", "absolute `PATH` of the only programs a capture may launch (repeatable); without it any program may be")
	ttlUsage := fmt.Sprintf("`AGE` an unkept capture may reach before retention deletes it, a Go duration; 0 keeps captures of any age; it wins over --limit Capture.TTL (default %v)", perfmcp.DefaultLimits().Capture.TTL)
	fs.Func("capture-ttl", ttlUsage, func(s string) error {
		d, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		if d < 0 {
			return errors.New("must not be negative (0 keeps captures of any age)")
		}
		v.captureTTL = &d
		return nil
	})
	return v
}

// Version reports whether --version was given.
func (v *Values) Version() bool { return *v.showVersion }

// Config checks the parsed flags and builds the server's configuration. A
// mistake is a *UsageError. Capture flags given without --perf-agent are
// warned of once on stderr. The caller has parsed fs and handled --version.
func (v *Values) Config(fs *flag.FlagSet, stderr io.Writer, version string) (perfmcp.Config, error) {
	name := fs.Name()
	usage := func(format string, a ...any) error {
		return &UsageError{Msg: name + ": " + fmt.Sprintf(format, a...)}
	}
	if fs.NArg() > 0 {
		return perfmcp.Config{}, usage("unexpected argument %q", fs.Arg(0))
	}
	if *v.profiles == "" {
		return perfmcp.Config{}, usage("--profiles DIR is required: the directory profiles are read from")
	}
	for _, p := range []struct{ flag, path string }{{"--perf-agent", *v.perfAgent}, {"--gpu-shim", *v.gpuShim}} {
		if p.path != "" && !filepath.IsAbs(p.path) {
			return perfmcp.Config{}, usage("%s %q is not an absolute path; name the file by its absolute path", p.flag, p.path)
		}
	}
	for _, c := range v.commands {
		if !filepath.IsAbs(c) {
			return perfmcp.Config{}, usage("--capture-command %q is not an absolute path; name each allowed program by its absolute path", c)
		}
	}
	captureDir := *v.captures
	// Without --perf-agent, --captures has no effect, and the warning
	// below says so: it is not checked.
	if captureDir != "" && *v.perfAgent != "" {
		var err error
		if captureDir, err = CheckCapturesDir(*v.profiles, captureDir); err != nil {
			return perfmcp.Config{}, usage("--captures %s: %v", *v.captures, err)
		}
	}
	if *v.perfAgent == "" {
		var unused []string
		fs.Visit(func(f *flag.Flag) {
			if slices.Contains(captureFlags, f.Name) {
				unused = append(unused, "--"+f.Name)
			}
		})
		if len(unused) > 0 {
			verb := "have"
			if len(unused) == 1 {
				verb = "has"
			}
			_, _ = fmt.Fprintf(stderr, "%s: %s %s no effect without --perf-agent: capture is disabled\n", name, strings.Join(unused, ", "), verb)
		}
	}
	lim := perfmcp.DefaultLimits()
	owner := "stdio"
	if v.kind == Hub {
		owner = "hub"
		lim.MaxOpenHandles = hubOpenHandles
	}
	for _, kv := range v.limits {
		if err := SetLimit(&lim, kv); err != nil {
			return perfmcp.Config{}, usage("--limit %s: %v", kv, err)
		}
	}
	if v.captureTTL != nil {
		lim.Capture.TTL = *v.captureTTL
	}
	if err := CheckLimits(&lim); err != nil {
		return perfmcp.Config{}, usage("%v", err)
	}
	return perfmcp.Config{
		ProfilesDir:     *v.profiles,
		SourceDirs:      v.sources,
		SourceManifests: v.manifests,
		SourceGit:       *v.sourceGit,
		OutputDir:       *v.output,
		Limits:          lim,
		Version:         version,
		Log:             slog.New(slog.NewTextHandler(stderr, nil)),
		Capture: perfmcp.CaptureConfig{
			PerfAgent: *v.perfAgent,
			Dir:       captureDir,
			GPUShim:   *v.gpuShim,
			Owner:     owner,
			Commands:  v.commands,
		},
	}, nil
}
