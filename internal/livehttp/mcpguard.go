package livehttp

import (
	"crypto/subtle"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// browserMetadata are the request headers that a browser adds to every
// request it sends to a loopback server, and that no MCP client sends.
// Sec-Fetch-Mode is not one of them: Node's fetch sends it on its own.
var browserMetadata = [...]string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Dest"}

// MCPGuard admits a request to the MCP endpoint only from a client holding
// the bearer token, and never from a browser. It runs behind Front, which has
// already checked Host. In order:
//   - any Origin, Sec-Fetch-Site or Sec-Fetch-Dest header, even empty or
//     repeated, gets 403: no browser is a client of this endpoint;
//   - a method other than POST or DELETE gets 405 with Allow: POST, DELETE;
//   - an Authorization that is not exactly "Bearer <bearer>" (the scheme in
//     any case) gets 401, with no WWW-Authenticate, so no client starts an
//     OAuth discovery. The token is compared in constant time;
//   - a POST needs an Accept whose media ranges admit both application/json
//     and text/event-stream, and Content-Type application/json, else 400;
//     a Content-Length, else 411 (chunked included); at most maxBody, else 413;
//   - a DELETE carries no body, else 400.
//
// Every refusal is answered from the headers, before any body byte is read,
// and closes the connection. An empty bearer admits nobody. maxBody must be
// positive.
func MCPGuard(bearer Token, maxBody int64, next http.Handler) http.Handler {
	if maxBody <= 0 {
		panic(fmt.Sprintf("livehttp.MCPGuard: maxBody %d must be positive", maxBody))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, key := range browserMetadata {
			if len(r.Header.Values(key)) > 0 {
				refuse(w, http.StatusForbidden, "browser requests are refused")
				return
			}
		}
		if r.Method != http.MethodPost && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "POST, DELETE")
			refuse(w, http.StatusMethodNotAllowed, "only POST and DELETE are served")
			return
		}
		if !bearerMatches(r.Header, bearer) {
			refuse(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		if r.Method == http.MethodDelete {
			if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
				refuse(w, http.StatusBadRequest, "DELETE takes no body")
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !acceptsBoth(r.Header.Values("Accept")) {
			refuse(w, http.StatusBadRequest, "Accept must admit application/json and text/event-stream")
			return
		}
		if !isJSON(r.Header) {
			refuse(w, http.StatusBadRequest, "Content-Type must be application/json")
			return
		}
		if code, msg := lengthProblem(r, maxBody); code != 0 {
			refuse(w, code, msg)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

// lengthProblem refuses a body sent chunked or without Content-Length (411),
// or longer than limit (413). It returns code 0 when the length is fine.
// net/http keeps the Content-Length header on a request, and drops it when
// the body is chunked.
func lengthProblem(r *http.Request, limit int64) (code int, msg string) {
	if len(r.TransferEncoding) > 0 || len(r.Header.Values("Content-Length")) == 0 {
		return http.StatusLengthRequired, "Content-Length is required"
	}
	if r.ContentLength > limit {
		return http.StatusRequestEntityTooLarge, "request body too large"
	}
	return 0, ""
}

// bearerMatches reports whether the one Authorization header is the bearer
// scheme, in any case, then one or more spaces and exactly tok.
func bearerMatches(h http.Header, tok Token) bool {
	auth, ok := single(h, "Authorization")
	if !ok || tok == "" {
		return false
	}
	scheme, cred, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	cred = strings.TrimLeft(cred, " ")
	return subtle.ConstantTimeCompare([]byte(cred), []byte(tok)) == 1
}

// isJSON reports whether the one Content-Type header is application/json,
// parameters allowed.
func isJSON(h http.Header) bool {
	ct, ok := single(h, "Content-Type")
	if !ok {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// acceptsBoth reports whether the Accept header values, taken together as
// one list of media ranges, give both application/json and
// text/event-stream a quality above zero.
func acceptsBoth(values []string) bool {
	var ranges []mediaRange
	for _, v := range values {
		for _, item := range splitList(v) {
			if mr, ok := parseRange(item); ok {
				ranges = append(ranges, mr)
			}
		}
	}
	return quality(ranges, "application", "json") > 0 && quality(ranges, "text", "event-stream") > 0
}

// mediaRange is one Accept item: type/subtype, either of which may be "*",
// and its quality.
type mediaRange struct {
	typ, sub string
	q        float64
}

// specificity orders the ranges that match a type: */* < type/* < type/sub.
func (m mediaRange) specificity() int {
	switch {
	case m.typ == "*":
		return 0
	case m.sub == "*":
		return 1
	default:
		return 2
	}
}

func (m mediaRange) matches(typ, sub string) bool {
	return m.typ == "*" || (m.typ == typ && (m.sub == "*" || m.sub == sub))
}

// parseRange parses one media range. A range that does not parse, a "*/x"
// range or a q outside [0, 1] is dropped.
func parseRange(item string) (mediaRange, bool) {
	mt, params, err := mime.ParseMediaType(item)
	if err != nil {
		return mediaRange{}, false
	}
	typ, sub, ok := strings.Cut(mt, "/")
	if !ok || typ == "" || sub == "" || (typ == "*" && sub != "*") {
		return mediaRange{}, false
	}
	q := 1.0
	if s, ok := params["q"]; ok {
		q, err = strconv.ParseFloat(s, 64)
		if err != nil || q < 0 || q > 1 {
			return mediaRange{}, false
		}
	}
	return mediaRange{typ: typ, sub: sub, q: q}, true
}

// quality is the q the most specific matching range gives typ/sub, or 0
// when none matches. Equally specific ranges give the highest of their q.
func quality(ranges []mediaRange, typ, sub string) float64 {
	best, q := -1, 0.0
	for _, m := range ranges {
		if !m.matches(typ, sub) {
			continue
		}
		switch s := m.specificity(); {
		case s > best:
			best, q = s, m.q
		case s == best:
			q = max(q, m.q)
		}
	}
	return q
}

// splitList splits a comma-separated header value, leaving commas inside
// quoted strings alone. Empty items are dropped.
func splitList(v string) []string {
	var (
		items  []string
		start  int
		quoted bool
	)
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			items = appendItem(items, v[start:i])
			start = i + 1
		}
	}
	return appendItem(items, v[start:])
}

func appendItem(items []string, item string) []string {
	if item = strings.TrimSpace(item); item != "" {
		items = append(items, item)
	}
	return items
}
