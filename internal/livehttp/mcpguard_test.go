package livehttp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testBearer  = Token("mcp-test-token-0123456789")
	testMaxBody = 4096
)

// mcpPost is a POST /mcp that MCPGuard admits, with a body of n bytes.
func mcpPost(n int) *http.Request {
	req := httptest.NewRequest("POST", "http://127.0.0.1:7341/mcp", strings.NewReader(strings.Repeat(" ", n)))
	req.Header.Set("Authorization", "Bearer "+string(testBearer))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", strconv.Itoa(n))
	return req
}

// guardMCP runs req through MCPGuard over a counter.
func guardMCP(req *http.Request) (*httptest.ResponseRecorder, int64) {
	next := &counter{name: "next"}
	w := answer(MCPGuard(testBearer, testMaxBody, next), req)
	return w, next.calls.Load()
}

func TestMCPGuardAdmitsAWellFormedPost(t *testing.T) {
	w, calls := guardMCP(mcpPost(10))
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status %d, next calls %d; want 200 and 1", w.Code, calls)
	}
	if w.Header().Get("Connection") == "close" {
		t.Errorf("an admitted request closes the connection")
	}
}

func TestMCPGuardRefusesAnyOrigin(t *testing.T) {
	cases := map[string][]string{
		"empty":       {""},
		"same origin": {"http://127.0.0.1:7341"},
		"null":        {"null"},
		"other":       {"https://evil.example"},
		"repeated":    {"http://127.0.0.1:7341", "http://127.0.0.1:7341"},
	}
	for name, origins := range cases {
		for _, method := range []string{"POST", "DELETE", "GET"} {
			req := mcpPost(10)
			req.Method = method
			req.Header["Origin"] = origins
			w, calls := guardMCP(req)
			if w.Code != http.StatusForbidden || calls != 0 {
				t.Errorf("Origin %s, %s: status %d, next calls %d; want 403 and none", name, method, w.Code, calls)
			}
			if w.Header().Get("Connection") != "close" {
				t.Errorf("Origin %s, %s: refusal without Connection: close", name, method)
			}
		}
		// Refused before the bearer is looked at.
		req := mcpPost(10)
		req.Header.Del("Authorization")
		req.Header["Origin"] = origins
		if w, _ := guardMCP(req); w.Code != http.StatusForbidden {
			t.Errorf("Origin %s without a bearer: status %d, want 403", name, w.Code)
		}
	}
}

func TestMCPGuardRefusesBrowserMetadata(t *testing.T) {
	refused := []struct {
		name   string
		header string
		values []string
	}{
		{"Sec-Fetch-Site same-origin", "Sec-Fetch-Site", []string{"same-origin"}},
		{"Sec-Fetch-Site none", "Sec-Fetch-Site", []string{"none"}},
		{"Sec-Fetch-Site cross-site", "Sec-Fetch-Site", []string{"cross-site"}},
		{"Sec-Fetch-Site empty value", "Sec-Fetch-Site", []string{""}},
		{"Sec-Fetch-Site repeated", "Sec-Fetch-Site", []string{"same-origin", "same-origin"}},
		{"Sec-Fetch-Dest empty", "Sec-Fetch-Dest", []string{"empty"}},
		{"Sec-Fetch-Dest document", "Sec-Fetch-Dest", []string{"document"}},
		{"Sec-Fetch-Dest empty value", "Sec-Fetch-Dest", []string{""}},
		{"Sec-Fetch-Dest repeated", "Sec-Fetch-Dest", []string{"empty", "empty"}},
	}
	for _, tc := range refused {
		for _, mode := range []string{"", "cors"} {
			req := mcpPost(10)
			req.Header[tc.header] = tc.values
			if mode != "" {
				req.Header.Set("Sec-Fetch-Mode", mode)
			}
			w, calls := guardMCP(req)
			if w.Code != http.StatusForbidden || calls != 0 {
				t.Errorf("%s (Sec-Fetch-Mode %q): status %d, next calls %d; want 403 and none", tc.name, mode, w.Code, calls)
			}
		}
	}

	// A Node client (undici) sends Sec-Fetch-Mode: cors and nothing else.
	for _, mode := range []string{"cors", "navigate", "no-cors"} {
		req := mcpPost(10)
		req.Header.Set("Sec-Fetch-Mode", mode)
		if w, calls := guardMCP(req); w.Code != http.StatusOK || calls != 1 {
			t.Errorf("Sec-Fetch-Mode %s alone: status %d, next calls %d; want 200 and 1", mode, w.Code, calls)
		}
	}
}

