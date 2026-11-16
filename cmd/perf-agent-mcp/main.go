// Command perf-agent-mcp serves perf-agent profiles to an MCP client over
// stdio. With --perf-agent it also captures profiles, by running the user's
// setcap'd perf-agent against a process of theirs or a command it launches;
// the server itself needs no capabilities and refuses to capture when it
// holds any. It never opens an outgoing connection and opens no listener.
// The only files it writes are pages under --output and captures under
// --captures, inside the profiles root, where retention may delete older
// captures. Its own binary, re-executed, is each capture's reaper and
// launcher.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// version is reported in serverInfo and by --version.
var version = "dev"

// configHook, when set, adjusts the server's configuration before it is
// built. The command sets none; tests use it to run the live page engine,
// which the stdio server does not expose.
var configHook func(*perfmcp.Config)

func main() {
	// A capture's reaper and launcher are this binary re-executed in an
	// internal mode; they never reach the flags.
	if code, handled := capture.RunInternal(); handled {
		os.Exit(code)
	}
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
		perfAgent   = fs.String("perf-agent", "", "absolute `PATH` of the setcap'd perf-agent binary; without it capture is disabled")
		captures    = fs.String("captures", "", "`DIR` captures are written to, inside --profiles (relative to it) (default <profiles>/captures)")
		gpuShim     = fs.String("gpu-shim", "", "absolute `PATH` of the CUPTI adapter gpu captures inject; without it gpu captures are refused")
		captureTTL  *time.Duration
		sources     multiFlag
		manifests   multiFlag
		limits      multiFlag
		commands    multiFlag
	)
	fs.Var(&sources, "source", "source root `DIR` (repeatable)")
	fs.Var(&manifests, "source-manifest", "source manifest `FILE` (repeatable)")
	fs.Var(&limits, "limit", "override a limit, `Name=Value` (repeatable), e.g. Model.MaxRawBytes=1073741824")
	fs.Var(&commands, "capture-command", "absolute `PATH` of the only programs a capture may launch (repeatable); without it any program may be")
	ttlUsage := fmt.Sprintf("`AGE` an unkept capture may reach before retention deletes it, a Go duration; 0 keeps captures of any age; it wins over --limit Capture.TTL (default %v)", perfmcp.DefaultLimits().Capture.TTL)
	fs.Func("capture-ttl", ttlUsage, func(v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		if d < 0 {
			return errors.New("must not be negative (0 keeps captures of any age)")
		}
		captureTTL = &d
		return nil
	})
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
	for _, p := range []struct{ flag, path string }{{"--perf-agent", *perfAgent}, {"--gpu-shim", *gpuShim}} {
		if p.path != "" && !filepath.IsAbs(p.path) {
			_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: %s %q is not an absolute path; name the file by its absolute path\n", p.flag, p.path)
			return 2
		}
	}
	for _, c := range commands {
		if !filepath.IsAbs(c) {
			_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: --capture-command %q is not an absolute path; name each allowed program by its absolute path\n", c)
			return 2
		}
	}
	captureDir := *captures
	if captureDir != "" {
		var err error
		if captureDir, err = checkCapturesDir(*profiles, captureDir); err != nil {
			_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: --captures %s: %v\n", *captures, err)
			return 2
		}
	}
	if *perfAgent == "" {
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
			_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: %s %s no effect without --perf-agent: capture is disabled\n", strings.Join(unused, ", "), verb)
		}
	}
	lim := perfmcp.DefaultLimits()
	for _, kv := range limits {
		if err := setLimit(&lim, kv); err != nil {
			_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: --limit %s: %v\n", kv, err)
			return 2
		}
	}
	if captureTTL != nil {
		lim.Capture.TTL = *captureTTL
	}

	if err := checkLimits(&lim); err != nil {
		_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: %v\n", err)
		return 2
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	cfg := perfmcp.Config{
		ProfilesDir:     *profiles,
		SourceDirs:      sources,
		SourceManifests: manifests,
		SourceGit:       *sourceGit,
		OutputDir:       *output,
		Limits:          lim,
		Version:         version,
		Log:             log,
		Capture: perfmcp.CaptureConfig{
			PerfAgent: *perfAgent,
			Dir:       captureDir,
			GPUShim:   *gpuShim,
			Owner:     "stdio",
			Commands:  commands,
		},
	}
	if configHook != nil {
		configHook(&cfg)
	}
	srv, err := perfmcp.New(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "perf-agent-mcp: %v\n", err)
		return 1
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ctx, stdin, stdout) }()
	var serveErr error
	served := false
	select {
	case serveErr = <-serveDone:
		served = true
	case <-ctx.Done():
	}
	signalled := ctx.Err() != nil
	if signalled {
		// The capture dies first, whatever happens to the output: its
		// reaper kills the children and cleans up on its own, and Close
		// then does not wait for the job to roll back; the next server's
		// recovery records it as interrupted.
		killCaptures(srv)
	}
	if !served {
		// Serve writes the answers already queued, for a bounded time. A
		// client that stopped reading stdout makes that drain fail; this
		// wait is the backstop should Serve not return at all.
		select {
		case serveErr = <-serveDone:
		case <-time.After(shutdownGrace):
			exitBlocked(srv, log, "output was blocked: the client is not reading stdout; exiting")
			return 0
		}
	}
	// A failed drain on a signal is both a gone client and ctx's error, so
	// the gone client is tested first. Serve finds a client that stopped
	// reading when its output queue overflows, or after mcpstdio's
	// StallTimeout (30 s) without a finished write; the captures run until
	// then, since a shorter wait would end slow but live clients.
	if errors.Is(serveErr, mcpstdio.ErrClientGone) {
		killCaptures(srv)
		exitBlocked(srv, log, "the client is not reading its output; captures killed, exiting")
		return 0
	}
	// At EOF Close waits for the capture jobs to roll back; a signal then
	// ends the wait. After a signal Close waits for no job.
	closed := make(chan error, 1)
	go func() { closed <- closeServer(srv) }()
	var interrupt <-chan struct{}
	if !signalled {
		interrupt = ctx.Done()
	}
	var closeErr error
	select {
	case closeErr = <-closed:
	case <-interrupt:
		killCaptures(srv)
		exitBlocked(srv, log, "signalled while closing: captures killed, exiting")
		return 0
	}
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		logBounded(log, "serve", "err", serveErr)
		return 1
	}
	if closeErr != nil {
		logBounded(log, "close", "err", closeErr)
		return 1
	}
	return 0
}

