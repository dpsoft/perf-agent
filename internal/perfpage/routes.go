package perfpage

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// maxAssetName is the longest asset name a route looks up; a longer one
// is not found without a lookup.
const maxAssetName = 64

const jsonType = "application/json; charset=utf-8"

// heavySlots is how many export and focus requests, which each build a
// page or a model of up to MaxPageBytes, the page serves at once.
const heavySlots = 2

// routes is the page's handler: the mux and the heavy routes' slots.
type routes struct {
	mux http.Handler
	// heavy holds a token per export or focus request being served; see
	// bounded.
	heavy chan struct{}
}

// Routes is the live page's routes, below the guard's /s/<token> prefix:
// the shell and its assets, the state, the event stream from events, and
// each publication's model, history, frame, focus and export. The state
// is PageView.State, the envelope of the engine's state and the inbox's,
// which every event carries too. Every handler reads the snapshot it
// takes first, and nothing else of srv, so a request never changes server
// state. Only GET and HEAD reach it; the page's reports go to Inbox.
//
// A server's page has one set of routes, so that their bounds, such as
// heavySlots, hold for the server: Routes claims srv's page, and panics
// for a server without one or one already routed. Either is a programming
// error, as non-positive limits are to livehttp.NewHub.
func Routes(srv *perfmcp.Server, events http.Handler) http.Handler {
	return newRoutes(srv, events)
}

func newRoutes(srv *perfmcp.Server, events http.Handler) *routes {
	if err := srv.ClaimPage(); err != nil {
		panic("perfpage.Routes: " + err.Error())
	}
	rt := &routes{heavy: make(chan struct{}, heavySlots)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { serveAsset(w, r, "") })
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if len(name) > maxAssetName {
			http.NotFound(w, r)
			return
		}
		serveAsset(w, r, "assets/"+name)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		writeBody(w, jsonType, srv.PageSnapshot().State())
	})
	mux.Handle("GET /events", events)
	mux.HandleFunc("GET /pub/{id}/model", func(w http.ResponseWriter, r *http.Request) {
		out, err := srv.PageSnapshot().Model(r.PathValue("id"))
		writeComputed(w, out, err)
	})
	mux.HandleFunc("GET /pub/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		out, err := srv.PageSnapshot().History(r.PathValue("id"))
		writeComputed(w, out, err)
	})
	mux.HandleFunc("GET /pub/{id}/frame/{node}", func(w http.ResponseWriter, r *http.Request) {
		out, err := srv.PageSnapshot().Frame(r.PathValue("id"), r.PathValue("node"))
		writeComputed(w, out, err)
	})
	mux.HandleFunc("GET /pub/{id}/focus/{node}", rt.bounded(func(w http.ResponseWriter, r *http.Request) {
		out, err := srv.PageSnapshot().Focus(r.PathValue("id"), r.PathValue("node"))
		writeComputed(w, out, err)
	}))
	mux.HandleFunc("GET /pub/{id}/export", rt.bounded(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		page, err := srv.PageSnapshot().Export(id)
		writeExport(w, id, page, err)
	}))
	mux.HandleFunc("/", http.NotFound)
	rt.mux = cleanPathsOnly(mux)
	return rt
}

func (rt *routes) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

// bounded serves h only while one of the heavy slots is free, and
// otherwise answers 503 at once: a few tabs asking for exports cannot
// hold many page-sized buffers between them. The slot is let go before
// the last byte of the answer is written (see lastByteWriter), so the
// buffer is held under the slot until all but one byte of it is sent,
// and a client holding the whole answer never finds its slot taken.
func (rt *routes) bounded(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case rt.heavy <- struct{}{}:
			lw := &lastByteWriter{ResponseWriter: w, release: sync.OnceFunc(func() { <-rt.heavy })}
			defer lw.release()
			h(lw, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy building another export or focus; retry in a moment", http.StatusServiceUnavailable)
		}
	}
}

