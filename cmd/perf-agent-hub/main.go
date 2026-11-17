// Command perf-agent-hub is one long-lived perf-agent MCP server on this
// machine, shared by several agents: it speaks MCP over Streamable HTTP
// at http://127.0.0.1:PORT/mcp behind a bearer token, and serves the live
// page, under a token of its own, to the user's browser. It listens on
// 127.0.0.1 only, opens no outgoing connection, and runs once per user:
// its runtime directory holds the lock, the token and hub.json. With
// --perf-agent it also captures profiles, as perf-agent-mcp does.
//
//	perf-agent-hub --profiles DIR [--port N] [--runtime-dir DIR] [flags]
//	perf-agent-hub token (--port N | --url URL) [--runtime-dir DIR]
//	perf-agent-hub url [--runtime-dir DIR]
//
// token prints {"Authorization":"Bearer <token>"} for a client's headers
// helper, and url the page's address.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/dpsoft/perf-agent/internal/capture"
	"github.com/dpsoft/perf-agent/internal/mcpflags"
)

// version is reported in serverInfo, in hub.json and by --version.
var version = "dev"

// DefaultPort is the port the hub listens on without --port.
const DefaultPort = 7341

func main() {
	// A capture's reaper and launcher are this binary re-executed in an
	// internal mode; they never reach the flags.
	if code, handled := capture.RunInternal(); handled {
		os.Exit(code)
	}
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "token":
			os.Exit(runToken(args[1:], os.Getenv, os.Stdout, os.Stderr))
		case "url":
			os.Exit(runURL(args[1:], os.Getenv, os.Stdout, os.Stderr))
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := runServe(ctx, args, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// runServe runs the hub until ctx is done, which is a clean end (0), or
// its HTTP server fails (1). A usage error is 2 and a failed start 1.
// Stdout carries only the page URL; diagnostics go to stderr. Neither is
// ever waited on for long: a terminal that stopped reading them never
// holds a request or the exit.
func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	errOut := newQueueWriter(stderr)
	out := newQueueWriter(stdout)
	defer func() { mcpflags.Bounded(mcpflags.BlockedWait, errOut.flush, out.flush) }()
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(errOut)
	vals := mcpflags.Register(fs, mcpflags.Hub)
	port := fs.Int("port", DefaultPort, "loopback `PORT` the hub listens on, at 127.0.0.1 (1 to 65535)")
	runtimeDir := fs.String("runtime-dir", "", "runtime `DIR` for the lock, the token and hub.json, mode 0700 and yours (default $XDG_RUNTIME_DIR/perf-agent-mcp)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if vals.Version() {
		_, _ = fmt.Fprintln(out, command, version)
		return 0
	}
	if *port < 1 || *port > 65535 {
		_, _ = fmt.Fprintf(errOut, "%s: --port %d is out of range; give a port from 1 to 65535\n", command, *port)
		return 2
	}
	cfg, err := vals.Config(fs, errOut, version)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 2
	}
	in, err := start(ctx, options{cfg: cfg, port: *port, runtimeDir: *runtimeDir, getenv: os.Getenv, stdout: out})
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "%s: %v\n", command, err)
		return 1
	}
	code := 0
	select {
	case <-ctx.Done():
	case err := <-in.served:
		mcpflags.LogBounded(in.log, "serve", "err", err)
		code = 1
	}
	in.shutdown()
	return code
}

// tokenUsage is the token subcommand's usage line.
const tokenUsage = "token (--port N | --url URL) [--runtime-dir DIR]"

