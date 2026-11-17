package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/mcpflags"
	"github.com/dpsoft/perf-agent/internal/mcphttp"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
	"github.com/dpsoft/perf-agent/internal/perfpage"
)

// command is the binary's name, in serverInfo and in every advice that
// tells the user to start or restart the server.
const command = "perf-agent-hub"

// host is the only address the hub listens on.
const host = "127.0.0.1"

// inboxRate bounds the page's reports: five a second, five at once.
var inboxRate = livehttp.Rate{PerSecond: 5, Burst: 5}

// maxConns bounds the connections the listener serves at once; one more
// waits in the kernel's backlog until another closes. It is a memory
// bound: an answer the client has not read yet is outside the
// transport's caps, at most one per connection, each at most
// MaxAnswerBytes, for at most the transport's WriteBound (10 s). So
// unread answers hold at most maxConns × MaxAnswerBytes (64 × 2 MiB with
// the default limits), and only for clients holding the MCP token.
const maxConns = 64

// The shutdown's bounds: httpGrace is what calls that ignore their
// context, and responses being written, get before their connections are
// dropped; every other step gets stepWait, and Close closeWait.
const (
	httpGrace = time.Second
	stepWait  = mcpflags.BlockedWait
	closeWait = 500 * time.Millisecond
)

// configHook, when set, adjusts the server's configuration before it is
// built. The command sets none; tests use it to set test seams, such as
// Config.HoldEngine.
var configHook func(*perfmcp.Config)

// listen binds the hub's address; a seam for tests, which hold the start
// there.
var listen = livehttp.Listen

// The shutdown's steps, in the order shutdown runs them; seams for tests,
// which record that order.
var (
	killCaptures   = (*perfmcp.Server).KillCaptures
	closeEvents    = (*livehttp.Hub).Close
	closeTransport = (*mcphttp.Transport).Close
	shutdownHTTP   = func(hs *http.Server) {
		ctx, cancel := context.WithTimeout(context.Background(), httpGrace)
		defer cancel()
		_ = hs.Shutdown(ctx)
		_ = hs.Close()
	}
	closeServer = (*perfmcp.Server).Close
	removeFiles = (*runtimeDir).remove
)

// options are what start needs besides the context.
type options struct {
	// cfg is the server's configuration from the flags; its Log is the
	// hub's log, which must never block.
	cfg perfmcp.Config
	// port is the loopback port; runtimeDir is --runtime-dir.
	port       int
	runtimeDir string
	getenv     func(string) string
	// stdout gets the page URL, once.
	stdout io.Writer
}

// instance is a running hub.
type instance struct {
	log    *slog.Logger
	dir    *runtimeDir
	srv    *perfmcp.Server
	events *livehttp.Hub
	tr     *mcphttp.Transport
	hs     *http.Server
	// served gets the HTTP server's error should it stop on its own.
	served chan error
	once   sync.Once
}

