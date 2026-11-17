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

// maxConns bounds the connections the listener holds at once, waiting
// for their first byte or served: a bound on file descriptors only. One
// more waits in the kernel's backlog until another closes.
const maxConns = 512

// firstByteWait is how long a new connection may send nothing: then it
// is closed unanswered. Once a byte has come, the request has the HTTP
// server's own header timeout (5 s).
const firstByteWait = 2 * time.Second

// maxAnswersWriting bounds the MCP answers being written at once, which
// is the memory bound on answers clients have not read: at most this many,
// each at most srv.MCP().MaxAnswerBytes() (half of stdio's output bound:
// 2 MiB + 255 B at the default MaxResponseBytes of 1 MiB, so 128 MiB in
// all), for at most the transport's WriteBound (10 s). Only requests past
// MCPGuard, from clients holding the MCP token, take these places.
//
// What remains: anyone on this machine can still fill a loopback
// listener, here by opening maxConns connections again and again, each
// held up to firstByteWait. That is inherent to a TCP port; the hub
// recovers once they stop, and they never reach a guard, the engine or
// the answers.
const maxAnswersWriting = 64

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
	if err := dir.take(); err != nil {
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
		MaxAnswersWriting:    maxAnswersWriting,
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
	limited := newConnListener(ln, maxConns, firstByteWait)
	go func() {
		if err := in.hs.Serve(limited); !errors.Is(err, http.ErrServerClosed) {
			in.served <- err
		}
	}()
	// A signal while starting ends the start before anything is
	// announced: no runtime file, no page URL.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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

// connListener holds at most n connections at once, and hands one to the
// HTTP server only once its first byte has come: a connection that sends
// nothing for the first-byte wait is closed unanswered, and never reaches
// the server.
type connListener struct {
	ln    net.Listener
	wait  time.Duration
	slots chan struct{}
	ready chan *slotConn
	errc  chan error
	done  chan struct{}
	once  sync.Once

	mu sync.Mutex
	// pending are the connections waiting for their first byte, which
	// Close closes.
	pending map[*slotConn]struct{}
}

func newConnListener(ln net.Listener, n int, wait time.Duration) *connListener {
	l := &connListener{ln: ln, wait: wait, slots: make(chan struct{}, n), ready: make(chan *slotConn),
		errc: make(chan error, 1), done: make(chan struct{}), pending: map[*slotConn]struct{}{}}
	go l.acceptLoop()
	return l
}

// acceptLoop takes a slot, then a connection from the backlog, and waits
// for that connection's first byte on its own goroutine.
func (l *connListener) acceptLoop() {
	for {
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			return
		}
		c, err := l.ln.Accept()
		if err != nil {
			<-l.slots
			select {
			case l.errc <- err:
			case <-l.done:
			}
			return
		}
		sc := &slotConn{Conn: c, release: sync.OnceFunc(func() { <-l.slots })}
		l.mu.Lock()
		l.pending[sc] = struct{}{}
		l.mu.Unlock()
		go l.awaitFirstByte(sc)
	}
}

func (l *connListener) awaitFirstByte(c *slotConn) {
	var b [1]byte
	_ = c.SetReadDeadline(time.Now().Add(l.wait))
	n, _ := c.Conn.Read(b[:])
	l.mu.Lock()
	delete(l.pending, c)
	l.mu.Unlock()
	if n == 0 {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	c.first = b[:]
	select {
	case l.ready <- c:
	case <-l.done:
		_ = c.Close()
	}
}

func (l *connListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ready:
		return c, nil
	case err := <-l.errc:
		return nil, err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting and closes the connections still waiting for
// their first byte; the server closes the ones it was handed.
func (l *connListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.mu.Lock()
		for c := range l.pending {
			_ = c.Conn.Close()
		}
		l.mu.Unlock()
	})
	return l.ln.Close()
}

func (l *connListener) Addr() net.Addr { return l.ln.Addr() }

// slotConn is a connection holding a slot, which it gives back when it is
// first closed, and whose first byte was already read.
type slotConn struct {
	net.Conn
	release func()
	first   []byte
}

func (c *slotConn) Read(p []byte) (int, error) {
	if len(c.first) > 0 {
		n := copy(p, c.first)
		c.first = c.first[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

func (c *slotConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