// runToken prints the MCP token as a client's headers helper reads it,
// {"Authorization":"Bearer <token>"}, but only when a hub runs and listens
// on the port the client names: another program may listen on a port the
// client's configuration still names, and must never get the token.
// Otherwise it prints nothing on stdout, so the client sends no header,
// and exits 1 with the reason on stderr. A usage error exits 2.
func runToken(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(command+" token", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Int("port", 0, "the `PORT` of http://127.0.0.1:PORT/mcp in the client's configuration")
	rawURL := fs.String("url", "", "the MCP `URL` in the client's configuration, http://127.0.0.1:PORT/mcp")
	runtimeDir := fs.String("runtime-dir", "", "the hub's runtime `DIR`, as it was started with (default $XDG_RUNTIME_DIR/perf-agent-mcp)")
	usage := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "%s token: %s\nusage: %s %s\n", command, fmt.Sprintf(format, a...), command, tokenUsage)
		return 2
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case fs.NArg() > 0:
		return usage("unexpected argument %q", fs.Arg(0))
	case set["port"] == set["url"]:
		return usage("give either --port N or --url URL: the port or URL the client connects to")
	case set["port"] && (*port < 1 || *port > 65535):
		return usage("--port %d is out of range; give a port from 1 to 65535", *port)
	}
	want := *port
	if set["url"] {
		p, err := urlPort(*rawURL)
		if err != nil {
			return usage("--url %s: %v", *rawURL, err)
		}
		want = p
	}
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "%s token: %s\n", command, fmt.Sprintf(format, a...))
		return 1
	}
	dir, err := openRuntimeDir(*runtimeDir, getenv)
	if err != nil {
		return fail("%v", err)
	}
	defer dir.close()
	// hub.json is read before and after the token, and the lock tested
	// last, so the token printed is the running hub's, for its port.
	before, err := dir.info()
	if err != nil {
		return fail("perf-agent-hub is not running (no hub.json in %s); start it with %s --profiles DIR", dir.path, command)
	}
	b, err := dir.root.ReadFile(tokenFile)
	if err != nil {
		return fail("perf-agent-hub is not running (no mcp-token in %s)", dir.path)
	}
	tok := strings.TrimSuffix(string(b), "\n")
	after, err := dir.info()
	if err != nil || after != before || !running(dir.path) {
		return fail("perf-agent-hub is not running (its lock in %s is free); start it with %s --profiles DIR", dir.path, command)
	}
	if before.Port != want {
		return fail("perf-agent-hub listens on port %d, not %d; point the client at http://%s:%d/mcp", before.Port, want, host, before.Port)
	}
	if !wellFormedToken(tok) {
		return fail("mcp-token in %s is not a token perf-agent-hub writes", dir.path)
	}
	header, err := json.Marshal(map[string]string{"Authorization": "Bearer " + tok})
	if err != nil {
		return fail("%v", err)
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", header); err != nil {
		return 1
	}
	return 0
}

// urlPort is the port of an MCP URL a client may hold for the hub:
// http, host 127.0.0.1 and a port. Any other host is refused, localhost
// included: it may resolve to ::1, where the hub does not listen and
// another program may.
func urlPort(raw string) (int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, errors.New("not a URL")
	}
	if u.Scheme != "http" || u.Hostname() != host || u.Port() == "" {
		return 0, fmt.Errorf("the hub's URL is http://%s:PORT/mcp; it listens on %s only", host, host)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p < 1 || p > 65535 {
		return 0, errors.New("the port is out of range")
	}
	return p, nil
}

// wellFormedToken reports a token as livehttp.NewToken makes them: 32
// bytes in unpadded base64url.
func wellFormedToken(tok string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(tok)
	return err == nil && len(b) == 32
}

// runURL prints the page URL of the running hub, or exits 1 with the
// reason on stderr. A usage error exits 2.
func runURL(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(command+" url", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runtimeDir := fs.String("runtime-dir", "", "the hub's runtime `DIR`, as it was started with (default $XDG_RUNTIME_DIR/perf-agent-mcp)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "%s url: unexpected argument %q\nusage: %s url [--runtime-dir DIR]\n", command, fs.Arg(0), command)
		return 2
	}
	dir, err := openRuntimeDir(*runtimeDir, getenv)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s url: %v\n", command, err)
		return 1
	}
	defer dir.close()
	b, err := dir.root.ReadFile(pageFile)
	if err != nil || !running(dir.path) {
		_, _ = fmt.Fprintf(stderr, "%s url: perf-agent-hub is not running (%s); start it with %s --profiles DIR\n", command, filepath.Join(dir.path, lockFile), command)
		return 1
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", strings.TrimSpace(string(b))); err != nil {
		return 1
	}
	return 0
}

// queueWriter hands each write to one goroutine that writes them in order
// to out, and never waits: a write that finds queueLines writes queued is
// dropped, and counted in the next line written. So the log, which
// request goroutines and tool calls write to, never blocks on a stderr
// nobody reads.
type queueWriter struct {
	out     io.Writer
	q       chan queued
	dropped atomic.Int64
}

// queueLines bounds the writes waiting.
const queueLines = 256

// queued is one write, or a flush marker when done is set.
type queued struct {
	b    []byte
	done chan struct{}
}

func newQueueWriter(out io.Writer) *queueWriter {
	w := &queueWriter{out: out, q: make(chan queued, queueLines)}
	go w.run()
	return w
}

func (w *queueWriter) Write(p []byte) (int, error) {
	select {
	case w.q <- queued{b: append([]byte(nil), p...)}:
	default:
		w.dropped.Add(1)
	}
	return len(p), nil
}

func (w *queueWriter) run() {
	for e := range w.q {
		if e.done != nil {
			close(e.done)
			continue
		}
		if n := w.dropped.Swap(0); n > 0 {
			_, _ = fmt.Fprintf(w.out, "%s: %d writes dropped: the output was not read\n", command, n)
		}
		_, _ = w.out.Write(e.b)
	}
}

// flush waits until every write queued before it is written. It may wait
// for good on a stuck output; callers bound it.
func (w *queueWriter) flush() {
	done := make(chan struct{})
	w.q <- queued{done: done}
	<-done
}
