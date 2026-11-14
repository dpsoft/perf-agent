// Command perf-agent-mcp serves perf-agent profiles to an MCP client over
// stdio. It is read-only, needs no capabilities and opens no network
// connection; the only files it writes are pages under --output.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// version is reported in serverInfo and by --version.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := runContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run is runContext without a way to cancel it: it ends at EOF on stdin.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runContext(context.Background(), args, stdin, stdout, stderr)
}

// multiFlag is a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// runContext serves MCP on stdin and stdout until EOF or ctx is done. It
// returns 0 on a clean end, 1 on a runtime error and 2 on a usage error.
// Stdout carries only JSON-RPC; diagnostics go to stderr.
func runContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("perf-agent-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		profiles    = fs.String("profiles", "", "`DIR` profiles and executions logs are read from (required)")
		output      = fs.String("output", "", "`DIR` rendered pages are written to; without it render_flamegraph is disabled")
		sourceGit   = fs.Bool("source-git", true, "allow read-only offline git probes in the source roots (--source-git=false disables)")
		showVersion = fs.Bool("version", false, "print the version and exit")
		sources     multiFlag
		manifests   multiFlag
		limits      multiFlag
	)
	fs.Var(&sources, "source", "source root `DIR` (repeatable)")
	fs.Var(&manifests, "source-manifest", "source manifest `FILE` (repeatable)")
	fs.Var(&limits, "limit", "override a limit, `Name=Value` (repeatable), e.g. Model.MaxRawBytes=1073741824")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, "perf-agent-mcp", version)
		return 0
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *profiles == "" {
		_, _ = fmt.Fprintln(stderr, "perf-agent-mcp: --profiles DIR is required: the directory profiles are read from")
		return 2
	}
	lim := perfmcp.DefaultLimits()
	for _, kv := range limits {
		if err := setLimit(&lim, kv); err != nil {
			_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: --limit %s: %v\n", kv, err)
			return 2
		}
	}

	if err := checkLimits(&lim); err != nil {
		_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: %v\n", err)
		return 2
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	srv, err := perfmcp.New(perfmcp.Config{
		ProfilesDir:     *profiles,
		SourceDirs:      sources,
		SourceManifests: manifests,
		SourceGit:       *sourceGit,
		OutputDir:       *output,
		Limits:          lim,
		Version:         version,
		Log:             log,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: %v\n", err)
		return 1
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, stdin, stdout) }()
	var serveErr error
	select {
	case serveErr = <-serveDone:
	case <-ctx.Done():
		// Cancelling ctx does not interrupt a write: a client that has
		// stopped reading stdout would keep Serve, and so the process,
		// alive. Give it a moment to finish, then exit regardless.
		select {
		case serveErr = <-serveDone:
		case <-time.After(shutdownGrace):
			// No synchronous I/O from here: stderr may be blocked too.
			go log.Error("output was blocked: the client is not reading stdout; exiting")
			closed := make(chan struct{})
			go func() { _ = srv.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(100 * time.Millisecond):
			}
			return 0
		}
	}
	closeErr := srv.Close()
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		log.Error("serve", "err", serveErr)
		return 1
	}
	if closeErr != nil {
		log.Error("close", "err", closeErr)
		return 1
	}
	return 0
}

// shutdownGrace is how long Serve gets to finish after a signal before the
// server exits anyway.
const shutdownGrace = time.Second

// limitFields maps every settable limit name ("MaxRows", "Model.MaxRawBytes")
// to its field in l.
func limitFields(l *perfmcp.Limits) map[string]reflect.Value {
	out := map[string]reflect.Value{}
	var walk func(prefix string, v reflect.Value)
	walk = func(prefix string, v reflect.Value) {
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			name := prefix + f.Name
			switch {
			case f.Type == durationType:
			case f.Type.Kind() == reflect.Struct:
				walk(name+".", v.Field(i))
				continue
			case isSignedInt(f.Type.Kind()):
			default:
				panic("perf-agent-mcp: limit " + name + " has a type --limit cannot set: " + f.Type.String())
			}
			out[name] = v.Field(i)
		}
	}
	walk("", reflect.ValueOf(l).Elem())
	return out
}

var durationType = reflect.TypeFor[time.Duration]()

// checkLimitTypes fails at start-up if a field of perfmcp.Limits could not
// be set by --limit, so a new field of another type cannot go unnoticed.
var _ = limitFields(&perfmcp.Limits{})

func isSignedInt(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// checkLimits refuses a limit that is not positive, naming it; the server
// would otherwise replace a zero with a default without saying so.
// Source.MaxContext may be zero (no context lines).
func checkLimits(l *perfmcp.Limits) error {
	for name, f := range limitFields(l) {
		if f.Int() > 0 || (f.Int() == 0 && name == "Source.MaxContext") {
			continue
		}
		return fmt.Errorf("limit %s must be positive, got %d", name, f.Int())
	}
	return nil
}

// setLimit applies one Name=Value to l.
func setLimit(l *perfmcp.Limits, kv string) error {
	name, val, ok := strings.Cut(kv, "=")
	if !ok {
		return errors.New("want Name=Value")
	}
	fields := limitFields(l)
	f, ok := fields[name]
	if !ok {
		names := make([]string, 0, len(fields))
		for n := range fields {
			names = append(names, n)
		}
		slices.Sort(names)
		return fmt.Errorf("unknown limit %q; valid names: %s", name, strings.Join(names, ", "))
	}
	if f.Type() == durationType {
		d, err := time.ParseDuration(val)
		if err != nil {
			return err
		}
		f.SetInt(int64(d))
		return nil
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if errors.Is(err, strconv.ErrRange) {
		return fmt.Errorf("%q is out of range for %s", val, name)
	}
	if err != nil {
		return fmt.Errorf("%q is not an integer", val)
	}
	if f.OverflowInt(n) {
		return fmt.Errorf("%d is out of range for %s", n, name)
	}
	f.SetInt(n)
	return nil
}
