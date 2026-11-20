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
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/mcpflags"
	"github.com/dpsoft/perf-agent/internal/mcpstdio"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// version is reported in serverInfo and by --version.
var version = "dev"

// configHook, when set, adjusts the server's configuration before it is
// built. The command sets none; tests use it to read the configuration or
// to set test seams, such as a smaller output queue.
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

// runContext serves MCP on stdin and stdout until EOF or ctx is done. It
// returns 0 on a clean end, 1 on a runtime error and 2 on a usage error.
// Stdout carries only JSON-RPC; diagnostics go to stderr.
func runContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("perf-agent-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	vals := mcpflags.Register(fs, mcpflags.Stdio)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if vals.Version() {
		_, _ = fmt.Fprintln(stdout, "perf-agent-mcp", version)
		return 0
	}
	cfg, err := vals.Config(fs, stderr, version)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	log := cfg.Log
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

// shutdownGrace is how long Serve gets to finish after a signal before the
// server exits anyway.
const shutdownGrace = time.Second

// killCaptures and closeServer are the server's KillCaptures and Close; a
// seam for tests, which record the order they run in.
var (
	killCaptures = (*perfmcp.Server).KillCaptures
	closeServer  = (*perfmcp.Server).Close
)

// logBounded logs msg as an error without waiting on stderr for more than
// mcpflags.BlockedWait: a client may have stopped reading it.
func logBounded(log *slog.Logger, msg string, args ...any) {
	mcpflags.LogBounded(log, msg, args...)
}

// exitBlocked logs msg and closes srv without waiting on either for more
// than mcpflags.BlockedWait: stdout is blocked, and stderr may be too. The
// captures must already be killed, so nothing depends on Close finishing.
func exitBlocked(srv *perfmcp.Server, log *slog.Logger, msg string) {
	mcpflags.Bounded(mcpflags.BlockedWait,
		func() { log.Error(msg) },
		func() { _ = closeServer(srv) })
}