// captureFlags are the flags that only matter with --perf-agent.
var captureFlags = []string{"captures", "capture-command", "capture-ttl", "gpu-shim"}

// checkCapturesDir checks --captures against the profiles root and returns
// it relative to the root. An absolute dir must lie inside the root, as
// given or with its symlinks resolved; a relative one is relative to the
// root and must stay inside it. What of it exists is followed through the
// root, as the server opens it, so no symlink leads outside, and must be a
// directory. A root that cannot be opened is left for the server to refuse.
func checkCapturesDir(profiles, dir string) (string, error) {
	rel := dir
	if filepath.IsAbs(dir) {
		rel = ""
		var bases []string
		if abs, err := filepath.Abs(profiles); err == nil {
			bases = append(bases, abs)
			if real, err := filepath.EvalSymlinks(abs); err == nil && real != abs {
				bases = append(bases, real)
			}
		}
		for _, b := range bases {
			if r, err := filepath.Rel(b, dir); err == nil && filepath.IsLocal(r) {
				rel = r
				break
			}
		}
	}
	if rel == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("it is outside the profiles root %s; name a directory inside it", profiles)
	}
	root, err := os.OpenRoot(profiles)
	if err != nil {
		return rel, nil
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat(rel)
	switch {
	case err == nil && !fi.IsDir():
		return "", errors.New("it is not a directory; name a directory inside the profiles root")
	case err == nil, errors.Is(err, fs.ErrNotExist):
		return rel, nil
	case errors.Is(err, fs.ErrPermission):
		return "", fmt.Errorf("it cannot be checked: %v", err)
	}
	return "", fmt.Errorf("it leads outside the profiles root %s through a symlink (%v); name a directory inside it", profiles, err)
}

// shutdownGrace is how long Serve gets to finish after a signal before the
// server exits anyway.
const shutdownGrace = time.Second

// killCaptures and closeServer are the server's KillCaptures and Close; a
// seam for tests, which record the order they run in.
var (
	killCaptures = (*perfmcp.Server).KillCaptures
	closeServer  = (*perfmcp.Server).Close
)

// blockedCloseWait bounds how long a server whose output is blocked waits
// for Close and its last log line before it exits.
const blockedCloseWait = 100 * time.Millisecond

// logBounded logs msg as an error without waiting on stderr for more than
// blockedCloseWait: a client may have stopped reading it.
func logBounded(log *slog.Logger, msg string, args ...any) {
	logged := make(chan struct{})
	go func() { log.Error(msg, args...); close(logged) }()
	select {
	case <-logged:
	case <-time.After(blockedCloseWait):
	}
}

// exitBlocked logs msg and closes srv without waiting on either for more
// than blockedCloseWait: stdout is blocked, and stderr may be too. The
// captures must already be killed, so nothing depends on Close finishing.
func exitBlocked(srv *perfmcp.Server, log *slog.Logger, msg string) {
	logged, closed := make(chan struct{}), make(chan struct{})
	go func() { log.Error(msg); close(logged) }()
	go func() { _ = closeServer(srv); close(closed) }()
	timeout := time.After(blockedCloseWait)
	for range 2 {
		select {
		case <-logged:
			logged = nil
		case <-closed:
			closed = nil
		case <-timeout:
			return
		}
	}
}

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

// zeroLimits are the limits that may be zero: Source.MaxContext (no
// context lines) and Capture.TTL (no age limit).
var zeroLimits = []string{"Source.MaxContext", "Capture.TTL"}

// checkLimits refuses a limit that is not positive, naming it; the server
// would otherwise replace a zero with a default without saying so.
// zeroLimits may be zero.
func checkLimits(l *perfmcp.Limits) error {
	for name, f := range limitFields(l) {
		zero := slices.Contains(zeroLimits, name)
		if f.Int() > 0 || (f.Int() == 0 && zero) {
			continue
		}
		if zero {
			return fmt.Errorf("limit %s must not be negative (0 is allowed), got %d", name, f.Int())
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