// lastByteWriter calls release, once, before it writes the last byte of a
// Write. Every answer of a heavy route is written whole in one Write, so
// the slot is held until all but the answer's last byte is written.
type lastByteWriter struct {
	http.ResponseWriter
	release func()
}

// Unwrap is the writer it wraps, so an http.ResponseController made on a
// heavy route still reaches the connection's deadlines and flush.
func (w *lastByteWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *lastByteWriter) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return w.ResponseWriter.Write(b)
	}
	n, err := w.ResponseWriter.Write(b[:len(b)-1])
	w.release()
	if err != nil {
		return n, err
	}
	m, err := w.ResponseWriter.Write(b[len(b)-1:])
	return n + m, err
}

// cleanPathsOnly answers 404 for a path that is not clean or that holds
// an escaped character the route would decode, such as %2F. The mux would
// redirect such a path to its cleaned form, from the root rather than
// below the token. The one escape served is a union ID's slash on a
// frame or focus route, which the page sends as %2F (see unionRoute).
func cleanPathsOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" && !unionRoute(r.URL.Path, r.URL.RawPath) || path.Clean(r.URL.Path) != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// unionRoute reports whether a path whose escaped form is raw is a frame
// or focus route of a union node, "pub/<id>/frame/<base>%2F<new>" or
// ".../focus/...", with each side a node ID or "-" and nothing else
// escaped: raw is exactly the decoded path with that one slash written
// %2F, as encodeURIComponent writes it.
func unionRoute(decoded, raw string) bool {
	parts := strings.Split(decoded, "/")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "pub" || parts[3] != "frame" && parts[3] != "focus" ||
		!isSideID(parts[4]) || !isSideID(parts[5]) {
		return false
	}
	return raw == strings.Join(parts[:5], "/")+"%2F"+parts[5]
}

// isSideID reports whether s is one side of a union ID: "-" for a side
// the node is absent from, or a node ID, 16 lowercase hex digits.
func isSideID(s string) bool {
	if s == "-" {
		return true
	}
	if len(s) != 16 {
		return false
	}
	return !strings.ContainsFunc(s, func(c rune) bool {
		return (c < '0' || c > '9') && (c < 'a' || c > 'f')
	})
}

// writeExport answers with page, publication id's export, as a download
// the browser runs nothing of: 404 for a publication the snapshot does
// not hold, 422 for one over the page limits, and 500 for any other
// failure, whose text says nothing of the limits.
func writeExport(w http.ResponseWriter, id string, page []byte, err error) {
	_, tooLarge := errors.AsType[*perfmcp.PageLimitError](err)
	switch {
	case errors.Is(err, perfmcp.ErrPageNotFound):
		notFound(w)
		return
	case tooLarge:
		unprocessable(w, err)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", livehttp.ExportCSP)
	// The ID is one the snapshot holds, "p<n>": nothing to quote.
	h.Set("Content-Disposition", `attachment; filename="`+id+`.html"`)
	writeBody(w, "text/html; charset=utf-8", page)
}

// writeComputed answers with what the engine built: 404 for a publication
// or node the snapshot does not hold, and 422, with the engine's reason,
// for one it cannot build.
func writeComputed(w http.ResponseWriter, out []byte, err error) {
	switch {
	case errors.Is(err, perfmcp.ErrPageNotFound):
		notFound(w)
	case err != nil:
		unprocessable(w, err)
	default:
		writeBody(w, jsonType, out)
	}
}

func serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	body, ctype, ok := flamegraph.LiveAsset(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeBody(w, ctype, body)
}

// writeBody answers 200 with body, whole and of a known length.
func writeBody(w http.ResponseWriter, ctype string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func notFound(w http.ResponseWriter) {
	http.Error(w, perfmcp.ErrPageNotFound.Error(), http.StatusNotFound)
}

// unprocessable answers 422 with err's text: the route exists, but what
// it names cannot be built within the page limits.
func unprocessable(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusUnprocessableEntity)
}