// start starts the hub, in this order: the runtime directory and its
// lock, which removes a dead server's files; the tokens; the page's
// events hub; the perfmcp server; the MCP transport; the listener; the
// HTTP server; the runtime files; the page URL, once, on stdout; and the
// "listening" log line, which names the MCP URL and no token. On an error
// it undoes what it did.
func start(ctx context.Context, o options) (_ *instance, err error) {
	log := o.cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	dir, err := openRuntimeDir(o.runtimeDir, o.getenv)
	if err != nil {
		return nil, err
	}
	if _, err := dir.take(); err != nil {
		dir.close()
		return nil, err
	}
	in := &instance{log: log, dir: dir, served: make(chan error, 1)}
	defer func() {
		if err != nil {
			in.undo()
		}
	}()
	mcpTok, err := livehttp.NewToken()
	if err != nil {
		return nil, err
	}
	pageTok, err := livehttp.NewToken()
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(host, strconv.Itoa(o.port))
	pageURL := "http://" + addr + "/s/" + string(pageTok) + "/"
	mcpURL := "http://" + addr + "/mcp"

	cfg := o.cfg
	in.events = perfpage.NewEvents(cfg.Limits)
	events := in.events
	cfg.Command = command
	cfg.Page = &perfmcp.PageConfig{
		URL:     pageURL,
		Notify:  func(state []byte) { _ = events.Publish(state) },
		Viewers: events.Clients,
	}
	if configHook != nil {
		configHook(&cfg)
	}
	if in.srv, err = perfmcp.New(cfg); err != nil {
		return nil, err
	}
	hub := cfg.Limits.Hub
	in.tr = mcphttp.New(in.srv.MCP(), mcphttp.Options{
		MaxSessions:          hub.MaxSessions,
		MaxMessageBytes:      int64(cfg.Limits.MaxMessageBytes),
		IngressBodyTimeout:   hub.IngressBodyTimeout,
		MaxAnswerBytes:       int64(in.srv.MCP().MaxAnswerBytes()),
		MaxIngressReaders:    hub.MaxIngressReaders,
		MaxIngressBytes:      hub.MaxIngressBytes,
		MaxControlInFlight:   hub.MaxControlInFlight,
		MaxPageStateInFlight: hub.MaxPageStateInFlight,
		MaxCalls:             hub.MaxCalls,
		MaxCallsPerSession:   hub.MaxCallsPerSession,
		MaxRequestBytes:      hub.MaxRequestBytes,
		Log:                  log,
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ln, err := listen(addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("port %d is in use; start with --port N", o.port)
		}
		return nil, err
	}
	in.hs = livehttp.NewServer(livehttp.Front(pageTok,
		livehttp.MCPGuard(mcpTok, int64(cfg.Limits.MaxMessageBytes), in.tr),
		livehttp.InboxGuard(pageTok, inboxRate, perfpage.Inbox(in.srv)),
		livehttp.Guard(pageTok, perfpage.Routes(in.srv, in.events))), log)
	limited := newLimitListener(ln, maxConns)
	go func() {
		if err := in.hs.Serve(limited); !errors.Is(err, http.ErrServerClosed) {
			in.served <- err
		}
	}()
	info := hubInfo{PID: os.Getpid(), Port: o.port, MCPURL: mcpURL, Started: time.Now(), Version: cfg.Version}
	if err := in.dir.write(info, mcpTok, pageURL); err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintln(o.stdout, pageURL)
	log.Info("perf-agent-hub listening", "mcp_url", mcpURL, "pid", info.PID)
	return in, nil
}

// undo releases what a failed start made: each part start reached is
// closed, and the runtime files it may have written are removed.
func (in *instance) undo() {
	in.once.Do(func() {
		if in.tr != nil {
			in.tr.Close()
		}
		if in.hs != nil {
			_ = in.hs.Close()
		}
		if in.events != nil {
			in.events.Close()
		}
		if in.srv != nil {
			_ = in.srv.Close()
		}
		_ = in.dir.remove()
	})
}

// shutdown stops the hub, each step bounded, in this order:
//  1. the running capture is killed: its reaper ends it whatever follows;
//  2. the page's streams end, so the HTTP server has none to wait for;
//  3. the transport closes: every call's context ends, so a capture_wait
//     or a wait for the engine answers at once, and later requests get
//     503;
//  4. the HTTP server stops listening and gives the answers being written
//     httpGrace, then drops every connection;
//  5. the perfmcp server closes;
//  6. the runtime files are removed and the lock released.
//
// A second shutdown does nothing.
func (in *instance) shutdown() {
	in.once.Do(func() {
		mcpflags.Bounded(stepWait, func() { killCaptures(in.srv) })
		mcpflags.Bounded(stepWait, func() { closeEvents(in.events) })
		mcpflags.Bounded(stepWait, func() { closeTransport(in.tr) })
		mcpflags.Bounded(httpGrace+stepWait, func() { shutdownHTTP(in.hs) })
		mcpflags.Bounded(closeWait, func() {
			if err := closeServer(in.srv); err != nil {
				mcpflags.LogBounded(in.log, "close", "err", err)
			}
		})
		mcpflags.Bounded(stepWait, func() {
			if err := removeFiles(in.dir); err != nil {
				mcpflags.LogBounded(in.log, "remove the runtime files", "err", err)
			}
		})
	})
}

// limitListener accepts at most n connections at once: Accept waits for
// one of them to close before it takes the next from the backlog.
type limitListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	close sync.Once
}

func newLimitListener(ln net.Listener, n int) *limitListener {
	return &limitListener{Listener: ln, slots: make(chan struct{}, n), done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: c, release: sync.OnceFunc(func() { <-l.slots })}, nil
}

func (l *limitListener) Close() error {
	l.close.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// limitConn gives its slot back when it is first closed.
type limitConn struct {
	net.Conn
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
