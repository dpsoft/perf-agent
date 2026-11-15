package perfmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/dpsoft/perf-agent/internal/flamegraph"
	"github.com/dpsoft/perf-agent/internal/livehttp"
	"github.com/dpsoft/perf-agent/internal/profilemodel"
)

// maxPathValue is the longest publication id, node id or asset name a
// route looks up; a longer one is not found without a lookup.
const maxPathValue = 64

// notFoundText is the answer for a publication or node the snapshot does
// not hold: the page fetched it from a state that is no longer current,
// and refreshes from the next one.
const notFoundText = "no such publication or node; the page will refresh"

const jsonType = "application/json; charset=utf-8"

// liveMux is the live page's routes, below the guard's /s/<token> prefix.
// Every handler reads the snapshot it loads first, the immutable
// publications and histories in it, and nothing else of the server, so a
// request never changes server state and never races the request
// goroutine. Only GET and HEAD reach it.
func (s *Server) liveMux() http.Handler {
	l := s.live
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { serveAsset(w, r, "") })
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if len(name) > maxPathValue {
			http.NotFound(w, r)
			return
		}
		serveAsset(w, r, "assets/"+name)
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		snap := l.snapshot()
		writeBody(w, jsonType, snap.state)
	})
	mux.Handle("GET /events", l.hub)
	mux.HandleFunc("GET /pub/{id}/model", func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		if p := lookupPub(w, r, snap); p != nil {
			writeBody(w, jsonType, p.model)
		}
	})
	mux.HandleFunc("GET /pub/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		p := lookupPub(w, r, snap)
		if p == nil {
			return
		}
		if h := snap.history[p.id]; h != nil {
			writeBody(w, jsonType, h.json)
			return
		}
		writeBody(w, jsonType, emptyHistory(p.id))
	})
	mux.HandleFunc("GET /pub/{id}/frame/{node}", func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		p, node := lookupNode(w, r, snap)
		if p == nil {
			return
		}
		var out []byte
		var err error
		if p.kind == pubDiff {
			out, err = flamegraph.DiffLiveFrame(p.diff, p.index, profilemodel.UnionID(node))
		} else {
			out, err = flamegraph.ExplainedLiveFrame(p.v, p.result, profilemodel.NodeID(node), p.opts.Digest)
		}
		writeComputed(w, out, err)
	})
	mux.HandleFunc("GET /pub/{id}/focus/{node}", l.heavy(func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		p, node := lookupNode(w, r, snap)
		if p == nil {
			return
		}
		var out []byte
		var err error
		if p.kind == pubDiff {
			opts := p.diffOpts
			opts.Focus = profilemodel.UnionID(node)
			out, err = flamegraph.DiffLiveModel(p.diff, opts)
		} else {
			opts := p.opts
			opts.Focus = profilemodel.NodeID(node)
			out, err = flamegraph.ExplainedLiveModel(p.v, p.result, opts)
		}
		writeComputed(w, out, err)
	}))
	mux.HandleFunc("GET /pub/{id}/export", l.heavy(func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		p := lookupPub(w, r, snap)
		if p == nil {
			return
		}
		var page []byte
		var err error
		if p.kind == pubDiff {
			page, err = exportDiff(p)
		} else {
			page, err = exportExplained(p, snap.history[p.id])
		}
		if _, ok := errors.AsType[*exportSizeError](err); ok {
			unprocessable(w, err)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", livehttp.ExportCSP)
		h.Set("Content-Disposition", `attachment; filename="`+p.id+`.html"`)
		writeBody(w, "text/html; charset=utf-8", page)
	}))
	mux.HandleFunc("/", http.NotFound)
	return cleanPathsOnly(mux)
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

// lookupPub is the snapshot's publication the route names, or nil after
// answering 404.
func lookupPub(w http.ResponseWriter, r *http.Request, snap *liveSnapshot) *publication {
	id := r.PathValue("id")
	if len(id) > maxPathValue {
		notFound(w)
		return nil
	}
	p := snap.pubs[id]
	if p == nil {
		notFound(w)
	}
	return p
}

// lookupNode is the snapshot's publication and the node the route names,
// or a nil publication after answering 404: a node ID of an explained
// publication's view, or a union ID of a diff's, looked up in its index.
// Whether an explained node is in the view is the engine's to say.
func lookupNode(w http.ResponseWriter, r *http.Request, snap *liveSnapshot) (*publication, string) {
	node := r.PathValue("node")
	if len(node) > maxPathValue {
		notFound(w)
		return nil, ""
	}
	p := lookupPub(w, r, snap)
	if p == nil {
		return nil, ""
	}
	if p.kind == pubDiff && !p.hasNode(node) {
		notFound(w)
		return nil, ""
	}
	return p, node
}