// captured is one request as a real client sent it to a dummy listener.
type captured struct {
	Method string            `json:"m"`
	Path   string            `json:"p"`
	Header map[string]string `json:"h"`
	Body   string            `json:"body"`
}

func loadCaptures(t *testing.T) map[string][]captured {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "headers-*.jsonl"))
	if err != nil || len(files) < 2 {
		t.Fatalf("header captures: %v, %v", files, err)
	}
	out := make(map[string][]captured)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%v", err)
		}
		for line := range strings.Lines(string(data)) {
			var c captured
			if err := json.Unmarshal([]byte(line), &c); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			out[filepath.Base(file)] = append(out[filepath.Base(file)], c)
		}
	}
	return out
}

func TestRealClientHeadersPass(t *testing.T) {
	var (
		mu      sync.Mutex
		methods []string
		bodies  []int
	)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		mu.Lock()
		methods = append(methods, r.Header.Get("Mcp-Method"))
		bodies = append(bodies, int(n))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	h := startHubRig(t, next)

	sent, probes := 0, 0
	for file, reqs := range loadCaptures(t) {
		for i, c := range reqs {
			name := file + "#" + strconv.Itoa(i+1)
			var headers []header
			length := -1
			bearerSeen := false
			for k, v := range c.Header {
				switch strings.ToLower(k) {
				case "authorization":
					if v != "Bearer x" {
						t.Fatalf("%s: Authorization %q, want the dummy %q", name, v, "Bearer x")
					}
					v = "Bearer " + string(h.bearer)
					bearerSeen = true
				case "content-length":
					n, err := strconv.Atoi(v)
					if err != nil {
						t.Fatalf("%s: Content-Length %q", name, v)
					}
					length = n
				case "origin", "sec-fetch-site", "sec-fetch-dest", "sec-fetch-mode":
					t.Fatalf("%s: the capture carries %s", name, k)
				case "mcp-method":
					if v == "server/discover" {
						probes++
					}
				}
				headers = append(headers, header{k, v})
			}
			if !bearerSeen || length < len(c.Body) {
				t.Fatalf("%s: bearer %v, Content-Length %d for a %d-byte body prefix", name, bearerSeen, length, len(c.Body))
			}
			body := c.Body + strings.Repeat(" ", length-len(c.Body))

			conn := h.dial(t)
			if _, err := io.WriteString(conn, rawRequest(c.Method, c.Path, headers)+body); err != nil {
				t.Fatalf("%s: write: %v", name, err)
			}
			if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
				t.Fatalf("SetReadDeadline: %v", err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: status %d, want 200 from next", name, resp.StatusCode)
			}
			sent++
		}
	}
	if sent < 3 || probes != 1 {
		t.Fatalf("replayed %d requests with %d server/discover probes; want at least 3 and 1", sent, probes)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != sent {
		t.Fatalf("next called %d times for %d requests", len(bodies), sent)
	}
	for _, n := range bodies {
		if n == 0 {
			t.Errorf("next read an empty body")
		}
	}
	if !strings.Contains(strings.Join(methods, ","), "server/discover") {
		t.Errorf("the server/discover probe never reached next: %q", methods)
	}
}

func TestMCPGuardMethods(t *testing.T) {
	for _, method := range []string{"GET", "PUT", "OPTIONS", "PATCH", "HEAD", "CONNECT", "TRACE"} {
		req := httptest.NewRequest(method, "http://127.0.0.1:7341/mcp", nil)
		w, calls := guardMCP(req)
		if w.Code != http.StatusMethodNotAllowed || calls != 0 {
			t.Errorf("%s: status %d, next calls %d; want 405 and none", method, w.Code, calls)
		}
		if got := w.Header().Get("Allow"); got != "POST, DELETE" {
			t.Errorf("%s: Allow %q, want %q", method, got, "POST, DELETE")
		}
	}
	del := httptest.NewRequest("DELETE", "http://127.0.0.1:7341/mcp", nil)
	del.Header.Set("Authorization", "Bearer "+string(testBearer))
	if w, calls := guardMCP(del); w.Code != http.StatusOK || calls != 1 {
		t.Errorf("DELETE: status %d, next calls %d; want 200 and 1", w.Code, calls)
	}
}

func TestMCPGuardBearer(t *testing.T) {
	tok := string(testBearer)
	refused := map[string][]string{
		"missing":             nil,
		"empty":               {""},
		"wrong":               {"Bearer wrong"},
		"scheme only":         {"Bearer"},
		"scheme and space":    {"Bearer "},
		"basic":               {"Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+tok))},
		"trailing space":      {"Bearer " + tok + " "},
		"extra byte":          {"Bearer " + tok + "x"},
		"prefix of the token": {"Bearer " + tok[:len(tok)-1]},
		"tab separator":       {"Bearer\t" + tok},
		"no separator":        {"Bearer" + tok},
		"token alone":         {tok},
		"basic scheme":        {"Basic " + tok},
		"other scheme":        {"Token " + tok},
		"scheme prefix":       {"Bear " + tok},
		"repeated":            {"Bearer " + tok, "Bearer " + tok},
	}
	for name, values := range refused {
		req := mcpPost(10)
		req.Header.Del("Authorization")
		if values != nil {
			req.Header["Authorization"] = values
		}
		w, calls := guardMCP(req)
		if w.Code != http.StatusUnauthorized || calls != 0 {
			t.Errorf("%s: status %d, next calls %d; want 401 and none", name, w.Code, calls)
		}
		if got := w.Header().Values("Www-Authenticate"); len(got) != 0 {
			t.Errorf("%s: WWW-Authenticate %q, want none", name, got)
		}
		if strings.Contains(w.Body.String(), tok[:8]) {
			t.Errorf("%s: the answer repeats the token: %q", name, w.Body.String())
		}
	}
	for _, auth := range []string{"Bearer " + tok, "bearer " + tok, "BEARER " + tok, "Bearer  " + tok} {
		req := mcpPost(10)
		req.Header.Set("Authorization", auth)
		if w, calls := guardMCP(req); w.Code != http.StatusOK || calls != 1 {
			t.Errorf("Authorization %q: status %d, next calls %d; want 200 and 1", strings.Replace(auth, tok, "<tok>", 1), w.Code, calls)
		}
	}

	// A guard without a token admits nobody, an empty bearer included.
	for _, auth := range []string{"Bearer ", "Bearer"} {
		req := mcpPost(10)
		req.Header.Set("Authorization", auth)
		next := &counter{name: "next"}
		if w := answer(MCPGuard("", testMaxBody, next), req); w.Code != http.StatusUnauthorized || next.calls.Load() != 0 {
			t.Errorf("empty bearer token, Authorization %q: status %d; want 401", auth, w.Code)
		}
	}
}

func TestAcceptAndContentTypeAreParsedAsMediaRanges(t *testing.T) {
	accepted := [][]string{
		{"application/json;q=0.9, text/event-stream"},
		{"text/event-stream, application/json"},
		{"*/*"},
		{"application/json", "text/event-stream"},
		{"application/json, text/event-stream"},
		{"APPLICATION/JSON, Text/Event-Stream"},
		{"application/*, text/*"},
		{"application/json ; q=0.5 , text/event-stream;q=0.001"},
		{`text/event-stream;x="a,b", application/json`},
		{"text/html, */*;q=0.1"},
		// A range whose q is not an RFC 9110 qvalue is dropped, not read.
		{"application/json, text/event-stream, text/event-stream;q=NaN"},
		{"application/json, text/event-stream, text/event-stream;q=0x0p0"},
		{"application/json;q=1.000, text/event-stream;q=0.001"},
		{"application/json;q=1., text/event-stream;q=0.5"},
	}
	for _, accept := range accepted {
		req := mcpPost(10)
		req.Header["Accept"] = accept
		if w, calls := guardMCP(req); w.Code != http.StatusOK || calls != 1 {
			t.Errorf("Accept %q: status %d, next calls %d; want 200 and 1", accept, w.Code, calls)
		}
	}
	refused := [][]string{
		nil,
		{""},
		{"application/json"},
		{"text/event-stream"},
		{"text/event-stream;q=0, application/json"},
		{"application/json, text/event-stream;q=0"},
		{"application/json, text/event-stream;q=0.000"},
		{"*/*;q=0"},
		{"*/*, application/json;q=0"},
		{"text/*;q=0, */*"},
		{"text/html, application/xml"},
		{"application/json, text/event-stream;q=2"},
		{"application/json, text/event-stream;q=abc"},
		{"*/json, text/event-stream"},
		{"application/json;q=0x1p-1, text/event-stream"},
		{"application/json;q=1e0, text/event-stream"},
		{"application/json;q=5e-1, text/event-stream"},
		{"application/json;q=NaN, text/event-stream"},
		{"application/json;q=Inf, text/event-stream"},
		{"application/json;q=.5, text/event-stream"},
		{"application/json;q=01, text/event-stream"},
		{"application/json;q=1.0001, text/event-stream"},
		{"application/json;q=0.1234, text/event-stream"},
		{"application/json;q=1.5, text/event-stream"},
		{"application/json;q=+0.5, text/event-stream"},
		{"application/json;q=0_5, text/event-stream"},
		{"application/json;q=, text/event-stream"},
		{"application/json, text/event-stream;q=0."},
	}
	for _, accept := range refused {
		req := mcpPost(10)
		req.Header.Del("Accept")
		if accept != nil {
			req.Header["Accept"] = accept
		}
		if w, calls := guardMCP(req); w.Code != http.StatusBadRequest || calls != 0 {
			t.Errorf("Accept %q: status %d, next calls %d; want 400 and none", accept, w.Code, calls)
		}
	}

	for _, ct := range []string{"application/json", "Application/JSON; charset=utf-8", "application/json;charset=UTF-8"} {
		req := mcpPost(10)
		req.Header.Set("Content-Type", ct)
		if w, calls := guardMCP(req); w.Code != http.StatusOK || calls != 1 {
			t.Errorf("Content-Type %q: status %d, next calls %d; want 200 and 1", ct, w.Code, calls)
		}
	}
	for _, ct := range [][]string{
		nil, {""}, {"text/plain"}, {"application/json-seq"}, {"application/jsonx"},
		{"application/*"}, {"application/json; charset"}, {"application/json", "application/json"},
		{"text/plain; x=application/json"},
	} {
		req := mcpPost(10)
		req.Header.Del("Content-Type")
		if ct != nil {
			req.Header["Content-Type"] = ct
		}
		if w, calls := guardMCP(req); w.Code != http.StatusBadRequest || calls != 0 {
			t.Errorf("Content-Type %q: status %d, next calls %d; want 400 and none", ct, w.Code, calls)
		}
	}
}

func TestMCPGuardBodyRules(t *testing.T) {
	chunked := mcpPost(10)
	chunked.Header.Del("Content-Length")
	chunked.ContentLength = -1
	chunked.TransferEncoding = []string{"chunked"}
	if w, calls := guardMCP(chunked); w.Code != http.StatusLengthRequired || calls != 0 {
		t.Errorf("chunked: status %d, next calls %d; want 411 and none", w.Code, calls)
	}

	noLength := mcpPost(0)
	noLength.Header.Del("Content-Length")
	if w, calls := guardMCP(noLength); w.Code != http.StatusLengthRequired || calls != 0 {
		t.Errorf("no Content-Length: status %d, next calls %d; want 411 and none", w.Code, calls)
	}

	if w, calls := guardMCP(mcpPost(testMaxBody + 1)); w.Code != http.StatusRequestEntityTooLarge || calls != 0 {
		t.Errorf("maxBody+1: status %d, next calls %d; want 413 and none", w.Code, calls)
	}
	if w, calls := guardMCP(mcpPost(testMaxBody)); w.Code != http.StatusOK || calls != 1 {
		t.Errorf("maxBody: status %d, next calls %d; want 200 and 1", w.Code, calls)
	}

	del := mcpPost(10)
	del.Method = "DELETE"
	if w, calls := guardMCP(del); w.Code != http.StatusBadRequest || calls != 0 {
		t.Errorf("DELETE with a body: status %d, next calls %d; want 400 and none", w.Code, calls)
	}
	delChunked := mcpPost(10)
	delChunked.Method = "DELETE"
	delChunked.Header.Del("Content-Length")
	delChunked.ContentLength = -1
	delChunked.TransferEncoding = []string{"chunked"}
	if w, calls := guardMCP(delChunked); w.Code != http.StatusBadRequest || calls != 0 {
		t.Errorf("DELETE chunked: status %d, next calls %d; want 400 and none", w.Code, calls)
	}
	delEmpty := httptest.NewRequest("DELETE", "http://127.0.0.1:7341/mcp", nil)
	delEmpty.Header.Set("Authorization", "Bearer "+string(testBearer))
	delEmpty.Header.Set("Content-Length", "0")
	if w, calls := guardMCP(delEmpty); w.Code != http.StatusOK || calls != 1 {
		t.Errorf("DELETE with Content-Length 0: status %d, next calls %d; want 200 and 1", w.Code, calls)
	}
}

func TestMCPGuardRefusesNonPositiveMaxBody(t *testing.T) {
	for _, n := range []int64{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("MCPGuard with maxBody %d did not panic", n)
				}
			}()
			MCPGuard(testBearer, n, &counter{})
		}()
	}
}
