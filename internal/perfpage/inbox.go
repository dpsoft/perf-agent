package perfpage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/dpsoft/perf-agent/internal/perfmcp"
)

// maxReport is the largest report body Inbox decodes, as InboxGuard
// admits: room for an ask's 280-character question in any script.
const maxReport = 2048

// errNotAReport is the reason for a body that is not one report.
var errNotAReport = fmt.Errorf("%w: the body must be one JSON object of the strings kind, publication, node, focus and question, each at most once", perfmcp.ErrBadReport)

// errNotText is the reason for a body that decoding would change: one
// that is not UTF-8, or escapes half a surrogate pair, either of which
// encoding/json turns into U+FFFD.
var errNotText = fmt.Errorf("%w: the body must be valid UTF-8, with no unpaired surrogate escape", perfmcp.ErrBadReport)

// Inbox is the page's report handler, POST inbox, mounted behind
// livehttp.InboxGuard, which admits only the page's own reports. It
// decodes one report of at most 2 KiB, a JSON object whose members are
// among the strings kind, publication, node, focus and question, each at
// most once, and nothing after it, and hands it to srv.Report, which
// validates the question. It answers 204 when
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
// among kind, publication, node, focus and question, each a string and at
// most once, matched exactly as written, with nothing after it. The body
// must decode to the text it holds: UTF-8 with every surrogate escape
// paired.
func decodeReport(body []byte) (perfmcp.PageReport, error) {
	var rep perfmcp.PageReport
	if !utf8.Valid(body) || !pairedSurrogates(body) {
		return rep, errNotText
	}
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
		case "question":
			rep.Question = s
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

// pairedSurrogates reports whether every \u escape in body that names a
// surrogate is a high one followed at once by an escaped low one. An
// escape too short to read is left to the decoder, which refuses it.
func pairedSurrogates(body []byte) bool {
	escaped := func(i int) (rune, bool) {
		if i+6 > len(body) || body[i] != '\\' || body[i+1] != 'u' {
			return 0, false
		}
		n, err := strconv.ParseUint(string(body[i+2:i+6]), 16, 16)
		return rune(n), err == nil
	}
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			continue
		}
		r, ok := escaped(i)
		switch {
		case !ok:
			i++ // an escaped character, such as \\ or \"
		case !utf16.IsSurrogate(r):
			i += 5
		case r >= 0xdc00:
			return false // a low surrogate with no high one before it
		default:
			if low, ok := escaped(i + 6); !ok || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 11
		}
	}
	return true
}