// writeComputed answers with a model or frame the engine built, 404 for a
// node the view does not have, and 422 for one it cannot build.
func writeComputed(w http.ResponseWriter, out []byte, err error) {
	switch {
	case errors.Is(err, profilemodel.ErrUnknownNode):
		notFound(w)
	case err != nil:
		unprocessable(w, err)
	default:
		writeBody(w, jsonType, out)
	}
}

// liveHeavy is how many export and focus requests, which each build a
// page or a model of up to MaxPageBytes, the page serves at once.
const liveHeavy = 2

// heavy serves h only while one of the live page's liveHeavy slots is
// free, and otherwise answers 503 at once: a few tabs asking for exports
// cannot hold many page-sized buffers between them.
func (l *live) heavy(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case l.heavySlots <- struct{}{}:
			defer func() { <-l.heavySlots }()
			h(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "busy building another export or focus; retry in a moment", http.StatusServiceUnavailable)
		}
	}
}

// exportSizeError is an export refused for its size.
type exportSizeError struct{ msg string }

func (e *exportSizeError) Error() string { return e.msg }

// exportExplained is p as the static explained page, with h, the rejected
// attempts its history keeps (nil for none), within the page limits. It is
// rendered whole, once, before anything is sent, so a failure sends no
// page. One over the limits is an *exportSizeError; any other failure
// says nothing of the limits. The rows h left out are counted on their
// attempts and in its dropped attempts, so the page says how many it does
// not list.
func exportExplained(p *publication, h *liveHistory) ([]byte, error) {
	opts := flamegraph.ExplainedPageOptions{
		Title: p.opts.Title, ProfileName: p.opts.ProfileName, Digest: p.opts.Digest, Attempt: p.opts.Attempt,
		MaxNodes: p.opts.MaxNodes, MaxBytes: p.opts.MaxBytes,
	}
	if h != nil {
		opts.Rejected, opts.DroppedRejected, opts.HistoryIncomplete = h.rejected, h.dropped, h.incomplete
	}
	page, err := flamegraph.ExplainedPage(p.v, p.result, opts)
	switch {
	case errors.Is(err, flamegraph.ErrPageTooLarge):
		return nil, &exportSizeError{fmt.Sprintf("the page cannot be exported within MaxPageBytes=%d: %v", p.opts.MaxBytes, err)}
	case err != nil:
		return nil, fmt.Errorf("the page cannot be exported: %v", err)
	case len(page) > p.opts.MaxBytes:
		return nil, &exportSizeError{fmt.Sprintf("the page is %d bytes, over MaxPageBytes=%d", len(page), p.opts.MaxBytes)}
	}
	return page, nil
}

// exportDiff is diff publication p as WriteDiffPage writes it, without
// the agent's title, whose numbers that page would show unmarked. It is
// bounded before any of it is built, against the limits the model's
// export state was judged by, and rendered whole before anything is sent.
// One over the limits is an *exportSizeError.
func exportDiff(p *publication) ([]byte, error) {
	var page bytes.Buffer
	err := flamegraph.WriteDiffPageBounded(&page, p.diff, p.export, p.diffOpts.MaxBytes, p.diffOpts.MaxNodes)
	switch {
	case errors.Is(err, flamegraph.ErrPageTooLarge):
		return nil, &exportSizeError{fmt.Sprintf("the diff cannot be exported within this server's MaxPageBytes=%d and MaxPageNodes=%d: %v",
			p.diffOpts.MaxBytes, p.diffOpts.MaxNodes, err)}
	case err != nil:
		return nil, fmt.Errorf("the diff cannot be exported: %v", err)
	}
	return page.Bytes(), nil
}

// emptyHistory is the history of a publication without one, a diff.
func emptyHistory(id string) []byte {
	out, err := json.Marshal(struct {
		Publication string   `json:"publication"`
		Version     string   `json:"version"`
		Rows        []string `json:"rows"`
	}{id, "0", []string{}})
	if err != nil {
		// A string and an empty list always encode.
		panic(err)
	}
	return out
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

func notFound(w http.ResponseWriter) { http.Error(w, notFoundText, http.StatusNotFound) }

// unprocessable answers 422 with err's text: the route exists, but what
// it names cannot be built within the page limits.
func unprocessable(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusUnprocessableEntity)
}
