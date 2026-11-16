package perfpage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// maxReport is the largest report body Inbox decodes, as InboxGuard
// admits.
const maxReport = 1024

// errNotAReport is the reason for a body that is not one report.
var errNotAReport = fmt.Errorf("%w: the body must be one JSON object of the strings kind, publication, node and focus, each at most once", perfmcp.ErrBadReport)

// Inbox is the page's report handler, POST inbox, mounted behind
// livehttp.InboxGuard, which admits only the page's own reports. It
// decodes one report of at most 1 KiB, a JSON object whose members are
// among the strings kind, publication, node and focus, each at most once,
// and nothing after it, and hands it to srv.Report. It answers 204 when
// the report is recorded or taken for a repeated click, and 422 with the
// reason when the body or the report is refused; the reason never repeats
// what the page sent. Only POST is served.
func Inbox(srv *perfmcp.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "only POST is served", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxReport+1))
		if err != nil {
			http.Error(w, "the report could not be read", http.StatusBadRequest)
			return
		}
		if len(body) > maxReport {
			unprocessable(w, fmt.Errorf("%w: the body is over %d bytes", perfmcp.ErrBadReport, maxReport))
			return
		}
		rep, err := decodeReport(body)
		if err == nil {
			err = srv.Report(rep)
		}
		switch {
		case errors.Is(err, perfmcp.ErrBadReport):
			unprocessable(w, err)
		case err != nil:
			http.Error(w, "the report could not be recorded", http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
}

// decodeReport decodes body strictly: one JSON object whose members are
// among kind, publication, node and focus, each a string and at most once,
// matched exactly as written, with nothing after it.
func decodeReport(body []byte) (perfmcp.PageReport, error) {
	var rep perfmcp.PageReport
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return rep, errNotAReport
	}
	seen := map[string]bool{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return rep, errNotAReport
		}
		name, _ := key.(string)
		val, err := dec.Token()
		if err != nil {
			return rep, errNotAReport
		}
		s, ok := val.(string)
		if !ok || seen[name] {
			return rep, errNotAReport
		}
		seen[name] = true
		switch name {
		case "kind":
			rep.Kind = s
		case "publication":
			rep.Publication = s
		case "node":
			rep.Node = s
		case "focus":
			rep.Focus = s
		default:
			return rep, errNotAReport
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return rep, errNotAReport
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return rep, errNotAReport
	}
	return rep, nil
}
