package perfmcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"

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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) { serveAsset(w, "") })
	mux.HandleFunc("GET /assets/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if len(name) > maxPathValue {
			http.NotFound(w, r)
			return
		}
		serveAsset(w, "assets/"+name)
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
		out, err := flamegraph.ExplainedLiveFrame(p.v, p.result, node, p.opts.Digest)
		writeComputed(w, out, err)
	})
	mux.HandleFunc("GET /pub/{id}/focus/{node}", func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		p, node := lookupNode(w, r, snap)
		if p == nil {
			return
		}
		opts := p.opts
		opts.Focus = node
		out, err := flamegraph.ExplainedLiveModel(p.v, p.result, opts)
		writeComputed(w, out, err)
	})
	mux.HandleFunc("GET /pub/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		snap := l.snapshot()
		p := lookupPub(w, r, snap)
		if p == nil {
			return
		}
		if p.kind != pubExplained {
			notFound(w)
			return
		}
		page, err := exportExplained(p, snap.history[p.id])
		if err != nil {
			unprocessable(w, err)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", livehttp.ExportCSP)
		h.Set("Content-Disposition", `attachment; filename="`+p.id+`.html"`)
		writeBody(w, "text/html; charset=utf-8", page)
	})
	mux.HandleFunc("/", http.NotFound)
	return cleanPathsOnly(mux)
}

// cleanPathsOnly answers 404 for a path that is not clean or that holds
// an escaped character the route would decode, such as %2F. The mux would
// redirect such a path to its cleaned form, from the root rather than
// below the token, and no route needs one.
func cleanPathsOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawPath != "" || path.Clean(r.URL.Path) != r.URL.Path {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
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

// lookupNode is the snapshot's explained publication and the node the
// route names, or a nil publication after answering 404. Whether the node
// is in the view is the engine's to say.
func lookupNode(w http.ResponseWriter, r *http.Request, snap *liveSnapshot) (*publication, profilemodel.NodeID) {
	node := r.PathValue("node")
	if len(node) > maxPathValue {
		notFound(w)
		return nil, ""
	}
	p := lookupPub(w, r, snap)
	if p == nil {
		return nil, ""
	}
	if p.kind != pubExplained {
		notFound(w)
		return nil, ""
	}
	return p, profilemodel.NodeID(node)
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

// exportExplained is p as the static explained page, with h, the rejected
// attempts its history keeps (nil for none), within the page limits. It is
// rendered whole before anything is sent, so a failure sends no page. The
// rows h left out are counted on their attempts and in its dropped
// attempts, so the page says how many it does not list.
func exportExplained(p *publication, h *liveHistory) ([]byte, error) {
	opts := flamegraph.ExplainedPageOptions{
		Title: p.opts.Title, ProfileName: p.opts.ProfileName, Digest: p.opts.Digest, Attempt: p.opts.Attempt,
		MaxNodes: p.opts.MaxNodes, MaxBytes: p.opts.MaxBytes,
	}
	if h != nil {
		opts.Rejected, opts.DroppedRejected, opts.HistoryIncomplete = h.rejected, h.dropped, h.incomplete
	}
	var page bytes.Buffer
	if err := flamegraph.WriteExplainedPage(&page, p.v, p.result, opts); err != nil {
		return nil, fmt.Errorf("the page cannot be exported within MaxPageBytes=%d: %v", p.opts.MaxBytes, err)
	}
	if p.opts.MaxBytes > 0 && page.Len() > p.opts.MaxBytes {
		return nil, fmt.Errorf("the page is %d bytes, over MaxPageBytes=%d", page.Len(), p.opts.MaxBytes)
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

func serveAsset(w http.ResponseWriter, name string) {
	body, ctype, ok := flamegraph.LiveAsset(name)
	if !ok {
		http.Error(w, "404 page not found", http.StatusNotFound)
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
